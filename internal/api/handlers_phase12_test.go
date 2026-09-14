// Phase 12's HTTP proof matrix (docs/phase-12-plan.md §11): authentication,
// deny-by-default authorization, scope enforcement, principal isolation,
// the zero-mutation proof for unauthorized cancellation, the uniform-401
// contract, and the negative proofs (no payload-supplied identity, no
// forwarded-header trust).
//
// Everything here runs against real PostgreSQL with real principals, real
// API keys, and the real authentication middleware. Where a claim is about
// durable state -- "this cancel mutated nothing" -- the test reads the row
// back from the database directly rather than inferring it from a status
// code.
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// routeCase is one of the six job/workflow routes, on one of the two
// surfaces (legacy unprefixed and canonical /v1).
type routeCase struct {
	name   string
	method string
	path   func(id uuid.UUID) string
	body   string
}

// allSixRoutes enumerates every job/workflow route Phase 12 must protect,
// per docs/phase-12-plan.md §7's table. Tests below run over this list on
// BOTH prefixes, because "a deprecated route is not an unauthenticated
// route" is exactly the kind of gap that is easy to leave open.
func allSixRoutes() []routeCase {
	return []routeCase{
		{"POST /jobs", http.MethodPost, func(uuid.UUID) string { return "/jobs" }, `{"job_type":"test.p12.route","payload":{}}`},
		{"GET /jobs/{id}", http.MethodGet, func(id uuid.UUID) string { return "/jobs/" + id.String() }, ""},
		{"POST /jobs/{id}/cancel", http.MethodPost, func(id uuid.UUID) string { return "/jobs/" + id.String() + "/cancel" }, ""},
		{"POST /workflows", http.MethodPost, func(uuid.UUID) string { return "/workflows" }, `{"nodes":[{"node_key":"a","job_type":"test.p12.route","payload":{}}]}`},
		{"GET /workflows/{id}", http.MethodGet, func(id uuid.UUID) string { return "/workflows/" + id.String() }, ""},
		{"POST /workflows/{id}/cancel", http.MethodPost, func(id uuid.UUID) string { return "/workflows/" + id.String() + "/cancel" }, ""},
	}
}

var bothPrefixes = []string{"", "/v1"}

// ---------------------------------------------------------------------
// #5 -- deny-by-default authentication over every route and both surfaces
// ---------------------------------------------------------------------

// TestAuth_NoCredential_EveryRouteRejects401 is G1's direct proof: with no
// Authorization header, every one of the six job/workflow routes on BOTH
// the legacy and /v1 surfaces returns 401 -- never 200, never 404, never a
// silent success.
func TestAuth_NoCredential_EveryRouteRejects401(t *testing.T) {
	db := testutil.DB(t)
	srv, _, _ := newAuthedServer(t, db)
	id := uuid.New()

	for _, prefix := range bothPrefixes {
		for _, rc := range allSixRoutes() {
			t.Run(prefix+" "+rc.name, func(t *testing.T) {
				status, body := doJSON(t, rc.method, srv.URL+prefix+rc.path(id), "", rc.body)
				require.Equal(t, http.StatusUnauthorized, status,
					"an unauthenticated request must never reach a handler")
				requireUniformUnauthorizedBody(t, body)
			})
		}
	}
}

// TestAuth_InvalidCredentials_EveryRouteRejects401 extends the same sweep
// to credentials that are present but wrong, in every shape the middleware
// can encounter.
func TestAuth_InvalidCredentials_EveryRouteRejects401(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)
	good := newIdentity(t, ps, principal.KindCaller, "good caller", principal.ScopeJobs)
	id := uuid.New()

	headers := map[string]string{
		"wrong scheme":              "Basic " + good.Credential,
		"no scheme":                 good.Credential,
		"malformed value":           "Bearer not-a-credential",
		"unknown key_id":            "Bearer 00000000000000000000000000000000.anysecret",
		"real key_id, wrong secret": "Bearer " + good.KeyID + ".wrong-secret",
		"empty bearer":              "Bearer ",
	}

	for _, prefix := range bothPrefixes {
		for _, rc := range allSixRoutes() {
			for name, header := range headers {
				t.Run(prefix+" "+rc.name+" "+name, func(t *testing.T) {
					status, body := doJSON(t, rc.method, srv.URL+prefix+rc.path(id), header, rc.body)
					require.Equal(t, http.StatusUnauthorized, status)
					requireUniformUnauthorizedBody(t, body)
				})
			}
		}
	}
}

