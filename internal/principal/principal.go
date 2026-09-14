// Package principal is TaskForge's application/API identity model, added
// in Phase 12 (docs/phase-12-plan.md, docs/enterprise-roadmap.md "Phase 12
// -- Security & Trust Boundaries", docs/security-model.md §1).
//
// It owns exactly three things:
//
//   - the durable identity of an API caller (Principal) and the
//     credentials that authenticate one (APIKey);
//   - the generation, hashing, and constant-time verification of those
//     credentials (credential.go, store.go);
//   - the small authorization value (AccessContext) that internal/api's
//     middleware attaches to a request and internal/store's queries scope
//     their WHERE clauses on (docs/phase-12-plan.md §4a).
//
// It is deliberately HTTP-agnostic -- it imports no net/http and knows
// nothing about headers, status codes, or response bodies -- mirroring the
// existing internal/job / internal/store split. internal/api depends on
// this package; this package never depends on internal/api.
//
// # What this package is NOT
//
// It is not a worker identity model. Per OD-3 (docs/phase-12-plan.md),
// TaskForge has two distinct trust boundaries in two different layers:
// the application/API boundary this package implements (multi-principal,
// potentially mutually distrusting HTTP callers, authenticated by API key
// in internal/api's middleware) and the infrastructure/database boundary
// (a worker process's identity is its PostgreSQL role credential, verified
// by PostgreSQL itself at connection time). Neither can be presented as
// the other, because they are checked by different systems. There is no
// 'worker' Kind here and migration 0005's CHECK constraint forbids one at
// the schema level too.
//
// It is also not a policy engine. Authorization in Phase 12 is exactly two
// things: a flat per-key scope list (Scope*, checked by the middleware)
// and an ownership predicate pushed into SQL (AccessContext, applied by
// internal/store). Full RBAC, permission graphs, and per-job-type policy
// are explicit non-goals (docs/phase-12-plan.md §3).
package principal

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Kind is a principal's class. There are exactly two (OD-3): an ordinary
// API caller, and an admin principal that bypasses the ownership scoping
// in docs/phase-12-plan.md §4a. Migration 0005's principals_kind_check
// enforces the same two values at the schema level.
type Kind string

const (
	// KindCaller is an ordinary API caller: it may read and cancel only
	// the jobs and workflows it itself submitted.
	KindCaller Kind = "caller"

	// KindAdmin is the documented exception to ownership scoping (G2):
	// an admin principal may read and cancel any principal's jobs and
	// workflows. This is the ONLY exception -- there is no per-resource
	// grant, no delegation, and no other way for one principal to reach
	// another's rows.
	KindAdmin Kind = "admin"
)

// SystemPrincipalID is the fixed, well-known identity every pre-Phase-12
// row is backfilled to by migration 0007 (OD-1). It is seeded by migration
// 0005 and deliberately holds no credential.
//
// "Nothing can authenticate AS the system principal" is enforced in two
// independent places, not merely asserted here: migration 0005's
// api_keys_no_system_principal CHECK constraint (schema level, effective
// against a direct INSERT) and Store.CreateAPIKey's guard (application
// level, returning ErrSystemPrincipalImmutable). Store.RevokePrincipal
// refuses it too, so "never revoked" is enforced as well. Backfilled legacy
// rows are therefore unreachable through any credential a caller could
// present -- see TestSystemPrincipal_CannotAuthenticate.
//
// It is a compile-time constant rather than a runtime lookup so no code
// path ever needs to query for it -- and, just as deliberately, NO code
// path outside migration 0007 ever assigns it. In particular neither
// txenqueue.EnqueueTx nor any HTTP handler resolves an unset principal to
// this value (docs/phase-12-plan.md §6b; proved by
// TestEnqueueTx_NoDefaultSystemPrincipalPath and
// TestCreateJob_NoDefaultSystemPrincipalOnJobRows).
var SystemPrincipalID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// Scopes are the flat, per-key capability list from OD-5. They are a
// capability list on a credential, not a role on a principal and not a
// policy language.
const (
	// ScopeJobs grants the ordinary caller capability: submit, read, and
	// cancel jobs and workflows. It grants nothing on GET /metrics.
	ScopeJobs = "jobs"

	// ScopeMetrics grants read access to GET /metrics and nothing else.
	// A metrics-only key cannot submit, read, or cancel a job.
	ScopeMetrics = "metrics"

	// ScopeAdmin is the capability superset: a key carrying it satisfies
	// every scope requirement (see AccessContext.HasScope).
	//
	// Note the deliberate separation from Kind: ScopeAdmin is about what
	// a CREDENTIAL may do, KindAdmin is about whose rows a PRINCIPAL may
	// reach. Ownership bypass comes from KindAdmin alone
	// (docs/phase-12-plan.md §6a step 6: "IsAdmin: principal.Kind ==
	// admin"), never from a scope string -- so minting a key with
	// ScopeAdmin against an ordinary caller principal widens that key's
	// capabilities without silently granting it cross-tenant read access.
	ScopeAdmin = "admin"
)

