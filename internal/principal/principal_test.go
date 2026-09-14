// Phase 12 (docs/phase-12-plan.md §11, verification points 3, 4): the
// credential model's own proofs -- generation, storage, verification,
// revocation, expiry, and the scope/admin distinction -- against a real
// PostgreSQL database, per this project's unbroken no-mocked-database
// discipline.
package principal_test

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

var testPepper = []byte("phase-12-principal-test-pepper-0123456789")

func newStore(t *testing.T) (*principal.Store, *sql.DB) {
	t.Helper()
	db := testutil.DB(t)
	ps, err := principal.NewStore(db, testPepper)
	require.NoError(t, err)
	return ps, db
}

// TestNewStore_RejectsEmptyPepper proves the pepper is not optional. An
// empty pepper would silently degrade HashSecret to an unkeyed HMAC --
// the exact property that makes a stolen api_keys table insufficient on
// its own to forge a credential -- so it must fail loudly at construction,
// not be discovered later.
func TestNewStore_RejectsEmptyPepper(t *testing.T) {
	db := testutil.DB(t)
	_, err := principal.NewStore(db, nil)
	require.Error(t, err)
	_, err = principal.NewStore(db, []byte{})
	require.Error(t, err)
}

// TestCreateAPIKey_NeverStoresRawSecret is verification point 3's core
// claim, checked against the actual stored bytes: the raw secret must
// appear NOWHERE in the api_keys row -- not in secret_hash, not in key_id,
// not anywhere else in the table's text.
func TestCreateAPIKey_NeverStoresRawSecret(t *testing.T) {
	ps, db := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "storage audit")
	require.NoError(t, err)
	cred, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, cred.Secret)

	// The whole row, rendered as text, must not contain the secret.
	var rowText string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT api_keys::text FROM api_keys WHERE key_id = $1`, cred.APIKey.KeyID).Scan(&rowText))
	require.NotContains(t, rowText, cred.Secret,
		"the raw secret must never be persisted in any column")
	require.NotContains(t, rowText, cred.Credential,
		"the full credential must never be persisted either")

	// And what IS stored is exactly the peppered HMAC -- not a bare
	// digest of the secret, which a stolen table alone could reproduce.
	var stored []byte
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT secret_hash FROM api_keys WHERE key_id = $1`, cred.APIKey.KeyID).Scan(&stored))
	require.Equal(t, principal.HashSecret(testPepper, cred.Secret), stored)
	require.NotEqual(t, principal.HashSecret(nil, cred.Secret), stored,
		"the stored hash must depend on the pepper, not on the secret alone")
}

// TestVerify_RoundTripsFreshlyCreatedKey is the positive case: a credential
// minted by CreateAPIKey authenticates, and the AccessContext it produces
// carries the right principal, kind, and scopes.
func TestVerify_RoundTripsFreshlyCreatedKey(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "round trip")
	require.NoError(t, err)
	cred, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs, principal.ScopeMetrics}, nil)
	require.NoError(t, err)

	ac, err := ps.Verify(ctx, cred.Credential)
	require.NoError(t, err)
	require.Equal(t, p.ID, ac.PrincipalID)
	require.False(t, ac.IsAdmin, "a caller-kind principal must not get the ownership bypass")
	require.True(t, ac.HasScope(principal.ScopeJobs))
	require.True(t, ac.HasScope(principal.ScopeMetrics))
	require.False(t, ac.HasScope(principal.ScopeAdmin))
}

