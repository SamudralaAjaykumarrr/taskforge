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
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

// JobStore is the persistence contract this package depends on.
//
// Phase 12 (docs/phase-12-plan.md §4a): every read and every cancellation
// method takes a principal.AccessContext, which internal/store folds into
// the WHERE clause of the same statement that does the work. The handlers
// in this package do not branch on ownership at all -- they obtain the
// AccessContext from the request (auth.go's accessContext) and pass it
// through. There is deliberately no handler-level authorizeJobAccess
// helper: an ownership check that is separate from the statement it
// guards is a check-then-act race, and for CancelJob specifically it would
// have run only AFTER a mutating store call had already fired.
type JobStore interface {
	InsertIdempotent(ctx context.Context, p job.NewParams) (*job.Job, bool, error)
	GetByID(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*job.Job, error)
	// CancelQueuedOrRetryWait and RequestCancellation were added in
	// Phase 6 — see docs/worker-protocol.md's POST /jobs/{id}/cancel
	// contract and internal/store/cancellation.go.
	CancelQueuedOrRetryWait(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*job.Job, error)
	RequestCancellation(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*job.Job, error)
	// CreateWorkflow, GetWorkflow, and CancelWorkflow were added in Phase
	// 7 — see docs/workflows.md and internal/store/workflow.go. Kept on
	// the same interface as the job methods (rather than a separate
	// WorkflowStore) since *store.Store already implements both and
	// Handlers has no reason to depend on two interfaces for one
	// underlying store.
	CreateWorkflow(ctx context.Context, g workflow.GraphSpec) (*workflow.Instance, error)
	GetWorkflow(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*workflow.Instance, error)
	CancelWorkflow(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*workflow.Instance, error)
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
	store   JobStore
	logger  *slog.Logger
	auth    Authenticator
	metrics *metrics.Metrics

	// governance and inflight are Phase 13's admission-check dependencies
	// (admission.go) -- both nil-safe (see WithGovernance,
	// WithMaxInflightSubmissions): a Handlers that never configures
	// either behaves exactly as a pre-Phase-13 one did.
	governance GovernanceStore
	inflight   chan struct{}
}

// NewHandlers constructs the HTTP handlers.
//
// Phase 12: opts supply the authenticator (WithAuthenticator) and the
// metrics recorder the auth-failure counter is written to (WithMetrics).
// Neither is required to construct a Handlers, and both have fail-safe
// defaults: without an authenticator, every request is rejected
// (denyAllAuthenticator), and without a metrics recorder, auth failures
// are counted into a private, unregistered instance -- the same pattern
// internal/store.New already uses, so no call site needs a nil check.
func NewHandlers(store JobStore, logger *slog.Logger, opts ...HandlersOption) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handlers{store: store, logger: logger, auth: denyAllAuthenticator{}, metrics: metrics.New()}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// HandlersOption configures optional Handlers dependencies.
type HandlersOption func(*Handlers)

// WithAuthenticator attaches the credential verifier every route's
// middleware calls (auth.go). Production wiring passes a
// *principal.Store built from the same *sql.DB as the JobStore.
//
// Omitting it does not disable authentication -- it makes every request
// fail. There is no "authentication off" mode (OD-6: hard cutover, no
// observe/enforce dual mode).
func WithAuthenticator(a Authenticator) HandlersOption {
	return func(h *Handlers) {
		if a != nil {
			h.auth = a
		}
	}
}

// WithMetrics attaches the shared metrics recorder, so
// taskforge_auth_failures_total lands in the same registry the process
// serves at GET /metrics.
func WithMetrics(m *metrics.Metrics) HandlersOption {
	return func(h *Handlers) {
		if m != nil {
			h.metrics = m
		}
	}
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
// Phase 12 (docs/phase-12-plan.md §6a): every route registered here is
// mounted through the single mount helper below, which wraps it in
// authentication and a scope requirement. NewRouter adds no route of its
// own and this phase adds no new endpoint anywhere -- key lifecycle
// management is operator tooling, deliberately not an HTTP surface
// (docs/phase-12-plan.md §3).
//
// It returns http.Handler rather than *http.ServeMux specifically so a
// caller cannot reach past the returned value and attach an unprotected
// route to the mux afterwards -- which is how GET /metrics was wired
// before this phase. That endpoint is now supplied through
// WithMetricsEndpoint and mounted through the same helper as everything
// else.
func NewRouter(h *Handlers, opts ...RouterOption) http.Handler {
	cfg := routerConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	mux := http.NewServeMux()
	registerJobRoutes(mux, h, "")
	registerJobRoutes(mux, h, "/v1")
	if cfg.metricsHandler != nil {
		// GET /metrics requires the metrics scope, not the jobs scope
		// (OD-5): an ordinary job-submission key must not be able to read
		// a deployment's operational volume, which is exactly
		// docs/security-model.md §5's "operational volume disclosure"
		// finding. A metrics-only key correspondingly cannot submit jobs.
		mount(mux, h, "GET /metrics", principal.ScopeMetrics, cfg.metricsHandler.ServeHTTP)
	}
	return mux
}

// RouterOption configures NewRouter.
type RouterOption func(*routerConfig)

type routerConfig struct {
	metricsHandler http.Handler
}

// WithMetricsEndpoint mounts h as GET /metrics, behind authentication and
// the metrics scope. Omitting it leaves the router with no /metrics route
// at all (a 404), which is what internal/api's own tests want and is never
// a silently-unauthenticated endpoint either way.
func WithMetricsEndpoint(handler http.Handler) RouterOption {
	return func(c *routerConfig) { c.metricsHandler = handler }
}

// mount is the ONLY place in this package where a handler is attached to a
// mux. Routing every registration through one function is what makes
// deny-by-default structural rather than a per-handler convention: a new
// route cannot be added without choosing a required scope, because mount's
// signature demands one, and cannot skip authentication, because mount
// applies it unconditionally.
//
// TestRouter_AllHandlersMountedThroughAuth enforces this by scanning this
// package's own source for any other mux.Handle/mux.HandleFunc call, so
// the property is checked mechanically and not just asserted here.
func mount(mux *http.ServeMux, h *Handlers, pattern, requiredScope string, handler http.HandlerFunc) {
	mux.HandleFunc(pattern, h.requireAuth(requiredScope, handler))
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
	// All six job/workflow routes require the jobs scope, on both the
	// legacy unprefixed surface and the canonical /v1 one
	// (docs/phase-12-plan.md §7): a deprecated route is not an
	// unauthenticated route.
	mount(mux, h, "POST "+prefix+"/jobs", principal.ScopeJobs, wrap("POST /jobs", h.CreateJob))
	mount(mux, h, "GET "+prefix+"/jobs/{id}", principal.ScopeJobs, wrap("GET /jobs/{id}", h.GetJob))
	mount(mux, h, "POST "+prefix+"/jobs/{id}/cancel", principal.ScopeJobs, wrap("POST /jobs/{id}/cancel", h.CancelJob))
	// Phase 7 — see docs/workflows.md and internal/api/workflow_handlers.go.
	mount(mux, h, "POST "+prefix+"/workflows", principal.ScopeJobs, wrap("POST /workflows", h.CreateWorkflow))
	mount(mux, h, "GET "+prefix+"/workflows/{id}", principal.ScopeJobs, wrap("GET /workflows/{id}", h.GetWorkflow))
	mount(mux, h, "POST "+prefix+"/workflows/{id}/cancel", principal.ScopeJobs, wrap("POST /workflows/{id}/cancel", h.CancelWorkflow))
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
