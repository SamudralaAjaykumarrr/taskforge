// Phase 12 observability proofs (docs/phase-12-plan.md §10, §11
// verification points 10 and 12): the audit-trail `actor` field, the
// absolute rule that a raw credential never reaches a log line or an error
// body, and the taskforge_auth_failures_total{reason} counter that gives
// an operator the credential-stuffing signal this phase provides in place
// of the rate limiting it explicitly defers to Phase 13.
package api_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// newAuditServer wires a fully authenticated server whose logs and metrics
// the test can inspect.
func newAuditServer(t *testing.T) (*httptest.Server, *bytes.Buffer, *metrics.Metrics, *principal.Store) {
	t.Helper()
	db := testutil.DB(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ps := newPrincipalStore(t, db)
	m := metrics.New()
	h := api.NewHandlers(store.New(db, store.WithLogger(logger)), logger,
		api.WithAuthenticator(ps), api.WithMetrics(m))
	srv := httptest.NewServer(api.NewRouter(h, api.WithMetricsEndpoint(stubMetricsHandler())))
	t.Cleanup(srv.Close)
	return srv, &buf, m, ps
}

// TestAuditLogging_ActorPresentOnSubmitAndCancel is G6: every
// submit/cancel log line carries the authenticated principal's id, closing
// docs/security-model.md §5's "no audit trail" gap. The actor is the
// principal ID -- a non-secret identifier -- never the credential that
// proved it.
func TestAuditLogging_ActorPresentOnSubmitAndCancel(t *testing.T) {
	srv, buf, _, ps := newAuditServer(t)
	caller := newIdentity(t, ps, principal.KindCaller, "audited caller", principal.ScopeJobs)

	jobID := createJobAs(t, srv, caller, "test.p12.audit.job")
	status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs/"+jobID.String()+"/cancel", caller.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status)
	wfID := createWorkflowAs(t, srv, caller, "test.p12.audit.wf")
	status, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/workflows/"+wfID.String()+"/cancel", caller.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status)

	actor := `"actor":"` + caller.PrincipalID.String() + `"`
	lines := logLinesByEvent(t, buf)

	for _, event := range []string{"submission", "cancellation_requested", "workflow_submitted", "workflow_cancel_requested"} {
		line, ok := lines[event]
		require.True(t, ok, "expected a %q log line", event)
		require.Contains(t, line, actor,
			"the %q log line must identify the authenticated actor", event)
	}
}

// TestAuditLogging_RawCredentialNeverLogged is verification point 10, run
// across every success AND failure path in the verification sequence.
//
// It checks for the full credential, the secret half on its own, and the
// whole Authorization header value -- because a partial leak (e.g. logging
// the header for "debugging") is just as damaging as logging the parsed
// secret.
func TestAuditLogging_RawCredentialNeverLogged(t *testing.T) {
	srv, buf, _, ps := newAuditServer(t)

	live := newIdentity(t, ps, principal.KindCaller, "leak audit live", principal.ScopeJobs)
	revoked := newIdentity(t, ps, principal.KindCaller, "leak audit revoked", principal.ScopeJobs)
	require.NoError(t, ps.RevokeAPIKey(t.Context(), revoked.KeyID))
	expired := newExpiredIdentity(t, ps, "leak audit expired", principal.ScopeJobs)
	metricsOnly := newIdentity(t, ps, principal.KindCaller, "leak audit metrics", principal.ScopeMetrics)

	badSecretCredential := principal.FormatCredential(live.KeyID, "a-secret-that-must-never-be-logged")

	// Success path.
	jobID := createJobAs(t, srv, live, "test.p12.leakaudit")
	status, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/jobs/"+jobID.String()+"/cancel", live.AuthHeader(), "")
	require.Equal(t, http.StatusOK, status)

	// Every failure path.
	for _, header := range []string{
		"",
		"Bearer malformed-credential-value",
		"Bearer 22222222222222222222222222222222.unknown-key-secret",
		revoked.AuthHeader(),
		expired.AuthHeader(),
		"Bearer " + badSecretCredential,
		metricsOnly.AuthHeader(), // 403, a different rejection path
	} {
		doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(), header, "")
	}

	logOutput := buf.String()
	require.NotEmpty(t, logOutput, "sanity: these requests must have produced log output")

	forbidden := map[string]string{
		"live credential":        live.Credential,
		"live secret":            live.Secret,
		"live auth header":       live.AuthHeader(),
		"revoked credential":     revoked.Credential,
		"revoked secret":         revoked.Secret,
		"expired credential":     expired.Credential,
		"expired secret":         expired.Secret,
		"metrics-only secret":    metricsOnly.Secret,
		"a presented bad secret": "a-secret-that-must-never-be-logged",
		"test pepper":            string(testPepper),
	}
	for what, value := range forbidden {
		require.NotContains(t, logOutput, value,
			"no log line may ever contain the %s", what)
	}

	// The diagnostic value an operator DOES get: the reason, as a bounded
	// label -- server-side only, never in the response.
	require.Contains(t, logOutput, `"event":"auth_failed"`)
	require.Contains(t, logOutput, `"reason":"`+principal.ReasonUnknownKey+`"`)
}