// TestVerify_AdminKindGrantsOwnershipBypass pins the exact source of
// IsAdmin: the PRINCIPAL's kind, not any scope string. A caller-kind
// principal holding an admin-scoped key gets every capability but NOT the
// cross-tenant ownership bypass -- so minting a powerful key cannot
// accidentally grant cross-tenant reads.
func TestVerify_AdminKindGrantsOwnershipBypass(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	admin, err := ps.CreatePrincipal(ctx, principal.KindAdmin, "real admin")
	require.NoError(t, err)
	adminCred, err := ps.CreateAPIKey(ctx, admin.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	ac, err := ps.Verify(ctx, adminCred.Credential)
	require.NoError(t, err)
	require.True(t, ac.IsAdmin, "an admin-KIND principal gets the ownership bypass")

	caller, err := ps.CreatePrincipal(ctx, principal.KindCaller, "caller with admin scope")
	require.NoError(t, err)
	scopedCred, err := ps.CreateAPIKey(ctx, caller.ID, []string{principal.ScopeAdmin}, nil)
	require.NoError(t, err)
	ac, err = ps.Verify(ctx, scopedCred.Credential)
	require.NoError(t, err)
	require.False(t, ac.IsAdmin,
		"the admin SCOPE must not confer the ownership bypass -- only the admin kind does")
	require.True(t, ac.HasScope(principal.ScopeJobs), "the admin scope satisfies every scope requirement")
	require.True(t, ac.HasScope(principal.ScopeMetrics))
}

// TestVerify_TamperedSecretAgainstRealKeyIDFails is the adversarial case
// that matters most: the attacker knows a real, valid key_id (it is not a
// secret) and guesses the secret.
func TestVerify_TamperedSecretAgainstRealKeyIDFails(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "tamper target")
	require.NoError(t, err)
	cred, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)

	for name, secret := range map[string]string{
		"wrong secret":     "not-the-right-secret",
		"empty secret":     "",
		"truncated secret": cred.Secret[:len(cred.Secret)-1],
		"secret plus byte": cred.Secret + "x",
		"another key's":    mustSecret(t, ps, ctx),
		"key_id as secret": cred.APIKey.KeyID,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ps.Verify(ctx, principal.FormatCredential(cred.APIKey.KeyID, secret))
			require.Error(t, err, "a wrong secret against a real key_id must never authenticate")
			require.NotErrorIs(t, err, nil)
		})
	}
}

func mustSecret(t *testing.T, ps *principal.Store, ctx context.Context) string {
	t.Helper()
	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "unrelated key holder")
	require.NoError(t, err)
	cred, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	return cred.Secret
}

// TestVerify_MalformedCredentialsRejectedWithoutDatabaseAccess covers step
// 1 of the verification sequence.
func TestVerify_MalformedCredentialsRejectedWithoutDatabaseAccess(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	for _, raw := range []string{"", ".", "keyidonly", ".secretonly", "keyid."} {
		_, err := ps.Verify(ctx, raw)
		require.ErrorIs(t, err, principal.ErrMalformedCredential, "credential %q", raw)
	}
}

// TestVerify_UnknownKeyRejected covers step 2.
func TestVerify_UnknownKeyRejected(t *testing.T) {
	ps, _ := newStore(t)
	_, err := ps.Verify(context.Background(), "0123456789abcdef0123456789abcdef.somesecret")
	require.ErrorIs(t, err, principal.ErrUnknownKey)
}

// TestVerify_RevokedKeyRejectedImmediately proves revocation needs no
// redeploy, restart, or cache expiry: the same credential authenticates
// before RevokeAPIKey and fails on the very next call after it.
func TestVerify_RevokedKeyRejectedImmediately(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "revocation target")
	require.NoError(t, err)
	cred, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)

	_, err = ps.Verify(ctx, cred.Credential)
	require.NoError(t, err, "the key must work before revocation")

	require.NoError(t, ps.RevokeAPIKey(ctx, cred.APIKey.KeyID))

	_, err = ps.Verify(ctx, cred.Credential)
	require.ErrorIs(t, err, principal.ErrKeyRevoked,
		"revocation must take effect on the very next verification, with no restart")
}

