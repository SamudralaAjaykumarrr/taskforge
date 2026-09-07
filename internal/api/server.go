// Package api implements the HTTP surface: POST /jobs, GET /jobs/{id},
// and (Phase 6) POST /jobs/{id}/cancel, per docs/worker-protocol.md "API
// Contract (Job Submission and Query)". History (GET /jobs/{id}/history)
// remains a documented, deliberately deferred endpoint. Phase 4 added
// optional Idempotency-Key support to POST /jobs (docs/idempotency.md);
// Phase 6 added an optional scheduled_at field to POST /jobs
// (docs/scheduling.md) and the cancellation endpoint
// (docs/execution-semantics.md, TF-INV-010) — no other endpoint's
// contract changed.
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
	InsertIdempotent(ctx context.Context, p job.NewParams) (*job.Job, bool, error)
	GetByID(ctx context.Context, id uuid.UUID) (*job.Job, error)
	// CancelQueuedOrRetryWait and RequestCancellation were added in
	// Phase 6 — see docs/worker-protocol.md's POST /jobs/{id}/cancel
	// contract and internal/store/cancellation.go.
	CancelQueuedOrRetryWait(ctx context.Context, id uuid.UUID) (*job.Job, error)
	RequestCancellation(ctx context.Context, id uuid.UUID) (*job.Job, error)
}

// Default values applied when a submission omits them. docs/data-model.md
// gives max_attempts a schema default of 5; execution_timeout_seconds has
// no schema default (it is NOT NULL with no DEFAULT), so the API layer
// supplies one for Phase 1 rather than requiring every caller to specify
// it.
const (
	DefaultMaxAttempts             = 5
	DefaultExecutionTimeoutSeconds = 30

	// MaxIdempotencyKeyLength bounds the optional Idempotency-Key request
	// header. docs/data-model.md places no length limit on the
	// idempotency_key column itself (plain TEXT); this bound is a Phase 4
	// API-layer choice, matching job_type's existing 255-character bound,
	// not a documented TaskForge contract -- see
	// docs/idempotency.md "Implementation Notes."
	MaxIdempotencyKeyLength = 255
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

// NewRouter wires the HTTP endpoints onto a fresh http.ServeMux using
// Go 1.22+ method+pattern routing.
func NewRouter(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", h.CreateJob)
	mux.HandleFunc("GET /jobs/{id}", h.GetJob)
	mux.HandleFunc("POST /jobs/{id}/cancel", h.CancelJob)
	return mux
}
