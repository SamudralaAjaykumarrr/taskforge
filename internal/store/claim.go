// Lease acquisition and reclaim: the Phase 2 claim query from
// docs/worker-protocol.md "Claiming", including its expired-lease RUNNING
// branch (the mechanism behind TF-INV-004) and the Lazy Dead-Letter Sweep
// that must run immediately before it (the mechanism behind TF-INV-006 on
// the reclaim path).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
)

// Attempt outcomes this package writes to job_attempts.outcome. These are
// the subset of docs/data-model.md's documented outcome enum that Phase
// 2's code paths actually produce — see migrations/0002_*.up.sql's
// comment for why the CHECK constraint permits the full enum already.
const (
	attemptOutcomeSucceeded       = "SUCCEEDED"
	attemptOutcomeFailedPermanent = "FAILED_PERMANENT"
	attemptOutcomeFailedRetryable = "FAILED_RETRYABLE"
	attemptOutcomeLeaseExpired    = "LEASE_EXPIRED"
	// attemptOutcomeTimedOut is Phase 6's execution-timeout attempt
	// outcome -- see retry.go's CompleteTimeout.
	attemptOutcomeTimedOut = "TIMED_OUT"
)

// pickClaimCandidateQuery is Phase 13's non-locking "pick" step (ADR-0009,
// following the two-step "non-locking pick, then targeted locking claim"
// pattern its own evidence package proved out --
// tools/phase13bench/claim_fairness.go's claimFairnessV2). It never takes
// a row lock: it is a plain SELECT that finds the single best (job,
// queue) candidate across every roster queue, so a concurrent claim
// attempt that picks the same winner only contends at the SECOND step
// (claimWithSlotQuery/claimPlainFreshQuery/claimPlainReclaimQuery below),
// not here -- and that second step is itself a queue-scoped scan, not a
// targeted single-row lookup, so concurrent contenders for the same
// winning queue still diverge onto different rows within one statement.
//
// For each queue_state row (optionally restricted to $1's subscription
// list -- NULL means "every queue," the worker-queue-blind compatibility
// default, docs/phase-13-plan.md §14), the LATERAL finds that queue's own
// single best candidate. The fresh-claim branch (QUEUED/RETRY_WAIT) and
// the reclaim branch (expired-lease RUNNING) are fetched as two SEPARATE,
// each-LIMIT-1 subqueries combined with UNION ALL, rather than one
// `WHERE (...) OR (...)` spanning both of idx_jobs_claimable_by_queue and
// idx_jobs_reclaimable_by_queue (migration 0012) -- an OR spanning two
// differently-shaped partial indexes plus a shared ORDER BY defeats index
// usage entirely at realistic backlog size, exactly the defect this
// phase's own concurrency evidence found and fixed
// (docs/phase-13-concurrency-evidence-v2.md; see migration 0012's own
// comment for the measured numbers). The outer per-queue ORDER BY/LIMIT 1
// then picks the single best of the (at most two) unioned rows.
//
// "Capacity-eligible" (ADR-0009's roster-membership definition) is
// evaluated PER CANDIDATE, inside each branch, before the two branches
// are collapsed to a single per-queue winner: a reclaim (old_state =
// 'RUNNING') is always capacity-eligible (it already holds its slot --
// see "Reclaim/slot-ownership semantics" in the ADR), unconditionally; a
// fresh candidate is capacity-eligible only if the queue has no
// queue_slots rows at all (unconfigured/unlimited -- the pre-Phase-13
// compatibility default) or has at least one free (held_by_job_id IS
// NULL) slot right now.
//
// This ordering matters and was previously wrong (independent review
// finding H1, docs/adr/0009-phase-13-concurrency-and-fairness.md's
// definitions still govern the property, this is an implementation
// correction, not a semantics change): an earlier version of this query
// evaluated capacity-eligibility only AFTER the two branches were already
// collapsed to one per-queue winner by (priority DESC, eligible_at ASC).
// If that collapsed winner happened to be fresh-shaped and the queue was
// at capacity, the ENTIRE queue was filtered out of the roster for that
// pick -- even when a different, lower-ranked candidate in the SAME queue
// was a reclaim, which is unconditionally capacity-eligible and should
// have won instead. Since priority is not settable via any public API
// (internal/job.NewParams has no Priority field), every job's priority is
// the schema default (0), so this was reachable by nothing more exotic
// than a later-submitted fresh job whose eligible_at happened to sort
// ahead of an already-reclaim-eligible job sharing its queue -- silently
// and indefinitely starving the reclaim, a TF-INV-004 violation. Filtering
// each branch by its own capacity-eligibility rule BEFORE the collapse
// means an ineligible fresh candidate simply never enters the union in
// the first place, so it can never hide an eligible reclaim candidate
// behind it; the queue's per-queue winner is now always drawn from
// whichever candidates are actually admittable. A queue leaves the roster
// (produces no row here) if and only if genuinely NEITHER branch has any
// eligible candidate left, matching the ADR's "Roster membership -- exit"
// definition exactly. See internal/store/phase13_claim_selection_fix_test.go
// for the regression test (reproduced against the pre-fix query, fails
// there, passes here).
//
// is_limited is returned alongside the winner so the caller's targeted
// step 2 knows, without a second lookup, whether it must go through the
// slot-acquiring variant.
//
// Final selection orders by qs.last_claimed_at ascending (ADR-0009's
// fairness ordering -- ties, most commonly every queue's shared
// '-infinity' default on a cold start, break by each candidate's own
// priority DESC, eligible_at ASC, exactly the tie-break the pre-Phase-13
// claim query already used).
const pickClaimCandidateQuery = `
	SELECT c.id, qs.queue_name, c.old_state,
	       EXISTS (SELECT 1 FROM queue_slots qsl WHERE qsl.queue_name = qs.queue_name) AS is_limited
	FROM queue_state qs
	JOIN LATERAL (
		SELECT id, old_state, priority, eligible_at
		FROM (
			(SELECT id, state AS old_state, priority, eligible_at
			 FROM jobs
			 WHERE queue_name = qs.queue_name AND state IN ('QUEUED', 'RETRY_WAIT') AND eligible_at <= now()
			   AND (
			         NOT EXISTS (SELECT 1 FROM queue_slots qsl WHERE qsl.queue_name = qs.queue_name)
			      OR EXISTS (SELECT 1 FROM queue_slots qsl WHERE qsl.queue_name = qs.queue_name AND qsl.held_by_job_id IS NULL)
			       )
			 ORDER BY priority DESC, eligible_at ASC LIMIT 1)
			UNION ALL
			(SELECT id, state AS old_state, priority, eligible_at
			 FROM jobs
			 WHERE queue_name = qs.queue_name AND state = 'RUNNING' AND lease_expires_at < now() AND attempt_count < max_attempts
			 ORDER BY priority DESC, eligible_at ASC LIMIT 1)
		) branch
		ORDER BY priority DESC, eligible_at ASC
		LIMIT 1
	) c ON true
	WHERE ($1::text[] IS NULL OR qs.queue_name = ANY($1::text[]))
	ORDER BY qs.last_claimed_at ASC, c.priority DESC, c.eligible_at ASC
	LIMIT 1`

