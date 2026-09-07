// Phase 6 HTTP-boundary tests: scheduled_at on POST /jobs
// (docs/scheduling.md) and POST /jobs/{id}/cancel
// (docs/worker-protocol.md's documented contract, TF-INV-010).
package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type phase6Job struct {
	ID                string     `json:"id"`
	State             string     `json:"state"`
	ScheduledAt       *time.Time `json:"scheduled_at"`
	EligibleAt        time.Time  `json:"eligible_at"`
	CancelRequested   bool       `json:"cancel_requested"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at"`
	TerminalAt        *time.Time `json:"terminal_at"`
}

func decodePhase6Job(t *testing.T, resp *http.Response) phase6Job {
	t.Helper()
	defer resp.Body.Close()
	var out phase6Job
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// ---------------------------------------------------------------------
// Scheduling (docs/scheduling.md)
// ---------------------------------------------------------------------

// TestCreateJob_ScheduledAt_DurablyRecordedAndNotImmediatelyEligible
// proves the HTTP-boundary version of TF-INV-011: submitting a job with
// a future scheduled_at durably records it, and the job is not returned
// by a claim (proven indirectly here via GET reflecting the still-QUEUED
// state with eligible_at in the future -- the actual claim-rejection
// mechanism is proven at the store level).
func TestCreateJob_ScheduledAt_DurablyRecordedAndNotImmediatelyEligible(t *testing.T) {
	srv, _ := newTestServer(t)

	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	body := `{"job_type":"test.http.sched","payload":{},"scheduled_at":"` + future.Format(time.RFC3339) + `"}`
	resp := postJob(t, srv, body, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodePhase6Job(t, resp)
	require.Equal(t, "QUEUED", created.State)
	require.NotNil(t, created.ScheduledAt)
	require.WithinDuration(t, future, *created.ScheduledAt, time.Second)
	require.WithinDuration(t, future, created.EligibleAt, time.Second)

	getResp, err := http.Get(srv.URL + "/jobs/" + created.ID)
	require.NoError(t, err)
	fetched := decodePhase6Job(t, getResp)
	require.Equal(t, "QUEUED", fetched.State)
	require.NotNil(t, fetched.ScheduledAt)
}

// TestCreateJob_NoScheduledAt_ImmediatelyEligible proves the unchanged
// default: omitting scheduled_at behaves exactly as every Phase 1-5
// submission always did.
func TestCreateJob_NoScheduledAt_ImmediatelyEligible(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := postJob(t, srv, `{"job_type":"test.http.sched.none","payload":{}}`, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodePhase6Job(t, resp)
	require.Nil(t, created.ScheduledAt)
	require.False(t, created.EligibleAt.After(time.Now().Add(time.Second)))
}

// TestCreateJob_MalformedScheduledAt_Returns400 proves validation: a
// scheduled_at value that is not a valid RFC 3339 timestamp is rejected
// before any database call, per encoding/json's automatic time.Time
// parsing failing inside CreateJob's initial decode step.
func TestCreateJob_MalformedScheduledAt_Returns400(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := postJob(t, srv, `{"job_type":"test.http.sched.bad","payload":{},"scheduled_at":"not-a-timestamp"}`, "")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---------------------------------------------------------------------
// Cancellation (docs/worker-protocol.md POST /jobs/{id}/cancel)
// ---------------------------------------------------------------------

func postCancel(t *testing.T, srvURL, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/jobs/"+id+"/cancel", bytes.NewReader(nil))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestCancelJob_Queued_TransitionsDirectlyToCancelled is SF-011 at the
// HTTP boundary.
func TestCancelJob_Queued_TransitionsDirectlyToCancelled(t *testing.T) {
	srv, _ := newTestServer(t)

	created := decodePhase6Job(t, postJob(t, srv, `{"job_type":"test.http.cancel.queued","payload":{}}`, ""))

	resp := postCancel(t, srv.URL, created.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cancelled := decodePhase6Job(t, resp)
	require.Equal(t, "CANCELLED", cancelled.State)
	require.NotNil(t, cancelled.TerminalAt)
}

// TestCancelJob_Running_RequestsCancellationWithoutChangingState proves
// the documented "not yet confirmed" response shape: cancelling a
// RUNNING job returns state=RUNNING with cancel_requested=true, not
// CANCELLED -- the caller must poll GET /jobs/{id} for the eventual
// outcome.
func TestCancelJob_Running_RequestsCancellationWithoutChangingState(t *testing.T) {
	srv, s := newTestServer(t)

	created := decodePhase6Job(t, postJob(t, srv, `{"job_type":"test.http.cancel.running","payload":{}}`, ""))
	_, ok, err := s.Claim(t.Context(), "worker-1")
	require.NoError(t, err)
	require.True(t, ok)

	resp := postCancel(t, srv.URL, created.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := decodePhase6Job(t, resp)
	require.Equal(t, "RUNNING", result.State, `cancelling a RUNNING job must not itself flip state to CANCELLED -- only the worker's own acknowledgement does`)
	require.True(t, result.CancelRequested)
	require.NotNil(t, result.CancelRequestedAt)
}

// TestCancelJob_AlreadyTerminal_IsIdempotentNoOp proves cancelling an
// already-SUCCEEDED job is not an error -- it reports the job's actual
// terminal state, per docs/worker-protocol.md: "If the job is already
// terminal: no-op, response indicates the job's actual terminal state."
func TestCancelJob_AlreadyTerminal_IsIdempotentNoOp(t *testing.T) {
	srv, s := newTestServer(t)

	created := decodePhase6Job(t, postJob(t, srv, `{"job_type":"test.http.cancel.terminal","payload":{}}`, ""))
	claimed, ok, err := s.Claim(t.Context(), "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(t.Context(), claimed.ID, "worker-1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	resp := postCancel(t, srv.URL, created.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := decodePhase6Job(t, resp)
	require.Equal(t, "SUCCEEDED", result.State, "cancelling an already-terminal job must report reality, not an error, and must never reopen it")
}

// TestCancelJob_DuplicateOnQueued_ReportsCancelledEachTime proves
// duplicate cancellation of the same job is idempotent end to end: the
// first call cancels it, and a second call is a no-op reporting the same
// CANCELLED state, never an error.
func TestCancelJob_DuplicateOnQueued_ReportsCancelledEachTime(t *testing.T) {
	srv, _ := newTestServer(t)

	created := decodePhase6Job(t, postJob(t, srv, `{"job_type":"test.http.cancel.duplicate","payload":{}}`, ""))

	first := decodePhase6Job(t, postCancel(t, srv.URL, created.ID))
	require.Equal(t, "CANCELLED", first.State)

	resp := postCancel(t, srv.URL, created.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	second := decodePhase6Job(t, resp)
	require.Equal(t, "CANCELLED", second.State)
}

// TestCancelJob_MissingJob_Returns404 proves cancellation of a
// nonexistent job id correctly falls through all three store-layer
// attempts to a final 404, per this handler's documented cascade.
func TestCancelJob_MissingJob_Returns404(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := postCancel(t, srv.URL, "00000000-0000-0000-0000-000000000000")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestCancelJob_MalformedID_Returns400 mirrors TestGetJob_MalformedIDReturns400
// for the cancel endpoint.
func TestCancelJob_MalformedID_Returns400(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := postCancel(t, srv.URL, "not-a-uuid")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestCreateJob_IdempotencyKey_DuplicateAfterCancellation_ReturnsExistingJob
// is the HTTP-boundary companion to the store-level
// TestInsertIdempotent_DuplicateSubmissionAfterCancellation_ReturnsExistingJob:
// resubmitting the same Idempotency-Key after the mapped job was
// cancelled returns that same (still-cancelled) job, never a new one.
func TestCreateJob_IdempotencyKey_DuplicateAfterCancellation_ReturnsExistingJob(t *testing.T) {
	srv, _ := newTestServer(t)

	first := decodePhase6Job(t, postJob(t, srv, `{"job_type":"test.http.cancel.idem","payload":{}}`, "http-cancel-idem-key"))
	cancelResp := postCancel(t, srv.URL, first.ID)
	require.Equal(t, http.StatusOK, cancelResp.StatusCode)
	require.Equal(t, "CANCELLED", decodePhase6Job(t, cancelResp).State)

	dup := decodePhase6Job(t, postJob(t, srv, `{"job_type":"test.http.cancel.idem","payload":{}}`, "http-cancel-idem-key"))
	require.Equal(t, first.ID, dup.ID)
	require.Equal(t, "CANCELLED", dup.State)
}