// TestVerify_ExpiredKeyRejected covers the expires_at branch against a
// real stored timestamp.
func TestVerify_ExpiredKeyRejected(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "expiry target")
	require.NoError(t, err)
	past := time.Now().Add(-time.Minute)
	expired, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, &past)
	require.NoError(t, err)
	future := time.Now().Add(time.Hour)
	live, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, &future)
	require.NoError(t, err)

	_, err = ps.Verify(ctx, expired.Credential)
	require.ErrorIs(t, err, principal.ErrKeyExpired)

	_, err = ps.Verify(ctx, live.Credential)
	require.NoError(t, err, "a key whose expiry is still in the future must authenticate")
}

// TestVerify_RevokedPrincipalInvalidatesEveryKeyAtOnce covers step 5: one
// write cuts off a compromised caller entirely, rather than requiring each
// of its credentials to be revoked individually.
func TestVerify_RevokedPrincipalInvalidatesEveryKeyAtOnce(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "compromised caller")
	require.NoError(t, err)
	first, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	second, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)

	for _, c := range []string{first.Credential, second.Credential} {
		_, err := ps.Verify(ctx, c)
		require.NoError(t, err)
	}

	require.NoError(t, ps.RevokePrincipal(ctx, p.ID))

	for _, c := range []string{first.Credential, second.Credential} {
		_, err := ps.Verify(ctx, c)
		require.ErrorIs(t, err, principal.ErrPrincipalRevoked,
			"revoking a principal must invalidate every key it holds, not just one")
	}
}

// TestRotationWithOverlap_BothKeysLiveUntilOldOneIsRevoked proves the
// rotation-with-overlap requirement (docs/security-model.md) falls out of
// the schema with no extra mechanism: mint, overlap, revoke -- never a
// synchronised cutover.
func TestRotationWithOverlap_BothKeysLiveUntilOldOneIsRevoked(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "rotating caller")
	require.NoError(t, err)
	old, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	new, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)

	// Overlap window: both authenticate, as the same principal.
	for _, c := range []string{old.Credential, new.Credential} {
		ac, err := ps.Verify(ctx, c)
		require.NoError(t, err)
		require.Equal(t, p.ID, ac.PrincipalID)
	}

	require.NoError(t, ps.RevokeAPIKey(ctx, old.APIKey.KeyID))

	_, err = ps.Verify(ctx, old.Credential)
	require.ErrorIs(t, err, principal.ErrKeyRevoked)
	_, err = ps.Verify(ctx, new.Credential)
	require.NoError(t, err, "the replacement key keeps working after the old one is retired")
}

// TestCreateAPIKey_RejectsUnknownScope proves scope validation happens
// once, centrally, in this package -- migration 0005 deliberately puts no
// CHECK on the array (see that file's comment).
func TestCreateAPIKey_RejectsUnknownScope(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()
	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "scope validation")
	require.NoError(t, err)

	_, err = ps.CreateAPIKey(ctx, p.ID, []string{"superuser"}, nil)
	require.Error(t, err)
	_, err = ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs, principal.ScopeJobs}, nil)
	require.Error(t, err, "a duplicated scope is a malformed capability list")
}

// TestCreatePrincipal_RejectsWorkerKind is OD-3 enforced at the
// application layer, complementing migration 0005's CHECK constraint:
// there is no application-level worker principal, and a future refactor
// cannot quietly introduce one through this constructor.
func TestCreatePrincipal_RejectsWorkerKind(t *testing.T) {
	ps, _ := newStore(t)
	_, err := ps.CreatePrincipal(context.Background(), principal.Kind("worker"), "a worker")
	require.Error(t, err,
		"OD-3: worker identity is a PostgreSQL role, not an application principal")
}

