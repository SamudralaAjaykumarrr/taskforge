// Package invariant implements Phase 9's ("Chaos, Load, and Failure
// Testing", docs/roadmap.md) durable invariant checker: a set of read-only
// PostgreSQL queries that inspect durable state (never in-memory
// bookkeeping) and report violations of the exact TF-INV-* invariants
// documented in docs/invariants.md.
//
// This package exists so that "did any invariant break under chaos" has
// one, reusable, precise answer -- both the CI-safe seeded chaos
// campaigns in internal/chaos and cmd/chaos's manual stress/soak harness
// call Checker.CheckAll (and, for the invariants that need
// harness-tracked context rather than a pure snapshot -- TF-INV-001 --
// Checker.CheckJobsExist) against the same real database the campaign
// just ran against.
//
// Every check here queries durable PostgreSQL state directly (the same
// authority docs/testing-strategy.md requires of every other test in this
// repository -- "PostgreSQL integration tests run against a real
// PostgreSQL instance ... never a mock"). Nothing here is inferred from
// logs or metrics: per docs/roadmap.md's Phase 9 scope, "Durable state
// remains authoritative" and "Do not use telemetry alone as proof."
//
// Not every TF-INV-* is expressible as a point-in-time snapshot query.
// TF-INV-003/010/013/015 are about the *rejection* of a specific stale or
// racing write, which is proven by the deterministic scenario tests in
// internal/store/internal/worker (SF-008, SF-012, SF-014, SF-016/017) and
// by this package's TF-INV-002/014 monotonicity check (a stale write that
// *had* succeeded would show up here as a reused or non-monotonic
// lease_generation) -- see each check's doc comment for exactly which
// invariant it proves and what it deliberately leaves to the dedicated
// scenario suite instead of re-deriving here.
package invariant

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Violation is one durable-state fact that contradicts a documented
// TF-INV-*. Subject identifies the concrete row(s) involved (e.g.
// "job:<uuid>" or "workflow:<uuid>") so a failing seed can be reproduced
// against exactly that row.
type Violation struct {
	InvariantID string
	Subject     string
	Detail      string
}

func (v Violation) String() string {
	return fmt.Sprintf("[%s] %s: %s", v.InvariantID, v.Subject, v.Detail)
}

// Checker runs every durable-state invariant check against db.
type Checker struct {
	db *sql.DB
}

// New returns a Checker backed by db. db is never mutated by any method
// here -- every check is a plain SELECT.
func New(db *sql.DB) *Checker {
	return &Checker{db: db}
}

// checkFunc is one named invariant check.
type checkFunc struct {
	name string
	fn   func(context.Context) ([]Violation, error)
}

// checks lists every invariant this package proves from a durable-state
// snapshot, in docs/invariants.md ID order.
func (c *Checker) checks() []checkFunc {
	return []checkFunc{
		{"TF-INV-002/014", c.checkLeaseGenerationMonotonicUnique},
		{"TF-INV-005", c.checkNoAttemptAfterTerminal},
		{"TF-INV-006", c.checkAttemptCountWithinLimit},
		{"TF-INV-007", c.checkAttemptHistoryMonotonic},
		{"TF-INV-008/016", c.checkIdempotencyKeyUniqueness},
		{"TF-INV-009", c.checkDeadLetteredHasReason},
		{"TF-INV-012", c.checkWorkflowDependencyGating},
		{"TF-INV-012", c.checkWorkflowTerminality},
	}
}

// CheckAll runs every durable-state invariant check and returns every
// violation found across all of them (nil if none). A query failure
// itself (e.g. context deadline) is returned as an error, distinct from a
// violation -- callers must not treat "the checker itself failed to run"
// as "no violations found."
func (c *Checker) CheckAll(ctx context.Context) ([]Violation, error) {
	var all []Violation
	for _, chk := range c.checks() {
		v, err := chk.fn(ctx)
		if err != nil {
			return nil, fmt.Errorf("invariant: %s check: %w", chk.name, err)
		}
		all = append(all, v...)
	}
	return all, nil
}

