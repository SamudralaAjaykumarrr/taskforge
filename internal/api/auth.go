// Phase 12 (docs/phase-12-plan.md §6a, docs/enterprise-roadmap.md "Phase
// 12 -- Security & Trust Boundaries"): HTTP authentication and scope
// enforcement. This file is middleware only -- it depends on
// internal/principal, never the reverse, and it adds no route.
//
// It is the single place a credential is turned into an identity. Every
// route in this package is mounted through server.go's mount helper, which
// wraps the handler in requireAuth: a route cannot exist in this router
// without passing through here, so authentication is deny-by-default
// structurally rather than by each handler remembering to opt in
// (verification point 5, proved by TestRouter_AllHandlersMountedThroughAuth).
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
)

// Authenticator turns a raw bearer credential into an AccessContext.
// *principal.Store is the production implementation; the interface exists
// for the same reason JobStore does -- so this package depends on a
// behaviour, not a concrete database-backed type, and so tests can wire a
// router without a live principals table when the test's subject is
// something else entirely.
type Authenticator interface {
	Verify(ctx context.Context, credential string) (principal.AccessContext, error)
}

// unauthorizedBody is the ONE response body every authentication failure
// produces, regardless of which of docs/phase-12-plan.md §6a's five
// failure steps produced it -- absent header, wrong scheme, malformed
// credential, unknown key_id, revoked key, expired key, wrong secret, or
// revoked principal.
//
// This uniformity is the point (verification point 9, extended from
// resources to credentials): if a revoked key produced a different body
// from an unknown one, a caller could enumerate which key_ids exist and
// which have been retired. The reason is still recorded -- as a bounded
// metric label and a server-side log field (see recordAuthFailure) -- it
// simply never crosses the response boundary.
//
// TestAuth_AllFailureReasons_ProduceByteIdenticalResponses compares the
// actual bytes across every failure path rather than trusting this
// constant's existence.
const unauthorizedBody = "unauthorized"

// denyAllAuthenticator is the authenticator a router gets when none was
// supplied. It rejects everything.
//
// Failing closed on a wiring mistake is deliberate: the alternative --
// treating "no authenticator configured" as "authentication disabled" --
// is exactly the shape of accident that silently ships an open API. There
// is no configuration flag that turns authentication off; OD-6's hard
// cutover means there is no observe/enforce dual mode to fall back into
// either.
type denyAllAuthenticator struct{}

func (denyAllAuthenticator) Verify(context.Context, string) (principal.AccessContext, error) {
	return principal.AccessContext{}, errors.New("api: no authenticator configured")
}

// requireAuth wraps next with authentication and a scope requirement. It
// is applied by server.go's mount helper to every route, including the
// legacy unprefixed ones -- a deprecated route is not an unauthenticated
// one (docs/phase-12-plan.md §7).
//
// Order of operations, and why:
//
//  1. Authenticate. A bad or absent credential is 401 with the uniform
//     body above. Nothing about the request beyond its Authorization
//     header has been examined at this point, so no unauthenticated
//     request ever reaches a handler, a path parameter, or the store.
//  2. Check the scope. A valid credential lacking the required scope is
//     403, deliberately NOT 404/401: a scope mismatch on a route with no
//     per-resource id at stake discloses nothing about any tenant's data,
//     and a clear 403 is far more operable for a caller who has simply
//     been issued the wrong kind of key. This is the documented
//     difference from G2's ownership rule, where "not yours" and "does
//     not exist" must be indistinguishable (docs/phase-12-plan.md §6a).
//  3. Attach the AccessContext and call the handler.
//
// Note what is NOT consulted anywhere in this function: no
// X-Forwarded-For, no X-Forwarded-Proto, no X-Real-IP, no client IP, no
// request body, no query parameter. Identity comes from the credential and
// from nothing else (docs/phase-12-plan.md §10, verification point 15;
// proved by TestAuth_ForwardedHeadersCannotImpersonatePrincipal).
func (h *Handlers) requireAuth(requiredScope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		credential, err := principal.ParseAuthorizationHeader(r.Header.Get("Authorization"))
		if err == nil {
			var ac principal.AccessContext
			ac, err = h.auth.Verify(r.Context(), credential)
			if err == nil {
				if !ac.HasScope(requiredScope) {
					h.logger.Warn("request rejected: credential lacks required scope",
						"event", "authz_scope_denied",
						"actor", ac.PrincipalID.String(),
						"required_scope", requiredScope,
						"method", r.Method, "path", r.URL.Path)
					writeError(w, http.StatusForbidden, "credential lacks required scope "+requiredScope)
					return
				}
				next(w, r.WithContext(principal.NewContext(r.Context(), ac)))
				return
			}
		}

		h.recordAuthFailure(err, r)
		// RFC 9110 §11.6.1 requires a WWW-Authenticate challenge on 401.
		// The value is a fixed constant, identical on every failure path,
		// so it discloses nothing that the uniform body does not.
		w.Header().Set("WWW-Authenticate", principal.BearerPrefix)
		writeError(w, http.StatusUnauthorized, unauthorizedBody)
	}
}