// TestGeneratedCredentials_AreDistinctAndHighEntropy guards against the
// single worst possible bug in this file -- a generator that returns the
// same value twice, or a short one.
func TestGeneratedCredentials_AreDistinctAndHighEntropy(t *testing.T) {
	seenKeyIDs := map[string]bool{}
	seenSecrets := map[string]bool{}
	for i := 0; i < 256; i++ {
		keyID, err := principal.GenerateKeyID()
		require.NoError(t, err)
		secret, err := principal.GenerateSecret()
		require.NoError(t, err)

		require.Len(t, keyID, principal.KeyIDBytes*2, "key_id is hex-encoded")
		require.GreaterOrEqual(t, len(secret), 43, "32 random bytes base64url-encode to 43 characters")
		require.False(t, seenKeyIDs[keyID], "key_id must never repeat")
		require.False(t, seenSecrets[secret], "secret must never repeat")
		seenKeyIDs[keyID] = true
		seenSecrets[secret] = true
	}
}

// TestVerify_UsesConstantTimeComparison is verification point 4
// (docs/phase-12-plan.md §11 #4): a MECHANISM-level proof that the secret
// comparison in Verify goes through crypto/subtle.ConstantTimeCompare.
//
// It parses this package's own source and asserts that Verify's body
// contains that call and contains no ordinary equality comparison against
// the stored hash. This is the same category of proof as TF-INV-016's
// "schema test asserting the constraint exists": a live timing measurement
// in CI would be flaky by nature and would not actually establish the
// property, whereas "the code calls the constant-time primitive" is both
// exactly the requirement and mechanically checkable, so a future refactor
// to bytes.Equal fails this test loudly.
func TestVerify_UsesConstantTimeComparison(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "store.go", nil, 0)
	require.NoError(t, err)

	var body string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Verify" || fn.Recv == nil {
			return true
		}
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		src := mustReadFile(t, "store.go")
		body = src[start:end]
		return false
	})
	require.NotEmpty(t, body, "Store.Verify must exist in store.go")

	require.Contains(t, body, "subtle.ConstantTimeCompare",
		"Verify must compare the secret's HMAC in constant time")
	for _, forbidden := range []string{"bytes.Equal", "== storedHash", "string(storedHash)"} {
		require.NotContains(t, body, forbidden,
			"Verify must not compare the stored hash with a variable-time operation")
	}
}

// TestVerify_NeverLeaksCredentialInErrors proves no error this package
// returns carries the secret, the credential, or the pepper -- the
// server-side counterpart of the log audit in internal/api.
func TestVerify_NeverLeaksCredentialInErrors(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "error leak audit")
	require.NoError(t, err)
	cred, err := ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	require.NoError(t, ps.RevokeAPIKey(ctx, cred.APIKey.KeyID))

	candidates := []string{
		cred.Credential,
		"not-a-credential.at-all",
		principal.FormatCredential(cred.APIKey.KeyID, "wrong-secret-value"),
		"malformed",
	}
	for _, c := range candidates {
		_, err := ps.Verify(ctx, c)
		require.Error(t, err)
		require.NotContains(t, err.Error(), cred.Secret, "error must not echo the raw secret")
		require.NotContains(t, err.Error(), "wrong-secret-value", "error must not echo a presented secret")
		require.NotContains(t, err.Error(), string(testPepper), "error must never contain the pepper")
	}
}

// TestFailureReason_CoversEveryVerificationBranch pins the metric label
// enum to the actual errors Verify can produce, so a new failure mode
// cannot silently land in the catch-all "internal_error" bucket and hide
// a credential problem as an infrastructure one.
func TestFailureReason_CoversEveryVerificationBranch(t *testing.T) {
	cases := map[error]string{
		principal.ErrMalformedCredential: principal.ReasonMalformed,
		principal.ErrUnknownKey:          principal.ReasonUnknownKey,
		principal.ErrKeyRevoked:          principal.ReasonRevoked,
		principal.ErrKeyExpired:          principal.ReasonExpired,
		principal.ErrBadSecret:           principal.ReasonBadSecret,
		principal.ErrPrincipalRevoked:    principal.ReasonPrincipalRevoked,
	}
	for err, want := range cases {
		require.Equal(t, want, principal.FailureReason(err))
	}
	require.Equal(t, principal.ReasonInternal, principal.FailureReason(sql.ErrConnDone),
		"an infrastructure failure must not be reported as a credential failure")

	// Every label is in the documented enum, and the enum has no extras.
	for _, r := range cases {
		require.Contains(t, principal.AllFailureReasons, r)
	}
	require.Len(t, principal.AllFailureReasons, len(cases)+1)
}

