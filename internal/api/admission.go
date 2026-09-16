// Phase 13 (docs/phase-13-plan.md §6/§8/§9; ADR-0009's own explicit
// non-scope for the concurrency-limit/admission layer split): the
// submission-path admission check ahead of the existing insert path.
//
// This is deliberately narrow, matching the roadmap's own "ordinary
// concurrency-cap saturation (job still enqueues and waits) explicitly
// not a rejection condition" rule: reaching a queue's configured
// concurrency_limit is never itself a 429 or 503 here -- concurrency is
// enforced only at CLAIM time (internal/store/claim.go's slot-table
// mechanism), never at submission time. This file governs exactly the two
// backpressure conditions the roadmap actually names for the API layer:
//
//   - 429 (rate limit): a queue's own configured, static submission-rate
//     policy has been exceeded (internal/governance's durable token
//     bucket).
//   - 503 (system capacity): this process's own bounded in-flight
//     submission-handler semaphore (TASKFORGE_MAX_INFLIGHT_SUBMISSIONS,
//     OD-6) is exhausted.
//
// Both carry Retry-After and neither response body discloses any
// tenant-specific detail (§9: "must not reveal another tenant's queue
// depth or limit configuration" for 429; 503 "is not principal-scoped at
// all" by construction, since it says nothing about any specific tenant).
package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/governance"
)

// GovernanceStore is the read/consume contract this package depends on
// for admission decisions. *governance.Store satisfies it structurally.
type GovernanceStore interface {
	// GetQueueLimit returns queueName's queue-wide configuration, or
	// (nil, nil) if none has ever been set (unlimited, no rate limit --
	// the default for any queue an operator has not configured).
	GetQueueLimit(ctx context.Context, queueName string) (*governance.QueueLimit, error)
	CheckAndConsumeRateLimit(ctx context.Context, scopeKey string, ratePerSec float64, burst int) (allowed bool, retryAfter time.Duration, err error)
}

// WithGovernance attaches the governance store the rate-limit half of
// admission reads/consumes from. Omitting it disables rate limiting
// entirely (every submission is admitted on the rate dimension) -- the
// same fail-safe-default pattern WithAuthenticator/WithMetrics already
// use, so no call site needs a nil check, and an operator who has not
// configured any queue's rate limit sees no behavior change from before
// this phase.
func WithGovernance(g GovernanceStore) HandlersOption {
	return func(h *Handlers) {
		if g != nil {
			h.governance = g
		}
	}
}

// WithMaxInflightSubmissions bounds concurrent in-flight POST
// /jobs/POST /workflows handler executions (docs/phase-13-plan.md §8 OD-6).
// n <= 0 disables the bound entirely (the pre-Phase-13 default: no system-
// capacity 503 of any kind) -- this default value is an implementation
// decision this plan explicitly leaves open, mirroring
// internal/config.ActiveWorkerWindow's own "implementation decision"
// precedent.
func WithMaxInflightSubmissions(n int) HandlersOption {
	return func(h *Handlers) {
		if n > 0 {
			h.inflight = make(chan struct{}, n)
		}
	}
}

// admissionRejectionRetryAfter is the fixed Retry-After this package
// reports for a 503 system-capacity rejection. Unlike a 429's Retry-After
// (computed exactly from the rate limiter's own refill state), system
// capacity has no analogous "when will a slot free up" computation
// available cheaply at reject time -- in-flight submissions are typically
// sub-second HTTP handlers, so a short fixed value is a reasonable,
// documented choice, not a measured guarantee.
const admissionRejectionRetryAfter = 1 * time.Second

// admitSubmission is the shared submission-admission gate CreateJob and
// CreateWorkflow both call, once queueName is known (after request-body
// validation, so the check applies to the caller's ACTUAL target queue,
// never a placeholder). If it returns ok=false, it has already written
// the complete HTTP response (429 or 503) and the caller must return
// immediately without inserting anything. If it returns ok=true, the
// returned release func must be deferred by the caller (release is a
// harmless no-op if nothing was acquired, e.g. when
// WithMaxInflightSubmissions was never configured).
func (h *Handlers) admitSubmission(w http.ResponseWriter, r *http.Request, actor, queueName string) (release func(), ok bool) {
	release = func() {}

	if h.inflight != nil {
		select {
		case h.inflight <- struct{}{}:
			release = func() { <-h.inflight }
		default:
			h.metrics.AdmissionRejectionsTotal.WithLabelValues("503", "capacity").Inc()
			h.logger.Warn("submission rejected: system at capacity", "event", "admission_rejected_capacity")
			writeAdmissionRejection(w, http.StatusServiceUnavailable, admissionRejectionRetryAfter)
			return func() {}, false
		}
	}

	if h.governance != nil {
		limit, err := h.governance.GetQueueLimit(r.Context(), queueName)
		if err != nil {
			// A governance lookup failure is an internal error, not a
			// caller-facing rate-limit decision -- fail the request
			// loudly rather than silently admitting or silently
			// rejecting on a condition the caller did nothing to cause.
			release()
			h.logger.Error("failed to read queue governance configuration", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to evaluate admission policy")
			return func() {}, false
		}
		if limit != nil && limit.RateLimitPerSec != nil && limit.RateLimitBurst != nil {
			allowed, retryAfter, err := h.governance.CheckAndConsumeRateLimit(
				r.Context(), governance.RateLimitScopeKey(queueName), *limit.RateLimitPerSec, *limit.RateLimitBurst)
			if err != nil {
				release()
				h.logger.Error("failed to evaluate rate limit", "error", err)
				writeError(w, http.StatusInternalServerError, "failed to evaluate admission policy")
				return func() {}, false
			}
			if !allowed {
				release()
				h.metrics.AdmissionRejectionsTotal.WithLabelValues("429", "rate_limited").Inc()
				// actor (the authenticated principal) is logged, per
				// Phase 12's audit-log precedent for a submit-path
				// rejection (docs/phase-12-plan.md §10) -- the raw
				// queue's configured rate/burst is server-side
				// diagnostic detail, never disclosed in the response
				// body itself.
				h.logger.Warn("submission rejected: rate limit exceeded", "event", "rate_limited", "actor", actor)
				writeAdmissionRejection(w, http.StatusTooManyRequests, retryAfter)
				return func() {}, false
			}
		}
	}

	return release, true
}

// writeAdmissionRejection writes a uniform, non-tenant-disclosing 429/503
// body with a Retry-After header (whole seconds, per RFC 9110 §10.2.3).
// Deliberately shares no fields with errorResponse beyond "error": a
// caller must not be able to distinguish "your queue's rate limit" from
// "the system's capacity" by response SHAPE, only by status code, and
// must never see another tenant's queue depth or configuration
// (docs/phase-13-plan.md §9).
func writeAdmissionRejection(w http.ResponseWriter, status int, retryAfter time.Duration) {
	seconds := int(retryAfter.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	writeError(w, status, "request rejected by admission policy; retry after the indicated interval")
}
