// Full-stack integration tests: real HTTP handlers, wired to
// internal/store, against a real PostgreSQL instance (internal/testutil).
// These prove TF-INV-001 ("accepted jobs cannot disappear") at the actual
// API boundary, not just at internal/store's boundary.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	db := testutil.DB(t)
	s := store.New(db)
	h := api.NewHandlers(s, discardLogger())
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)
	return srv, s
}

// TestCreateJob_AcknowledgementImpliesDurableCommit is the API-level proof
// of TF-INV-001's mechanism: a 201 response is only ever produced after
// internal/store.Insert's underlying INSERT has committed, and the job
// returned by GET /jobs/{id} immediately afterward (a completely separate
// request) matches — proving the commit already happened, not that the
// response merely echoed an in-memory value.
func TestCreateJob_AcknowledgementImpliesDurableCommit(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"job_type":"email.send","payload":{"to":"a@example.com"}}`
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "QUEUED", created.State)

	getResp, err := http.Get(srv.URL + "/jobs/" + created.ID)
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	var fetched struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&fetched))
	require.Equal(t, created.ID, fetched.ID)
	require.Equal(t, "QUEUED", fetched.State)
}

func TestCreateJob_InvalidJSONRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(`{not json`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCreateJob_MissingJobTypeRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(`{"payload":{}}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGetJob_NotFoundReturns404(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/jobs/" + "00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestGetJob_MalformedIDReturns400(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/jobs/not-a-uuid")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestGetJob_ReflectsWorkerCompletion proves GET /jobs/{id} reads live
// durable state, not a cached snapshot from submission time: a job
// completed directly via the store (simulating a worker process) is
// visible through the API in the same request cycle.
func TestGetJob_ReflectsWorkerCompletion(t *testing.T) {
	srv, s := newTestServer(t)
	ctx := context.Background()

	body := `{"job_type":"test.reflect","payload":{}}`
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	resp.Body.Close()

	claimed, ok, err := s.Claim(ctx, "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimed.ID, *claimed.LeaseOwner, claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	getResp, err := http.Get(srv.URL + "/jobs/" + created.ID)
	require.NoError(t, err)
	defer getResp.Body.Close()
	var fetched struct {
		State      string  `json:"state"`
		TerminalAt *string `json:"terminal_at"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&fetched))
	require.Equal(t, "SUCCEEDED", fetched.State)
	require.NotNil(t, fetched.TerminalAt)
}