// TestAuth_ValidCredential_EveryRouteAccepted is the positive counterpart:
// the same sweep with a real, jobs-scoped credential must NOT produce 401
// or 403 anywhere -- otherwise "everything returns 401" would trivially
// satisfy the negative tests above while breaking the API.
func TestAuth_ValidCredential_EveryRouteAccepted(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)
	caller := newIdentity(t, ps, principal.KindCaller, "valid caller", principal.ScopeJobs)

	for _, prefix := range bothPrefixes {
		jobID := createJobAs(t, srv, caller, "test.p12.accept")
		wfID := createWorkflowAs(t, srv, caller, "test.p12.accept.wf")

		for _, rc := range allSixRoutes() {
			t.Run(prefix+" "+rc.name, func(t *testing.T) {
				id := jobID
				if strings.Contains(rc.name, "workflows/") {
					id = wfID
				}
				status, body := doJSON(t, rc.method, srv.URL+prefix+rc.path(id), caller.AuthHeader(), rc.body)
				require.NotEqual(t, http.StatusUnauthorized, status, "body: %s", body)
				require.NotEqual(t, http.StatusForbidden, status, "body: %s", body)
				require.Less(t, status, 500, "a valid, in-scope request must not 5xx; body: %s", body)
			})
		}
	}
}

// TestAuth_RevokedKeyRejectedAtTheHTTPBoundary proves revocation is
// effective through the whole stack, not just in internal/principal's unit
// tests: the same credential works, is revoked, and immediately stops
// working -- with no server restart in between.
func TestAuth_RevokedKeyRejectedAtTheHTTPBoundary(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)
	caller := newIdentity(t, ps, principal.KindCaller, "revoked caller", principal.ScopeJobs)

	status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs", caller.AuthHeader(), `{"job_type":"test.p12.revoke","payload":{}}`)
	require.Equal(t, http.StatusCreated, status, "the key must work before revocation")

	require.NoError(t, ps.RevokeAPIKey(t.Context(), caller.KeyID))

	status, body := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs", caller.AuthHeader(), `{"job_type":"test.p12.revoke","payload":{}}`)
	require.Equal(t, http.StatusUnauthorized, status,
		"a revoked key must stop authenticating on its very next request, with no redeploy")
	requireUniformUnauthorizedBody(t, body)
}

// TestAuth_ExpiredKeyRejectedAtTheHTTPBoundary is the expiry equivalent.
func TestAuth_ExpiredKeyRejectedAtTheHTTPBoundary(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)
	expired := newExpiredIdentity(t, ps, "expired caller", principal.ScopeJobs)

	status, body := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs", expired.AuthHeader(), `{"job_type":"test.p12.expired","payload":{}}`)
	require.Equal(t, http.StatusUnauthorized, status)
	requireUniformUnauthorizedBody(t, body)
}

// TestAuth_RevokedPrincipalRejectedAtTheHTTPBoundary covers the "cut off
// the whole caller" path end to end.
func TestAuth_RevokedPrincipalRejectedAtTheHTTPBoundary(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)
	caller := newIdentity(t, ps, principal.KindCaller, "principal to revoke", principal.ScopeJobs)

	require.NoError(t, ps.RevokePrincipal(t.Context(), caller.PrincipalID))

	status, body := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(), caller.AuthHeader(), "")
	require.Equal(t, http.StatusUnauthorized, status)
	requireUniformUnauthorizedBody(t, body)
}

// ---------------------------------------------------------------------
// #9 -- uniform 401: no failure reason is distinguishable by a caller
// ---------------------------------------------------------------------

