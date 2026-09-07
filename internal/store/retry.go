// Phase 3: the RUNNING -> RETRY_WAIT / RUNNING -> DEAD_LETTERED (via
// exhaustion) transition, per docs/worker-protocol.md "Retryable Failure"
// and docs/retry-semantics.md's transition decision:
//
//	if outcome == FAILED_PERMANENT:
//	    -> DEAD_LETTERED (regardless of attempt_count)
//	elif attempt_count >= max_attempts:
//	    -> DEAD_LETTERED
//	else:
//	    -> RETRY_WAIT (eligible_at = now() + backoff(attempt_count))
//
// The FAILED_PERMANENT branch above is CompleteFailure (complete.go),
// unchanged from Phase 1/2. This file implements the other two branches,
// which share one fenced UPDATE (the destination -- RETRY_WAIT or
// DEAD_LETTERED -- is chosen by a SQL CASE on attempt_count vs
// max_attempts, evaluated against the current row inside the same
// statement that performs the write, so the decision and the write can
// never disagree or be interleaved with a concurrent change to either
// value).
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
)

// CompleteRetryableFailure fences on (id, leaseOwner, leaseGeneration,
// state = RUNNING) exactly like CompleteSuccess/CompleteFailure -- a stale
// generation or wrong owner is rejected with ErrStaleTransition and leaves
// the row untouched (TF-INV-003/014), and the caller is never any more
// authoritative here than it is for any other completion call: a worker
// that has lost its lease cannot schedule a retry or dead-letter a job it
// no longer owns.
//
// delay is the caller-computed backoff duration (see internal/retry and
// docs/retry-semantics.md's Backoff Policy) for the attempt that just
// failed. It is applied as `eligible_at = now() + delay`, with `now()`
// evaluated by PostgreSQL itself (not the caller's clock), per
// docs/failure-model.md's Clock Model -- delay is a relative duration, not
// an absolute timestamp, so computing it outside PostgreSQL introduces no
// clock-skew risk.
//
// If attempt_count has already reached max_attempts, the job goes straight
// to DEAD_LETTERED instead -- delay is ignored in that branch (eligible_at
// is left unchanged, matching docs/worker-protocol.md's literal SQL) since
// there will be no further retry to wait for. Per that same SQL,
// last_error_class is unconditionally 'RETRYABLE' in both branches: it
// records that the *attempt* was classified retryable, distinct from
// last_error_class = 'PERMANENT' (CompleteFailure), even when the job's own
// state ends up DEAD_LETTERED because the retry budget, not the failure's
// own classification, is what stopped it (TF-INV-009: the job's terminal
// failure reason is preserved either way).
//
// The corresponding job_attempts row is finalized (outcome
// FAILED_RETRYABLE, regardless of which state the job lands in -- the
// *attempt* failed retryably either way, per docs/data-model.md's
// attempt-level vs job-level distinction) in the SAME transaction as the
// jobs row update (TF-INV-013).
func (s *Store) CompleteRetryableFailure(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, errMessage string, delay time.Duration) (*job.Job, error) {
	// This single UPDATE can legally land on either RETRY_WAIT or
	// DEAD_LETTERED depending on attempt_count vs max_attempts at write
	// time (chosen by the SQL CASE below, not by this Go code) -- both
	// destinations must be legal RUNNING-sourced transitions before any
	// SQL is issued, per docs/execution-semantics.md's transition table.
	if !jobstate.IsValidTransition(jobstate.Running, jobstate.RetryWait) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, jobstate.Running, jobstate.RetryWait)
	}
	if !jobstate.IsValidTransition(jobstate.Running, jobstate.DeadLettered) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, jobstate.Running, jobstate.DeadLettered)
	}

	if delay < 0 {
		delay = 0
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: complete retryable failure: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	j, err := scanTransitionResult(ctx, tx, `
		UPDATE jobs
		SET state = CASE WHEN attempt_count >= max_attempts THEN 'DEAD_LETTERED' ELSE 'RETRY_WAIT' END,
			lease_owner = NULL,
			lease_expires_at = NULL,
			eligible_at = CASE
				WHEN attempt_count >= max_attempts THEN eligible_at
				ELSE now() + make_interval(secs => $5::double precision)
			END,
			last_error = $4,
			last_error_class = 'RETRYABLE',
			terminal_at = CASE WHEN attempt_count >= max_attempts THEN now() ELSE NULL END,
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING'
		RETURNING `+jobColumns,
		id, leaseOwner, leaseGeneration, errMessage, delay.Seconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("store: complete retryable failure: %w", err)
	}

	if err := finalizeOpenAttemptForGeneration(ctx, tx, id, leaseGeneration, attemptOutcomeFailedRetryable, "RETRYABLE", errMessage); err != nil {
		return nil, fmt.Errorf("store: complete retryable failure: record attempt outcome: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: complete retryable failure: commit: %w", err)
	}
	return j, nil
}
