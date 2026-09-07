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
	"strings"
	"sync"
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

// ---------------------------------------------------------------------
// Phase 4: submission idempotency, at the full HTTP boundary (real
// handlers, real routing, real header parsing -- not just internal/store
// calls directly, see idempotency_test.go in internal/store for that).
// Per docs/idempotency.md and docs/worker-protocol.md's API Contract.
// ---------------------------------------------------------------------

type createdJob struct {
	ID             string  `json:"id"`
	State          string  `json:"state"`
	IdempotencyKey *string `json:"idempotency_key"`
}

func postJob(t *testing.T, srv *httptest.Server, body, idempotencyKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/jobs", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func decodeCreatedJob(t *testing.T, resp *http.Response) createdJob {
	t.Helper()
	defer resp.Body.Close()
	var out createdJob
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// TestCreateJob_IdempotencyKey_FirstSubmissionCreatesJob is the baseline:
// a fresh Idempotency-Key creates a job and echoes the key back on
// GET /jobs/{id} (part of the durable row, per docs/data-model.md).
func TestCreateJob_IdempotencyKey_FirstSubmissionCreatesJob(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := postJob(t, srv, `{"job_type":"test.idem.http.first","payload":{}}`, "http-key-1")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeCreatedJob(t, resp)
	require.NotEmpty(t, created.ID)
	require.NotNil(t, created.IdempotencyKey)
	require.Equal(t, "http-key-1", *created.IdempotencyKey)
}

// TestCreateJob_IdempotencyKey_SequentialDuplicateReturnsSameJob is the
// "Acknowledgement / Response Loss" scenario documented in this task's
// PHASE 4 OBJECTIVE, proved end to end through the actual HTTP handler:
// commit, (simulated) response loss, client retries with the same key,
// TaskForge returns the SAME existing job -- same 201 status both times,
// no duplicate logical job.
func TestCreateJob_IdempotencyKey_SequentialDuplicateReturnsSameJob(t *testing.T) {
	srv, s := newTestServer(t)

	body := `{"job_type":"test.idem.http.seq","payload":{"a":1}}`
	resp1 := postJob(t, srv, body, "http-retry-key")
	require.Equal(t, http.StatusCreated, resp1.StatusCode)
	first := decodeCreatedJob(t, resp1)

	// The client never saw resp1 (network failure) and retries the exact
	// same logical request with the same key.
	resp2 := postJob(t, srv, body, "http-retry-key")
	require.Equal(t, http.StatusCreated, resp2.StatusCode, "the retried call gets the SAME success status the original would have produced")
	second := decodeCreatedJob(t, resp2)

	require.Equal(t, first.ID, second.ID, "no duplicate logical job is created")

	mapped, err := s.GetByIdempotencyKey(context.Background(), "test.idem.http.seq", "http-retry-key")
	require.NoError(t, err)
	require.Equal(t, first.ID, mapped.ID.String(), "TF-INV-008: the (job_type, idempotency_key) pair maps to exactly this one job")
}

// TestCreateJob_IdempotencyKey_ConcurrentDuplicates_ExactlyOneJobCreated is
// the roadmap's Phase 4 quality gate at the actual HTTP boundary: many
// concurrent POST /jobs calls with the identical Idempotency-Key must
// yield exactly one job row, and every response must carry the same
// job_id (SF-005, TF-INV-008/016).
func TestCreateJob_IdempotencyKey_ConcurrentDuplicates_ExactlyOneJobCreated(t *testing.T) {
	srv, _ := newTestServer(t)

	const n = 25
	body := `{"job_type":"test.idem.http.concurrent","payload":{}}`

	var wg sync.WaitGroup
	ids := make([]string, n)
	statuses := make([]int, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := postJob(t, srv, body, "http-concurrent-key")
			statuses[i] = resp.StatusCode
			ids[i] = decodeCreatedJob(t, resp).ID
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		require.Equal(t, http.StatusCreated, statuses[i], "every caller gets the same success status")
		require.NotEmpty(t, ids[i])
		require.Equal(t, ids[0], ids[i], "every caller must observe the same job_id")
	}
}

// TestCreateJob_IdempotencyKey_DifferentJobTypeSameKey_CreatesSeparateJobs
// proves the documented (job_type, idempotency_key) scope at the HTTP
// boundary: the same key value submitted under a different job_type is not
// a duplicate.
func TestCreateJob_IdempotencyKey_DifferentJobTypeSameKey_CreatesSeparateJobs(t *testing.T) {
	srv, _ := newTestServer(t)

	a := decodeCreatedJob(t, postJob(t, srv, `{"job_type":"test.idem.http.scope.a","payload":{}}`, "shared-http-key"))
	b := decodeCreatedJob(t, postJob(t, srv, `{"job_type":"test.idem.http.scope.b","payload":{}}`, "shared-http-key"))

	require.NotEqual(t, a.ID, b.ID)
}

// TestCreateJob_IdempotencyKey_EmptyHeaderTreatedAsAbsent proves the
// documented Phase 4 implementation decision (docs/idempotency.md
// "Implementation Notes"): an empty/whitespace-only Idempotency-Key header
// is treated as "no key supplied," not a validation error -- each such
// call creates its own job, just like omitting the header entirely.
func TestCreateJob_IdempotencyKey_EmptyHeaderTreatedAsAbsent(t *testing.T) {
	srv, _ := newTestServer(t)
	body := `{"job_type":"test.idem.http.empty","payload":{}}`

	req1, err := http.NewRequest(http.MethodPost, srv.URL+"/jobs", bytes.NewBufferString(body))
	require.NoError(t, err)
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "   ")
	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp1.StatusCode)
	first := decodeCreatedJob(t, resp1)
	require.Nil(t, first.IdempotencyKey)

	second := decodeCreatedJob(t, postJob(t, srv, body, ""))
	require.NotEqual(t, first.ID, second.ID, "two submissions with no effective key must each create a new job")
}

