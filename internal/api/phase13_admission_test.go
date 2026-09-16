// Phase 13 (checkpoint 5): submission-admission proofs -- SF-039 (429
// only on a crossed rate/admission threshold, never at ordinary
// concurrency-cap saturation, which this layer never even checks),
// SF-040 (503 only on configured system capacity, retry-after-Retry-After
// succeeds), and SF-048 (neither response body discloses tenant-specific
// detail). Real HTTP handlers, real store, real governance store, against
// real PostgreSQL.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/governance"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

func postJobTo(t *testing.T, srv *httptest.Server, ident testIdentity, body map[string]any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/jobs", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Authorization", ident.AuthHeader())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestAdmission_SF039_RateLimitOnlyFiresOnceThresholdCrossed proves the
// roadmap's own required rule: an ordinary submission to an unconfigured
// queue is never rate-limited (no threshold exists to cross), and once a
// queue's static rate limit IS configured, exceeding its burst produces
// exactly 429 with Retry-After, never earlier.
func TestAdmission_SF039_RateLimitOnlyFiresOnceThresholdCrossed(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	g := governance.New(db)
	ps := newPrincipalStore(t, db)
	ident := newIdentity(t, ps, principal.KindCaller, "sf039", principal.ScopeJobs)

	h := api.NewHandlers(s, discardLogger(), api.WithAuthenticator(ps), api.WithGovernance(g))
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)

	// Unconfigured queue: no threshold exists, so many submissions in a
	// row must all succeed.
	for i := 0; i < 10; i++ {
		resp := postJobTo(t, srv, ident, map[string]any{"job_type": "test.sf039.unlimited", "queue_name": "unconfigured"})
		require.Equal(t, http.StatusCreated, resp.StatusCode, "an unconfigured queue must never be rate-limited")
		resp.Body.Close()
	}

	// Configure a tight burst on a different queue. rate=1/s (not a large
	// number) so the negligible wall-clock time between these sequential
	// HTTP requests cannot refill a meaningful fraction of a token --
	// matching internal/governance's own proven test scale for the same
	// reason.
	require.NoError(t, g.SetRateLimit(context.Background(), "limited", 1, 2))
	for i := 0; i < 2; i++ {
		resp := postJobTo(t, srv, ident, map[string]any{"job_type": "test.sf039.limited", "queue_name": "limited"})
		require.Equal(t, http.StatusCreated, resp.StatusCode, "requests within burst must be admitted")
		resp.Body.Close()
	}
	resp := postJobTo(t, srv, ident, map[string]any{"job_type": "test.sf039.limited", "queue_name": "limited"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "the request beyond burst must be rejected with 429")
	require.NotEmpty(t, resp.Header.Get("Retry-After"))
}

// TestAdmission_SF040_SystemCapacity503_RetryAfterSucceeds proves the
// system-capacity bound: with TASKFORGE_MAX_INFLIGHT_SUBMISSIONS-style
// bound at 1, a second concurrent in-flight submission is rejected 503
// with Retry-After, and retrying once the first has completed succeeds.
func TestAdmission_SF040_SystemCapacity503_RetryAfterSucceeds(t *testing.T) {
	db := testutil.DB(t)
	real := store.New(db)
	ps := newPrincipalStore(t, db)
	ident := newIdentity(t, ps, principal.KindCaller, "sf040", principal.ScopeJobs)

	block := &blockingJobStore{real: real, release: make(chan struct{}), entered: make(chan struct{}, 1)}
	h := api.NewHandlers(block, discardLogger(), api.WithAuthenticator(ps), api.WithMaxInflightSubmissions(1))
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)

	firstDone := make(chan *http.Response, 1)
	go func() {
		resp := postJobTo(t, srv, ident, map[string]any{"job_type": "test.sf040.first"})
		firstDone <- resp
	}()

	select {
	case <-block.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never entered the handler")
	}

	// The first request now holds the one available in-flight slot.
	resp := postJobTo(t, srv, ident, map[string]any{"job_type": "test.sf040.second"})
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a second concurrent submission beyond the configured bound must be rejected 503")
	require.NotEmpty(t, resp.Header.Get("Retry-After"))
	resp.Body.Close()

	close(block.release)
	first := <-firstDone
	require.Equal(t, http.StatusCreated, first.StatusCode, "the first, legitimately-admitted request must still succeed")
	first.Body.Close()

	// Retrying after the slot has freed must succeed.
	resp = postJobTo(t, srv, ident, map[string]any{"job_type": "test.sf040.retry"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, "retrying after Retry-After (the slot having freed) must succeed")
	resp.Body.Close()
}

