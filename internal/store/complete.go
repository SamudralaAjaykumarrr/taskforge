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

// CompleteSuccess fences on (id, leaseOwner, leaseGeneration, state =
// RUNNING) and transitions the job RUNNING -> SUCCEEDED, per
// docs/worker-protocol.md "Completion / Success". If the guard does not
// match — the job is not RUNNING, or is RUNNING under a different lease —
// this returns ErrStaleTransition and leaves the row untouched
// (TF-INV-003).
//
// The corresponding job_attempts row (matched by (job_id, lease_generation),
// which uniquely identifies the attempt Claim opened for this generation)
// is finalized in the SAME transaction as the jobs row update, per
// docs/worker-protocol.md ("Both statements commit together or not at
// all") — TF-INV-013.
func (s *Store) CompleteSuccess(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, resultMetadata []byte) (*job.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: complete success: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	j, err := s.transition(ctx, tx, jobstate.Running, jobstate.Succeeded, `
		UPDATE jobs
		SET state = 'SUCCEEDED',
			lease_owner = NULL,
			lease_expires_at = NULL,
			result_metadata = $4,
			terminal_at = now(),
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING'
		RETURNING `+jobColumns,
		id, leaseOwner, leaseGeneration, resultMetadata,
	)
	if err != nil {
		if errors.Is(err, ErrStaleTransition) {
			s.metrics.StaleCompletionRejectionsTotal.Inc()
		}
		return nil, err
	}

	startedAt, err := finalizeOpenAttemptForGeneration(ctx, tx, id, leaseGeneration, attemptOutcomeSucceeded, "", "")
	if err != nil {
		return nil, fmt.Errorf("store: complete success: record attempt outcome: %w", err)
	}

	// Phase 7: if id is a workflow node's underlying job, this SUCCEEDED
	// transition may satisfy dependents' dependency conditions -- see
	// internal/store/workflow.go. A no-op for an ordinary standalone job.
	workflowID, finalState, err := s.propagateWorkflowTransition(ctx, tx, id, jobstate.Succeeded)
	if err != nil {
		return nil, fmt.Errorf("store: complete success: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: complete success: commit: %w", err)
	}
	s.logWorkflowFinalized(workflowID, finalState)

	s.metrics.JobsCompletedTotal.WithLabelValues(j.JobType, metrics.OutcomeSucceeded).Inc()
	s.metrics.ExecutionDurationSeconds.WithLabelValues(j.JobType, metrics.OutcomeSucceeded).Observe(j.UpdatedAt.Sub(startedAt).Seconds())
	s.metrics.RetryCount.WithLabelValues(j.JobType).Observe(float64(j.AttemptCount))
	return j, nil
}

// CompleteFailure fences the same way as CompleteSuccess and transitions
// RUNNING -> DEAD_LETTERED unconditionally.
//
// This is Phase 1/2's documented simplification of
// docs/worker-protocol.md's "Retryable Failure" / "Permanent Failure"
// queries: docs/roadmap.md's Phase 2 non-goals still say "still failure =
// dead-letter" — retry backoff and the real RETRY_WAIT branch are Phase 3.
// What Phase 2 adds is that a job which fails AFTER being reclaimed (i.e.
// its worker crashed once already) now correctly went through the
// reclaim/fencing path to get here, rather than Phase 1's single-worker,
// no-reclaim world.
func (s *Store) CompleteFailure(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, errMessage, errClass string) (*job.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: complete failure: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	j, err := s.transition(ctx, tx, jobstate.Running, jobstate.DeadLettered, `
		UPDATE jobs
		SET state = 'DEAD_LETTERED',
			lease_owner = NULL,
			lease_expires_at = NULL,
			last_error = $4,
			last_error_class = $5,
			terminal_at = now(),
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING'
		RETURNING `+jobColumns,
		id, leaseOwner, leaseGeneration, errMessage, errClass,
	)
	if err != nil {
		if errors.Is(err, ErrStaleTransition) {
			s.metrics.StaleCompletionRejectionsTotal.Inc()
		}
		return nil, err
	}

	startedAt, err := finalizeOpenAttemptForGeneration(ctx, tx, id, leaseGeneration, attemptOutcomeFailedPermanent, errClass, errMessage)
	if err != nil {
		return nil, fmt.Errorf("store: complete failure: record attempt outcome: %w", err)
	}

	// Phase 7: a permanently-failed workflow node's dependents are
	// cancelled by default (docs/workflows.md's Failure Propagation
	// table) -- a no-op for an ordinary standalone job.
	workflowID, finalState, err := s.propagateWorkflowTransition(ctx, tx, id, jobstate.DeadLettered)
	if err != nil {
		return nil, fmt.Errorf("store: complete failure: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: complete failure: commit: %w", err)
	}
	s.logWorkflowFinalized(workflowID, finalState)

	s.metrics.JobsCompletedTotal.WithLabelValues(j.JobType, metrics.OutcomeFailedPermanent).Inc()
	s.metrics.JobsDeadLetteredTotal.WithLabelValues(j.JobType).Inc()
	s.metrics.ExecutionDurationSeconds.WithLabelValues(j.JobType, metrics.OutcomeFailedPermanent).Observe(j.UpdatedAt.Sub(startedAt).Seconds())
	s.metrics.RetryCount.WithLabelValues(j.JobType).Observe(float64(j.AttemptCount))
	return j, nil
}

// finalizeOpenAttemptForGeneration closes out the job_attempts row for
// jobID's specific leaseGeneration (rather than "whichever attempt is
// currently open", which claim.go's finalizeOpenAttempt uses when it has
// no generation to key on yet). Completion calls always know their exact
// lease_generation (it is part of the fencing credential the caller
// presents), so keying on it here is both precise and an extra
// belt-and-braces check: it can only ever finalize the attempt this exact
// completion call is fenced against, never a different one.
//
// Phase 8: returns the finalized attempt's started_at, so callers can
// compute taskforge_execution_duration_seconds as their own transition's
// updated_at (already fetched via jobColumns, same-transaction now())
// minus started_at, with no additional query.
func finalizeOpenAttemptForGeneration(ctx context.Context, tx *sql.Tx, jobID uuid.UUID, leaseGeneration int64, outcome, errClass, errMessage string) (time.Time, error) {
	var startedAt time.Time
	err := tx.QueryRowContext(ctx, `
		UPDATE job_attempts
		SET finished_at = now(),
			outcome = $3,
			error_class = NULLIF($4, ''),
			error_message = NULLIF($5, '')
		WHERE job_id = $1 AND lease_generation = $2 AND finished_at IS NULL
		RETURNING started_at`,
		jobID, leaseGeneration, outcome, errClass, errMessage,
	).Scan(&startedAt)
	return startedAt, err
}
