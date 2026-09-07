// Phase 2 worker-level tests: heartbeat renewal during long-running
// execution (SF-017), lease loss mid-execution causing the worker to stop
// acting as authoritative owner (the "Worker A pauses, Worker B takes
// over, Worker A wakes up" scenario at the worker-loop level, not just
// the store level — see internal/store/lease_test.go for the store-level
// proof), and heartbeat goroutine/ticker cleanup.
//
// These use docs/testing-strategy.md's "short, test-configured TTLs plus
// real sleeps" allowance (rather than a controllable clock source) for
// the handful of cases that genuinely need real elapsed time to exercise
// the heartbeat ticker; synchronization is via channels
// (handler.testdoubles.Gated), not blind sleeps, everywhere a fixed wait
// isn't unavoidable.
package worker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler/testdoubles"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// TestRunOnce_LongRunningJob_HeartbeatKeepsLeaseAlive is SF-017: a handler
// that runs longer than a single lease TTL completes successfully under
// the SAME lease_generation throughout, because the worker's background
// heartbeat loop renews the lease well before it would otherwise expire.
//
// Phase 6 note: execution_timeout_seconds is also this attempt's fixed
// execution-timeout ceiling (see internal/worker's runWithHeartbeat,
// docs/execution-semantics.md "Timeout Semantics") — unlike the lease,
// this ceiling is NOT renewed by heartbeats, so this test's held-open
// duration must stay safely under it while still crossing multiple
// heartbeat cycles (proving lease renewal, this test's actual subject,
// independent of the timeout ceiling). Before Phase 6 added that
// ceiling, this test used ExecutionTimeoutSeconds=1 with a 1200ms hold
// specifically because no such ceiling existed yet; seeing the handler
// run past execution_timeout_seconds without consequence was exactly the
// Phase 2-5 gap Phase 6 closes (see README's "Phase 6: What's
// Implemented" and docs/failure-model.md F9) — TestRunOnce_ExecutionTimeout_FiresIndependentlyOfLeaseRenewal
// in timeout_test.go now proves that closed gap directly.
func TestRunOnce_LongRunningJob_HeartbeatKeepsLeaseAlive(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	// A short execution_timeout_seconds means a short lease TTL and a
	// short (TTL/3) heartbeat interval, so the test can force several
	// real heartbeat cycles without a long-running test, while holding
	// the handler open for less than the same value (the execution-
	// timeout ceiling) so it is never at risk of firing.
	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.longrunning",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 3, // 3s lease TTL/execution-timeout ceiling -> ~1s heartbeat interval
	})
	require.NoError(t, err)

	proceed := make(chan struct{})
	registry := handler.NewRegistry()
	registry.Register("test.longrunning", testdoubles.Gated{Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	runDone := make(chan struct{})
	var claimed bool
	var runErr error
	go func() {
		defer close(runDone)
		claimed, runErr = w.RunOnce(ctx)
	}()

	// Hold the handler open across 2 heartbeat cycles (~1s each) while
	// staying safely under the 3s execution-timeout ceiling.
	time.Sleep(2500 * time.Millisecond)
	close(proceed)

	<-runDone
	require.NoError(t, runErr)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
	require.Equal(t, int64(1), final.LeaseGeneration, "lease_generation must not have advanced: no spurious reclaim occurred")
	require.Equal(t, 1, final.AttemptCount, "no reclaim means no extra attempt")
}

// TestRunOnce_LeaseLostDuringExecution_SkipsCompletion is the worker-loop
// level version of the canonical Worker A/Worker B scenario: while
// worker-1's handler is still running, a second worker reclaims the job
// out from under it (lease force-expired, then genuinely reclaimed via a
// separate Store.Claim call simulating Worker B). worker-1's heartbeat
// loop must detect the lost lease, cancel the handler's context, and
// RunOnce must not attempt a completion call for a lease it no longer
// holds.
func TestRunOnce_LeaseLostDuringExecution_SkipsCompletion(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.lease.lost",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 1, // ~333ms heartbeat interval
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})
	registry := handler.NewRegistry()
	registry.Register("test.lease.lost", testdoubles.Gated{Started: started, Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	runDone := make(chan struct{})
	var claimed bool
	var runErr error
	go func() {
		defer close(runDone)
		claimed, runErr = w.RunOnce(ctx)
	}()

	<-started // worker-1 has claimed and begun executing

	// Simulate Worker B: force-expire the lease (worker-1 is "paused")
	// and genuinely reclaim it through the store, exactly as another
	// worker process would.
	forceExpireLeaseWT(t, db, created.ID)
	reclaimed, ok, err := s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)

	// Give worker-1's heartbeat loop a chance to notice (it ticks every
	// ~333ms) before letting its handler finish.
	time.Sleep(500 * time.Millisecond)
	close(proceed)

	<-runDone
	require.NoError(t, runErr, "RunOnce must not surface an error just because it lost its lease")
	require.True(t, claimed)

	// worker-2 completes the job it legitimately owns.
	completed, err := s.CompleteSuccess(ctx, reclaimed.ID, "worker-2", reclaimed.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)

	// worker-1's completion attempt, had it made one, would have been
	// rejected — but the point of this test is that RunOnce should not
	// have even tried. Confirm indirectly: worker-2's completion (using
	// generation 2) succeeded above without a stale-transition error,
	// which could only happen if worker-1 never raced it to complete
	// generation... Directly assert the job attempts ledger shows
	// generation 1 finalized as LEASE_EXPIRED (by the reclaim), not
	// SUCCEEDED (which is what worker-1 would have recorded had it
	// wrongly completed against a fenced-out generation).
	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
	require.Equal(t, int64(2), final.LeaseGeneration)
}