// TestAdmission_ConcurrencyCapSaturation_NeverTriggers429Or503 is the
// roadmap's explicit negative case: a queue merely at its configured
// concurrency cap (no admission/rate threshold crossed -- concurrency is
// enforced only at claim time, never at submission time) must not itself
// trigger either backpressure code. Submission always succeeds regardless
// of how saturated the queue's claim-time capacity is.
func TestAdmission_ConcurrencyCapSaturation_NeverTriggers429Or503(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	g := governance.New(db)
	ps := newPrincipalStore(t, db)
	ident := newIdentity(t, ps, principal.KindCaller, "concurrency-saturation", principal.ScopeJobs)

	// A fully-saturated concurrency-limited queue: capacity 1, already
	// held.
	require.NoError(t, g.SetConcurrencyLimit(context.Background(), "saturated", intPtr(1)))
	_, err := s.Insert(context.Background(), job.NewParams{
		PrincipalID: ident.PrincipalID, JobType: "test.saturated.holder", Payload: []byte(`{}`),
		MaxAttempts: 5, ExecutionTimeoutSeconds: 30, QueueName: "saturated",
	})
	require.NoError(t, err)
	_, ok, err := s.Claim(context.Background(), "holder")
	require.NoError(t, err)
	require.True(t, ok, "test setup: the queue's one slot must now be held")

	h := api.NewHandlers(s, discardLogger(), api.WithAuthenticator(ps), api.WithGovernance(g))
	srv := httptest.NewServer(api.NewRouter(h))
	t.Cleanup(srv.Close)

	resp := postJobTo(t, srv, ident, map[string]any{"job_type": "test.saturated.new", "queue_name": "saturated"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode,
		"submitting to a queue already at its concurrency cap must still succeed -- the job enqueues and waits, never a rejection")
}

// TestAdmission_SF048_RejectionBodiesDiscloseNothingTenantSpecific proves
// the disclosure discipline: a 429 body reveals neither the queue's
// configured rate/burst nor any other tenant's state, and a 503 body
// carries no principal-scoped detail at all -- both bodies are the
// generic admission-rejection shape, indistinguishable from each other
// except by status code.
func TestAdmission_SF048_RejectionBodiesDiscloseNothingTenantSpecific(t *testing.T) {
	db := testutil.DB(t)
	real := store.New(db)
	g := governance.New(db)
	ps := newPrincipalStore(t, db)
	ident := newIdentity(t, ps, principal.KindCaller, "sf048", principal.ScopeJobs)

	require.NoError(t, g.SetRateLimit(context.Background(), "disclosure-test", 1, 1))
	h429 := api.NewHandlers(real, discardLogger(), api.WithAuthenticator(ps), api.WithGovernance(g))
	srv429 := httptest.NewServer(api.NewRouter(h429))
	t.Cleanup(srv429.Close)

	resp := postJobTo(t, srv429, ident, map[string]any{"job_type": "test.sf048.a", "queue_name": "disclosure-test"})
	resp.Body.Close()
	resp = postJobTo(t, srv429, ident, map[string]any{"job_type": "test.sf048.b", "queue_name": "disclosure-test"})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	body429, err := decodeErrorBody(resp)
	require.NoError(t, err)
	resp.Body.Close()

	require.NotContains(t, body429, "1", "the 429 body must not leak the configured rate (1/s) or burst (1)")
	require.NotContains(t, body429, "disclosure-test", "the 429 body must not name the queue")
	require.NotContains(t, body429, "rate_limited", "the specific server-side reason is diagnostic detail (metrics/logs only), not exposed in the caller-facing body")

	block := &blockingJobStore{real: real, release: make(chan struct{}), entered: make(chan struct{}, 1)}
	h503 := api.NewHandlers(block, discardLogger(), api.WithAuthenticator(ps), api.WithMaxInflightSubmissions(1))
	srv503 := httptest.NewServer(api.NewRouter(h503))
	t.Cleanup(srv503.Close)

	go func() { postJobTo(t, srv503, ident, map[string]any{"job_type": "test.sf048.c"}) }()
	<-block.entered
	defer close(block.release)

	resp = postJobTo(t, srv503, ident, map[string]any{"job_type": "test.sf048.d"})
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body503, err := decodeErrorBody(resp)
	require.NoError(t, err)
	resp.Body.Close()

	require.Equal(t, body429, body503, "both backpressure codes must share the identical, generic response body shape -- a caller must not be able to distinguish rate-limiting from system capacity by body content, only by status code")
}

func decodeErrorBody(resp *http.Response) (string, error) {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Error, nil
}

func intPtr(n int) *int { return &n }

// blockingJobStore wraps a real *store.Store, blocking exactly the FIRST
// InsertIdempotent call until release is closed -- deterministic control
// over "an in-flight submission handler," without a sleep, for the
// system-capacity (503) tests above.
type blockingJobStore struct {
	real    *store.Store
	release chan struct{}
	entered chan struct{}
}

func (b *blockingJobStore) InsertIdempotent(ctx context.Context, p job.NewParams) (*job.Job, bool, error) {
	select {
	case b.entered <- struct{}{}:
		<-b.release
	default:
	}
	return b.real.InsertIdempotent(ctx, p)
}

func (b *blockingJobStore) GetByID(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*job.Job, error) {
	return b.real.GetByID(ctx, id, authz)
}

func (b *blockingJobStore) CancelQueuedOrRetryWait(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*job.Job, error) {
	return b.real.CancelQueuedOrRetryWait(ctx, id, authz)
}

func (b *blockingJobStore) RequestCancellation(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*job.Job, error) {
	return b.real.RequestCancellation(ctx, id, authz)
}

func (b *blockingJobStore) CreateWorkflow(ctx context.Context, g workflow.GraphSpec) (*workflow.Instance, error) {
	return b.real.CreateWorkflow(ctx, g)
}

func (b *blockingJobStore) GetWorkflow(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*workflow.Instance, error) {
	return b.real.GetWorkflow(ctx, id, authz)
}

func (b *blockingJobStore) CancelWorkflow(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*workflow.Instance, error) {
	return b.real.CancelWorkflow(ctx, id, authz)
}