// TestSystemPrincipal_ExistsAndHoldsNoCredential checks the seeded state:
// the system principal exists, is a caller, is not revoked, and holds no
// credential in a freshly migrated database.
//
// Scope note, because this test previously claimed more than it proved: it
// asserts current table CONTENTS, not an invariant. The stronger property --
// that nothing may EVER authenticate as the backfill identity -- is enforced
// and proved separately by TestCreateAPIKey_RefusesTheSystemPrincipal,
// TestSchema_RefusesAnAPIKeyForTheSystemPrincipal and
// TestSystemPrincipal_CannotAuthenticate below.
func TestSystemPrincipal_ExistsAndHoldsNoCredential(t *testing.T) {
	ps, db := newStore(t)
	ctx := context.Background()

	p, err := ps.GetPrincipal(ctx, principal.SystemPrincipalID)
	require.NoError(t, err, "migration 0005 must seed the system principal")
	require.Equal(t, principal.KindCaller, p.Kind)
	require.False(t, p.IsRevoked(), "the system principal is never revoked")

	var keyCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM api_keys WHERE principal_id = $1`, principal.SystemPrincipalID).Scan(&keyCount))
	require.Zero(t, keyCount,
		"the system principal must hold no API key: nothing may authenticate as the backfill identity")
}

// TestParseAuthorizationHeader_OnlyAcceptsBearer pins the transport
// contract from OD-2: Bearer, case-insensitive per RFC 9110, and nothing
// else -- in particular there is no X-API-Key fallback.
func TestParseAuthorizationHeader_OnlyAcceptsBearer(t *testing.T) {
	for _, header := range []string{"Bearer abc.def", "bearer abc.def", "BEARER abc.def"} {
		v, err := principal.ParseAuthorizationHeader(header)
		require.NoError(t, err, "header %q", header)
		require.Equal(t, "abc.def", v)
	}
	for _, header := range []string{"", "abc.def", "Basic abc.def", "Token abc.def", "Bearer", "Bearer   "} {
		_, err := principal.ParseAuthorizationHeader(header)
		require.ErrorIs(t, err, principal.ErrMalformedCredential, "header %q", header)
	}
}

func mustReadFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	require.NoError(t, err)
	return string(raw)
}

// ---------------------------------------------------------------------
// The system principal cannot hold a credential (MEDIUM-2)
// ---------------------------------------------------------------------
//
// TestSystemPrincipal_ExistsAndHoldsNoCredential above observes that a
// freshly migrated database has no key for the system principal. An
// independent review pointed out that this proves only the current table
// contents, while the surrounding comments -- in migration 0005, in
// SystemPrincipalID's doc comment, and in this package -- claimed something
// far stronger: that nothing can EVER authenticate as it. Nothing enforced
// that. `taskforge-admin create-key -principal=00000000-...-0001` minted a
// working credential with read and cancel access to every backfilled
// pre-Phase-12 row.
//
// The claim is now enforced twice, and these tests prove both halves.

// TestCreateAPIKey_RefusesTheSystemPrincipal is the application-level guard.
func TestCreateAPIKey_RefusesTheSystemPrincipal(t *testing.T) {
	ps, db := newStore(t)
	ctx := context.Background()

	_, err := ps.CreateAPIKey(ctx, principal.SystemPrincipalID, []string{principal.ScopeJobs}, nil)
	require.ErrorIs(t, err, principal.ErrSystemPrincipalImmutable,
		"minting a credential for the backfill identity would grant access to every legacy row")

	// Every scope combination, including none at all, is refused -- the
	// guard is on the principal, not on what the key would be allowed to do.
	for _, scopes := range [][]string{nil, {}, {principal.ScopeMetrics}, {principal.ScopeAdmin}, {principal.ScopeJobs, principal.ScopeMetrics}} {
		_, err := ps.CreateAPIKey(ctx, principal.SystemPrincipalID, scopes, nil)
		require.ErrorIs(t, err, principal.ErrSystemPrincipalImmutable, "scopes %v", scopes)
	}

	var keyCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM api_keys WHERE principal_id = $1`, principal.SystemPrincipalID).Scan(&keyCount))
	require.Zero(t, keyCount, "no refused attempt may have written a row")

	// An ordinary principal is unaffected: this guard is narrow.
	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "ordinary caller")
	require.NoError(t, err)
	_, err = ps.CreateAPIKey(ctx, p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err, "the guard must apply only to the system principal")
}