// TestAuth_AllFailureReasons_ProduceByteIdenticalResponses is verification
// point 9 applied to credentials. Every distinct internal failure reason --
// malformed, unknown key, revoked key, expired key, bad secret, revoked
// principal -- must be indistinguishable from the outside, or an attacker
// could enumerate which key_ids exist and which have been retired.
//
// The comparison is on the raw response bytes, not on a parsed field.
func TestAuth_AllFailureReasons_ProduceByteIdenticalResponses(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)

	live := newIdentity(t, ps, principal.KindCaller, "live", principal.ScopeJobs)
	revokedKey := newIdentity(t, ps, principal.KindCaller, "revoked key holder", principal.ScopeJobs)
	require.NoError(t, ps.RevokeAPIKey(t.Context(), revokedKey.KeyID))
	expired := newExpiredIdentity(t, ps, "expired", principal.ScopeJobs)
	revokedPrincipal := newIdentity(t, ps, principal.KindCaller, "revoked principal", principal.ScopeJobs)
	require.NoError(t, ps.RevokePrincipal(t.Context(), revokedPrincipal.PrincipalID))

	headers := map[string]string{
		"absent":            "",
		"malformed":         "Bearer garbage",
		"unknown key":       "Bearer 11111111111111111111111111111111.secret",
		"revoked key":       revokedKey.AuthHeader(),
		"expired key":       expired.AuthHeader(),
		"bad secret":        "Bearer " + live.KeyID + ".definitely-the-wrong-secret",
		"revoked principal": revokedPrincipal.AuthHeader(),
	}

	var reference []byte
	var referenceName string
	for name, header := range headers {
		status, body := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(), header, "")
		require.Equal(t, http.StatusUnauthorized, status, "reason %q", name)
		if reference == nil {
			reference, referenceName = body, name
			continue
		}
		require.Equal(t, string(reference), string(body),
			"the %q and %q failure reasons must be byte-for-byte indistinguishable to a caller", referenceName, name)
	}
	requireUniformUnauthorizedBody(t, reference)
}

func requireUniformUnauthorizedBody(t *testing.T, body []byte) {
	t.Helper()
	var parsed struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &parsed))
	require.Equal(t, "unauthorized", parsed.Error,
		"every authentication failure must produce the one generic body")
	// And it must not leak the reason through some other field.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Len(t, raw, 1, "the 401 body must carry exactly one field")
}

// ---------------------------------------------------------------------
// #11 -- scope enforcement (jobs vs. metrics vs. admin)
// ---------------------------------------------------------------------

// TestScopes_JobsAndMetricsAreSeparateCapabilities is OD-5's proof: a
// job-submission key cannot scrape metrics (closing
// docs/security-model.md §5's operational-volume-disclosure finding), a
// metrics key cannot submit jobs, and the mismatch is a 403 -- deliberately
// distinguishable from 401, because a scope mismatch on a route with no
// per-resource id at stake discloses nothing about any tenant's data.
func TestScopes_JobsAndMetricsAreSeparateCapabilities(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db, api.WithMetricsEndpoint(stubMetricsHandler()))

	jobsOnly := newIdentity(t, ps, principal.KindCaller, "jobs only", principal.ScopeJobs)
	metricsOnly := newIdentity(t, ps, principal.KindCaller, "metrics only", principal.ScopeMetrics)
	adminScoped := newIdentity(t, ps, principal.KindAdmin, "admin", principal.ScopeAdmin)

	t.Run("jobs-scoped key is forbidden from GET /metrics", func(t *testing.T) {
		status, body := doJSON(t, http.MethodGet, srv.URL+"/metrics", jobsOnly.AuthHeader(), "")
		require.Equal(t, http.StatusForbidden, status, "body: %s", body)
		require.NotEqual(t, http.StatusUnauthorized, status,
			"the credential is valid -- it simply lacks the scope, which is a 403, not a 401")
	})

	t.Run("metrics-scoped key is forbidden from every job/workflow route", func(t *testing.T) {
		id := uuid.New()
		for _, prefix := range bothPrefixes {
			for _, rc := range allSixRoutes() {
				status, body := doJSON(t, rc.method, srv.URL+prefix+rc.path(id), metricsOnly.AuthHeader(), rc.body)
				require.Equal(t, http.StatusForbidden, status,
					"%s%s must reject a metrics-only key; body: %s", prefix, rc.name, body)
			}
		}
	})

	t.Run("metrics-scoped key may scrape metrics", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodGet, srv.URL+"/metrics", metricsOnly.AuthHeader(), "")
		require.Equal(t, http.StatusOK, status)
	})

	t.Run("jobs-scoped key may submit jobs", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs", jobsOnly.AuthHeader(), `{"job_type":"test.p12.scope","payload":{}}`)
		require.Equal(t, http.StatusCreated, status)
	})

	t.Run("admin scope satisfies both", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodGet, srv.URL+"/metrics", adminScoped.AuthHeader(), "")
		require.Equal(t, http.StatusOK, status)
		status, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/jobs", adminScoped.AuthHeader(), `{"job_type":"test.p12.scope.admin","payload":{}}`)
		require.Equal(t, http.StatusCreated, status)
	})

	t.Run("GET /metrics still requires a credential at all", func(t *testing.T) {
		status, body := doJSON(t, http.MethodGet, srv.URL+"/metrics", "", "")
		require.Equal(t, http.StatusUnauthorized, status)
		requireUniformUnauthorizedBody(t, body)
	})
}

func stubMetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# HELP taskforge_stub metrics body\n"))
	})
}

// ---------------------------------------------------------------------
// #8, #9 -- principal isolation on reads and cancellation
// ---------------------------------------------------------------------

// TestGetJob_CrossPrincipal_IndistinguishableFromNonexistent is G2 on the
// read path: principal B asking for principal A's job must get exactly the
// response it gets for a UUID that has never existed -- same status, same
// bytes.
func TestGetJob_CrossPrincipal_IndistinguishableFromNonexistent(t *testing.T) {
	srv, _, a, b := newTwoPrincipalServer(t)
	aJob := createJobAs(t, srv, a, "test.p12.isolation.read")

	for _, prefix := range bothPrefixes {
		notYoursStatus, notYours := doJSON(t, http.MethodGet, srv.URL+prefix+"/jobs/"+aJob.String(), b.AuthHeader(), "")
		nonexistentStatus, nonexistent := doJSON(t, http.MethodGet, srv.URL+prefix+"/jobs/"+uuid.New().String(), b.AuthHeader(), "")

		require.Equal(t, http.StatusNotFound, notYoursStatus, "prefix %q", prefix)
		require.Equal(t, nonexistentStatus, notYoursStatus)
		require.Equal(t, string(nonexistent), string(notYours),
			"\"found but not yours\" and \"does not exist\" must be byte-for-byte identical")
	}

	// The owner still sees it -- isolation must not mean "nobody can read".
	status, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+aJob.String(), a.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status, "the owning principal must still be able to read its own job")
}

// TestGetWorkflow_CrossPrincipal_IndistinguishableFromNonexistent is the
// workflow equivalent. It matters independently of the job case because a
// workflow response would otherwise disclose the node list -- job ids and
// job types belonging to another tenant.
func TestGetWorkflow_CrossPrincipal_IndistinguishableFromNonexistent(t *testing.T) {
	srv, _, a, b := newTwoPrincipalServer(t)
	aWorkflow := createWorkflowAs(t, srv, a, "test.p12.isolation.wfread")

	for _, prefix := range bothPrefixes {
		notYoursStatus, notYours := doJSON(t, http.MethodGet, srv.URL+prefix+"/workflows/"+aWorkflow.String(), b.AuthHeader(), "")
		nonexistentStatus, nonexistent := doJSON(t, http.MethodGet, srv.URL+prefix+"/workflows/"+uuid.New().String(), b.AuthHeader(), "")

		require.Equal(t, http.StatusNotFound, notYoursStatus)
		require.Equal(t, nonexistentStatus, notYoursStatus)
		require.Equal(t, string(nonexistent), string(notYours))
		require.NotContains(t, string(notYours), "node",
			"a rejected cross-principal workflow read must not disclose the node list")
	}

	status, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/workflows/"+aWorkflow.String(), a.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status)
}

