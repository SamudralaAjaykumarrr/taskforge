// Package workerhost is the minimum generic extension needed for an
// external Go module to host TaskForge job execution using its own
// handler implementations.
//
// Repository evidence for why this package exists (see
// github.com/SamudralaAjaykumarrr/autonomy-platform docs/boundaries/TASKFORGE.md
// and docs/validation/FIRST-VERTICAL-SLICE.md for the full justification
// this comment summarizes): TaskForge already exposes a fully generic
// job-handler registration mechanism (internal/handler.Registry) and a
// fully generic worker loop (internal/worker.Worker) driven by
// internal/store.Store. Both are sufficient, unmodified, for a caller to
// register an arbitrary Handler under a job_type and have TaskForge
// durably execute it, on the first attempt and every mechanical retry,
// with no changes to TaskForge's execution mechanics, retry engine, or
// schema. The one genuine gap is Go's own package-visibility rule: every
// one of those types lives under internal/, which only code inside this
// module (github.com/SamudralaAjaykumarrr/taskforge) may import. An
// external module implementing its own Handler cannot satisfy
// internal/handler.Handler, and cannot call internal/worker.New or
// internal/store.New, no matter how generic its use is.
//
// This package closes exactly that gap and nothing else: it re-exports
// the existing generic types verbatim (via Go type aliases, so a Handler
// implemented against workerhost.Handler literally IS an
// internal/handler.Handler -- no adapter, no behavior change) and wraps
// the existing store/worker/migrate construction in one constructor,
// matching cmd/worker/main.go's own wiring exactly. It adds no new
// mechanics, no new schema, no retry logic, and no knowledge of what any
// registered Handler's job_type or payload means.
package workerhost

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// Job is exactly internal/job.Job. A Handler reads the same fields
// (ID, Payload, AttemptCount, ...) any built-in TaskForge handler would.
type Job = job.Job

// Result is exactly internal/handler.Result.
type Result = handler.Result

// Handler is exactly internal/handler.Handler. Implementing this
// interface against the alias below is implementing the real
// TaskForge Handler interface -- Go type aliases denote the same type.
type Handler = handler.Handler

// FailureClass is exactly internal/handler.FailureClass.
type FailureClass = handler.FailureClass

// Registry is exactly internal/handler.Registry.
type Registry = handler.Registry

// NewRegistry returns an empty Registry (internal/handler.NewRegistry).
func NewRegistry() *Registry { return handler.NewRegistry() }

// Retryable marks err as a failure TaskForge's own mechanical
// retry/backoff policy should retry (internal/handler.Retryable).
func Retryable(err error) error { return handler.Retryable(err) }

// Permanent marks err as a failure TaskForge should not retry
// (internal/handler.Permanent).
func Permanent(err error) error { return handler.Permanent(err) }

// Classify extracts a Handler failure's retry classification
// (internal/handler.Classify).
func Classify(err error) (FailureClass, error) { return handler.Classify(err) }

// HostConfig configures a Host. It mirrors the fields cmd/worker/main.go
// already sets on a *worker.Worker -- nothing here is new configuration
// surface, only exposure of what already existed.
type HostConfig struct {
	WorkerID     string
	PollInterval time.Duration
	Queues       []string
	DrainTimeout time.Duration
	Logger       *slog.Logger
}

// Host runs TaskForge's existing claim-execute-complete loop
// (internal/worker.Worker) against a caller-supplied Registry. It is a
// thin wrapper: NewHost performs the same migrate.Up + store.New +
// worker.New sequence cmd/worker/main.go performs today.
type Host struct {
	w *worker.Worker
}

// NewHost applies TaskForge's own migrations to db (idempotent, the same
// call cmd/worker/main.go and cmd/api/main.go already make), constructs a
// Store, and wires a Worker to execute jobs via registry. db is expected
// to be TaskForge's own database (ADR-0010 leaves same-instance vs.
// separate-instance an implementation choice; this constructor does not
// care which).
func NewHost(ctx context.Context, db *sql.DB, registry *Registry, cfg HostConfig) (*Host, error) {
	if err := migrate.Up(ctx, db); err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	m := metrics.New()
	st := store.New(db, store.WithMetrics(m), store.WithLogger(logger))

	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 200 * time.Millisecond
	}
	w := worker.New(cfg.WorkerID, st, registry, pollInterval, logger)
	if len(cfg.Queues) > 0 {
		w.SetQueues(cfg.Queues)
	}
	if cfg.DrainTimeout > 0 {
		w.SetDrainTimeout(cfg.DrainTimeout)
	}
	return &Host{w: w}, nil
}

// Run blocks, running TaskForge's claim-execute-complete loop until ctx
// is cancelled (internal/worker.Worker.Run).
func (h *Host) Run(ctx context.Context) error { return h.w.Run(ctx) }