// claimCandidate is pickClaimCandidateQuery's winning row.
type claimCandidate struct {
	id        uuid.UUID
	queueName string
	oldState  jobstate.State
	limited   bool // true iff this queue has any provisioned queue_slots rows
}

// queueNameArrayLiteral renders queues as a PostgreSQL text[] array
// literal for a `$n::text[]` parameter, escaping backslashes and double
// quotes per PostgreSQL's array-literal syntax (queue names are
// operator-supplied free-form text, OD-8 -- unlike
// internal/store/workflow.go's uuidArrayLiteral, which can join UUIDs
// with bare commas because a UUID's own syntax can never contain one,
// this cannot assume the same about an arbitrary queue name). A nil slice
// (the worker-queue-blind default) renders as a real SQL NULL, not an
// empty array -- `$1::text[] IS NULL` in pickClaimCandidateQuery is what
// distinguishes "no subscription filter at all" from "subscribed to
// nothing," and passing Go nil through database/sql already produces a
// NULL bind value, so no explicit sentinel is needed for that case.
func queueNameArrayLiteral(queues []string) any {
	if queues == nil {
		return nil
	}
	parts := make([]string, len(queues))
	for i, q := range queues {
		escaped := strings.ReplaceAll(q, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		parts[i] = `"` + escaped + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// pickClaimCandidate runs the non-locking pick step above. found is false
// (with a nil error) when no roster queue currently has a pending,
// capacity-eligible candidate -- a normal outcome, not an error.
func pickClaimCandidate(ctx context.Context, tx *sql.Tx, subscribedQueues []string) (claimCandidate, bool, error) {
	var c claimCandidate
	var oldState string
	err := tx.QueryRowContext(ctx, pickClaimCandidateQuery, queueNameArrayLiteral(subscribedQueues)).
		Scan(&c.id, &c.queueName, &oldState, &c.limited)
	if errors.Is(err, sql.ErrNoRows) {
		return claimCandidate{}, false, nil
	}
	if err != nil {
		return claimCandidate{}, false, err
	}
	c.oldState = jobstate.State(oldState)
	return c, true, nil
}

// claimWithSlotQuery is step 2's fresh-claim, capacity-limited variant.
//
// It is deliberately a QUEUE-SCOPED SCAN (WHERE queue_name = $1 ...
// ORDER BY ... FOR UPDATE SKIP LOCKED LIMIT 1), not a lookup targeted at
// the one specific job id pickClaimCandidate happened to find, and the
// slot half is the same kind of scan over queue_slots. This is a
// deliberate, evidence-driven correction to an earlier draft of this
// mechanism (caught by TestMetrics_ConcurrentRecording_RaceSafe: 50
// workers racing 50 same-queue jobs claimed as few as 6 of them when step
// 2 targeted one fixed id, because many concurrent callers' non-locking
// pick step deterministically agree on the SAME globally-best candidate,
// and only one can win a targeted lock on it -- exactly the "many
// concurrent workers ... converged on the identical single winning row"
// failure mode tools/phase13bench/claim_concurrency.go's own comment
// documents for a single-queue, no-fairness-alternative shape). Scoping
// each SKIP LOCKED clause to the whole winning queue (not one row)
// restores this codebase's foundational per-statement divergence
// property -- PostgreSQL itself makes every concurrent scanner skip a
// row (or slot) another has already locked and move on to the
// next-best one, WITHIN this one statement, for both the job and the
// slot independently. It still uses idx_jobs_claimable_by_queue (a
// single, unqualified index scan -- no OR across branches, since the
// caller already knows, from pickClaimCandidate's own old_state, that
// this candidate is fresh-shaped, not a reclaim).
const claimWithSlotQuery = `
	WITH candidate AS (
		SELECT id, state AS old_state, attempt_count AS old_attempt_count
		FROM jobs
		WHERE queue_name = $1 AND state IN ('QUEUED', 'RETRY_WAIT') AND eligible_at <= now()
		ORDER BY priority DESC, eligible_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	), slot AS (
		SELECT slot_index
		FROM queue_slots
		WHERE queue_name = $1 AND held_by_job_id IS NULL
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	)
	UPDATE jobs
	SET state = 'RUNNING',
		lease_owner = $2,
		lease_generation = jobs.lease_generation + 1,
		lease_expires_at = now() + make_interval(secs => jobs.execution_timeout_seconds),
		heartbeat_at = now(),
		attempt_count = jobs.attempt_count + 1,
		updated_at = now(),
		cancel_requested = false,
		cancel_requested_at = NULL,
		version = jobs.version + 1
	FROM candidate, slot
	WHERE jobs.id = candidate.id
	RETURNING ` + jobColumns + `, candidate.old_state, candidate.old_attempt_count, slot.slot_index`

// claimPlainFreshQuery is step 2's variant for a fresh claim on a queue
// with no provisioned queue_slots rows at all (unconfigured, unlimited
// concurrency -- the pre-Phase-13 compatibility default). Queue-scoped
// SKIP LOCKED scan, same divergence reasoning as claimWithSlotQuery
// above, using idx_jobs_claimable_by_queue alone (no OR, no reclaim
// branch -- pickClaimCandidate already determined this candidate is
// fresh-shaped).
const claimPlainFreshQuery = `
	WITH candidate AS (
		SELECT id, state AS old_state, attempt_count AS old_attempt_count
		FROM jobs
		WHERE queue_name = $1 AND state IN ('QUEUED', 'RETRY_WAIT') AND eligible_at <= now()
		ORDER BY priority DESC, eligible_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	)
	UPDATE jobs
	SET state = 'RUNNING',
		lease_owner = $2,
		lease_generation = jobs.lease_generation + 1,
		lease_expires_at = now() + make_interval(secs => jobs.execution_timeout_seconds),
		heartbeat_at = now(),
		attempt_count = jobs.attempt_count + 1,
		updated_at = now(),
		cancel_requested = false,
		cancel_requested_at = NULL,
		version = jobs.version + 1
	FROM candidate
	WHERE jobs.id = candidate.id
	RETURNING ` + jobColumns + `, candidate.old_state, candidate.old_attempt_count`

// claimPlainReclaimQuery is step 2's variant for a reclaim (old_state =
// 'RUNNING' -- never touches queue_slots, per SF-052/the ADR's
// "Reclaim/slot-ownership semantics": a reclaimed job retains and reuses
// its original capacity-slot binding, so reclaiming it must never
// allocate, free, or otherwise write a slot row). Queue-scoped SKIP
// LOCKED scan using idx_jobs_reclaimable_by_queue alone.
const claimPlainReclaimQuery = `
	WITH candidate AS (
		SELECT id, state AS old_state, attempt_count AS old_attempt_count
		FROM jobs
		WHERE queue_name = $1 AND state = 'RUNNING' AND lease_expires_at < now() AND attempt_count < max_attempts
		ORDER BY priority DESC, eligible_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	)
	UPDATE jobs
	SET state = 'RUNNING',
		lease_owner = $2,
		lease_generation = jobs.lease_generation + 1,
		lease_expires_at = now() + make_interval(secs => jobs.execution_timeout_seconds),
		heartbeat_at = now(),
		attempt_count = jobs.attempt_count + 1,
		updated_at = now(),
		cancel_requested = false,
		cancel_requested_at = NULL,
		version = jobs.version + 1
	FROM candidate
	WHERE jobs.id = candidate.id
	RETURNING ` + jobColumns + `, candidate.old_state, candidate.old_attempt_count`

func scanClaimJob(row rowScanner) (*job.Job, jobstate.State, int, error) {
	var f jobScanFields
	var oldState string
	var oldAttemptCount int
	dest := append(f.dest(), &oldState, &oldAttemptCount)
	if err := row.Scan(dest...); err != nil {
		return nil, "", 0, err
	}
	return f.materialize(), jobstate.State(oldState), oldAttemptCount, nil
}

// scanClaimJobWithSlot is scanClaimJob's counterpart for claimWithSlotQuery,
// whose RETURNING clause carries one extra column (the acquired slot's
// slot_index) so the caller can mark that exact slot held in a following
// statement without a second lookup.
func scanClaimJobWithSlot(row rowScanner) (*job.Job, jobstate.State, int, int, error) {
	var f jobScanFields
	var oldState string
	var oldAttemptCount int
	var slotIndex int
	dest := append(f.dest(), &oldState, &oldAttemptCount, &slotIndex)
	if err := row.Scan(dest...); err != nil {
		return nil, "", 0, 0, err
	}
	return f.materialize(), jobstate.State(oldState), oldAttemptCount, slotIndex, nil
}

// releaseSlot durably releases the queue_slots row (if any) held by
// jobID, in the caller's transaction. It is idempotent and safe to call
// unconditionally: a job that never held a slot (an unlimited queue, or a
// job that was already released) simply matches zero rows -- matching by
// held_by_job_id (not by queue_name/slot_index) means this can never
// disturb a DIFFERENT job's later, legitimate hold on the same slot index
// (SF-058's idempotency requirement). Called from every path that stops a
// job being RUNNING: CompleteSuccess, CompleteFailure, CompleteCancelled,
// completeRetryableOutcome (both its RETRY_WAIT and DEAD_LETTERED
// destinations -- a job that stops being RUNNING must not still hold a
// slot regardless of which non-RUNNING state it lands in, or SF-053's
// per-row invariant (every RUNNING job holds exactly one slot, every held
// slot belongs to exactly one RUNNING job) would be violated the moment a
// RETRY_WAIT job kept its slot across a claim cycle it does not occupy),
// and the Lazy Dead-Letter Sweep (via releaseSlotsForJobs below, batched).
func releaseSlot(ctx context.Context, tx *sql.Tx, jobID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `UPDATE queue_slots SET held_by_job_id = NULL WHERE held_by_job_id = $1`, jobID)
	return err
}

// releaseSlotsForJobs is releaseSlot batched for the Lazy Dead-Letter
// Sweep, which can dead-letter many jobs in one statement. A no-op for an
// empty slice.
func releaseSlotsForJobs(ctx context.Context, tx *sql.Tx, jobIDs []uuid.UUID) error {
	if len(jobIDs) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE queue_slots SET held_by_job_id = NULL WHERE held_by_job_id = ANY($1::uuid[])`,
		uuidArrayLiteral(jobIDs))
	return err
}

// upsertLastClaimedAt advances queueName's fairness clock to now(),
// unconditionally -- ADR-0009: "a successful claim (fresh or reclaim)
// updates it to the current time," with no distinction between the two
// (SF-057). The INSERT ... ON CONFLICT form (rather than a plain UPDATE)
// is defense in depth against a queue_name that somehow reached this point
// without an existing queue_state row (every job-insert path upserts one
// eagerly -- internal/store/idempotency.go, tx.go, workflow.go -- but a
// plain UPDATE would silently do nothing, not fail, against a missing
// row, which would make a real fairness bug invisible).
func upsertLastClaimedAt(ctx context.Context, tx *sql.Tx, queueName string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO queue_state (queue_name, last_claimed_at) VALUES ($1, now())
		ON CONFLICT (queue_name) DO UPDATE SET last_claimed_at = now()`, queueName)
	return err
}

// Claim is the compatibility entry point: it claims from every queue,
// exactly as every pre-Phase-13 caller's behavior must remain
// (docs/phase-13-plan.md §14 -- a worker with no queue-subscription
// configuration retains today's exact queue-blind behavior, including
// reclaiming every queue's expired leases). It is claim(ctx, workerID,
// nil) -- see that method for the full claim/reclaim/slot/fairness
// mechanism.
func (s *Store) Claim(ctx context.Context, workerID string) (*job.Job, bool, error) {
	return s.claim(ctx, workerID, nil)
}

// ClaimFromQueues is Phase 13's queue-subscription-aware entry point
// (docs/phase-13-plan.md §6b/§8): identical to Claim, except eligibility
// is additionally restricted to queueNames -- applied identically to both
// the fresh-claim branch and the expired-lease reclaim branch (§6b's
// settled rule: a worker's subscription is one authorization boundary,
// not two). A nil or empty queueNames means "subscribed to nothing" here
// (this is the EXPLICIT-subscription entry point; a worker that wants the
// queue-blind default calls Claim instead, never this method with a nil
// slice) -- internal/worker.Worker chooses between the two based on its
// own configured subscription, so this method's own zero-value behavior
// is never relied upon by production code.
func (s *Store) ClaimFromQueues(ctx context.Context, workerID string, queueNames []string) (*job.Job, bool, error) {
	if len(queueNames) == 0 {
		return nil, false, nil
	}
	return s.claim(ctx, workerID, queueNames)
}

// claim is the shared implementation behind Claim and ClaimFromQueues: it
// atomically finds and takes ownership of at most one eligible job for
// workerID, restricted to subscribedQueues (nil means every queue --
// Claim's compatibility behavior). Both a freshly QUEUED/RETRY_WAIT job
// and a RUNNING job whose lease has expired with attempt budget remaining
// (reclaim) transition to RUNNING under a strictly incremented
// lease_generation (TF-INV-002) -- the same transaction and the same
// generation-increment arithmetic handle both cases, exactly as
// docs/architecture.md describes ("there is no separate reaper process
// required for correctness").
//
// Phase 13 (ADR-0009) adds two mechanisms on top of that unchanged core:
//
//   - Queue-subscription filtering (§6b): subscribedQueues restricts which
//     queue_state rows the non-locking pick step (pickClaimCandidate)
//     considers, identically for both branches.
//   - Slot-table concurrency limiting + last_claimed_at fairness: the pick
//     step orders roster candidates by fairness and filters by capacity
//     eligibility; the second, queue-scoped-scan step (claimWithSlotQuery
//     for a fresh claim on a capacity-limited queue, claimPlainReclaimQuery
//     for every reclaim, claimPlainFreshQuery for every unlimited-queue
//     fresh claim) performs the actual locking claim, atomically acquiring
//     a slot only in the first case
//     (SF-051/SF-052/SF-053). Every successful claim -- fresh or reclaim
//     alike -- advances the claimed queue's last_claimed_at (SF-057).
//
// Before claiming, this runs the Lazy Dead-Letter Sweep
// (docs/worker-protocol.md) in the same transaction, so an
// attempt-budget-exhausted expired lease is dead-lettered rather than
// reclaimed (TF-INV-006); Phase 13 extends that sweep to also release the
// swept job's capacity slot, durably and idempotently, in the same
// transaction (SF-058) — see sweepExpiredExhaustedLeases.
//
// When this claim is a reclaim (the previous state was RUNNING), the
// superseded attempt's job_attempts row is finalized (outcome
// LEASE_EXPIRED) in the SAME transaction as the claim itself, so the
// ledger and the job row can never disagree about whether the previous
// attempt is still open (TF-INV-013).
//
// The second return value is false (with a nil error) both when nothing
// is eligible to claim right now, and when the non-locking pick step's
// winning candidate was claimed by a concurrent transaction (or its slot
// exhausted) before this method's own targeted, locking attempt ran --
// both are normal, expected outcomes under contention, not errors; the
// caller's next poll tries again, exactly as it already does for the
// ordinary "nothing eligible" case.
//
// Phase 8: after a successful commit, this records
// taskforge_claim_latency_seconds/taskforge_queue_age_seconds (computed
// entirely from the DB-returned eligible_at/updated_at -- no extra
// query) and distinguishes, via oldState (already computed above for the
// superseded-attempt-finalization decision), a genuine lease-expiry
// reclaim (oldState == RUNNING; taskforge_lease_expirations_total,
// event="job_reclaimed") from an ordinary post-backoff RETRY_WAIT claim
// (event="job_claimed") -- see recordClaim's doc comment for why this
// distinction requires no new query or schema change, only surfacing
// data Claim already computes. Metrics/logging for any jobs the Lazy
// Dead-Letter Sweep dead-lettered this same transaction are recorded via
// recordSweptJobs, using data collected by sweepExpiredExhaustedLeases
// but only emitted after this transaction has actually committed (a
// sweep whose enclosing transaction rolls back must never be recorded).
func (s *Store) claim(ctx context.Context, workerID string, subscribedQueues []string) (*job.Job, bool, error) {
	for _, from := range []jobstate.State{jobstate.Queued, jobstate.RetryWait, jobstate.Running} {
		if !jobstate.IsValidTransition(from, jobstate.Running) {
			return nil, false, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, jobstate.Running)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("store: claim: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	swept, err := s.sweepExpiredExhaustedLeases(ctx, tx)
	if err != nil {
		return nil, false, fmt.Errorf("store: claim: %w", err)
	}

	// maxPickAndLockAttempts bounds the pick-then-lock retry loop below.
	// Unlike the pre-Phase-13 claimQuery (one atomic FOR UPDATE SKIP
	// LOCKED statement scanning the whole eligible set, so many concurrent
	// claimers naturally diverge onto different rows within that single
	// statement), the two-step "non-locking pick, then targeted locking
	// claim" pattern this ADR selects (tools/phase13bench/claim_fairness.go's
	// claimFairnessV2) has a real race window between the two steps: many
	// concurrent callers' non-locking pick can compute the IDENTICAL
	// globally-best (queue, job) candidate -- deterministically, from the
	// same committed state -- especially with few roster queues, where
	// there is little for them to naturally diverge onto. Only one wins
	// the second, targeted lock; every other single pick+lock attempt
	// would otherwise report "nothing claimed" even though other, merely
	// slightly-less-fair candidates remain genuinely claimable. Retrying
	// the pick (which reflects each retry's own fresh, post-loss committed
	// state -- READ COMMITTED gives each statement in this still-open
	// transaction its own snapshot) resolves this: the row or slot that
	// was just lost is no longer a candidate on the next iteration, so
	// concurrent claimers converge, lose, retry, and redistribute onto
	// the remaining work within the SAME Claim call, rather than each
	// separately waiting a full pollInterval to try again. This bound
	// only caps a single call's own retries; it never changes what gets
	// claimed or which queue's fairness turn is honored -- every retry
	// re-runs the identical, unmodified pick/lock logic.
	const maxPickAndLockAttempts = 32

	var j *job.Job
	var oldState jobstate.State
	var cand claimCandidate
	var acquiringSlot bool
	var slotIndex int

	for attempt := 0; attempt < maxPickAndLockAttempts; attempt++ {
		var found bool
		cand, found, err = pickClaimCandidate(ctx, tx, subscribedQueues)
		if err != nil {
			return nil, false, fmt.Errorf("store: claim: pick candidate: %w", err)
		}
		if !found {
			if cerr := tx.Commit(); cerr != nil {
				return nil, false, fmt.Errorf("store: claim: commit (nothing eligible): %w", cerr)
			}
			s.recordSweptJobs(swept)
			return nil, false, nil
		}

		switch {
		case cand.oldState == jobstate.Running:
			acquiringSlot = false
			row := tx.QueryRowContext(ctx, claimPlainReclaimQuery, cand.queueName, workerID)
			j, oldState, _, err = scanClaimJob(row)
		case cand.limited:
			acquiringSlot = true
			row := tx.QueryRowContext(ctx, claimWithSlotQuery, cand.queueName, workerID)
			j, oldState, _, slotIndex, err = scanClaimJobWithSlot(row)
		default:
			acquiringSlot = false
			row := tx.QueryRowContext(ctx, claimPlainFreshQuery, cand.queueName, workerID)
			j, oldState, _, err = scanClaimJob(row)
		}
		if err == nil {
			break // won this candidate
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("store: claim: %w", err)
		}
		// Lost the race between the non-locking pick and this targeted
		// attempt (a concurrent claimer took the row, or the free slot we
		// were counting on, first) -- retry with a fresh pick; see the
		// loop's own doc comment above.
		j = nil
	}
	if j == nil {
		// Every retry lost the race -- a normal outcome under sustained
		// contention, not an error; the caller's next poll tries again,
		// exactly as the ordinary "nothing eligible" case already does.
		if cerr := tx.Commit(); cerr != nil {
			return nil, false, fmt.Errorf("store: claim: commit (lost race): %w", cerr)
		}
		s.recordSweptJobs(swept)
		return nil, false, nil
	}

	if acquiringSlot {
		if _, err := tx.ExecContext(ctx,
			`UPDATE queue_slots SET held_by_job_id = $1 WHERE queue_name = $2 AND slot_index = $3`,
			j.ID, cand.queueName, slotIndex,
		); err != nil {
			return nil, false, fmt.Errorf("store: claim: acquire slot: %w", err)
		}
	}

	if err := upsertLastClaimedAt(ctx, tx, cand.queueName); err != nil {
		return nil, false, fmt.Errorf("store: claim: update fairness state: %w", err)
	}

	var supersededStartedAt time.Time
	var haveSupersededStartedAt bool
	if oldState == jobstate.Running {
		startedAt, err := finalizeOpenAttempt(ctx, tx, j.ID, attemptOutcomeLeaseExpired, "", "")
		if err != nil {
			return nil, false, fmt.Errorf("store: claim: finalize superseded attempt: %w", err)
		}
		supersededStartedAt, haveSupersededStartedAt = startedAt, true
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO job_attempts (id, job_id, attempt_number, lease_generation, worker_id, started_at)
		VALUES ($1, $2, $3, $4, $5, now())`,
		uuid.New(), j.ID, j.AttemptCount, j.LeaseGeneration, workerID,
	); err != nil {
		return nil, false, fmt.Errorf("store: claim: record attempt: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("store: claim: commit: %w", err)
	}

	s.recordSweptJobs(swept)
	s.recordClaim(j, oldState, workerID, supersededStartedAt, haveSupersededStartedAt)
	return j, true, nil
}

// recordClaim records this commit's claim-latency histograms and
// distinguishes a genuine lease-expiry reclaim from an ordinary claim,
// per Claim's doc comment above. Called only after Claim's transaction
// has committed.
//
// oldState == jobstate.Running is the ONLY way a claim reaches this
// point via claimQuery's second WHERE branch (state = 'RUNNING' AND
// lease_expires_at < now()) -- a currently-RUNNING job with a still-valid
// lease can never match either branch, so this is an exact signal, not a
// heuristic. Prior to Phase 8, internal/worker approximated this using
// "lease_generation > 1," which is WRONG as of Phase 3: lease_generation
// and attempt_count are incremented together on every single claim
// (fresh or reclaimed), so that heuristic also fires on attempt 2+ of an
// ordinary RETRY_WAIT claim after backoff, mislabeling every routine
// retry as "job reclaimed after lease expiration" in the log. Store
// already computes the real oldState for the superseded-attempt-
// finalization decision above; this just surfaces it accurately instead
// of re-deriving an ambiguous proxy in the caller. See
// docs/observability.md's "Implementation Notes" for this fix and its
// regression test.
func (s *Store) recordClaim(j *job.Job, oldState jobstate.State, workerID string, supersededStartedAt time.Time, haveSupersededStartedAt bool) {
	claimLatency := j.UpdatedAt.Sub(j.EligibleAt)
	if claimLatency < 0 {
		claimLatency = 0
	}
	s.metrics.ClaimLatencySeconds.Observe(claimLatency.Seconds())
	s.metrics.QueueAgeSeconds.Observe(claimLatency.Seconds())

	if oldState == jobstate.Running {
		s.metrics.LeaseExpirationsTotal.WithLabelValues(j.JobType).Inc()
		s.metrics.JobsCompletedTotal.WithLabelValues(j.JobType, metrics.OutcomeLeaseExpired).Inc()
		if haveSupersededStartedAt {
			s.metrics.ExecutionDurationSeconds.
				WithLabelValues(j.JobType, metrics.OutcomeLeaseExpired).
				Observe(j.UpdatedAt.Sub(supersededStartedAt).Seconds())
		}
		s.logger.Warn("job reclaimed after lease expiration",
			"event", "job_reclaimed", "job_id", j.ID.String(), "job_type", j.JobType,
			"worker_id", workerID, "attempt", j.AttemptCount, "lease_generation", j.LeaseGeneration,
			"previous_lease_generation", j.LeaseGeneration-1, "state", string(j.State))
		return
	}

	s.logger.Info("job claimed",
		"event", "job_claimed", "job_id", j.ID.String(), "job_type", j.JobType,
		"worker_id", workerID, "attempt", j.AttemptCount, "lease_generation", j.LeaseGeneration,
		"state", string(j.State))
}

// recordSweptJobs records metrics/logging for every job the Lazy
// Dead-Letter Sweep moved straight to DEAD_LETTERED in this same,
// now-committed transaction. Unlike an ordinary completion, no worker
// ever "claims" or "completes" these jobs -- they are detected and
// dead-lettered purely as a side effect of some OTHER job's Claim call
// -- so this is the only place in the system that can attribute this
// event to metrics/logs at all. taskforge_execution_duration_seconds is
// deliberately NOT observed here: doing so would require an extra query
// per swept job purely for a diagnostic histogram, which
// docs/roadmap.md's "avoid a database query per log event" performance
// guidance rules out -- see docs/observability.md's "Implementation
// Notes".
func (s *Store) recordSweptJobs(swept []sweptJob) {
	for _, sj := range swept {
		s.metrics.JobsCompletedTotal.WithLabelValues(sj.jobType, metrics.OutcomeLeaseExpired).Inc()
		s.metrics.JobsDeadLetteredTotal.WithLabelValues(sj.jobType).Inc()
		s.metrics.RetryCount.WithLabelValues(sj.jobType).Observe(float64(sj.attemptCount))
		s.logger.Warn("job dead-lettered by lazy sweep: lease expired, retry budget exhausted",
			"event", "dead_lettered", "job_id", sj.id.String(), "job_type", sj.jobType,
			"attempt", sj.attemptCount, "last_error_class", "LEASE_EXPIRED", "retryable", false,
			"state", string(jobstate.DeadLettered))
		s.logWorkflowFinalized(sj.workflowID, sj.workflowFinalState)
	}
}

// sweepExpiredExhaustedLeases is docs/worker-protocol.md's "Lazy
// Dead-Letter Sweep": before claiming, move any expired-lease RUNNING job
// that has already exhausted its retry budget straight to DEAD_LETTERED,
// so the claim query's reclaim branch never has to reclaim (and its
// defense-in-depth "AND attempt_count < max_attempts" clause never has
// to reject) a row past its attempt budget.
//
// A method (not a bare function) since Phase 7: a swept job may be a
// workflow node, and its dependents must be cascade-cancelled exactly as
// they would be if a worker had explicitly reported this same permanent
// exhaustion via CompleteRetryableFailure -- see
// propagateWorkflowTransition below. Without this, a workflow node that
// exhausts its retry budget purely via repeated lease expiration (no
// worker ever calls a Complete* method for its final attempt) would leave
// its dependents blocked forever, violating TF-INV-004's "no permanently
// stranded" guarantee at the workflow level.
//
// Phase 8: returns the job_type/attempt_count of every job it swept, so
// Claim can record metrics/logging for them AFTER its enclosing
// transaction has actually committed (see recordSweptJobs) -- this
// method itself performs no observability side effect, since its work is
// not yet durable until the caller's transaction commits.
func (s *Store) sweepExpiredExhaustedLeases(ctx context.Context, tx *sql.Tx) ([]sweptJob, error) {
	// SF-053 regression (TaskForge issue #24): this is a multi-row UPDATE
	// -- unlike every other jobs-table write in this package, it is not
	// bounded to a single targeted row (WHERE id = $1) or protected by
	// SKIP LOCKED (the claim pick queries' FOR UPDATE SKIP LOCKED ... LIMIT
	// 1). It must take a blocking row lock on EVERY currently
	// expired-and-exhausted row across the whole table, in one statement,
	// so no candidate can be silently left un-dead-lettered (SKIP LOCKED
	// would be wrong here: a row this sweep skips stays wrongly RUNNING
	// until some later Claim call's sweep catches it, which is still
	// eventually correct but was rejected in favor of matching this
	// method's existing "resolved by the time this transaction commits"
	// contract). Without an explicit ORDER BY, PostgreSQL gives no
	// guarantee that two concurrent executions of this identical statement
	// lock an overlapping row set in the same relative order (the chosen
	// scan path, e.g. idx_jobs_reclaimable_by_queue vs. a sequential/
	// bitmap scan, can differ run to run as the table's size and
	// statistics change over the table's lifetime) -- when it doesn't,
	// two concurrent sweeps can each hold a lock the other is waiting on,
	// a genuine PostgreSQL deadlock (SQLSTATE 40P01), not a serialization
	// anomaly needing a retry. The CTE below sorts every
	// candidate row by its primary key BEFORE locking it (confirmed by
	// EXPLAIN: the LockRows node sits above the Sort node, so FOR UPDATE
	// acquires locks in that sorted order, not scan order) -- every
	// concurrent sweep now locks its overlapping rows in the same
	// ascending-id order, which makes a lock cycle between two sweeps
	// impossible, exactly the discipline this package's own claim queries
	// already apply to their own single-row FOR UPDATE picks. See
	// TestSlotTable_SF053_SweepDeadlockFreeUnderConcurrentClaims.
	rows, err := tx.QueryContext(ctx, `
		WITH to_sweep AS (
			SELECT id
			FROM jobs
			WHERE state = 'RUNNING'
			  AND lease_expires_at < now()
			  AND attempt_count >= max_attempts
			ORDER BY id
			FOR UPDATE
		)
		UPDATE jobs
		SET state = 'DEAD_LETTERED',
			lease_owner = NULL,
			lease_expires_at = NULL,
			last_error = COALESCE(last_error, 'lease expired, retry budget exhausted'),
			last_error_class = 'LEASE_EXPIRED',
			terminal_at = now(),
			terminal_attempt_count = COALESCE(jobs.terminal_attempt_count, jobs.attempt_count),
			updated_at = now(),
			version = version + 1
		FROM to_sweep
		WHERE jobs.id = to_sweep.id
		RETURNING jobs.id, jobs.job_type, jobs.attempt_count`)
	if err != nil {
		return nil, fmt.Errorf("sweep expired-exhausted leases: %w", err)
	}

	var swept []sweptJob
	for rows.Next() {
		var sj sweptJob
		if scanErr := rows.Scan(&sj.id, &sj.jobType, &sj.attemptCount); scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("sweep expired-exhausted leases: scan: %w", scanErr)
		}
		swept = append(swept, sj)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sweep expired-exhausted leases: %w", err)
	}

	// Phase 13 (ADR-0009, SF-058): release every swept job's capacity slot
	// in the SAME transaction as its DEAD_LETTERED transition -- durable
	// (a crash immediately after this transaction's commit cannot leave
	// the slot ambiguous, since the release and the state transition
	// either both commit or neither does) and idempotent (releaseSlotsForJobs
	// matches by held_by_job_id, so a second sweep pass against an
	// already-DEAD_LETTERED job -- whose slot this already released -- is
	// a no-op: it cannot re-release, or otherwise disturb, a slot a
	// different job may since have legitimately claimed). This is the
	// specific call site the ADR's own drafting found completely
	// untested by every prior evidence pass -- see "Reclaim/slot-ownership
	// semantics" in docs/adr/0009-phase-13-concurrency-and-fairness.md.
	if len(swept) > 0 {
		ids := make([]uuid.UUID, len(swept))
		for i := range swept {
			ids[i] = swept[i].id
		}
		if err := releaseSlotsForJobs(ctx, tx, ids); err != nil {
			return nil, fmt.Errorf("sweep expired-exhausted leases: release slots: %w", err)
		}
	}

	for i := range swept {
		if _, err := finalizeOpenAttempt(ctx, tx, swept[i].id, attemptOutcomeLeaseExpired, "", ""); err != nil {
			return nil, fmt.Errorf("sweep expired-exhausted leases: finalize attempt for job %s: %w", swept[i].id, err)
		}
		workflowID, finalState, err := s.propagateWorkflowTransition(ctx, tx, swept[i].id, jobstate.DeadLettered)
		if err != nil {
			return nil, fmt.Errorf("sweep expired-exhausted leases: propagate workflow for job %s: %w", swept[i].id, err)
		}
		swept[i].workflowID, swept[i].workflowFinalState = workflowID, finalState
	}
	return swept, nil
}

// sweptJob is one job the Lazy Dead-Letter Sweep moved to DEAD_LETTERED
// in the current transaction — see sweepExpiredExhaustedLeases and
// recordSweptJobs. workflowFinalState is set only if sweeping this job
// is what finalized its owning workflow (see propagateWorkflowTransition's
// doc comment); "" otherwise, including for a non-workflow job.
type sweptJob struct {
	id                 uuid.UUID
	jobType            string
	attemptCount       int
	workflowID         uuid.UUID
	workflowFinalState string
}

// finalizeOpenAttempt closes out the single still-open (finished_at IS
// NULL) job_attempts row for jobID, if any. At most one attempt is ever
// open for a given job at a time (a new attempt row is only inserted once
// Claim has either found no open attempt or already finalized the
// previous one in the same transaction — see Claim above), so matching on
// "open" rather than a specific attempt_number/lease_generation is
// sufficient and avoids the caller needing to know which attempt number
// is currently open.
//
// Phase 8: returns the finalized attempt's started_at, so callers can
// compute taskforge_execution_duration_seconds as (this transaction's
// now(), already available to them as a RETURNING column on their own
// UPDATE) minus started_at, with no additional query.
func finalizeOpenAttempt(ctx context.Context, tx *sql.Tx, jobID uuid.UUID, outcome, errClass, errMessage string) (time.Time, error) {
	var startedAt time.Time
	err := tx.QueryRowContext(ctx, `
		UPDATE job_attempts
		SET finished_at = now(),
			outcome = $2,
			error_class = NULLIF($3, ''),
			error_message = NULLIF($4, '')
		WHERE job_id = $1 AND finished_at IS NULL
		RETURNING started_at`,
		jobID, outcome, errClass, errMessage,
	).Scan(&startedAt)
	return startedAt, err
}