// TestSchema_RefusesAnAPIKeyForTheSystemPrincipal is the schema-level
// backstop: the guarantee must survive a direct INSERT that bypasses
// internal/principal entirely -- an operator at a psql prompt, or a future
// code path that forgets to ask.
func TestSchema_RefusesAnAPIKeyForTheSystemPrincipal(t *testing.T) {
	_, db := newStore(t)

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO api_keys (id, principal_id, key_id, secret_hash, scopes)
		VALUES ($1, $2, 'direct-insert-key', '\x00', '{jobs}')`,
		uuid.New(), principal.SystemPrincipalID)
	require.Error(t, err,
		"migration 0005's api_keys_no_system_principal CHECK must reject this even from raw SQL")

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23514", pgErr.Code, "must be a check_violation, not some other failure: %v", err)
	require.Equal(t, "api_keys_no_system_principal", pgErr.ConstraintName)
}

// TestRevokePrincipal_RefusesTheSystemPrincipal pins the other half of
// migration 0005's stated invariant: the backfill identity is "never deleted
// and never revoked." Revoking it would grant nobody anything, but it would
// make the durable owner of every backfilled legacy row read as revoked,
// quietly turning a documented fact into a false one.
func TestRevokePrincipal_RefusesTheSystemPrincipal(t *testing.T) {
	ps, db := newStore(t)
	ctx := context.Background()

	require.ErrorIs(t, ps.RevokePrincipal(ctx, principal.SystemPrincipalID),
		principal.ErrSystemPrincipalImmutable)

	var revokedAt sql.NullTime
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT revoked_at FROM principals WHERE id = $1`, principal.SystemPrincipalID).Scan(&revokedAt))
	require.False(t, revokedAt.Valid, "the system principal must still be un-revoked")

	// Ordinary principals are still revocable.
	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, "revocable caller")
	require.NoError(t, err)
	require.NoError(t, ps.RevokePrincipal(ctx, p.ID))
}

// TestSystemPrincipal_CannotAuthenticate closes the loop end to end: with no
// credential mintable for it by any route, there is no bearer value a caller
// could present that Verify resolves to the system principal.
func TestSystemPrincipal_CannotAuthenticate(t *testing.T) {
	ps, _ := newStore(t)
	ctx := context.Background()

	// The only way to get a credential is CreateAPIKey, and it refuses.
	_, err := ps.CreateAPIKey(ctx, principal.SystemPrincipalID, []string{principal.ScopeAdmin}, nil)
	require.ErrorIs(t, err, principal.ErrSystemPrincipalImmutable)

	// A caller guessing the system principal's UUID as a key_id gets the
	// ordinary unknown-key rejection -- no special case, no disclosure.
	_, err = ps.Verify(ctx, principal.FormatCredential(principal.SystemPrincipalID.String(), "whatever"))
	require.ErrorIs(t, err, principal.ErrUnknownKey)
}
