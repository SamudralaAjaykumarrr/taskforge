// Phase 12 test harness (docs/phase-12-plan.md): real principals, real
// API keys, real credential verification against real PostgreSQL. No
// mocked database and no stubbed-out authentication on any path that is
// actually proving an authentication or authorization property.
package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// testPepper is the HMAC pepper these tests hash secrets under. A fixed
// literal is correct here (the property under test is "the same pepper
// verifies what it minted", not the pepper's own entropy) and it is
// deliberately at least config.MinAPIKeyPepperLength bytes so it also
// exercises a realistically-sized key.
var testPepper = []byte("phase-12-test-pepper-0123456789abcdef")

// testIdentity is one real principal plus one real, live API key for it.
type testIdentity struct {
	PrincipalID uuid.UUID
	Kind        principal.Kind
	KeyID       string

	// Credential is the exact "<key_id>.<secret>" string that goes after
	// "Bearer ". Tests assert this value never appears in a log line.
	Credential string

	// Secret is the raw secret half on its own, so a log audit can search
	// for it independently of the key_id it is concatenated with.
	Secret string
}

// AuthHeader renders the Authorization header value for this identity.
func (i testIdentity) AuthHeader() string { return "Bearer " + i.Credential }

// newPrincipalStore builds the real, PostgreSQL-backed verifier.
func newPrincipalStore(t *testing.T, db *sql.DB) *principal.Store {
	t.Helper()
	ps, err := principal.NewStore(db, testPepper)
	require.NoError(t, err)
	return ps
}

// newIdentity creates a real principals row and a real api_keys row for
// it, returning the one-time credential. Everything here goes through the
// same code an operator's taskforge-admin invocation would use.
func newIdentity(t *testing.T, ps *principal.Store, kind principal.Kind, name string, scopes ...string) testIdentity {
	t.Helper()
	ctx := context.Background()
	p, err := ps.CreatePrincipal(ctx, kind, name)
	require.NoError(t, err)
	cred, err := ps.CreateAPIKey(ctx, p.ID, scopes, nil)
	require.NoError(t, err)
	return testIdentity{
		PrincipalID: p.ID,
		Kind:        kind,
		KeyID:       cred.APIKey.KeyID,
		Credential:  cred.Credential,
		Secret:      cred.Secret,
	}
}

// newExpiredIdentity creates a real key whose expires_at is already in the
// past, so the expiry branch is exercised against real stored state rather
// than a fake clock.
func newExpiredIdentity(t *testing.T, ps *principal.Store, name string, scopes ...string) testIdentity {
	t.Helper()
	ctx := context.Background()
	p, err := ps.CreatePrincipal(ctx, principal.KindCaller, name)
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	cred, err := ps.CreateAPIKey(ctx, p.ID, scopes, &past)
	require.NoError(t, err)
	return testIdentity{PrincipalID: p.ID, Kind: principal.KindCaller, KeyID: cred.APIKey.KeyID, Credential: cred.Credential, Secret: cred.Secret}
}

// newAuthedServer starts an httptest.Server whose router is fully
// authenticated -- no header injection, no bypass. Callers must send a
// real Authorization header themselves. This is what every Phase 12 test
// uses.
func newAuthedServer(t *testing.T, db *sql.DB, opts ...api.RouterOption) (*httptest.Server, *store.Store, *principal.Store) {
	t.Helper()
	st := store.New(db)
	ps := newPrincipalStore(t, db)
	h := api.NewHandlers(st, discardLogger(), api.WithAuthenticator(ps))
	srv := httptest.NewServer(api.NewRouter(h, opts...))
	t.Cleanup(srv.Close)
	return srv, st, ps
}

// injectCredential wraps an already-authenticated handler so that a
// request arriving WITHOUT an Authorization header gets ident's real
// credential attached before routing.
//
// This exists so the Phase 1-11 test suite -- whose subject is job state,
// idempotency, scheduling, cancellation, and workflow behaviour, not
// identity -- keeps running unchanged, which is itself the regression
// evidence that Phase 12 added a boundary in front of the engine without
// altering the engine. It is NOT an authentication bypass: the credential
// injected is a real one, minted through principal.Store.CreateAPIKey and
// verified on every request by the same middleware production uses,
// against the same principals/api_keys rows. A request that supplies its
// own Authorization header is left untouched, so a test can still present
// a wrong credential and observe a genuine rejection.
//
// Every Phase 12 test uses newAuthedServer instead and sends its own
// headers.
func injectCredential(next http.Handler, ident testIdentity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", ident.AuthHeader())
		}
		next.ServeHTTP(w, r)
	})
}