// TestCancelJob_CrossPrincipal_MutatesZeroRows is verification point 8's
// load-bearing proof and the direct answer to docs/phase-12-plan.md §4a's
// architecture blocker.
//
// A 404 response alone would NOT prove the property: CancelJob's very first
// action is a mutating store call, so an implementation could plausibly
// flip cancel_requested on A's row and still return 404 to B afterwards.
// This test therefore snapshots every mutable field of A's row straight
// from PostgreSQL, has B attempt the cancel on every surface, and asserts
// the row is byte-identical afterwards -- including version, which
// increments on any successful fenced UPDATE and so cannot be left
// unchanged by an UPDATE that actually matched.
func TestCancelJob_CrossPrincipal_MutatesZeroRows(t *testing.T) {
	srv, db, a, b := newTwoPrincipalServer(t)
	aJob := createJobAs(t, srv, a, "test.p12.isolation.cancel")

	before := snapshotJobRow(t, db, aJob)
	require.Equal(t, "QUEUED", before.State, "test setup: the job must start cancellable")
	require.Equal(t, a.PrincipalID, before.PrincipalID)

	for _, prefix := range bothPrefixes {
		notYoursStatus, notYours := doJSON(t, http.MethodPost, srv.URL+prefix+"/jobs/"+aJob.String()+"/cancel", b.AuthHeader(), "")
		nonexistentStatus, nonexistent := doJSON(t, http.MethodPost, srv.URL+prefix+"/jobs/"+uuid.New().String()+"/cancel", b.AuthHeader(), "")

		require.Equal(t, http.StatusNotFound, notYoursStatus, "prefix %q", prefix)
		require.Equal(t, nonexistentStatus, notYoursStatus)
		require.Equal(t, string(nonexistent), string(notYours),
			"an unauthorized cancel must be indistinguishable from cancelling a nonexistent job")

		after := snapshotJobRow(t, db, aJob)
		require.Equal(t, before, after,
			"an unauthorized cancel must mutate ZERO rows -- state, cancel_requested, "+
				"cancel_requested_at, updated_at, terminal_at and version must all be untouched")
	}

	// And the owner can still cancel it, so the guard is scoping access,
	// not breaking the operation.
	status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs/"+aJob.String()+"/cancel", a.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status)
	owned := snapshotJobRow(t, db, aJob)
	require.Equal(t, "CANCELLED", owned.State, "the owning principal's cancel must actually work")
	require.Greater(t, owned.Version, before.Version, "a real cancellation does bump version")
}

// TestCancelJob_CrossPrincipal_RunningJob_NeverSetsCancelRequested is the
// specific scenario §4a names: a RUNNING job, where the cascade's SECOND
// step (RequestCancellation) is the mutating statement an unauthorized
// caller would otherwise reach. CancelQueuedOrRetryWait cannot match a
// RUNNING job on state alone, so this exercises a different guard than the
// QUEUED case above.
func TestCancelJob_CrossPrincipal_RunningJob_NeverSetsCancelRequested(t *testing.T) {
	srv, db, a, b := newTwoPrincipalServer(t)
	aJob := createJobAs(t, srv, a, "test.p12.isolation.cancel.running")

	// Drive the job to RUNNING the way a worker would.
	_, err := db.Exec(`UPDATE jobs SET state = 'RUNNING', lease_owner = 'w1', lease_generation = 1,
		lease_expires_at = now() + interval '1 minute' WHERE id = $1`, aJob)
	require.NoError(t, err)

	before := snapshotJobRow(t, db, aJob)
	require.Equal(t, "RUNNING", before.State)
	require.False(t, before.CancelRequested)

	status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs/"+aJob.String()+"/cancel", b.AuthHeader(), "")
	require.Equal(t, http.StatusNotFound, status)

	after := snapshotJobRow(t, db, aJob)
	require.False(t, after.CancelRequested,
		"an unauthorized cancel must never set cancel_requested on another principal's RUNNING job")
	require.Equal(t, before, after, "the RUNNING row must be entirely untouched")

	// The owner's cancel does reach it.
	status, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/jobs/"+aJob.String()+"/cancel", a.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status)
	require.True(t, snapshotJobRow(t, db, aJob).CancelRequested)
}

