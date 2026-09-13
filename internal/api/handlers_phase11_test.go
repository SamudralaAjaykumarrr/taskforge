// Phase 11 (docs/enterprise-roadmap.md "Transactional Enqueue & API
// Contract Hardening") API-contract-hardening tests: the /v1/ version
// prefix (with the legacy unprefixed surface kept fully functional but
// marked deprecated), the request-body size limit, and unknown-field
// tolerance in both directions. See handlers_integration_test.go for this
// file's shared newTestServer/discardLogger helpers.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

// TestRouter_V1PrefixServesSameHandlersAsLegacy proves all six canonical
// job/workflow endpoints -- not merely POST /jobs and GET /jobs/{id}, which
// is all an earlier version of this test actually exercised despite its
// doc comment's "every endpoint" claim (audit finding, corrected here) --
// are registered and reach their intended handler/behavior under both the
// new canonical /v1/ prefix and the legacy unprefixed path: POST /jobs,
// GET /jobs/{id}, POST /jobs/{id}/cancel, POST /workflows,
// GET /workflows/{id}, POST /workflows/{id}/cancel.
func TestRouter_V1PrefixServesSameHandlersAsLegacy(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{"legacy_unprefixed", ""},
		{"v1", "/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := tc.prefix

			// POST /jobs
			jobBody := `{"job_type":"email.send","payload":{"to":"a@example.com"}}`
			jobResp, err := http.Post(srv.URL+prefix+"/jobs", "application/json", bytes.NewBufferString(jobBody))
			require.NoError(t, err)
			defer jobResp.Body.Close()
			require.Equal(t, http.StatusCreated, jobResp.StatusCode, "POST %s/jobs", prefix)
			var createdJob struct {
				ID    string `json:"id"`
				State string `json:"state"`
			}
			require.NoError(t, json.NewDecoder(jobResp.Body).Decode(&createdJob))
			require.NotEmpty(t, createdJob.ID)
			require.Equal(t, "QUEUED", createdJob.State)

			// GET /jobs/{id}
			getJobResp, err := http.Get(srv.URL + prefix + "/jobs/" + createdJob.ID)
			require.NoError(t, err)
			defer getJobResp.Body.Close()
			require.Equal(t, http.StatusOK, getJobResp.StatusCode, "GET %s/jobs/{id}", prefix)

			// POST /jobs/{id}/cancel -- the job is still QUEUED, so this
			// must resolve directly to CANCELLED, proving the handler
			// actually ran (internal/store.CancelQueuedOrRetryWait), not
			// merely that the route returned some 2xx.
			cancelJobResp, err := http.Post(srv.URL+prefix+"/jobs/"+createdJob.ID+"/cancel", "application/json", nil)
			require.NoError(t, err)
			defer cancelJobResp.Body.Close()
			require.Equal(t, http.StatusOK, cancelJobResp.StatusCode, "POST %s/jobs/{id}/cancel", prefix)
			var cancelledJob struct {
				State string `json:"state"`
			}
			require.NoError(t, json.NewDecoder(cancelJobResp.Body).Decode(&cancelledJob))
			require.Equal(t, "CANCELLED", cancelledJob.State, "POST %s/jobs/{id}/cancel must actually cancel the job", prefix)

			// POST /workflows
			wfBody := `{"nodes":[{"node_key":"n1","job_type":"test.node"}]}`
			wfResp, err := http.Post(srv.URL+prefix+"/workflows", "application/json", bytes.NewBufferString(wfBody))
			require.NoError(t, err)
			defer wfResp.Body.Close()
			require.Equal(t, http.StatusCreated, wfResp.StatusCode, "POST %s/workflows", prefix)
			var createdWf struct {
				ID    string `json:"id"`
				State string `json:"state"`
			}
			require.NoError(t, json.NewDecoder(wfResp.Body).Decode(&createdWf))
			require.NotEmpty(t, createdWf.ID)

			// GET /workflows/{id}
			getWfResp, err := http.Get(srv.URL + prefix + "/workflows/" + createdWf.ID)
			require.NoError(t, err)
			defer getWfResp.Body.Close()
			require.Equal(t, http.StatusOK, getWfResp.StatusCode, "GET %s/workflows/{id}", prefix)

			// POST /workflows/{id}/cancel -- the workflow's single node is
			// still QUEUED, so this must resolve to CANCELLED too.
			cancelWfResp, err := http.Post(srv.URL+prefix+"/workflows/"+createdWf.ID+"/cancel", "application/json", nil)
			require.NoError(t, err)
			defer cancelWfResp.Body.Close()
			require.Equal(t, http.StatusOK, cancelWfResp.StatusCode, "POST %s/workflows/{id}/cancel", prefix)
			var cancelledWf struct {
				State string `json:"state"`
			}
			require.NoError(t, json.NewDecoder(cancelWfResp.Body).Decode(&cancelledWf))
			require.Equal(t, "CANCELLED", cancelledWf.State, "POST %s/workflows/{id}/cancel must actually cancel the workflow", prefix)
		})
	}
}