// staticAuthenticator is the one place these tests do not use a real
// principals table: it serves the handful of tests whose subject is a
// store failure or a router-structure property and which therefore have no
// database at all (see handlers_phase11_test.go's poisonStore). It returns
// a fixed AccessContext for any non-empty credential.
type staticAuthenticator struct {
	ac  principal.AccessContext
	err error
}

func (s staticAuthenticator) Verify(context.Context, string) (principal.AccessContext, error) {
	if s.err != nil {
		return principal.AccessContext{}, s.err
	}
	return s.ac, nil
}

// doJSON issues an arbitrary request with an explicit Authorization header
// value ("" sends none at all) and returns the status and raw body.
func doJSON(t *testing.T, method, url, authHeader, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// createJobAs submits a job as ident and returns the created job's id.
func createJobAs(t *testing.T, srv *httptest.Server, ident testIdentity, jobType string) uuid.UUID {
	t.Helper()
	status, raw := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs", ident.AuthHeader(),
		`{"job_type":"`+jobType+`","payload":{}}`)
	require.Equal(t, http.StatusCreated, status, "body: %s", raw)
	var out struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return uuid.MustParse(out.ID)
}

// createWorkflowAs submits a single-node workflow as ident and returns its
// id.
func createWorkflowAs(t *testing.T, srv *httptest.Server, ident testIdentity, jobType string) uuid.UUID {
	t.Helper()
	status, raw := doJSON(t, http.MethodPost, srv.URL+"/v1/workflows", ident.AuthHeader(),
		`{"nodes":[{"node_key":"only","job_type":"`+jobType+`","payload":{}}]}`)
	require.Equal(t, http.StatusCreated, status, "body: %s", raw)
	var out struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return uuid.MustParse(out.ID)
}

// jobRowSnapshot is every mutable field POST /jobs/{id}/cancel could
// possibly touch, read straight from PostgreSQL. Tests compare a snapshot
// taken before an unauthorized cancel with one taken after, which is a
// direct proof that zero rows were mutated -- not an inference from the
// HTTP status code.
type jobRowSnapshot struct {
	State             string
	CancelRequested   bool
	CancelRequestedAt sql.NullTime
	UpdatedAt         time.Time
	TerminalAt        sql.NullTime
	Version           int64
	PrincipalID       uuid.UUID
}

func snapshotJobRow(t *testing.T, db *sql.DB, id uuid.UUID) jobRowSnapshot {
	t.Helper()
	var s jobRowSnapshot
	err := db.QueryRowContext(context.Background(), `
		SELECT state, cancel_requested, cancel_requested_at, updated_at, terminal_at, version, principal_id
		FROM jobs WHERE id = $1`, id,
	).Scan(&s.State, &s.CancelRequested, &s.CancelRequestedAt, &s.UpdatedAt, &s.TerminalAt, &s.Version, &s.PrincipalID)
	require.NoError(t, err)
	return s
}

// workflowRowSnapshot is the workflow-level equivalent of jobRowSnapshot.
type workflowRowSnapshot struct {
	State             string
	CancelRequested   bool
	CancelRequestedAt sql.NullTime
	UpdatedAt         time.Time
	PrincipalID       uuid.UUID
}

func snapshotWorkflowRow(t *testing.T, db *sql.DB, id uuid.UUID) workflowRowSnapshot {
	t.Helper()
	var s workflowRowSnapshot
	err := db.QueryRowContext(context.Background(), `
		SELECT state, cancel_requested, cancel_requested_at, updated_at, principal_id
		FROM workflow_instances WHERE id = $1`, id,
	).Scan(&s.State, &s.CancelRequested, &s.CancelRequestedAt, &s.UpdatedAt, &s.PrincipalID)
	require.NoError(t, err)
	return s
}

// newTwoPrincipalServer is the setup every isolation test shares: a fully
// authenticated server plus two unrelated caller principals, A and B.
func newTwoPrincipalServer(t *testing.T) (*httptest.Server, *sql.DB, testIdentity, testIdentity) {
	t.Helper()
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)
	a := newIdentity(t, ps, principal.KindCaller, "principal A", principal.ScopeJobs)
	b := newIdentity(t, ps, principal.KindCaller, "principal B", principal.ScopeJobs)
	return srv, db, a, b
}

// newStoreForTest is a bare store.Store, for the few tests that need a
// router without caring about the store at all.
func newStoreForTest(db *sql.DB) *store.Store { return store.New(db) }
