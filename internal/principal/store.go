package principal

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Store is the PostgreSQL-backed principal/credential repository. Like
// internal/store.Store it wraps an already-open *sql.DB owned by the
// caller, and holds one additional piece of state: the server-held pepper
// (docs/phase-12-plan.md §6a).
//
// The pepper lives only in memory, read once at process start from
// TASKFORGE_API_KEY_PEPPER. It is never persisted, never logged, and never
// included in any error this package returns. Losing it invalidates every
// stored secret_hash's verifiability (all keys must be reissued) -- an
// accepted, documented risk for this phase, proportionate to
// docs/security-model.md §3's existing framing of secrets management.
type Store struct {
	db     *sql.DB
	pepper []byte
	now    func() time.Time
}

// NewStore constructs a Store. pepper must be non-empty: an empty pepper
// would silently degrade HashSecret to a plain keyed-with-nothing HMAC,
// which is exactly the "stolen table is sufficient to forge" property the
// pepper exists to prevent, so it is rejected loudly at construction
// rather than discovered later.
func NewStore(db *sql.DB, pepper []byte) (*Store, error) {
	if db == nil {
		return nil, errors.New("principal: NewStore requires a non-nil *sql.DB")
	}
	if len(pepper) == 0 {
		return nil, errors.New("principal: NewStore requires a non-empty pepper (TASKFORGE_API_KEY_PEPPER)")
	}
	return &Store{db: db, pepper: pepper, now: time.Now}, nil
}

// NewCredential is the one-time result of CreateAPIKey. Secret (and
// therefore Credential) is the only moment the raw secret exists outside
// the caller's own memory: it is not stored, not recoverable, and not
// re-displayable. Operator tooling shows it once and the operator is
// responsible for delivering it to whoever will present it.
type NewCredential struct {
	APIKey APIKey

	// Secret is the raw secret half. Never log this.
	Secret string

	// Credential is the exact string that goes after "Bearer " on the
	// wire: "<key_id>.<secret>". Never log this either.
	Credential string
}

// CreatePrincipal durably creates a new API-caller identity. kind must be
// KindCaller or KindAdmin; migration 0005's principals_kind_check enforces
// the same set at the schema level, so an invalid kind fails either way --
// this check simply fails with a clearer message and without a round trip.
//
// This is operator tooling, not an HTTP endpoint. Phase 12 deliberately
// adds no self-service key-management API (docs/phase-12-plan.md §3): key
// lifecycle is an operator-run operation against this package.
func (s *Store) CreatePrincipal(ctx context.Context, kind Kind, displayName string) (*Principal, error) {
	if kind != KindCaller && kind != KindAdmin {
		return nil, fmt.Errorf("principal: invalid kind %q (valid kinds: %q, %q)", kind, KindCaller, KindAdmin)
	}
	if strings.TrimSpace(displayName) == "" {
		return nil, errors.New("principal: display_name is required")
	}

	p := Principal{ID: uuid.New(), Kind: kind, DisplayName: displayName}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO principals (id, kind, display_name)
		VALUES ($1, $2, $3)
		RETURNING created_at`, p.ID, string(p.Kind), p.DisplayName,
	).Scan(&p.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("principal: create principal: %w", err)
	}
	return &p, nil
}

// CreateAPIKey mints a fresh credential for principalID and returns it
// exactly once (docs/phase-12-plan.md §6a). Only the HMAC of the secret is
// persisted -- the raw secret never reaches the database.
//
// Multiple live, non-revoked keys per principal are ordinary and
// supported: that IS the rotation-with-overlap mechanism
// (docs/security-model.md's "rotation with an overlap window"). Mint the
// new key, deploy it, then RevokeAPIKey the old one -- no synchronised
// cutover and no redeploy required at any point.
func (s *Store) CreateAPIKey(ctx context.Context, principalID uuid.UUID, scopes []string, expiresAt *time.Time) (*NewCredential, error) {
	// The system principal must never hold a credential (OD-1). It is the
	// identity migration 0007 backfills every pre-Phase-12 row to, so a key
	// minted against it would authenticate a caller directly into every
	// legacy row in the database.
	//
	// This is checked here AND by migration 0005's
	// api_keys_no_system_principal CHECK constraint. Two guards rather than
	// one because the property was previously stated in comments and
	// enforced in neither: an independent review minted a working
	// credential for this principal through the ordinary operator tool.
	// The application check gives a clear error without a round trip; the
	// schema check makes the guarantee hold against a direct INSERT and
	// against any future code path that forgets to ask.
	if principalID == SystemPrincipalID {
		return nil, ErrSystemPrincipalImmutable
	}
	if err := ValidateScopes(scopes); err != nil {
		return nil, err
	}

	keyID, err := GenerateKeyID()
	if err != nil {
		return nil, err
	}
	secret, err := GenerateSecret()
	if err != nil {
		return nil, err
	}

	k := APIKey{
		ID:          uuid.New(),
		PrincipalID: principalID,
		KeyID:       keyID,
		SecretHash:  HashSecret(s.pepper, secret),
		Scopes:      append([]string(nil), scopes...),
		ExpiresAt:   expiresAt,
	}

	var expires sql.NullTime
	if expiresAt != nil {
		expires = sql.NullTime{Time: *expiresAt, Valid: true}
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO api_keys (id, principal_id, key_id, secret_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5::text[], $6)
		RETURNING created_at`,
		k.ID, k.PrincipalID, k.KeyID, k.SecretHash, textArrayLiteral(k.Scopes), expires,
	).Scan(&k.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("principal: create api key: %w", err)
	}

	return &NewCredential{
		APIKey:     k,
		Secret:     secret,
		Credential: FormatCredential(keyID, secret),
	}, nil
}

