// Phase 6: cancellation, per docs/execution-semantics.md "Cancellation and
// Timeouts", docs/worker-protocol.md's "POST /jobs/{id}/cancel" and
// "Cancellation Acknowledgement" SQL, and TF-INV-010. No schema migration
// is required: state='CANCELLED', cancel_requested, and
// cancel_requested_at have all existed since migration 0001 (see that
// migration's comment).
//
// Cancellation is split into exactly the three store operations the
// documented state machine names:
//
//   - CancelQueuedOrRetryWait: the uncontested QUEUED/RETRY_WAIT ->
//     CANCELLED transition (no worker involved, no race to resolve).
//   - RequestCancellation: durably records a cancellation request against
//     a RUNNING job (cancel_requested = true). This is NOT itself a state
//     transition -- the job stays RUNNING -- so it carries no
//     lease_owner/lease_generation fencing: any caller who knows the job
//     id may request cancellation, exactly as docs/worker-protocol.md's
//     POST /jobs/{id}/cancel documents ("sets cancel_requested = true").
//   - CompleteCancelled: the worker's own acknowledgement, RUNNING ->
//     CANCELLED, fenced exactly like CompleteSuccess/CompleteFailure
//     (lease_owner/lease_generation/state='RUNNING'), with the additional
//     "AND cancel_requested = true" guard from
//     docs/worker-protocol.md's "Cancellation Acknowledgement" SQL --
//     only the current, valid lease holder can ever move a job into
//     CANCELLED, and only once a cancellation has actually been
//     requested.
//
// TF-INV-010's race rule ("first durable write to commit wins") falls out
// of this shape with no extra code: CompleteCancelled and
// CompleteSuccess/CompleteFailure/CompleteRetryableFailure/CompleteTimeout
// all share the same `WHERE ... state = 'RUNNING'` guard on the same row.
// PostgreSQL serializes concurrent UPDATEs to that row, so whichever one
// commits first changes state away from RUNNING, and the other's guard
// no longer matches (ErrStaleTransition, zero rows affected) -- exactly
// the same fencing mechanism TF-INV-003/014 already rely on, applied to a
// third kind of completion rather than requiring new machinery.
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
)

// attemptOutcomeCancelled is the job_attempts.outcome value recorded when
// a RUNNING attempt is closed out by a worker's cancellation
// acknowledgement (see docs/data-model.md's outcome enum, already
// permitted by migration 0002's CHECK constraint).
const attemptOutcomeCancelled = "CANCELLED"

// CancelQueuedOrRetryWait implements the direct, uncontested cancellation
// path from docs/execution-semantics.md's transition table: a job that
// has not yet been claimed (QUEUED or RETRY_WAIT) transitions straight to
// CANCELLED. There is no lease to fence on -- no worker is involved, so
// there is nothing to race (TF-INV-010's race rule only applies once a
// job is RUNNING; see RequestCancellation/CompleteCancelled below for
// that case).
//
// If the job is not currently QUEUED or RETRY_WAIT (already RUNNING,
// already terminal, or does not exist), this returns ErrStaleTransition
// and leaves the row untouched -- callers (see internal/api.CancelJob)
// fall through to RequestCancellation next, per
// docs/worker-protocol.md's documented per-state cancellation semantics.
func (s *Store) CancelQueuedOrRetryWait(ctx context.Context, id uuid.UUID) (*job.Job, error) {
	if !jobstate.IsValidTransition(jobstate.Queued, jobstate.Cancelled) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, jobstate.Queued, jobstate.Cancelled)
	}
	if !jobstate.IsValidTransition(jobstate.RetryWait, jobstate.Cancelled) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, jobstate.RetryWait, jobstate.Cancelled)
	}

	j, err := scanTransitionResult(ctx, s.db, `
		UPDATE jobs
		SET state = 'CANCELLED',
			terminal_at = now(),
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND state IN ('QUEUED', 'RETRY_WAIT')
		RETURNING `+jobColumns,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("store: cancel queued/retry_wait job: %w", err)
	}
	return j, nil
}

