package principal

import "errors"

// Verification failure reasons (docs/phase-12-plan.md §6a, steps 1-5).
//
// These exist so internal/api can label the taskforge_auth_failures_total
// metric and its server-side diagnostic log line with WHY a credential was
// rejected. They are never surfaced to the caller: every one of them
// produces the identical generic 401 body, so a caller cannot use response
// differences to probe which key_ids exist, which are revoked, or which
// merely had the wrong secret (docs/phase-12-plan.md §6a, verification
// point 9).
//
// This is a deliberate, reviewed asymmetry with txenqueue's public error
// contract, which deliberately leaks nothing at all (see
// docs/transactional-enqueue.md "Error contract" and
// docs/phase-12-plan.md §14): an internal Go error consumed by an internal
// metric label is a different exposure surface from a public package's
// returned error. internal/principal is unimportable outside this module.
var (
	// ErrMalformedCredential means the Authorization header was absent,
	// did not use the Bearer scheme, or the bearer value was not of the
	// form <key_id>.<secret>. No database lookup is performed.
	ErrMalformedCredential = errors.New("principal: malformed credential")

	// ErrUnknownKey means no api_keys row exists for the presented
	// key_id.
	ErrUnknownKey = errors.New("principal: unknown key")

	// ErrKeyRevoked means the api_keys row exists but has revoked_at set.
	ErrKeyRevoked = errors.New("principal: key revoked")

	// ErrKeyExpired means the api_keys row exists but its expires_at has
	// passed.
	ErrKeyExpired = errors.New("principal: key expired")

	// ErrBadSecret means the presented secret's HMAC did not match the
	// stored secret_hash. The comparison that produces this is
	// constant-time (see Store.Verify).
	ErrBadSecret = errors.New("principal: secret mismatch")

	// ErrPrincipalRevoked means the credential itself is intact but the
	// principal owning it has been revoked -- which invalidates every one
	// of that principal's keys at once, regardless of their own
	// revoked_at.
	ErrPrincipalRevoked = errors.New("principal: principal revoked")

	// ErrSystemPrincipalImmutable is returned when operator tooling tries to
	// mint a credential for, or revoke, the system principal (OD-1).
	//
	// That identity is migration 0007's backfill target for every
	// pre-Phase-12 row and nothing else. A credential for it would
	// authenticate a caller into every legacy row in the database, so it is
	// refused here and by migration 0005's api_keys_no_system_principal
	// CHECK constraint -- application-level for a clear message,
	// schema-level so the guarantee survives a direct INSERT.
	//
	// It is never produced on the verification path: the system principal
	// simply has no api_keys row to find, so Verify reports ErrUnknownKey
	// like any other unknown key_id and discloses nothing.
	ErrSystemPrincipalImmutable = errors.New("principal: the system principal cannot hold a credential or be revoked")

	// ErrNotFound is returned by the operator-facing lookup helpers
	// (GetPrincipal, GetAPIKeyByKeyID) when no such row exists. It is
	// never produced on the verification path -- that path reports
	// ErrUnknownKey instead, so a "does this key exist" question and a
	// "did this credential authenticate" question can never be conflated.
	ErrNotFound = errors.New("principal: not found")
)

// Failure reason labels for taskforge_auth_failures_total (OD-5/§10).
// This is a fixed, small, cardinality-safe enum -- never a raw key,
// principal ID, header value, or error string.
const (
	ReasonMalformed        = "malformed"
	ReasonUnknownKey       = "unknown_key"
	ReasonRevoked          = "revoked"
	ReasonExpired          = "expired"
	ReasonBadSecret        = "bad_secret"
	ReasonPrincipalRevoked = "principal_revoked"
	ReasonInternal         = "internal_error"
)

// FailureReason maps a Verify error onto its metric/log label. Anything
// unrecognised (e.g. a genuine database failure) maps to ReasonInternal
// rather than to a credential-specific reason, so an infrastructure
// outage can never be mistaken for, or reported as, a credential problem.
func FailureReason(err error) string {
	switch {
	case errors.Is(err, ErrMalformedCredential):
		return ReasonMalformed
	case errors.Is(err, ErrUnknownKey):
		return ReasonUnknownKey
	case errors.Is(err, ErrKeyRevoked):
		return ReasonRevoked
	case errors.Is(err, ErrKeyExpired):
		return ReasonExpired
	case errors.Is(err, ErrBadSecret):
		return ReasonBadSecret
	case errors.Is(err, ErrPrincipalRevoked):
		return ReasonPrincipalRevoked
	default:
		return ReasonInternal
	}
}

// AllFailureReasons is every label FailureReason can produce, for tests
// and for pre-initialising the metric's label set.
var AllFailureReasons = []string{
	ReasonMalformed,
	ReasonUnknownKey,
	ReasonRevoked,
	ReasonExpired,
	ReasonBadSecret,
	ReasonPrincipalRevoked,
	ReasonInternal,
}