// recordAuthFailure is the server-side half of docs/phase-12-plan.md §10:
// the specific reason a credential was rejected is preserved for the
// operator (a bounded metric label plus a structured log line), while the
// caller sees only the uniform 401 above.
//
// The credential itself -- raw secret, key_id, or the whole Authorization
// header -- is never included in either. reason is a value from
// principal.AllFailureReasons, a fixed six-value enum plus
// "internal_error", so the metric stays cardinality-safe in exactly the
// sense docs/observability.md's Cardinality Policy already requires of
// job_type and outcome.
//
// This gives an operator the signal to notice a credential-stuffing
// pattern and respond with their own edge/WAF tooling. It is deliberately
// NOT a rate limiter or a lockout: automated brute-force mitigation is an
// explicit, reasoned Phase 13 deferral (docs/phase-12-plan.md §3), not a
// gap this phase claims to have closed.
//
// KNOWN, DOCUMENTED FOLLOW-UP -- not fixed in Phase 12, and not claimed to
// be. Every Verify error lands here, INCLUDING a wrapped database error
// from the api_keys lookup, and every one of them produces a 401. So a
// credential-store OUTAGE is reported to the caller as "your credential is
// bad", and a well-behaved client stops retrying during an incident it
// should retry through. This fails closed, which is the right security
// posture, and the distinction does survive server-side: FailureReason maps
// an unrecognised error to ReasonInternal, so
// taskforge_auth_failures_total{reason="internal_error"} is the operator's
// signal that this is an outage rather than credential stuffing. Returning
// 503 for that case instead is deferred, not done -- the uniform-401 rule
// would not forbid it, since an infrastructure failure discloses nothing
// about any credential. See docs/security-model.md §5.
func (h *Handlers) recordAuthFailure(err error, r *http.Request) {
	reason := principal.FailureReason(err)
	h.metrics.AuthFailuresTotal.WithLabelValues(reason).Inc()
	h.logger.Warn("request rejected: authentication failed",
		"event", "auth_failed", "reason", reason,
		"method", r.Method, "path", r.URL.Path)
}

// accessContext retrieves the authenticated identity a successful
// requireAuth attached. It fails closed: if the value is missing -- which,
// given mount is the only way a handler is reachable, means a wiring bug
// rather than an ordinary request -- the request is rejected with the same
// uniform 401 rather than continuing with a zero-valued AccessContext.
//
// Handlers must use ONLY this function to learn who is calling. The
// authenticated principal never comes from a request body field, a path
// or query parameter, or a header (docs/phase-12-plan.md §6a, verification
// point 6).
func (h *Handlers) accessContext(w http.ResponseWriter, r *http.Request) (principal.AccessContext, bool) {
	ac, ok := principal.FromContext(r.Context())
	if !ok {
		h.logger.Error("handler reached without an authenticated principal; refusing the request",
			"event", "auth_context_missing", "method", r.Method, "path", r.URL.Path)
		w.Header().Set("WWW-Authenticate", principal.BearerPrefix)
		writeError(w, http.StatusUnauthorized, unauthorizedBody)
		return principal.AccessContext{}, false
	}
	return ac, true
}
