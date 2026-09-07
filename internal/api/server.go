// Package api implements Phase 1's HTTP surface: POST /jobs and
// GET /jobs/{id}, per docs/worker-protocol.md "API Contract (Job
// Submission and Query)". No other endpoint exists yet — cancellation,
// idempotency keys, scheduling, and history are later-phase surface per
// docs/roadmap.md and are intentionally not exposed here.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
)

// JobStore is the persistence contract this package depends on.
type JobStore interface {
	Insert(ctx context.Context, p job.NewParams) (*job.Job, error)
	GetByID(ctx context.Context, id uuid.UUID) (*job.Job, error)
}

// Default values applied when a submission omits them. docs/data-model.md
// gives max_attempts a schema default of 5; execution_timeout_seconds has
// no schema default (it is NOT NULL with no DEFAULT), so the API layer
// supplies one for Phase 1 rather than requiring every caller to specify
// it.
const (
	DefaultMaxAttempts             = 5
	DefaultExecutionTimeoutSeconds = 30
)

// Handlers holds the dependencies for the job HTTP endpoints.
type Handlers struct {
	store  JobStore
	logger *slog.Logger
}

// NewHandlers constructs the Phase 1 HTTP handlers.
func NewHandlers(store JobStore, logger *slog.Logger) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{store: store, logger: logger}
}

// NewRouter wires the Phase 1 endpoints onto a fresh http.ServeMux using
// Go 1.22+ method+pattern routing.
func NewRouter(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", h.CreateJob)
	mux.HandleFunc("GET /jobs/{id}", h.GetJob)
	return mux
}
