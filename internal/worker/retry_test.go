// Phase 3 worker-loop-level retry tests: the full
// claim-execute-classify-retry/dead-letter cycle through worker.RunOnce
// (not just internal/store calls directly -- see internal/store/retry_test.go
// for the store-level proofs). Timing is controlled via
// forceExpireLease-style deterministic DB manipulation and via a small
// injected retry.Config, never a real wait for the backoff window to
// elapse blindly.
package worker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler/testdoubles"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/retry"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// forceSetEligibleAtWT mirrors internal/store's test-only
// forceSetEligibleAt helper (unexported to that package), for the same
// reason forceExpireLeaseWT duplicates forceExpireLease in worker_test.go's
// lease_test.go.
func forceSetEligibleAtWT(t *testing.T, db *sql.DB, jobID uuid.UUID, delta time.Duration) {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `
		UPDATE jobs SET eligible_at = now() + $2 * interval '1 second'
		WHERE id = $1 AND state = 'RETRY_WAIT'`, jobID, delta.Seconds())
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "forceSetEligibleAtWT: job %s was not RETRY_WAIT", jobID)
}

// TestRunOnce_RetryableFailure_TransitionsToRetryWait proves the worker
// loop, not just the store, correctly routes a handler.Retryable failure
// to CompleteRetryableFailure -- the job ends up RETRY_WAIT, not
// DEAD_LETTERED, with attempts remaining.
func TestRunOnce_RetryableFailure_TransitionsToRetryWait(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.retry.wait",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.retry.wait", testdoubles.NewRetryableFail(errors.New("transient")))
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, final.State)
	require.Equal(t, 1, final.AttemptCount)
	require.Nil(t, final.TerminalAt)
	require.NotNil(t, final.LastError)
	require.Equal(t, "transient", *final.LastError)
}

// TestRunOnce_PermanentFailure_DeadLettersImmediately proves a
// handler.Permanent failure dead-letters on the very first attempt
// regardless of max_attempts, consuming no further retry budget.
func TestRunOnce_PermanentFailure_DeadLettersImmediately(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.permanent",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.permanent", testdoubles.NewPermanentFail(errors.New("bad payload")))
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, final.State)
	require.Equal(t, 1, final.AttemptCount, "a permanent failure must not consume more than the one attempt it actually made")
	require.NotNil(t, final.LastErrorClass)
	require.Equal(t, job.ErrorClassPermanent, *final.LastErrorClass)
}

// TestRunOnce_UnclassifiedFailure_DefaultsToPermanent proves
// handler.Classify's documented default (an error not wrapped via
// Retryable/Permanent is treated as Permanent) holds at the worker-loop
// level too -- this is also what keeps the pre-Phase-3 AlwaysFail test
// double's behavior in TestRunOnce_FailurePath (worker_test.go) unchanged.
func TestRunOnce_UnclassifiedFailure_DefaultsToPermanent(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.unclassified",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.unclassified", testdoubles.NewAlwaysFail(errors.New("unclassified error")))
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, final.State, "an unclassified handler error must default to permanent, not retry forever")
}

// TestRunOnce_SF009_RetryableFailureEventuallySucceeds is SF-009 at the
// worker-loop level: a handler that fails retryably twice then succeeds,
// driven through two full RunOnce cycles (claim -> fail -> RETRY_WAIT,
// then claim -> succeed), with eligible_at fast-forwarded deterministically
// between cycles rather than waiting out the real backoff window.
func TestRunOnce_SF009_RetryableFailureEventuallySucceeds(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.sf009.worker",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	flaky := testdoubles.NewFlakyThenSucceed(2, errors.New("transient"))
	registry := handler.NewRegistry()
	registry.Register("test.sf009.worker", flaky)
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	for i := 0; i < 2; i++ {
		claimed, err := w.RunOnce(ctx)
		require.NoError(t, err)
		require.True(t, claimed)

		mid, err := s.GetByID(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, jobstate.RetryWait, mid.State)
		forceSetEligibleAtWT(t, db, created.ID, -time.Second)
	}

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
	require.Equal(t, 3, final.AttemptCount)
}

