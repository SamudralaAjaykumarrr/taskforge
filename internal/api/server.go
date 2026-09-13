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
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
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
	// CreateWorkflow, GetWorkflow, and CancelWorkflow were added in Phase
	// 7 — see docs/workflows.md and internal/store/workflow.go. Kept on
	// the same interface as the job methods (rather than a separate
	// WorkflowStore) since *store.Store already implements both and
	// Handlers has no reason to depend on two interfaces for one
	// underlying store.
	CreateWorkflow(ctx context.Context, g workflow.GraphSpec) (*workflow.Instance, error)
	GetWorkflow(ctx context.Context, id uuid.UUID) (*workflow.Instance, error)
	CancelWorkflow(ctx context.Context, id uuid.UUID) (*workflow.Instance, error)
}

// Default values applied when a submission omits them. docs/data-model.md
// gives max_attempts a schema default of 5; execution_timeout_seconds has
// no schema default (it is NOT NULL with no DEFAULT), so the API layer
// supplies one for Phase 1 rather than requiring every caller to specify
// it.
//
// These are defined once in internal/job (DefaultMaxAttempts etc.) as of
// Phase 11, since the direct-Go transactional enqueue API (txenqueue) needs
// the identical defaults; these package-level names are kept as aliases so
// every existing reference to api.DefaultMaxAttempts etc. (including
// existing tests) continues to compile and behave identically.
const (
	DefaultMaxAttempts             = job.DefaultMaxAttempts
	DefaultExecutionTimeoutSeconds = job.DefaultExecutionTimeoutSeconds

	// MaxIdempotencyKeyLength bounds the optional Idempotency-Key request
	// header. docs/data-model.md places no length limit on the
	// idempotency_key column itself (plain TEXT); this bound is a Phase 4
	// API-layer choice, matching job_type's existing 255-character bound,
	// not a documented TaskForge contract -- see
	// docs/idempotency.md "Implementation Notes."
	MaxIdempotencyKeyLength = job.MaxIdempotencyKeyLength

	// DeprecationEffectiveDate is Phase 11's fixed, documented
	// legacy-unversioned-route deprecation effective date: 2026-09-10T00:00:00Z.
	// It is NOT a removal/sunset date -- Phase 11 deliberately does not
	// decide when (or whether) the legacy unprefixed surface is actually
	// removed; see docs/compatibility-policy.md "API Evolution." Exported,
	// and defined exactly once, so no other file/doc/test hand-computes or
	// duplicates it or DeprecationHeaderValue below.
	DeprecationEffectiveDate = "2026-09-10T00:00:00Z"

	// DeprecationHeaderValue is the exact value every legacy (unprefixed)
	// route's response carries in its Deprecation response header, per RFC
	// 9745 (https://www.rfc-editor.org/rfc/rfc9745): the Deprecation field
	// is an HTTP Structured Field Item whose value MUST be a Date,
	// serialized as "@" followed by the value's Unix timestamp in seconds
	// -- NOT the earlier, non-standard "Deprecation: true" convention this
	// codebase originally shipped (audit finding, corrected here). 1788998400
	// is DeprecationEffectiveDate's Unix timestamp.
	DeprecationHeaderValue = "@1788998400"

	// MaxRequestBodyBytes bounds POST /jobs and POST /workflows request
	// bodies (Phase 11, docs/enterprise-roadmap.md "API-contract
	// hardening": "Enforce a documented request body size limit"). jobs
	// hold small, opaque JSON payloads (docs/data-model.md: "TaskForge is
	// not an artifact store"), so 1 MiB is a generous bound for any
	// legitimate submission while still rejecting a pathological
	// multi-megabyte body before it is fully buffered for JSON decoding
	// -- enforced via http.MaxBytesReader, which aborts the read (not
	// just the eventual decode) once the limit is crossed.
	MaxRequestBodyBytes = 1 << 20 // 1 MiB
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
//
// Phase 11 (docs/enterprise-roadmap.md, docs/compatibility-policy.md "API
// Evolution"): every endpoint is now served under both an unprefixed path
// (Phase 1-10 behavior, unchanged, so every existing caller keeps working
// with zero migration) and the new canonical /v1/-prefixed path. The
// unprefixed routes are marked deprecated (an RFC 9745 Deprecation response
// header, plus a structured log warning on first use per request, per
// docs/compatibility-policy.md's already-drafted deprecation policy) but
// remain fully functional -- this phase does not remove or redirect them;
// removal is a future, separately-decided contract change, never silently
// bundled into introducing the new prefix.
func NewRouter(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	registerJobRoutes(mux, h, "")
	registerJobRoutes(mux, h, "/v1")
	return mux
}

// registerJobRoutes mounts every job/workflow endpoint under prefix ("" for
// the legacy unprefixed surface, "/v1" for the current canonical one). The
// legacy surface's handlers are wrapped with deprecationWarning; the /v1
// surface's are not.
func registerJobRoutes(mux *http.ServeMux, h *Handlers, prefix string) {
	wrap := func(route string, next http.HandlerFunc) http.HandlerFunc { return next }
	if prefix == "" {
		wrap = func(route string, next http.HandlerFunc) http.HandlerFunc {
			return deprecationWarning(h.logger, route, next)
		}
	}
	mux.HandleFunc("POST "+prefix+"/jobs", wrap("POST /jobs", h.CreateJob))
	mux.HandleFunc("GET "+prefix+"/jobs/{id}", wrap("GET /jobs/{id}", h.GetJob))
	mux.HandleFunc("POST "+prefix+"/jobs/{id}/cancel", wrap("POST /jobs/{id}/cancel", h.CancelJob))
	// Phase 7 — see docs/workflows.md and internal/api/workflow_handlers.go.
	mux.HandleFunc("POST "+prefix+"/workflows", wrap("POST /workflows", h.CreateWorkflow))
	mux.HandleFunc("GET "+prefix+"/workflows/{id}", wrap("GET /workflows/{id}", h.GetWorkflow))
	mux.HandleFunc("POST "+prefix+"/workflows/{id}/cancel", wrap("POST /workflows/{id}/cancel", h.CancelWorkflow))
}

// deprecationWarning wraps a legacy, unprefixed-route handler: it sets an
// RFC 9745 Deprecation response header (docs/compatibility-policy.md's
// already-drafted deprecation-header rule, applied here rather than only
// planned; RFC 9745 defines Deprecation as an HTTP Structured Field Item
// whose value MUST be a Date, serialized "@<unix-seconds>" -- NOT the bare
// "Deprecation: true" this codebase originally shipped, which is not valid
// RFC 9745 syntax and was corrected as a Phase 11 audit finding) and emits
// a structured log warning identifying which unversioned route was used,
// so operators can measure real usage before any future removal decision
// -- never removed or redirected silently. The header's value is fixed at
// DeprecationEffectiveDate/DeprecationHeaderValue (server.go) -- a
// deprecation *effective* date, not a removal/sunset date; Phase 11 does
// not decide when, or whether, the legacy surface is actually removed, so
// no Sunset header is emitted.
func deprecationWarning(logger *slog.Logger, route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", DeprecationHeaderValue)
		logger.Warn("deprecated unversioned API route used; prefer the /v1 equivalent",
			"event", "deprecated_route_used", "route", route, "path", r.URL.Path)
		next(w, r)
	}
}