// TestCancelWorkflow_CrossPrincipal_MutatesZeroRows proves the
// workflow-level check gates its NODES, not just its top-level row
// (docs/phase-12-plan.md §7): the workflow row and every node job must be
// untouched after an unauthorized cancel.
func TestCancelWorkflow_CrossPrincipal_MutatesZeroRows(t *testing.T) {
	srv, db, a, b := newTwoPrincipalServer(t)

	status, raw := doJSON(t, http.MethodPost, srv.URL+"/v1/workflows", a.AuthHeader(),
		`{"nodes":[{"node_key":"root","job_type":"test.p12.iso.wf.root","payload":{}},
		           {"node_key":"leaf","job_type":"test.p12.iso.wf.leaf","payload":{},"depends_on":["root"]}]}`)
	require.Equal(t, http.StatusCreated, status, "body: %s", raw)

	var created struct {
		ID    string `json:"id"`
		Nodes []struct {
			JobID string `json:"job_id"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	wfID := uuid.MustParse(created.ID)
	require.Len(t, created.Nodes, 2)

	wfBefore := snapshotWorkflowRow(t, db, wfID)
	nodeBefore := map[string]jobRowSnapshot{}
	for _, n := range created.Nodes {
		nodeBefore[n.JobID] = snapshotJobRow(t, db, uuid.MustParse(n.JobID))
	}

	for _, prefix := range bothPrefixes {
		notYoursStatus, notYours := doJSON(t, http.MethodPost, srv.URL+prefix+"/workflows/"+wfID.String()+"/cancel", b.AuthHeader(), "")
		nonexistentStatus, nonexistent := doJSON(t, http.MethodPost, srv.URL+prefix+"/workflows/"+uuid.New().String()+"/cancel", b.AuthHeader(), "")

		require.Equal(t, http.StatusNotFound, notYoursStatus)
		require.Equal(t, nonexistentStatus, notYoursStatus)
		require.Equal(t, string(nonexistent), string(notYours))

		require.Equal(t, wfBefore, snapshotWorkflowRow(t, db, wfID),
			"an unauthorized workflow cancel must not touch the workflow_instances row")
		for jobID, before := range nodeBefore {
			require.Equal(t, before, snapshotJobRow(t, db, uuid.MustParse(jobID)),
				"an unauthorized workflow cancel must not touch node job %s", jobID)
		}
	}

	// The owner's cancel does work.
	status, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/workflows/"+wfID.String()+"/cancel", a.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status)
	require.True(t, snapshotWorkflowRow(t, db, wfID).CancelRequested)
}

// ---------------------------------------------------------------------
// Admin exception
// ---------------------------------------------------------------------

// TestAdminPrincipal_MayReadAndCancelAnyPrincipalsResources pins the ONE
// documented exception to ownership scoping, and pins its boundary: an
// admin-KIND principal reaches everything; an ordinary caller holding an
// admin-SCOPED key does not.
func TestAdminPrincipal_MayReadAndCancelAnyPrincipalsResources(t *testing.T) {
	db := testutil.DB(t)
	srv, _, ps := newAuthedServer(t, db)
	owner := newIdentity(t, ps, principal.KindCaller, "resource owner", principal.ScopeJobs)
	admin := newIdentity(t, ps, principal.KindAdmin, "operator", principal.ScopeJobs)
	// A caller-kind principal whose KEY carries the admin scope: full
	// capabilities, but NOT the ownership bypass.
	scopedOnly := newIdentity(t, ps, principal.KindCaller, "admin-scoped caller", principal.ScopeAdmin)

	jobID := createJobAs(t, srv, owner, "test.p12.admin.job")
	wfID := createWorkflowAs(t, srv, owner, "test.p12.admin.wf")

	t.Run("admin reads another principal's job", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+jobID.String(), admin.AuthHeader(), "")
		require.Equal(t, http.StatusOK, status)
	})
	t.Run("admin reads another principal's workflow", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/workflows/"+wfID.String(), admin.AuthHeader(), "")
		require.Equal(t, http.StatusOK, status)
	})
	t.Run("admin-scoped ordinary caller does NOT get the bypass", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+jobID.String(), scopedOnly.AuthHeader(), "")
		require.Equal(t, http.StatusNotFound, status,
			"the admin SCOPE widens capabilities; only the admin KIND grants cross-principal access")
	})
	t.Run("admin cancels another principal's job", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs/"+jobID.String()+"/cancel", admin.AuthHeader(), "")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "CANCELLED", snapshotJobRow(t, db, jobID).State)
	})
	t.Run("admin cancels another principal's workflow", func(t *testing.T) {
		status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/workflows/"+wfID.String()+"/cancel", admin.AuthHeader(), "")
		require.Equal(t, http.StatusOK, status)
		require.True(t, snapshotWorkflowRow(t, db, wfID).CancelRequested)
	})
}

// ---------------------------------------------------------------------
// #6 -- identity cannot be supplied by the request
// ---------------------------------------------------------------------

// TestCreateJob_RequestBodyPrincipalIDFieldIgnored is verification point 6.
// Phase 11 made the decoder tolerant of unknown fields, so a body carrying
// "principal_id" decodes cleanly -- and must be completely ignored. The
// created row's owner is checked in the DATABASE, not in the response.
func TestCreateJob_RequestBodyPrincipalIDFieldIgnored(t *testing.T) {
	srv, db, a, victim := newTwoPrincipalServer(t)

	for _, field := range []string{"principal_id", "tenant_id", "actor", "owner", "principalId"} {
		t.Run(field, func(t *testing.T) {
			body := fmt.Sprintf(`{"job_type":"test.p12.spoof","payload":{},%q:%q}`, field, victim.PrincipalID.String())
			status, raw := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs", a.AuthHeader(), body)
			require.Equal(t, http.StatusCreated, status, "an unknown field must be ignored, not rejected; body: %s", raw)

			var out struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal(raw, &out))
			got := snapshotJobRow(t, db, uuid.MustParse(out.ID))
			require.Equal(t, a.PrincipalID, got.PrincipalID,
				"the job must be attributed to the AUTHENTICATED caller, never to a body-supplied identity")
			require.NotEqual(t, victim.PrincipalID, got.PrincipalID)
		})
	}
}

// TestCreateWorkflow_RequestBodyPrincipalIDFieldIgnored is the workflow
// equivalent, checking the workflow_instances row and every node job.
func TestCreateWorkflow_RequestBodyPrincipalIDFieldIgnored(t *testing.T) {
	srv, db, a, victim := newTwoPrincipalServer(t)

	body := fmt.Sprintf(
		`{"principal_id":%q,"nodes":[{"node_key":"n","job_type":"test.p12.spoof.wf","payload":{},"principal_id":%q}]}`,
		victim.PrincipalID, victim.PrincipalID)
	status, raw := doJSON(t, http.MethodPost, srv.URL+"/v1/workflows", a.AuthHeader(), body)
	require.Equal(t, http.StatusCreated, status, "body: %s", raw)

	var out struct {
		ID    string `json:"id"`
		Nodes []struct {
			JobID string `json:"job_id"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, a.PrincipalID, snapshotWorkflowRow(t, db, uuid.MustParse(out.ID)).PrincipalID)
	for _, n := range out.Nodes {
		require.Equal(t, a.PrincipalID, snapshotJobRow(t, db, uuid.MustParse(n.JobID)).PrincipalID,
			"every node job must belong to the authenticated caller")
	}
}

// ---------------------------------------------------------------------
// #15 -- no forwarded-header trust
// ---------------------------------------------------------------------

// TestAuth_ForwardedHeadersCannotImpersonatePrincipal is verification
// point 15's behavioural half (the absence-of-code half is
// TestAPIPackage_ReadsNoForwardedOrIdentityHeaders below).
//
// Because TLS terminates at an external proxy (OD-7), TaskForge receives
// plaintext HTTP and every request arrives looking like it came from that
// proxy. This test sends every forwarded/identity-shaped header an
// attacker might hope is trusted, and asserts that (a) they never
// authenticate an unauthenticated request, and (b) they never change whose
// identity an authenticated request runs as.
func TestAuth_ForwardedHeadersCannotImpersonatePrincipal(t *testing.T) {
	srv, db, a, victim := newTwoPrincipalServer(t)

	spoofHeaders := map[string]string{
		"X-Forwarded-For":           "10.0.0.1",
		"X-Forwarded-Proto":         "https",
		"X-Real-IP":                 "10.0.0.1",
		"X-Forwarded-User":          victim.PrincipalID.String(),
		"X-Authenticated-User":      victim.PrincipalID.String(),
		"X-Principal-Id":            victim.PrincipalID.String(),
		"X-Taskforge-Principal":     victim.PrincipalID.String(),
		"X-API-Key":                 a.Credential,
		"X-Forwarded-Authorization": a.AuthHeader(),
		"X-Admin":                   "true",
		"X-Scopes":                  "admin",
	}

	t.Run("forwarded headers never authenticate on their own", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(), nil)
		require.NoError(t, err)
		for k, v := range spoofHeaders {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"no proxy-injected header may substitute for a credential -- including X-API-Key, "+
				"which OD-2 deliberately does not accept as a transport")
	})

	t.Run("forwarded headers never change the authenticated identity", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/jobs",
			strings.NewReader(`{"job_type":"test.p12.forwarded","payload":{}}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", a.AuthHeader())
		req.Header.Set("Content-Type", "application/json")
		for k, v := range spoofHeaders {
			if k == "Authorization" {
				continue
			}
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		var out struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		require.Equal(t, a.PrincipalID, snapshotJobRow(t, db, uuid.MustParse(out.ID)).PrincipalID,
			"headers claiming another principal must not re-attribute the job")
	})

	t.Run("an admin-claiming header grants no ownership bypass", func(t *testing.T) {
		victimJob := createJobAs(t, srv, victim, "test.p12.forwarded.victim")

		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/jobs/"+victimJob.String(), nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", a.AuthHeader())
		for k, v := range spoofHeaders {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode,
			"X-Admin/X-Scopes headers must not confer admin privileges")
	})
}

// ---------------------------------------------------------------------
// G7 -- principal-scoped idempotency at the HTTP boundary
// ---------------------------------------------------------------------

// TestIdempotency_IsScopedPerPrincipal proves the tenant-scoping half of
// migration 0009 end to end: two principals using the SAME job_type and
// Idempotency-Key get two independent jobs, while one principal reusing
// its own key still gets first-write-wins deduplication.
func TestIdempotency_IsScopedPerPrincipal(t *testing.T) {
	srv, db, a, b := newTwoPrincipalServer(t)

	const jobType = "test.p12.idem.shared"
	const key = "shared-key-both-tenants-chose"
	body := `{"job_type":"` + jobType + `","payload":{}}`

	post := func(ident testIdentity) string {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/jobs", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", ident.AuthHeader())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		var out struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out.ID
	}

	aFirst := post(a)
	bFirst := post(b)
	require.NotEqual(t, aFirst, bFirst,
		"two tenants using the same job_type and Idempotency-Key must get two independent jobs")

	require.Equal(t, aFirst, post(a), "a principal reusing its own key still deduplicates")
	require.Equal(t, bFirst, post(b))

	require.Equal(t, a.PrincipalID, snapshotJobRow(t, db, uuid.MustParse(aFirst)).PrincipalID)
	require.Equal(t, b.PrincipalID, snapshotJobRow(t, db, uuid.MustParse(bFirst)).PrincipalID)

	var total int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key = $2`, jobType, key).Scan(&total))
	require.Equal(t, 2, total, "exactly one row per principal, not one globally and not four")
}