// ValidScopes is every scope string TaskForge recognises. Migration 0005
// deliberately puts no CHECK on api_keys.scopes (see that file's comment);
// this slice plus ValidateScopes is the single, central validation point
// the plan requires (docs/phase-12-plan.md §5).
var ValidScopes = []string{ScopeJobs, ScopeMetrics, ScopeAdmin}

// ValidateScopes rejects an unknown or duplicated scope string. It is
// called by CreateAPIKey, so an unrecognised scope can never be persisted
// through TaskForge's own tooling.
func ValidateScopes(scopes []string) error {
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !slices.Contains(ValidScopes, s) {
			return fmt.Errorf("principal: unknown scope %q (valid scopes: %v)", s, ValidScopes)
		}
		if seen[s] {
			return fmt.Errorf("principal: duplicate scope %q", s)
		}
		seen[s] = true
	}
	return nil
}

// Principal is one durable API-caller identity (the principals table).
type Principal struct {
	ID          uuid.UUID
	Kind        Kind
	DisplayName string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}

// IsRevoked reports whether this principal has been revoked. A revoked
// principal's keys are all treated as revoked regardless of their own
// revoked_at (docs/phase-12-plan.md §5), so revoking a principal is a
// single write that invalidates every credential it holds.
func (p *Principal) IsRevoked() bool { return p.RevokedAt != nil }

// APIKey is one credential belonging to a principal (the api_keys table).
// It never holds the raw secret: SecretHash is HMAC-SHA256(pepper, secret)
// and the raw secret exists only in the one-time value CreateAPIKey
// returns to the operator (docs/phase-12-plan.md §6a).
type APIKey struct {
	ID          uuid.UUID
	PrincipalID uuid.UUID
	KeyID       string
	SecretHash  []byte
	Scopes      []string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	LastUsedAt  *time.Time
}

// AccessContext is the authorization value that travels from the
// authentication middleware (internal/api/auth.go) into the handlers and
// on into internal/store's SQL (docs/phase-12-plan.md §4a). It is
// deliberately tiny and immutable-by-convention: it carries the answer to
// "whose rows may this request touch", nothing more.
//
// It is ALWAYS derived from a verified credential. It is never built from
// anything in a request body, query string, path parameter, or forwarded
// header -- see internal/api's TestAuth_ForwardedHeadersCannotImpersonate
// and TestCreateJob_RequestBodyPrincipalIDFieldIgnored, which prove both
// halves of that rule against real behaviour.
type AccessContext struct {
	// PrincipalID is the authenticated caller's identity. Every
	// principal-scoped query binds it as a parameter.
	PrincipalID uuid.UUID

	// IsAdmin is true only when the owning principal's Kind is KindAdmin.
	// It is the single documented exception to ownership scoping (G2):
	// when true, internal/store's scoping predicate matches every row
	// regardless of principal_id.
	IsAdmin bool

	// Scopes is the verified credential's capability list.
	Scopes []string
}

// HasScope reports whether this credential carries required. A credential
// carrying ScopeAdmin satisfies every requirement (OD-5: "admin ...
// implies jobs"), which is what lets one admin key both submit jobs and
// scrape GET /metrics.
func (a AccessContext) HasScope(required string) bool {
	return slices.Contains(a.Scopes, required) || slices.Contains(a.Scopes, ScopeAdmin)
}

// contextKey is unexported so no package outside this one can inject or
// overwrite an AccessContext on a request context: the only way a value
// reaches FromContext is through NewContext, and the only production
// caller of NewContext is internal/api's authentication middleware, after
// a successful Verify.
type contextKey struct{}

// NewContext returns a copy of ctx carrying ac.
func NewContext(ctx context.Context, ac AccessContext) context.Context {
	return context.WithValue(ctx, contextKey{}, ac)
}

// FromContext returns the AccessContext attached by the authentication
// middleware. ok is false when no authenticated identity is present, which
// -- given the middleware is the only mount point for every route
// (internal/api/server.go's mount helper) -- means a wiring bug, not an
// ordinary request. Callers must fail closed on !ok, never continue with a
// zero-valued AccessContext (a zero AccessContext has uuid.Nil as its
// PrincipalID, which matches no row, but "matches no row" is not a safe
// thing to rely on for a mutating path).
func FromContext(ctx context.Context) (AccessContext, bool) {
	ac, ok := ctx.Value(contextKey{}).(AccessContext)
	return ac, ok
}