// RevokeAPIKey revokes one credential. It takes effect on that key's very
// next verification -- Verify reads revoked_at from the database on every
// request, with no cache and no TTL anywhere in the path, so revocation
// never requires a restart or a redeploy (docs/security-model.md).
// Revoking an already-revoked key is an idempotent no-op that preserves
// the original revocation timestamp.
func (s *Store) RevokeAPIKey(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now()) WHERE key_id = $1`, keyID)
	if err != nil {
		return fmt.Errorf("principal: revoke api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokePrincipal revokes an identity outright. Every key that principal
// holds is then rejected regardless of its own revoked_at (Verify step 5),
// so this is the single write that cuts off a compromised caller entirely
// rather than one credential at a time.
func (s *Store) RevokePrincipal(ctx context.Context, id uuid.UUID) error {
	// The system principal is documented as never deleted and never revoked
	// (migration 0005, OD-1). Revoking it would not grant anyone access --
	// it holds no credential and cannot -- but it would make the durable
	// owner of every backfilled legacy row read as revoked, contradicting a
	// property other code and docs state as fact. Refuse rather than let the
	// documented invariant quietly become false.
	if id == SystemPrincipalID {
		return ErrSystemPrincipalImmutable
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE principals SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("principal: revoke principal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetPrincipal reads one principal by id.
func (s *Store) GetPrincipal(ctx context.Context, id uuid.UUID) (*Principal, error) {
	var p Principal
	var kind string
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, display_name, created_at, revoked_at FROM principals WHERE id = $1`, id,
	).Scan(&p.ID, &kind, &p.DisplayName, &p.CreatedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("principal: get principal: %w", err)
	}
	p.Kind = Kind(kind)
	if revokedAt.Valid {
		p.RevokedAt = &revokedAt.Time
	}
	return &p, nil
}