// TestCreateJob_IdempotencyKey_TooLongRejected proves the documented
// MaxIdempotencyKeyLength bound is enforced with a 400, not silently
// truncated or passed through to the database.
func TestCreateJob_IdempotencyKey_TooLongRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	long := strings.Repeat("k", api.MaxIdempotencyKeyLength+1)
	resp := postJob(t, srv, `{"job_type":"test.idem.http.toolong","payload":{}}`, long)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestCreateJob_IdempotencyKey_ProcessRestartThenDuplicateReturnsExistingJob
// simulates an API server restart (a fresh Handlers/Store/httptest.Server
// sharing only the durable database -- no in-memory state survives) and
// proves a duplicate submission afterward still returns the pre-restart
// job, exactly as docs/failure-model.md F4 requires ("API servers are
// stateless ... no in-memory state is lost because none is load-bearing").
func TestCreateJob_IdempotencyKey_ProcessRestartThenDuplicateReturnsExistingJob(t *testing.T) {
	db := testutil.DB(t)
	before := httptest.NewServer(api.NewRouter(api.NewHandlers(store.New(db), discardLogger())))
	t.Cleanup(before.Close)

	first := decodeCreatedJob(t, postJob(t, before, `{"job_type":"test.idem.http.restart","payload":{}}`, "restart-http-key"))
	before.Close()

	// "Restart": a brand-new server, brand-new Handlers, brand-new Store --
	// sharing only the underlying database.
	after := httptest.NewServer(api.NewRouter(api.NewHandlers(store.New(db), discardLogger())))
	t.Cleanup(after.Close)

	second := decodeCreatedJob(t, postJob(t, after, `{"job_type":"test.idem.http.restart","payload":{}}`, "restart-http-key"))
	require.Equal(t, first.ID, second.ID)
}

// TestCreateJob_NoIdempotencyKey_EachSubmissionCreatesNewJob is the
// regression guard for existing (pre-Phase-4) clients: omitting the header
// entirely must behave exactly as before -- every call creates a new job,
// even with byte-identical job_type and payload.
func TestCreateJob_NoIdempotencyKey_EachSubmissionCreatesNewJob(t *testing.T) {
	srv, _ := newTestServer(t)
	body := `{"job_type":"test.idem.http.nokey","payload":{}}`

	a := decodeCreatedJob(t, postJob(t, srv, body, ""))
	b := decodeCreatedJob(t, postJob(t, srv, body, ""))
	require.NotEqual(t, a.ID, b.ID)
}