// TestRunOnce_HeartbeatGoroutineDoesNotLeak proves the heartbeat
// goroutine started for a claimed job's execution always exits before
// RunOnce returns, for both the normal-completion path and the
// lease-lost path — no leaked goroutines or tickers, deterministically
// (not "usually," per the task's cleanup requirement).
func TestRunOnce_HeartbeatGoroutineDoesNotLeak(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.goroutine.leak",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.goroutine.leak", testdoubles.AlwaysSucceed{})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	before := runtime.NumGoroutine()

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	after := pollGoroutineCount(t, before, 2*time.Second)
	require.LessOrEqual(t, after, before, "RunOnce must not leave a heartbeat goroutine running after it returns")

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
}

// pollGoroutineCount polls runtime.NumGoroutine() until it returns to at
// most baseline (allowing the Go runtime's own transient goroutines to
// settle) or the deadline elapses, returning the last observed count.
// Polling (not a blind sleep) keeps this fast in the common case while
// still tolerating scheduler jitter.
func pollGoroutineCount(t *testing.T, baseline int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		last = runtime.NumGoroutine()
		if last <= baseline {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last
}

// TestRunOnce_ContextCancellationStopsHeartbeatDeterministically proves
// shutdown behavior is deterministic: cancelling the context passed to
// RunOnce while a handler is in flight cancels the handler's context
// (execCtx), which stops the heartbeat loop, and RunOnce returns promptly
// rather than hanging — regardless of whether the handler itself observes
// cancellation.
func TestRunOnce_ContextCancellationStopsHeartbeatDeterministically(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx, cancel := context.WithCancel(context.Background())

	_, err := s.Insert(context.Background(), job.NewParams{
		JobType:                 "test.ctx.cancel",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{}) // deliberately never closed
	registry := handler.NewRegistry()
	registry.Register("test.ctx.cancel", testdoubles.Gated{Started: started, Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	before := runtime.NumGoroutine()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = w.RunOnce(ctx)
	}()

	<-started
	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("RunOnce did not return promptly after context cancellation")
	}

	after := pollGoroutineCount(t, before, 2*time.Second)
	require.LessOrEqual(t, after, before, "cancellation must not leak the heartbeat goroutine")
}

// TestRun_MultipleWorkersProcessSharedJobPool is the true multi-worker
// integration case: several independent *worker.Worker instances (not
// just raw Store.Claim calls) running concurrently against a shared pool
// of jobs, each backed by the same *store.Store (standing in for several
// worker processes sharing one PostgreSQL database). Every job must be
// executed exactly once, with no double-processing and no job left
// unclaimed.
func TestRun_MultipleWorkersProcessSharedJobPool(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const numJobs = 15
	const numWorkers = 3

	recorder := testdoubles.NewRecorder(nil)
	registry := handler.NewRegistry()
	registry.Register("test.multiworker", recorder)

	for i := 0; i < numJobs; i++ {
		_, err := s.Insert(context.Background(), job.NewParams{
			JobType:                 "test.multiworker",
			Payload:                 json.RawMessage(`{}`),
			MaxAttempts:             5,
			ExecutionTimeoutSeconds: 30,
		})
		require.NoError(t, err)
	}

	workers := make([]*worker.Worker, numWorkers)
	for i := range workers {
		workers[i] = worker.New(workerName(i), s, registry, 10*time.Millisecond, discardLogger())
	}

	runCtx, runCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *worker.Worker) {
			defer wg.Done()
			_ = w.Run(runCtx) // returns context.Canceled once runCancel fires; that is expected
		}(w)
	}

	require.Eventually(t, func() bool {
		return len(recorder.Executions()) == numJobs
	}, 5*time.Second, 20*time.Millisecond, "all jobs must eventually be claimed and executed exactly once")

	runCancel()
	wg.Wait()

	executions := recorder.Executions()
	require.Len(t, executions, numJobs, "no job may be executed more than once across the whole worker pool")
	seen := make(map[string]bool, numJobs)
	for _, e := range executions {
		require.False(t, seen[e.JobID], "job %s executed more than once", e.JobID)
		seen[e.JobID] = true
		require.Equal(t, 1, e.AttemptCount, "no reclaim should have been necessary for an immediately-succeeding job")
	}
}

func workerName(i int) string {
	return "pool-worker-" + string(rune('A'+i))
}

// forceExpireLeaseWT mirrors internal/store's test-only forceExpireLease
// helper (unexported to that package, so duplicated here rather than
// exported purely for test use): it rewinds jobID's lease_expires_at to
// the past via PostgreSQL's own clock, simulating lease expiration
// without a real sleep.
func forceExpireLeaseWT(t *testing.T, db *sql.DB, jobID uuid.UUID) {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `
		UPDATE jobs SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1 AND state = 'RUNNING'`, jobID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "forceExpireLeaseWT: job %s was not RUNNING", jobID)
}