// GetAPIKeyByKeyID reads one api_keys row by its non-secret identifier.
// It is operator/diagnostic tooling, not part of the verification path --
// Verify issues its own single query so credential checking never depends
// on a helper whose error contract is "does this row exist".
func (s *Store) GetAPIKeyByKeyID(ctx context.Context, keyID string) (*APIKey, error) {
	var k APIKey
	var scopesText string
	var expiresAt, revokedAt, lastUsedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, principal_id, key_id, secret_hash, scopes::text, created_at, expires_at, revoked_at, last_used_at
		FROM api_keys WHERE key_id = $1`, keyID,
	).Scan(&k.ID, &k.PrincipalID, &k.KeyID, &k.SecretHash, &scopesText, &k.CreatedAt, &expiresAt, &revokedAt, &lastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("principal: get api key: %w", err)
	}
	k.Scopes = parseTextArrayLiteral(scopesText)
	if expiresAt.Valid {
		k.ExpiresAt = &expiresAt.Time
	}
	if revokedAt.Valid {
		k.RevokedAt = &revokedAt.Time
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = &lastUsedAt.Time
	}
	return &k, nil
}

// Verify authenticates a raw bearer credential ("<key_id>.<secret>") and
// returns the AccessContext the rest of the request runs under. It is the
// exact sequence docs/phase-12-plan.md §6a specifies:
//
//  1. Parse key_id/secret. Malformed -> ErrMalformedCredential, with no
//     database access at all.
//  2. Look the row up by the NON-SECRET key_id, in one indexed query.
//     Not found -> ErrUnknownKey.
//  3. Revoked or expired -> ErrKeyRevoked / ErrKeyExpired.
//  4. Recompute HMAC-SHA256(pepper, presented secret) and compare it
//     against the stored hash with crypto/subtle.ConstantTimeCompare.
//     Mismatch -> ErrBadSecret.
//  5. The owning principal itself revoked -> ErrPrincipalRevoked.
//  6. Success: build the AccessContext, and record last_used_at
//     best-effort, off the request's critical path.
//
// # Timing
//
// Step 4's comparison is constant-time with respect to WHERE the two
// digests differ, which is the property that matters: an attacker holding
// a valid key_id must not be able to learn a correct secret byte-by-byte
// from response latency. Both operands are fixed-width (32-byte) HMAC
// outputs, so length never varies with input either.
//
// Stated honestly rather than overclaimed: steps 2, 3 and 5 return before
// any HMAC is computed, so a request against a nonexistent key_id is
// measurably cheaper than one against a real key_id with a wrong secret.
// That distinction is inherent to any design that looks a credential up by
// identifier (which docs/security-model.md explicitly requires, so that
// keys can be rotated and revoked without scanning raw secrets), it leaks
// only "this key_id exists" -- never any part of a secret -- and key_id is
// not a secret value. The client-visible response is identical in every
// case (internal/api/auth.go collapses all of these to one generic 401),
// so this channel is not observable through the response contract itself.
//
// # Errors
//
// A non-nil error is always one of this package's reason sentinels (which
// internal/api maps onto a metric label and a server-side log field) or a
// wrapped database error, which FailureReason classifies as
// ReasonInternal. No error returned here ever contains the raw secret,
// the credential, or the pepper.
func (s *Store) Verify(ctx context.Context, credential string) (AccessContext, error) {
	keyID, secret, err := ParseCredential(credential)
	if err != nil {
		return AccessContext{}, err
	}

	var (
		rowID       uuid.UUID
		principalID uuid.UUID
		storedHash  []byte
		scopesText  string
		expiresAt   sql.NullTime
		revokedAt   sql.NullTime
		kind        string
		pRevokedAt  sql.NullTime
	)
	// One query, joined: the api_keys row and its owning principal's
	// revocation state arrive together, so step 5 needs no second round
	// trip on the hot path.
	err = s.db.QueryRowContext(ctx, `
		SELECT k.id, k.principal_id, k.secret_hash, k.scopes::text, k.expires_at, k.revoked_at,
		       p.kind, p.revoked_at
		FROM api_keys k JOIN principals p ON p.id = k.principal_id
		WHERE k.key_id = $1`, keyID,
	).Scan(&rowID, &principalID, &storedHash, &scopesText, &expiresAt, &revokedAt, &kind, &pRevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessContext{}, ErrUnknownKey
	}
	if err != nil {
		return AccessContext{}, fmt.Errorf("principal: verify: lookup: %w", err)
	}

	if revokedAt.Valid {
		return AccessContext{}, ErrKeyRevoked
	}
	if expiresAt.Valid && !expiresAt.Time.After(s.now()) {
		return AccessContext{}, ErrKeyExpired
	}

	presented := HashSecret(s.pepper, secret)
	if subtle.ConstantTimeCompare(presented, storedHash) != 1 {
		return AccessContext{}, ErrBadSecret
	}

	if pRevokedAt.Valid {
		return AccessContext{}, ErrPrincipalRevoked
	}

	s.touchLastUsed(rowID)

	return AccessContext{
		PrincipalID: principalID,
		IsAdmin:     Kind(kind) == KindAdmin,
		Scopes:      parseTextArrayLiteral(scopesText),
	}, nil
}

// touchLastUsed records best-effort telemetry. It runs on its own
// goroutine with its own detached, bounded context so it can never block,
// slow, or fail the request that triggered it -- last_used_at is never a
// correctness signal and no code path reads it to make a decision
// (docs/phase-12-plan.md §5, §8). Errors are deliberately discarded: a
// failure to record telemetry must not turn a successfully authenticated
// request into a rejected one.
//
// KNOWN, DOCUMENTED FOLLOW-UP -- not fixed in Phase 12, and not claimed to
// be. The goroutine is unbounded: one per successful verification, each
// taking a SECOND connection from the shared pool, while cmd/api sets no
// SetMaxOpenConns. A request burst can therefore exhaust PostgreSQL's
// max_connections and take the request path down with it, and Phase 12
// ships no rate limiting to bound the burst (an explicit non-goal --
// docs/phase-12-plan.md §3). Bounding this -- a semaphore, a single
// background writer, or a pool ceiling on cmd/api -- is deferred. See
// docs/security-model.md §5, "last_used_at telemetry is unbounded per
// authenticated request".
func (s *Store) touchLastUsed(apiKeyRowID uuid.UUID) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, apiKeyRowID)
	}()
}

// textArrayLiteral renders scopes as a PostgreSQL array literal for a
// `$n::text[]` parameter, mirroring internal/store/workflow.go's
// uuidArrayLiteral: it sidesteps any question of whether the database/sql
// driver marshals []string as a PostgreSQL array, since PostgreSQL's own
// array input function parses this text form identically.
//
// Every element is a value ValidateScopes has already accepted (so it
// contains no comma, brace, quote, or backslash), but each is quoted and
// escaped anyway so this helper cannot become an injection vector if it is
// ever reused for a less constrained column.
func textArrayLiteral(values []string) string {
	if len(values) == 0 {
		return "{}"
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		quoted[i] = `"` + v + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

// parseTextArrayLiteral parses a PostgreSQL text[] rendered via ::text
// back into a Go slice. The empty array "{}" parses to a nil slice.
func parseTextArrayLiteral(s string) []string {
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) {
			p = p[1 : len(p)-1]
			p = strings.ReplaceAll(p, `\"`, `"`)
			p = strings.ReplaceAll(p, `\\`, `\`)
		}
		out = append(out, p)
	}
	return out
}