// TestRouter_LegacyRoutesRemainFunctional_ButMarkedDeprecated proves the
// old, unprefixed surface's documented fate for this phase: still fully
// functional (never silently broken or redirected), but carrying an RFC
// 9745 Deprecation response header the new /v1/ surface does not --
// docs/compatibility-policy.md's already-drafted deprecation-header rule,
// applied rather than only planned. RFC 9745
// (https://www.rfc-editor.org/rfc/rfc9745) requires the Deprecation field's
// value to be an HTTP Structured Field Item Date, serialized
// "@<unix-seconds>" -- "Deprecation: true" (this codebase's original,
// corrected-here value) is not valid RFC 9745 syntax at all.
func TestRouter_LegacyRoutesRemainFunctional_ButMarkedDeprecated(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"job_type":"email.send","payload":{}}`

	legacyResp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer legacyResp.Body.Close()
	require.Equal(t, http.StatusCreated, legacyResp.StatusCode, "the legacy unprefixed route must remain fully functional")
	require.Equal(t, api.DeprecationHeaderValue, legacyResp.Header.Get("Deprecation"), "must be the RFC 9745 structured-field Date form, not the old non-conformant \"true\"")
	require.Equal(t, "@1788998400", legacyResp.Header.Get("Deprecation"), "exact literal value, per docs/compatibility-policy.md's fixed Phase-11 deprecation effective date (2026-09-10T00:00:00Z)")

	v1Resp, err := http.Post(srv.URL+"/v1/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer v1Resp.Body.Close()
	require.Equal(t, http.StatusCreated, v1Resp.StatusCode)
	require.Empty(t, v1Resp.Header.Get("Deprecation"), "the canonical /v1 route must not be marked deprecated")
}

// TestRouter_LegacyDeprecationHeader_DoesNotChangeLegacyBehavior proves the
// RFC 9745 header correction is purely additive to the response header set
// -- the legacy route's body, status code, and durable side effect (the
// job is actually created and independently fetchable) are byte-for-byte
// the same as before this phase's header-format fix.
func TestRouter_LegacyDeprecationHeader_DoesNotChangeLegacyBehavior(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"job_type":"email.send","payload":{"to":"a@example.com"}}`
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var created struct {
		ID      string `json:"id"`
		JobType string `json:"job_type"`
		State   string `json:"state"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "email.send", created.JobType)
	require.Equal(t, "QUEUED", created.State)

	getResp, err := http.Get(srv.URL + "/jobs/" + created.ID)
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode, "the legacy route's durable side effect (job actually created) is unchanged")
}

// TestCreateJob_OversizedBodyRejected413 is the exact-scope requirement
// "POST /jobs ... reject oversized bodies with 413," proven against a body
// larger than api.MaxRequestBodyBytes, not merely a documented number.
func TestCreateJob_OversizedBodyRejected413(t *testing.T) {
	srv, _ := newTestServer(t)

	oversizedPayload := `"` + strings.Repeat("a", api.MaxRequestBodyBytes+1024) + `"`
	body := `{"job_type":"email.send","payload":` + oversizedPayload + `}`

	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

// TestCreateWorkflow_OversizedBodyRejected413 is the same proof for
// POST /workflows.
func TestCreateWorkflow_OversizedBodyRejected413(t *testing.T) {
	srv, _ := newTestServer(t)

	oversizedPayload := `"` + strings.Repeat("a", api.MaxRequestBodyBytes+1024) + `"`
	body := `{"nodes":[{"node_key":"n1","job_type":"email.send","payload":` + oversizedPayload + `}]}`

	resp, err := http.Post(srv.URL+"/workflows", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

// TestCreateJob_UnknownFieldsAreIgnored_RoundTrip is the exact-scope
// requirement "A passing test demonstrates unknown-field tolerance in both
// directions": an old-shape request (omitting optional fields added by
// later phases) and a new-shape request (an extra, currently-unrecognized
// field) both succeed unchanged, per
// docs/compatibility-policy.md "Additive changes ... do not require a
// version bump."
func TestCreateJob_UnknownFieldsAreIgnored_RoundTrip(t *testing.T) {
	srv, _ := newTestServer(t)

	// Old-shape: only the fields Phase 1 ever had.
	oldShape := `{"job_type":"email.send","payload":{"to":"a@example.com"}}`
	oldResp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(oldShape))
	require.NoError(t, err)
	defer oldResp.Body.Close()
	require.Equal(t, http.StatusCreated, oldResp.StatusCode, "an old-shape request missing newer optional fields must still succeed")

	// New-shape: a field this server does not (yet) recognize at all.
	newShape := `{"job_type":"email.send","payload":{"to":"a@example.com"},"a_future_field_this_server_does_not_know_about":"x"}`
	newResp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(newShape))
	require.NoError(t, err)
	defer newResp.Body.Close()
	require.Equal(t, http.StatusCreated, newResp.StatusCode, "an unrecognized additive field must be ignored, not rejected")

	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(newResp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
}

// TestCreateWorkflow_UnknownFieldsAreIgnored proves the same tolerance for
// POST /workflows.
func TestCreateWorkflow_UnknownFieldsAreIgnored(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"nodes":[{"node_key":"n1","job_type":"email.send","payload":{},"a_future_field":"x"}],"a_future_top_level_field":true}`
	resp, err := http.Post(srv.URL+"/workflows", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

// poisonStore is a minimal api.JobStore fake (no real PostgreSQL involved)
// whose every method returns an error deliberately shaped like a raw
// internal/store/PostgreSQL failure -- a "store: ..." prefix, SQL-looking
// text, a fake SQLSTATE, and a fake connection-string-shaped detail -- so
// TestCreateJob_StoreFailure_Returns500WithoutLeakingInternalDetails can
// prove the HTTP layer never lets any of that reach a response body,
// regardless of what internal/store itself ever puts in an error's text.
type poisonStore struct{}

var errPoisoned = errors.New(`store: insert job: pq: SQLSTATE 23505 duplicate key value violates unique constraint "idx_jobs_idempotency_key" (host=10.0.0.5 user=taskforge password=hunter2)`)

func (poisonStore) InsertIdempotent(context.Context, job.NewParams) (*job.Job, bool, error) {
	return nil, false, errPoisoned
}
func (poisonStore) GetByID(context.Context, uuid.UUID) (*job.Job, error) { return nil, errPoisoned }
func (poisonStore) CancelQueuedOrRetryWait(context.Context, uuid.UUID) (*job.Job, error) {
	return nil, errPoisoned
}
func (poisonStore) RequestCancellation(context.Context, uuid.UUID) (*job.Job, error) {
	return nil, errPoisoned
}
func (poisonStore) CreateWorkflow(context.Context, workflow.GraphSpec) (*workflow.Instance, error) {
	return nil, errPoisoned
}
func (poisonStore) GetWorkflow(context.Context, uuid.UUID) (*workflow.Instance, error) {
	return nil, errPoisoned
}
func (poisonStore) CancelWorkflow(context.Context, uuid.UUID) (*workflow.Instance, error) {
	return nil, errPoisoned
}

// TestCreateJob_StoreFailure_Returns500WithoutLeakingInternalDetails proves
// Phase 11's public-API error-leakage requirement at the HTTP boundary:
// when internal/store.InsertIdempotent fails, CreateJob's response is
// TaskForge's ordinary generic 500 body ("failed to durably persist job"),
// never the underlying error's own text -- which, for a real PostgreSQL
// failure, can contain SQL, a SQLSTATE, a constraint name, or other
// connection-level detail. h.logger still receives the full error (see
// handlers.go's h.logger.Error call); only the HTTP response is
// constrained.
func TestCreateJob_StoreFailure_Returns500WithoutLeakingInternalDetails(t *testing.T) {
	h := api.NewHandlers(poisonStore{}, discardLogger())
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)

	body := `{"job_type":"email.send","payload":{}}`
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	for _, leak := range []string{"SQLSTATE", "23505", "pq:", "store:", "password", "hunter2", "10.0.0.5", "constraint", "idx_jobs_idempotency_key"} {
		require.NotContains(t, string(raw), leak, "response body must not leak internal/store/PostgreSQL detail %q", leak)
	}

	var parsed struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, "failed to durably persist job", parsed.Error, "must be exactly the generic public error, not any part of the underlying error's text")
}