// RequestCancellation durably records a cancellation request against a
// RUNNING job, per docs/worker-protocol.md's POST /jobs/{id}/cancel
// semantics ("sets cancel_requested = true"). This is deliberately NOT a
// state transition -- the job remains RUNNING, still owned by whichever
// worker currently holds its lease -- so, unlike every other
// transition-writing method in this package, it carries no
// lease_owner/lease_generation fencing: cancellation is an operator/
// caller-facing action asserted against the job's id alone, not a
// worker's own authority over a lease it holds.
//
// cancel_requested_at is set only the first time (COALESCE), so a
// duplicate cancellation request against an already-cancel-requested
// RUNNING job is idempotent: it succeeds again (this is not an error --
// docs/worker-protocol.md documents cancellation as idempotent-per-state)
// without disturbing the original request timestamp.
//
// If the job is not currently RUNNING (already terminal, or was never
// claimed), this returns ErrStaleTransition and changes nothing --
// callers (see internal/api.CancelJob) fall through to a plain read,
// reporting the job's actual current (terminal) state, per
// docs/worker-protocol.md: "If the job is already terminal: no-op,
// response indicates the job's actual terminal state."
//
// The actual RUNNING -> CANCELLED transition only ever happens via
// CompleteCancelled, once the current lease holder observes this flag
// (see internal/worker) and acknowledges it -- see that method's doc
// comment and TF-INV-010.
func (s *Store) RequestCancellation(ctx context.Context, id uuid.UUID) (*job.Job, error) {
	// RUNNING -> RUNNING (self-loop) is already a legal edge in
	// docs/execution-semantics.md's transition table (it also covers
	// reclaim and heartbeat renewal) -- checked here purely as the same
	// defense-in-depth every other method in this package applies before
	// issuing SQL, per internal/store.transition's doc comment.
	if !jobstate.IsValidTransition(jobstate.Running, jobstate.Running) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, jobstate.Running, jobstate.Running)
	}

	j, err := scanTransitionResult(ctx, s.db, `
		UPDATE jobs
		SET cancel_requested = true,
			cancel_requested_at = COALESCE(cancel_requested_at, now()),
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND state = 'RUNNING'
		RETURNING `+jobColumns,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("store: request cancellation: %w", err)
	}
	return j, nil
}

// CompleteCancelled is the worker's cancellation acknowledgement, per
// docs/worker-protocol.md's "Cancellation Acknowledgement" SQL: RUNNING ->
// CANCELLED, fenced exactly like CompleteSuccess/CompleteFailure
// (lease_owner/lease_generation/state='RUNNING' -- TF-INV-003/014, so a
// stale generation can never acknowledge a cancellation it no longer
// authoritatively owns any more than it can report success), with the
// additional "AND cancel_requested = true" guard: a worker can only ever
// acknowledge a cancellation that has actually, durably, been requested --
// it cannot unilaterally cancel a job that was never asked to be
// cancelled.
//
// If the guard does not match -- wrong owner/generation, job no longer
// RUNNING (already completed via a different path, or already reclaimed),
// or cancel_requested is still false -- this returns ErrStaleTransition
// and leaves the row untouched. This is precisely the TF-INV-010
// mechanism: whichever of CompleteCancelled or
// CompleteSuccess/CompleteFailure/CompleteRetryableFailure/CompleteTimeout
// commits first for this row wins, and the other is rejected as stale.
//
// The corresponding job_attempts row is finalized (outcome CANCELLED) in
// the SAME transaction as the jobs row update (TF-INV-013).
func (s *Store) CompleteCancelled(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64) (*job.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: complete cancelled: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	j, err := s.transition(ctx, tx, jobstate.Running, jobstate.Cancelled, `
		UPDATE jobs
		SET state = 'CANCELLED',
			lease_owner = NULL,
			lease_expires_at = NULL,
			terminal_at = now(),
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING' AND cancel_requested = true
		RETURNING `+jobColumns,
		id, leaseOwner, leaseGeneration,
	)
	if err != nil {
		return nil, err
	}

	if err := finalizeOpenAttemptForGeneration(ctx, tx, id, leaseGeneration, attemptOutcomeCancelled, "", ""); err != nil {
		return nil, fmt.Errorf("store: complete cancelled: record attempt outcome: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: complete cancelled: commit: %w", err)
	}
	return j, nil
}