// ---------------------------------------------------------------------
// #5 (structural) -- deny-by-default is enforced by construction
// ---------------------------------------------------------------------

// TestRouter_DenyByDefaultWhenNoAuthenticatorConfigured proves the failure
// mode of a wiring mistake is "reject everything", not "authenticate
// nothing". A Handlers built without WithAuthenticator must 401 every
// route rather than serving them openly.
func TestRouter_DenyByDefaultWhenNoAuthenticatorConfigured(t *testing.T) {
	db := testutil.DB(t)
	h := api.NewHandlers(newStoreForTest(db), discardLogger()) // deliberately no authenticator
	srv := httptest.NewServer(api.NewRouter(h, api.WithMetricsEndpoint(stubMetricsHandler())))
	t.Cleanup(srv.Close)

	id := uuid.New()
	for _, prefix := range bothPrefixes {
		for _, rc := range allSixRoutes() {
			status, body := doJSON(t, rc.method, srv.URL+prefix+rc.path(id), "Bearer anything.at-all", rc.body)
			require.Equal(t, http.StatusUnauthorized, status,
				"%s%s must fail closed when no authenticator is wired", prefix, rc.name)
			requireUniformUnauthorizedBody(t, body)
		}
	}
	status, _ := doJSON(t, http.MethodGet, srv.URL+"/metrics", "Bearer anything.at-all", "")
	require.Equal(t, http.StatusUnauthorized, status)
}