// TestRunOnce_SF010_RetriesExhaustedDeadLetters is SF-010 at the
// worker-loop level: max_attempts=3, every attempt fails retryably; the
// job dead-letters after the 3rd with last_error from that final attempt.
func TestRunOnce_SF010_RetriesExhaustedDeadLetters(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.sf010.worker",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             3,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.sf010.worker", testdoubles.NewRetryableFail(errors.New("sustained failure")))
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	for attempt := 1; attempt <= 3; attempt++ {
		claimed, err := w.RunOnce(ctx)
		require.NoError(t, err)
		require.True(t, claimed)

		mid, err := s.GetByID(ctx, created.ID)
		require.NoError(t, err)
		if attempt < 3 {
			require.Equal(t, jobstate.RetryWait, mid.State)
			forceSetEligibleAtWT(t, db, created.ID, -time.Second)
		} else {
			require.Equal(t, jobstate.DeadLettered, mid.State)
			require.NotNil(t, mid.LastError)
			require.Equal(t, "sustained failure", *mid.LastError)
		}
	}

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.False(t, claimed, "an exhausted, dead-lettered job must never be claimed again")
}

// TestRunOnce_RetryableFailureRejectedAfterLeaseLoss is the worker-loop
// analogue of TestCompleteRetryableFailure_RejectedAfterReclaim: a
// handler's retryable failure is reported after this worker's lease was
// already reclaimed by another worker, so it must be silently discarded
// (no error surfaced, no state change), never override the reclaiming
// worker's outcome.
func TestRunOnce_RetryableFailureRejectedAfterLeaseLoss(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.retry.lease.lost",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 1, // ~333ms heartbeat interval
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})
	registry := handler.NewRegistry()
	registry.Register("test.retry.lease.lost", testdoubles.Gated{
		Started: started,
		Proceed: proceed,
		Err:     handler.Retryable(errors.New("would have retried")),
	})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	runDone := make(chan struct{})
	var claimed bool
	var runErr error
	go func() {
		defer close(runDone)
		claimed, runErr = w.RunOnce(ctx)
	}()

	<-started

	forceExpireLeaseWT(t, db, created.ID)
	reclaimed, ok, err := s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)

	time.Sleep(500 * time.Millisecond) // let worker-1's heartbeat loop notice
	close(proceed)

	<-runDone
	require.NoError(t, runErr, "RunOnce must not surface an error just because it lost its lease")
	require.True(t, claimed)

	// worker-2 legitimately completes the job it now owns.
	completed, err := s.CompleteSuccess(ctx, reclaimed.ID, "worker-2", reclaimed.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State, "worker-1's stale retryable-failure report must not have altered the job")
	require.Equal(t, int64(2), final.LeaseGeneration)
}

// TestRunOnce_RetryPath_HeartbeatGoroutineDoesNotLeak extends
// TestRunOnce_HeartbeatGoroutineDoesNotLeak (worker_test.go) to the retry
// path: a retryable failure must still result in the heartbeat
// goroutine/ticker exiting deterministically before RunOnce returns.
func TestRunOnce_RetryPath_HeartbeatGoroutineDoesNotLeak(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	_, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.retry.goroutine.leak",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.retry.goroutine.leak", testdoubles.NewRetryableFail(errors.New("boom")))
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	before := runtime.NumGoroutine()
	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	after := pollGoroutineCount(t, before, 2*time.Second)
	require.LessOrEqual(t, after, before, "RunOnce must not leave a heartbeat goroutine running after a retryable failure")
}

// TestRunOnce_CustomRetryConfig_UsesFastBackoff proves SetRetryConfig
// actually takes effect: with a tiny base delay, a retryable failure's
// eligible_at lands well within a couple of seconds of now, not the 1s-300s
// production defaults -- letting fast-but-real (not DB-time-manipulated)
// retry-then-succeed tests run quickly and deterministically bounded.
func TestRunOnce_CustomRetryConfig_UsesFastBackoff(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.retry.fastconfig",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.retry.fastconfig", testdoubles.NewRetryableFail(errors.New("boom")))
	w := worker.New("worker-1", s, registry, 0, discardLogger())
	w.SetRetryConfig(retry.Config{BaseDelay: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond})

	before := time.Now()
	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, final.State)
	require.True(t, final.EligibleAt.Before(before.Add(5*time.Second)),
		"custom fast retry config must produce a near-immediate eligible_at, not the production 1s-300s default")
}
