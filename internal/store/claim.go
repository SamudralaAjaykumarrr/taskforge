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

// claimQuery is docs/worker-protocol.md's "Claim Query" verbatim, with two
// additions to the RETURNING clause (candidate.old_state,
// candidate.old_attempt_count) so Claim can tell, without a second round
// trip, whether the row it just claimed was a fresh QUEUED/RETRY_WAIT
// claim or a reclaim of a previously RUNNING job — which determines
// whether a superseded job_attempts row needs to be finalized (see
// Claim below).
const claimQuery = `
	WITH candidate AS (
		SELECT id, state AS old_state, attempt_count AS old_attempt_count
		FROM jobs
		WHERE (
				state IN ('QUEUED', 'RETRY_WAIT')
				AND eligible_at <= now()
			  )
		   OR (
				state = 'RUNNING'
				AND lease_expires_at < now()
				AND attempt_count < max_attempts
			  )
		ORDER BY priority DESC, eligible_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	)
	UPDATE jobs
	SET state = 'RUNNING',
		lease_owner = $1,
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

// Claim atomically finds and takes ownership of at most one eligible job
// for workerID: either a freshly QUEUED/RETRY_WAIT job, or a RUNNING job
// whose lease has expired and still has attempt budget remaining
// (reclaim), per docs/worker-protocol.md's claim query. Both paths
// transition the job to RUNNING under a strictly incremented
// lease_generation (TF-INV-002); the same query, the same transaction,
// and the same generation-increment arithmetic handle both cases, exactly
// as docs/architecture.md describes ("there is no separate reaper
// process required for correctness").
//
// Before claiming, this runs the Lazy Dead-Letter Sweep
// (docs/worker-protocol.md) in the same transaction, so an
// attempt-budget-exhausted expired lease is dead-lettered rather than
// reclaimed (TF-INV-006) — Phase 2 has no retry backoff yet, so the only
// way a job's attempt_count reaches max_attempts is repeated reclaim of a
// job whose worker keeps disappearing before completing; without this
// sweep the claim query's own "AND attempt_count < max_attempts" guard
// would otherwise just leave such a job permanently RUNNING-but-unclaimable
// once exhausted, which is exactly the "stranded job" TF-INV-004 exists to
// prevent.
//
// When this claim is a reclaim (the previous state was RUNNING), the
// superseded attempt's job_attempts row is finalized (outcome
// LEASE_EXPIRED) in the SAME transaction as the claim itself, so the
// ledger and the job row can never disagree about whether the previous
// attempt is still open (TF-INV-013).
//
// The second return value is false (with a nil error) when there is
// nothing eligible to claim right now — a normal, expected outcome, not
// an error.
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
func (s *Store) Claim(ctx context.Context, workerID string) (*job.Job, bool, error) {
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

	row := tx.QueryRowContext(ctx, claimQuery, workerID)
	j, oldState, _, err := scanClaimJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		if cerr := tx.Commit(); cerr != nil {
			return nil, false, fmt.Errorf("store: claim: commit (nothing eligible): %w", cerr)
		}
		s.recordSweptJobs(swept)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: claim: %w", err)
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
	rows, err := tx.QueryContext(ctx, `
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
		WHERE state = 'RUNNING'
		  AND lease_expires_at < now()
		  AND attempt_count >= max_attempts
		RETURNING id, job_type, attempt_count`)
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