// CheckJobsExist proves the harness-observable half of TF-INV-001 ("an
// accepted job is never silently lost"): every id a chaos/load campaign
// recorded as successfully submitted (Insert/InsertIdempotent returned
// without error) must still have a durable row. This cannot be derived
// from a pure snapshot the way the other checks can -- it needs the
// campaign's own bookkeeping of which IDs it believes it created -- so it
// is a separate method rather than part of CheckAll.
func (c *Checker) CheckJobsExist(ctx context.Context, ids []uuid.UUID) ([]Violation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT id FROM jobs WHERE id = ANY($1::uuid[])`, uuidArrayLiteral(ids))
	if err != nil {
		return nil, fmt.Errorf("invariant: check jobs exist: %w", err)
	}
	defer rows.Close()

	present := make(map[uuid.UUID]bool, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("invariant: check jobs exist: scan: %w", err)
		}
		present[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("invariant: check jobs exist: %w", err)
	}

	var violations []Violation
	for _, id := range ids {
		if !present[id] {
			violations = append(violations, Violation{
				InvariantID: "TF-INV-001",
				Subject:     "job:" + id.String(),
				Detail:      "job was acknowledged as submitted but no longer has a durable row",
			})
		}
	}
	return violations, nil
}

// checkAttemptCountWithinLimit proves TF-INV-006: attempt_count must
// never exceed max_attempts.
func (c *Checker) checkAttemptCountWithinLimit(ctx context.Context) ([]Violation, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, attempt_count, max_attempts FROM jobs WHERE attempt_count > max_attempts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Violation
	for rows.Next() {
		var id uuid.UUID
		var attemptCount, maxAttempts int
		if err := rows.Scan(&id, &attemptCount, &maxAttempts); err != nil {
			return nil, err
		}
		out = append(out, Violation{
			InvariantID: "TF-INV-006",
			Subject:     "job:" + id.String(),
			Detail:      fmt.Sprintf("attempt_count=%d exceeds max_attempts=%d", attemptCount, maxAttempts),
		})
	}
	return out, rows.Err()
}

// checkAttemptHistoryMonotonic proves TF-INV-007: every job's
// job_attempts.attempt_number values are gapless, 1-based, and match
// attempt_count exactly, with no duplicate attempt_number for the same
// job (append-only, never overwritten in place).
func (c *Checker) checkAttemptHistoryMonotonic(ctx context.Context) ([]Violation, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT j.id, j.attempt_count,
			COUNT(a.id) AS attempt_rows,
			COALESCE(MAX(a.attempt_number), 0) AS max_attempt_number,
			COUNT(DISTINCT a.attempt_number) AS distinct_attempt_numbers
		FROM jobs j LEFT JOIN job_attempts a ON a.job_id = j.id
		GROUP BY j.id, j.attempt_count
		HAVING j.attempt_count <> COUNT(a.id)
			OR COALESCE(MAX(a.attempt_number), 0) <> j.attempt_count
			OR COUNT(a.id) <> COUNT(DISTINCT a.attempt_number)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Violation
	for rows.Next() {
		var id uuid.UUID
		var attemptCount, attemptRows, maxAttemptNumber, distinctAttemptNumbers int
		if err := rows.Scan(&id, &attemptCount, &attemptRows, &maxAttemptNumber, &distinctAttemptNumbers); err != nil {
			return nil, err
		}
		out = append(out, Violation{
			InvariantID: "TF-INV-007",
			Subject:     "job:" + id.String(),
			Detail: fmt.Sprintf(
				"attempt_count=%d but job_attempts has %d rows (max attempt_number=%d, %d distinct) -- not gapless/1-based/duplicate-free",
				attemptCount, attemptRows, maxAttemptNumber, distinctAttemptNumbers),
		})
	}
	return out, rows.Err()
}

// checkLeaseGenerationMonotonicUnique proves TF-INV-002 ("at most one
// valid lease per job ... lease_generation is strictly increasing per
// job") and TF-INV-014 (a stale generation can never succeed, which -- if
// violated -- would show up here as a lease_generation value reused
// across two attempts of the same job): every job_attempts.lease_generation
// value is unique within a job, and generations are strictly increasing
// in attempt order.
func (c *Checker) checkLeaseGenerationMonotonicUnique(ctx context.Context) ([]Violation, error) {
	dup, err := c.db.QueryContext(ctx, `
		SELECT job_id, lease_generation, COUNT(*)
		FROM job_attempts
		GROUP BY job_id, lease_generation
		HAVING COUNT(*) > 1`)
	if err != nil {
		return nil, err
	}
	var out []Violation
	for dup.Next() {
		var jobID uuid.UUID
		var gen int64
		var count int
		if err := dup.Scan(&jobID, &gen, &count); err != nil {
			dup.Close()
			return nil, err
		}
		out = append(out, Violation{
			InvariantID: "TF-INV-002",
			Subject:     "job:" + jobID.String(),
			Detail:      fmt.Sprintf("lease_generation %d issued %d times for the same job", gen, count),
		})
	}
	if err := dup.Err(); err != nil {
		dup.Close()
		return nil, err
	}
	dup.Close()

	ordered, err := c.db.QueryContext(ctx, `
		SELECT job_id, attempt_number, lease_generation
		FROM job_attempts
		ORDER BY job_id, attempt_number ASC`)
	if err != nil {
		return nil, err
	}
	defer ordered.Close()

	var prevJob uuid.UUID
	var prevGen int64 = -1
	haveEver := false
	for ordered.Next() {
		var jobID uuid.UUID
		var attemptNumber int
		var gen int64
		if err := ordered.Scan(&jobID, &attemptNumber, &gen); err != nil {
			return nil, err
		}
		if !haveEver || jobID != prevJob {
			prevJob, prevGen, haveEver = jobID, gen, true
			continue
		}
		if gen <= prevGen {
			out = append(out, Violation{
				InvariantID: "TF-INV-014",
				Subject:     "job:" + jobID.String(),
				Detail:      fmt.Sprintf("lease_generation did not strictly increase across attempts (saw %d after %d)", gen, prevGen),
			})
		}
		prevGen = gen
	}
	return out, ordered.Err()
}

// checkDeadLetteredHasReason proves TF-INV-009: a DEAD_LETTERED job
// always carries a non-null last_error/last_error_class explaining why it
// stopped being retried.
func (c *Checker) checkDeadLetteredHasReason(ctx context.Context) ([]Violation, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id FROM jobs
		WHERE state = 'DEAD_LETTERED' AND (last_error IS NULL OR last_error_class IS NULL)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Violation
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, Violation{
			InvariantID: "TF-INV-009",
			Subject:     "job:" + id.String(),
			Detail:      "DEAD_LETTERED job is missing last_error/last_error_class",
		})
	}
	return out, rows.Err()
}

// checkNoAttemptAfterTerminal proves TF-INV-005 ("terminal states never
// become non-terminal", docs/invariants.md: "SUCCEEDED, CANCELLED, and
// DEAD_LETTERED are terminal. No transition out of a terminal state
// exists, in the state machine or in any code path."). It reports a
// violation for either of two independent, durable facts -- either is
// sufficient on its own:
//
//  1. CURRENTLY reopened: terminal_at IS NOT NULL but the job's CURRENT
//     state is not SUCCEEDED/CANCELLED/DEAD_LETTERED. terminal_at is
//     written to a non-NULL value ONLY in the same statement that also
//     sets jobs.state to one of those three states (grep every
//     "terminal_at = now()" site in internal/store: complete.go,
//     cancellation.go, retry.go's exhaustion branch, claim.go's lazy
//     sweep, workflow.go's cascade-cancel), and claimQuery's own reclaim
//     UPDATE never touches terminal_at -- so a stale, non-cleared
//     terminal_at next to a non-terminal state is exactly the artifact a
//     buggy reclaim of an already-terminal row would leave behind, and
//     comparing two columns on the same row/statement needs no
//     cross-transaction ordering at all.
//
//  2. HISTORICALLY reopened, even after a second terminalization masked
//     case 1: a job_attempts row exists whose attempt_number is greater
//     than jobs.terminal_attempt_count (migration 0004). Case 1 alone
//     misses a job that was terminalized, illegitimately reclaimed
//     (reopening it), and then reached a terminal state a SECOND time --
//     at that point jobs.state reads terminal again and jobs.terminal_at
//     has been overwritten with the second terminalization's timestamp,
//     so case 1's query sees nothing wrong. terminal_attempt_count closes
//     this gap: every terminal_at-writing statement also sets it via
//     `COALESCE(jobs.terminal_attempt_count, jobs.attempt_count)`, so it
//     captures attempt_count exactly once -- on the transition that FIRST
//     made the job terminal -- and no code path ever assigns it a second
//     time. Because job_attempts is append-only and gapless
//     (TF-INV-007: attempt_number is unique, 1-based, and matches
//     attempt_count exactly), any attempt_number found above that frozen
//     count is proof a new attempt was opened after the job had already,
//     durably, reached a terminal state at least once -- regardless of
//     what jobs.state/terminal_at read now. This covers all three
//     terminal states uniformly, including CANCELLED reached with zero
//     job_attempts rows at all (CancelQueuedOrRetryWait,
//     resolveDependent's cascade-cancel): terminal_attempt_count is still
//     captured (as 0) on that transition, so any later attempt_number > 0
//     is caught the same way.
//
// Because terminal_attempt_count is now correctness-critical to case 2,
// this same check ALSO reports malformed marker state directly, rather
// than silently trusting it -- a future terminalization code path that
// writes terminal_at/state but forgets terminal_attempt_count would
// otherwise silently disable case 2's historical detection for that row,
// with nothing here ever reporting why. Three durable shapes are
// self-evidently invalid, independent of anything else on the row:
//
//   - MISSING: terminal_at IS NOT NULL but terminal_attempt_count IS
//     NULL. After migration 0004 (which backfills every pre-existing
//     terminal row), every supported terminalization path sets both
//     columns in the same statement -- see complete.go, cancellation.go,
//     retry.go's exhaustion branch, claim.go's sweep, and workflow.go's
//     resolveDependent, all of which write
//     `terminal_attempt_count = COALESCE(jobs.terminal_attempt_count,
//     jobs.attempt_count)` in the identical UPDATE that sets terminal_at
//     = now(). A terminal row with the marker still NULL is proof some
//     write path bypassed that convention.
//   - NEGATIVE: terminal_attempt_count < 0. attempt_count itself is never
//     negative (it only ever increments), so a negative marker cannot be
//     a legitimate snapshot of it.
//   - EXCEEDS: terminal_attempt_count > attempt_count. The marker is
//     defined as attempt_count captured on the FIRST terminalization, and
//     attempt_count only ever increases afterward (every claim
//     increments it, and job_attempts is append-only/gapless per
//     TF-INV-007) -- so it can never durably exceed the job's current
//     attempt_count.
//
// These are reported under TF-INV-005, not a new invariant ID: all three
// are the marker's own durable meaning being contradicted, exactly the
// same property (an untrustworthy or absent record of "was this job ever
// terminal before") case 2 above depends on -- a separate invariant ID
// would split one property across two IDs for no reader benefit. See
// TestCheckAll_DetectsMissingTerminalAttemptCountMarker,
// TestCheckAll_DetectsNegativeTerminalAttemptCount, and
// TestCheckAll_DetectsTerminalAttemptCountExceedsAttemptCount for the
// regression proofs.
//
// Both of the original two cases compare durable integer/enum columns on
// rows serialized by PostgreSQL's own row-level locking (every write
// above requires the same row's UPDATE lock) -- neither ever compares a
// wall-clock timestamp written by one transaction against one written by
// a different transaction. An earlier version of this check instead
// compared
// job_attempts.started_at (written by the transaction that opened an
// attempt) against jobs.terminal_at (written by a LATER, causally
// downstream transaction) and flagged started_at > terminal_at as a
// reopening. That comparison was unsound: PostgreSQL's now() is fixed at
// each transaction's own BEGIN from the OS wall clock, and the wall clock
// is not guaranteed monotonic -- an NTP/hypervisor correction landing
// between two transactions can make a causally-earlier transaction's
// now() read LATER than a causally-later transaction's now(), even though
// row-level locking strictly serialized them in the other order. This
// produced real false positives under `go test -race` (observed as a
// ~30-40ms backward wall-clock step approximately every 30s under
// sustained load in this project's WSL2 CI/dev environment -- long
// enough for a heavy chaos run to cross one). See
// TestCheckAll_NoFalsePositiveOnClockSkewedAttemptTimestamp for that
// regression proof, TestCheckAll_DetectsTerminalStateReopened for proof
// case 1 still detects a currently-reopened terminal state, and
// TestCheckAll_DetectsTerminalStateReopenedThenReterminalized for proof
// case 2 detects a reopening masked by a second terminalization -- the
// exact gap case 1 alone cannot see.
func (c *Checker) checkNoAttemptAfterTerminal(ctx context.Context) ([]Violation, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT j.id, j.state, j.terminal_at, j.terminal_attempt_count, j.attempt_count,
			(SELECT COUNT(*) FROM job_attempts a WHERE a.job_id = j.id) AS attempt_rows,
			(SELECT COALESCE(MAX(a.attempt_number), 0) FROM job_attempts a WHERE a.job_id = j.id) AS max_attempt_number
		FROM jobs j
		WHERE (j.terminal_at IS NOT NULL AND j.state NOT IN ('SUCCEEDED', 'CANCELLED', 'DEAD_LETTERED'))
		   OR (j.terminal_attempt_count IS NOT NULL AND EXISTS (
		         SELECT 1 FROM job_attempts a
		         WHERE a.job_id = j.id AND a.attempt_number > j.terminal_attempt_count
		       ))
		   OR (j.terminal_at IS NOT NULL AND j.terminal_attempt_count IS NULL)
		   OR (j.terminal_attempt_count IS NOT NULL AND j.terminal_attempt_count < 0)
		   OR (j.terminal_attempt_count IS NOT NULL AND j.terminal_attempt_count > j.attempt_count)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Violation
	for rows.Next() {
		var jobID uuid.UUID
		var state string
		var terminalAt sql.NullTime
		var terminalAttemptCount sql.NullInt64
		var attemptCount, attemptRows, maxAttemptNumber int
		if err := rows.Scan(&jobID, &state, &terminalAt, &terminalAttemptCount, &attemptCount, &attemptRows, &maxAttemptNumber); err != nil {
			return nil, err
		}

		currentlyReopened := terminalAt.Valid && state != "SUCCEEDED" && state != "CANCELLED" && state != "DEAD_LETTERED"
		historicallyReopened := terminalAttemptCount.Valid && int64(maxAttemptNumber) > terminalAttemptCount.Int64
		missingMarker := terminalAt.Valid && !terminalAttemptCount.Valid
		negativeMarker := terminalAttemptCount.Valid && terminalAttemptCount.Int64 < 0
		markerExceedsAttempts := terminalAttemptCount.Valid && terminalAttemptCount.Int64 > int64(attemptCount)

		var reasons []string
		if currentlyReopened {
			reasons = append(reasons, fmt.Sprintf(
				"terminal_at=%s is set but current state=%s is not terminal (SUCCEEDED/CANCELLED/DEAD_LETTERED) -- terminal state was reopened (%d job_attempts row(s) on record)",
				terminalAt.Time.Format(time.RFC3339Nano), state, attemptRows))
		}
		if historicallyReopened {
			reasons = append(reasons, fmt.Sprintf(
				"job_attempts has attempt_number up to %d, beyond terminal_attempt_count=%d recorded when this job first became terminal (current state=%s) -- terminal state was reopened and then re-terminalized, masking current-state detection (%d job_attempts row(s) on record)",
				maxAttemptNumber, terminalAttemptCount.Int64, state, attemptRows))
		}
		if missingMarker {
			reasons = append(reasons, fmt.Sprintf(
				"terminal_at=%s is set but terminal_attempt_count is NULL -- every supported terminalization path must capture it atomically (migration 0004); historical reopen detection is silently disabled for this row",
				terminalAt.Time.Format(time.RFC3339Nano)))
		}
		if negativeMarker {
			reasons = append(reasons, fmt.Sprintf(
				"terminal_attempt_count=%d is negative -- attempt_count never decreases below 0, so this cannot be a legitimate first-terminalization snapshot",
				terminalAttemptCount.Int64))
		}
		if markerExceedsAttempts {
			reasons = append(reasons, fmt.Sprintf(
				"terminal_attempt_count=%d exceeds current attempt_count=%d -- the marker can never durably exceed the count it was captured from",
				terminalAttemptCount.Int64, attemptCount))
		}
		if len(reasons) == 0 {
			// Unreachable: the WHERE clause above is the exact disjunction
			// of the five conditions checked here.
			continue
		}

		out = append(out, Violation{
			InvariantID: "TF-INV-005",
			Subject:     "job:" + jobID.String(),
			Detail:      strings.Join(reasons, "; "),
		})
	}
	return out, rows.Err()
}

// checkIdempotencyKeyUniqueness proves TF-INV-008/016: a given
// (job_type, idempotency_key) pair maps to at most one job row.
func (c *Checker) checkIdempotencyKeyUniqueness(ctx context.Context) ([]Violation, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT job_type, idempotency_key, COUNT(*)
		FROM jobs
		WHERE idempotency_key IS NOT NULL
		GROUP BY job_type, idempotency_key
		HAVING COUNT(*) > 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Violation
	for rows.Next() {
		var jobType, key string
		var count int
		if err := rows.Scan(&jobType, &key, &count); err != nil {
			return nil, err
		}
		out = append(out, Violation{
			InvariantID: "TF-INV-008",
			Subject:     fmt.Sprintf("idempotency_key:%s/%s", jobType, key),
			Detail:      fmt.Sprintf("%d job rows share (job_type, idempotency_key)", count),
		})
	}
	return out, rows.Err()
}

// checkWorkflowDependencyGating proves TF-INV-012's gating half: a
// workflow node with one or more declared dependencies must never be
// claimed (attempt_count > 0, i.e. RUNNING/SUCCEEDED/RETRY_WAIT/
// DEAD_LETTERED-via-a-genuine-attempt) while any of its predecessors'
// underlying jobs has not reached SUCCEEDED. Because activation
// (advancing eligible_at off the blocked sentinel) only ever happens
// inside the same transaction as a predecessor's own terminal SUCCEEDED
// write (internal/store/workflow.go's resolveDependent), and TF-INV-005
// guarantees a SUCCEEDED job never un-succeeds, this snapshot check is
// race-free: if a dependent was ever claimed, every one of its
// predecessors must be durably SUCCEEDED right now, not just "was
// SUCCEEDED at claim time."
func (c *Checker) checkWorkflowDependencyGating(ctx context.Context) ([]Violation, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT wn.id, wn.job_id
		FROM workflow_nodes wn
		JOIN jobs j ON j.id = wn.job_id
		WHERE array_length(wn.depends_on, 1) > 0
		  AND j.attempt_count > 0
		  AND EXISTS (
		      SELECT 1 FROM workflow_nodes pred
		      JOIN jobs pj ON pj.id = pred.job_id
		      WHERE pred.id = ANY(wn.depends_on) AND pj.state <> 'SUCCEEDED'
		  )`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Violation
	for rows.Next() {
		var nodeID, jobID uuid.UUID
		if err := rows.Scan(&nodeID, &jobID); err != nil {
			return nil, err
		}
		out = append(out, Violation{
			InvariantID: "TF-INV-012",
			Subject:     "workflow_node:" + nodeID.String(),
			Detail:      fmt.Sprintf("job %s was claimed despite an unsatisfied predecessor dependency", jobID),
		})
	}
	return out, rows.Err()
}

// checkWorkflowTerminality proves the workflow-level half of TF-INV-012 /
// docs/workflows.md's finalization contract: once every node in a
// workflow has reached a terminal job state, the owning workflow_instances
// row must itself already be terminal (finalization happens in the same
// transaction as the last node's own terminal write -- see
// finalizeWorkflowIfComplete) -- a workflow left RUNNING despite every
// node being done is a stuck/lost finalization, not a legitimate
// in-progress state.
func (c *Checker) checkWorkflowTerminality(ctx context.Context) ([]Violation, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT wi.id
		FROM workflow_instances wi
		WHERE wi.state = 'RUNNING'
		  AND EXISTS (SELECT 1 FROM workflow_nodes wn WHERE wn.workflow_instance_id = wi.id)
		  AND NOT EXISTS (
		      SELECT 1 FROM workflow_nodes wn JOIN jobs j ON j.id = wn.job_id
		      WHERE wn.workflow_instance_id = wi.id
		        AND j.state NOT IN ('SUCCEEDED', 'DEAD_LETTERED', 'CANCELLED')
		  )`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Violation
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, Violation{
			InvariantID: "TF-INV-012",
			Subject:     "workflow:" + id.String(),
			Detail:      "every node has reached a terminal job state but the workflow itself is still RUNNING",
		})
	}
	return out, rows.Err()
}

// uuidArrayLiteral renders ids as a PostgreSQL array-literal string (e.g.
// "{id1,id2}"), mirroring internal/store/workflow.go's helper of the same
// name and rationale: this sidesteps any question of whether the
// database/sql driver in use (pgx/v5's stdlib wrapper) natively marshals
// []uuid.UUID as a PostgreSQL array -- PostgreSQL's own array input
// function parses this text form identically regardless of protocol.
func uuidArrayLiteral(ids []uuid.UUID) string {
	if len(ids) == 0 {
		return "{}"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	out := "{"
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out + "}"
}
