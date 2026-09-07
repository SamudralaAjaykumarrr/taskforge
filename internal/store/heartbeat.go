package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
)

// Heartbeat renews id's lease for the caller identified by
// (leaseOwner, leaseGeneration), extending lease_expires_at to
// now() + extension, per docs/worker-protocol.md "Heartbeating". now() is
// PostgreSQL's clock, not the caller's (docs/failure-model.md Clock
// Model), so this is correct regardless of skew between the worker's host
// clock and the database's.
//
// The guard is the same fencing shape as completion (TF-INV-015): if
// leaseOwner/leaseGeneration no longer match the current row, or the job
// is no longer RUNNING (already reclaimed, already completed, already
// cancelled), this returns ErrStaleTransition and changes nothing — a
// heartbeat can only extend a lease it currently, authoritatively holds;
// it can never resurrect a terminal job, reopen a non-running job, or
// steal/extend another generation's lease. Per docs/worker-protocol.md,
// the caller "must abort execution immediately" on this error — see
// internal/worker's heartbeat loop.
func (s *Store) Heartbeat(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, extension time.Duration) (*job.Job, error) {
	return s.transition(ctx, s.db, jobstate.Running, jobstate.Running, `
		UPDATE jobs
		SET lease_expires_at = now() + make_interval(secs => $4::double precision),
			heartbeat_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING'
		RETURNING `+jobColumns,
		id, leaseOwner, leaseGeneration, extension.Seconds(),
	)
}
