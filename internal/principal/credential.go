package principal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Credential format (OD-2, docs/phase-12-plan.md §6a).
//
//	Authorization: Bearer <key_id>.<secret>
//
// There is deliberately no X-API-Key header and no second accepted
// transport: one credential shape, checked in one place.
const (
	// KeyIDBytes is the length of the non-secret lookup identifier: 16
	// random bytes, hex-encoded to 32 characters. It is not a secret --
	// it is safe in an operator's inventory, a support ticket, or a
	// server-side log line -- and exists so verification can find the
	// right row with an O(1) indexed lookup on a NON-secret value. Never
	// using a raw secret as a lookup key removes a timing/observability
	// channel that a "SELECT ... WHERE secret = $1" design would have.
	KeyIDBytes = 16

	// SecretBytes is the credential's entropy: 32 bytes = 256 bits from
	// crypto/rand, base64url-encoded without padding. This is why
	// docs/phase-12-plan.md §3 can classify online brute-forcing as a
	// non-goal with a real rationale rather than an omission: guessing a
	// 256-bit random token is not the guessable-password threat model a
	// login form has.
	SecretBytes = 32

	// CredentialSeparator joins the two halves on the wire.
	CredentialSeparator = "."

	// BearerPrefix is the Authorization scheme, matched
	// case-insensitively per RFC 9110 §11.1 (auth-scheme is
	// case-insensitive).
	BearerPrefix = "Bearer"
)

// GenerateKeyID returns a fresh, non-secret key identifier.
func GenerateKeyID() (string, error) {
	b := make([]byte, KeyIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("principal: generate key id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateSecret returns a fresh raw secret. The returned value is the
// ONLY time this secret ever exists outside the caller's own memory: it is
// never persisted (only its HMAC is), never logged, and not re-derivable
// afterwards.
func GenerateSecret() (string, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("principal: generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// FormatCredential renders the wire form a caller puts after "Bearer ".
func FormatCredential(keyID, secret string) string {
	return keyID + CredentialSeparator + secret
}

// ParseCredential splits a bearer value into its non-secret key_id and its
// secret halves. It performs no I/O and no comparison -- it is pure
// syntax, so a malformed credential is rejected (step 1 of
// docs/phase-12-plan.md §6a's verification sequence) before any database
// lookup happens at all.
//
// SplitN with n=2 means a secret containing the separator is preserved
// intact rather than truncated; base64url never emits '.', so this only
// matters for defensive correctness against a hand-written credential.
func ParseCredential(raw string) (keyID, secret string, err error) {
	keyID, secret, found := strings.Cut(raw, CredentialSeparator)
	if !found || keyID == "" || secret == "" {
		return "", "", ErrMalformedCredential
	}
	return keyID, secret, nil
}

// ParseAuthorizationHeader extracts the bearer value from an Authorization
// header value, returning ErrMalformedCredential for an absent header, a
// non-Bearer scheme, or an empty value. It does not parse the credential
// itself -- ParseCredential does that -- so the two syntactic failures stay
// separable in code while collapsing to the same caller-visible response.
func ParseAuthorizationHeader(header string) (string, error) {
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, BearerPrefix) {
		return "", ErrMalformedCredential
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ErrMalformedCredential
	}
	return value, nil
}

// HashSecret computes the value stored in api_keys.secret_hash:
// HMAC-SHA256(pepper, secret).
//
// An HMAC under a server-held pepper rather than a bare SHA-256(secret) is
// what makes a stolen api_keys table insufficient on its own to forge a
// credential: an attacker with the table but not the running process's
// TASKFORGE_API_KEY_PEPPER cannot compute a matching hash for any secret
// they choose, and cannot verify a guess offline either
// (docs/phase-12-plan.md §6a, verification point 3).
//
// A slow password KDF (bcrypt/scrypt/argon2) is deliberately NOT used
// here: those exist to make offline brute-forcing of LOW-entropy
// human-chosen passwords expensive. This credential's secret is 256 bits
// from crypto/rand (SecretBytes), so there is no low-entropy guess space
// to protect, and a per-request KDF would add latency to every
// authenticated request for no threat-model benefit. The pepper, not work
// factor, is what does the work here.
func HashSecret(pepper []byte, secret string) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}