// TestAuthFailureMetric_IncrementsPerReason is verification point 12's one
// required test: the counter that replaces (and is explicitly not) rate
// limiting must break down correctly by reason, so an operator can tell
// "someone is stuffing credentials" apart from "one deployment is still
// using a rotated-out key."
func TestAuthFailureMetric_IncrementsPerReason(t *testing.T) {
	srv, _, m, ps := newAuditServer(t)

	live := newIdentity(t, ps, principal.KindCaller, "metric live", principal.ScopeJobs)
	revoked := newIdentity(t, ps, principal.KindCaller, "metric revoked", principal.ScopeJobs)
	require.NoError(t, ps.RevokeAPIKey(t.Context(), revoked.KeyID))
	expired := newExpiredIdentity(t, ps, "metric expired", principal.ScopeJobs)
	revokedPrincipal := newIdentity(t, ps, principal.KindCaller, "metric revoked principal", principal.ScopeJobs)
	require.NoError(t, ps.RevokePrincipal(t.Context(), revokedPrincipal.PrincipalID))

	cases := map[string]string{
		principal.ReasonMalformed:        "Bearer not-a-credential",
		principal.ReasonUnknownKey:       "Bearer 33333333333333333333333333333333.secret",
		principal.ReasonRevoked:          revoked.AuthHeader(),
		principal.ReasonExpired:          expired.AuthHeader(),
		principal.ReasonBadSecret:        "Bearer " + principal.FormatCredential(live.KeyID, "wrong"),
		principal.ReasonPrincipalRevoked: revokedPrincipal.AuthHeader(),
	}
	for reason, header := range cases {
		status, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(), header, "")
		require.Equal(t, http.StatusUnauthorized, status, "reason %q", reason)
		require.Equal(t, 1.0, counterValue(t, m.AuthFailuresTotal, reason),
			"taskforge_auth_failures_total{reason=%q} must have incremented exactly once", reason)
	}

	// A missing header is classified as malformed, so that bucket now
	// holds two.
	status, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(), "", "")
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, 2.0, counterValue(t, m.AuthFailuresTotal, principal.ReasonMalformed))

	// A SUCCESSFUL request increments nothing.
	before := counterValue(t, m.AuthFailuresTotal, principal.ReasonBadSecret)
	createJobAs(t, srv, live, "test.p12.metric.success")
	require.Equal(t, before, counterValue(t, m.AuthFailuresTotal, principal.ReasonBadSecret))
}

// TestAuthFailureMetric_NeverLabeledWithCredentialOrPrincipal is the
// cardinality-and-sensitivity audit docs/observability.md's Cardinality
// Policy requires, applied to this phase's one new metric: every label
// value must come from the fixed reason enum -- never a key_id, secret,
// principal id, path, or IP.
func TestAuthFailureMetric_NeverLabeledWithCredentialOrPrincipal(t *testing.T) {
	srv, _, m, ps := newAuditServer(t)
	live := newIdentity(t, ps, principal.KindCaller, "cardinality audit", principal.ScopeJobs)

	for i := 0; i < 5; i++ {
		doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(),
			"Bearer "+principal.FormatCredential(live.KeyID, "wrong-secret-attempt"), "")
	}

	families, err := m.Registry.Gather()
	require.NoError(t, err)
	var found bool
	for _, f := range families {
		if f.GetName() != "taskforge_auth_failures_total" {
			continue
		}
		found = true
		for _, metric := range f.GetMetric() {
			labels := metric.GetLabel()
			require.Len(t, labels, 1, "the counter must carry exactly one label")
			require.Equal(t, "reason", labels[0].GetName())
			require.Contains(t, principal.AllFailureReasons, labels[0].GetValue(),
				"every label value must come from the fixed reason enum")
			require.NotContains(t, labels[0].GetValue(), live.KeyID)
			require.NotContains(t, labels[0].GetValue(), live.Secret)
			require.NotContains(t, labels[0].GetValue(), live.PrincipalID.String())
		}
	}
	require.True(t, found, "taskforge_auth_failures_total must be registered")
}

// TestErrorResponses_NeverContainCredentialMaterial extends the
// no-leak rule from logs to response bodies: the client-facing side must
// be uniform AND must not echo back anything the caller sent as a
// credential.
func TestErrorResponses_NeverContainCredentialMaterial(t *testing.T) {
	srv, _, _, ps := newAuditServer(t)
	live := newIdentity(t, ps, principal.KindCaller, "response leak audit", principal.ScopeJobs)

	const presented = "presented-secret-value-must-not-echo"
	for _, header := range []string{
		"Bearer " + principal.FormatCredential(live.KeyID, presented),
		"Bearer " + presented,
		live.AuthHeader(),
	} {
		_, body := doJSON(t, http.MethodGet, srv.URL+"/v1/jobs/"+uuid.New().String(), header, "")
		require.NotContains(t, string(body), presented)
		require.NotContains(t, string(body), live.Secret)
		require.NotContains(t, string(body), live.KeyID,
			"not even the non-secret key_id needs to be echoed back")
	}
}

// logLinesByEvent indexes the JSON log buffer by each line's "event"
// field, so an assertion can target the specific line it means.
func logLinesByEvent(t *testing.T, buf *bytes.Buffer) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var parsed struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(line, &parsed); err != nil || parsed.Event == "" {
			continue
		}
		out[parsed.Event] = string(line)
	}
	return out
}

// counterValue reads one labeled counter's current value.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	c, err := vec.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}
