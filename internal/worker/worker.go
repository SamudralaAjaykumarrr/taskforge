// Package worker implements Phase 1's single-worker synchronous
// claim-execute-complete loop, per docs/roadmap.md: "A single worker
// process, synchronous claim-execute-complete loop, no concurrency
// hardening yet (single worker instance assumed)."
//
// Per docs/worker-protocol.md ("Why Not Hold a Transaction Open for the
// Whole Job"), claiming and completing are each a single short
// transaction; the job handler runs entirely outside any database
// transaction. If this process crashes between claim and completion, the
// job is left RUNNING with no reclaim mechanism — Phase 1 has none
// (lease expiration/reclaim is Phase 2 scope) — and requires manual
// resubmission, exactly as docs/roadmap.md's Phase 1 completion criteria
// describe.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
)

// Store is the persistence contract this package depends on
// (*store.Store satisfies it). Depending on an interface, rather than the
// concrete type, lets loop/dispatch tests substitute a fake without a real
// database; tests that assert persistence correctness still exercise this
// against a real PostgreSQL-backed *store.Store.
type Store interface {
	Claim(ctx context.Context, workerID string) (*job.Job, bool, error)
	CompleteSuccess(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, resultMetadata []byte) (*job.Job, error)
	CompleteFailure(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, errMessage, errClass string) (*job.Job, error)
}

// Worker runs the Phase 1 claim-execute-complete loop for a single logical
// worker identity.
type Worker struct {
	ID           string
	store        Store
	registry     *handler.Registry
	pollInterval time.Duration
	logger       *slog.Logger
}

// New constructs a Worker. id is the lease_owner value recorded on every
// claimed job (docs/data-model.md: "opaque worker identifier, e.g.
// hostname+pid+random").
func New(id string, store Store, registry *handler.Registry, pollInterval time.Duration, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		ID:           id,
		store:        store,
		registry:     registry,
		pollInterval: pollInterval,
		logger:       logger,
	}
}

// RunOnce attempts to claim and fully execute a single job. It returns
// claimed=false (with a nil error) when there was nothing eligible to
// claim, which is the normal steady-state outcome, not a failure.
func (w *Worker) RunOnce(ctx context.Context) (claimed bool, err error) {
	j, ok, err := w.store.Claim(ctx, w.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	log := w.logger.With("job_id", j.ID.String(), "job_type", j.JobType, "attempt", j.AttemptCount)

	h, found := w.registry.Lookup(j.JobType)
	if !found {
		log.Warn("no handler registered for job_type; dead-lettering")
		errMsg := handler.ErrNoHandler{JobType: j.JobType}.Error()
		_, ferr := w.store.CompleteFailure(ctx, j.ID, w.ID, j.LeaseGeneration, errMsg, job.ErrorClassPermanent)
		return true, ferr
	}

	result, execErr := h.Execute(ctx, j)
	if execErr != nil {
		log.Info("job execution failed; dead-lettering (Phase 1 has no retry path)", "error", execErr)
		_, ferr := w.store.CompleteFailure(ctx, j.ID, w.ID, j.LeaseGeneration, execErr.Error(), job.ErrorClassPermanent)
		return true, ferr
	}

	log.Info("job execution succeeded")
	_, cerr := w.store.CompleteSuccess(ctx, j.ID, w.ID, j.LeaseGeneration, result.Metadata)
	return true, cerr
}

// Run polls in a loop, sleeping pollInterval whenever RunOnce finds
// nothing to claim, until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		claimed, err := w.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			w.logger.Error("worker iteration failed", "error", err)
		}
		if !claimed {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.pollInterval):
			}
		}
	}
}