// TestCreateJob_MaxAttempts_LargestRepresentableValue_Accepted proves the
// storage-representability boundary end-to-end through real PostgreSQL:
// math.MaxInt32 (job.MaxRepresentableMaxAttempts, the largest value
// jobs.max_attempts's INTEGER column can hold) is accepted by POST /jobs
// and actually durably stored, round-tripping unchanged through GET
// /jobs/{id}.
func TestCreateJob_MaxAttempts_LargestRepresentableValue_Accepted(t *testing.T) {
	srv, _ := newTestServer(t)

	body := fmt.Sprintf(`{"job_type":"email.send","payload":{},"max_attempts":%d}`, math.MaxInt32)
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created struct {
		ID          string `json:"id"`
		MaxAttempts int    `json:"max_attempts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.Equal(t, math.MaxInt32, created.MaxAttempts)

	getResp, err := http.Get(srv.URL + "/jobs/" + created.ID)
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var fetched struct {
		MaxAttempts int `json:"max_attempts"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&fetched))
	require.Equal(t, math.MaxInt32, fetched.MaxAttempts, "durably stored in PostgreSQL's INTEGER column unchanged")
}

// TestCreateJob_MaxAttempts_FirstUnrepresentableValue_Rejected400NotDBError
// is the exact scenario Phase 11's storage-representability fix exists to
// prevent: math.MaxInt32+1 is a perfectly valid Go int (a naive ">= 1"
// check would have let it through) but cannot be stored in PostgreSQL's
// INTEGER jobs.max_attempts column. This must be an ordinary 400
// validation response -- never an internal 500 caused by the INSERT
// itself failing against PostgreSQL.
func TestCreateJob_MaxAttempts_FirstUnrepresentableValue_Rejected400NotDBError(t *testing.T) {
	srv, _ := newTestServer(t)

	body := fmt.Sprintf(`{"job_type":"email.send","payload":{},"max_attempts":%d}`, int64(math.MaxInt32)+1)
	resp, err := http.Post(srv.URL+"/jobs", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "must be a validation 400, not a database-triggered 500")

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(raw), "max_attempts must be at most")
}
