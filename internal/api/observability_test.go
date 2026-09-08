// Phase 8: structured-logging assertions at the HTTP boundary --
// sensitive-data policy (the raw Idempotency-Key header must never
// appear in a log line) and correlation-field presence
// (docs/observability.md's "every log line relevant to a job's
// lifecycle includes ... job_id").
package api_test

import (
	"bytes"
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

func newBufferLoggingServer(t *testing.T) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	db := testutil.DB(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := api.NewHandlers(store.New(db, store.WithLogger(logger)), logger)
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)
	return srv, &buf
}

// TestSubmissionLogging_RawIdempotencyKeyNeverLogged is this phase's
// sensitive-data policy adversarial-audit item: the Idempotency-Key
// header value must never be emitted into any log line, only the fact
// that one was supplied (docs/roadmap.md's Idempotency Observability
// guidance: "Avoid logging it unless docs explicitly permit safe
// redaction/hashing").
func TestSubmissionLogging_RawIdempotencyKeyNeverLogged(t *testing.T) {
	srv, buf := newBufferLoggingServer(t)

	const secretKey = "super-secret-caller-chosen-idempotency-key-9f8e7d"
	body, _ := json.Marshal(map[string]any{"job_type": "test.obslog.submit", "payload": map[string]any{}})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/jobs", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Idempotency-Key", secretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// Submit a second, duplicate request too -- the "duplicate submission
	// hit" log path is a separate code path from the "new submission" one
	// and must be audited independently.
	req2, err := http.NewRequest(http.MethodPost, srv.URL+"/jobs", bytes.NewReader(body))
	require.NoError(t, err)
	req2.Header.Set("Idempotency-Key", secretKey)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusCreated, resp2.StatusCode)

	logOutput := buf.String()
	require.NotEmpty(t, logOutput, "sanity: submission must have produced some log output")
	require.NotContains(t, logOutput, secretKey,
		"the raw Idempotency-Key value must never appear in structured logs")
	require.Contains(t, logOutput, `"had_idempotency_key":true`)
	require.Contains(t, logOutput, `"event":"duplicate_submission_hit"`)
}

// TestSubmissionLogging_CorrelationFieldsPresent proves every
// submission-lifecycle log line carries the stable correlation fields
// docs/observability.md requires: job_id, job_type, state, event.
func TestSubmissionLogging_CorrelationFieldsPresent(t *testing.T) {
	srv, buf := newBufferLoggingServer(t)

	body, _ := json.Marshal(map[string]any{"job_type": "test.obslog.correlation", "payload": map[string]any{}})
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))

	logOutput := buf.String()
	require.Contains(t, logOutput, `"event":"submission"`)
	require.Contains(t, logOutput, `"job_id":"`+created.ID+`"`)
	require.Contains(t, logOutput, `"job_type":"test.obslog.correlation"`)
	require.Contains(t, logOutput, `"state":"QUEUED"`)
}

// TestCancellationLogging_RequestedEventLogged proves
// docs/observability.md's "Cancellation requested / cancellation race
// outcome (which side won)" logging requirement: POST /jobs/{id}/cancel
// against a not-yet-claimed job logs a "cancellation_requested" event
// carrying the job's resulting state and which cascade path resolved it.
func TestCancellationLogging_RequestedEventLogged(t *testing.T) {
	srv, buf := newBufferLoggingServer(t)

	body, _ := json.Marshal(map[string]any{"job_type": "test.obslog.cancel", "payload": map[string]any{}})
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))

	cancelResp, err := http.Post(srv.URL+"/jobs/"+created.ID+"/cancel", "application/json", nil)
	require.NoError(t, err)
	defer cancelResp.Body.Close()
	require.Equal(t, http.StatusOK, cancelResp.StatusCode)

	logOutput := buf.String()
	require.Contains(t, logOutput, `"event":"cancellation_requested"`)
	require.Contains(t, logOutput, `"job_id":"`+created.ID+`"`)
	require.Contains(t, logOutput, `"path":"direct"`)
	require.Contains(t, logOutput, `"state":"CANCELLED"`)
}
