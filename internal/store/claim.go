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

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
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

	if err := s.sweepExpiredExhaustedLeases(ctx, tx); err != nil {
		return nil, false, fmt.Errorf("store: claim: %w", err)
	}

	row := tx.QueryRowContext(ctx, claimQuery, workerID)
	j, oldState, _, err := scanClaimJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		if cerr := tx.Commit(); cerr != nil {
			return nil, false, fmt.Errorf("store: claim: commit (nothing eligible): %w", cerr)
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: claim: %w", err)
	}

	if oldState == jobstate.Running {
		if err := finalizeOpenAttempt(ctx, tx, j.ID, attemptOutcomeLeaseExpired, "", ""); err != nil {
			return nil, false, fmt.Errorf("store: claim: finalize superseded attempt: %w", err)
		}
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
	return j, true, nil
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
func (s *Store) sweepExpiredExhaustedLeases(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		UPDATE jobs
		SET state = 'DEAD_LETTERED',
			lease_owner = NULL,
			lease_expires_at = NULL,
			last_error = COALESCE(last_error, 'lease expired, retry budget exhausted'),
			last_error_class = 'LEASE_EXPIRED',
			terminal_at = now(),
			updated_at = now(),
			version = version + 1
		WHERE state = 'RUNNING'
		  AND lease_expires_at < now()
		  AND attempt_count >= max_attempts
		RETURNING id`)
	if err != nil {
		return fmt.Errorf("sweep expired-exhausted leases: %w", err)
	}

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return fmt.Errorf("sweep expired-exhausted leases: scan id: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sweep expired-exhausted leases: %w", err)
	}

	for _, id := range ids {
		if err := finalizeOpenAttempt(ctx, tx, id, attemptOutcomeLeaseExpired, "", ""); err != nil {
			return fmt.Errorf("sweep expired-exhausted leases: finalize attempt for job %s: %w", id, err)
		}
		if err := s.propagateWorkflowTransition(ctx, tx, id, jobstate.DeadLettered); err != nil {
			return fmt.Errorf("sweep expired-exhausted leases: propagate workflow for job %s: %w", id, err)
		}
	}
	return nil
}

// finalizeOpenAttempt closes out the single still-open (finished_at IS
// NULL) job_attempts row for jobID, if any. At most one attempt is ever
// open for a given job at a time (a new attempt row is only inserted once
// Claim has either found no open attempt or already finalized the
// previous one in the same transaction — see Claim above), so matching on
// "open" rather than a specific attempt_number/lease_generation is
// sufficient and avoids the caller needing to know which attempt number
// is currently open.
func finalizeOpenAttempt(ctx context.Context, tx *sql.Tx, jobID uuid.UUID, outcome, errClass, errMessage string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE job_attempts
		SET finished_at = now(),
			outcome = $2,
			error_class = NULLIF($3, ''),
			error_message = NULLIF($4, '')
		WHERE job_id = $1 AND finished_at IS NULL`,
		jobID, outcome, errClass, errMessage,
	)
	return err
}
