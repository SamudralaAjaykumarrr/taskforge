// Phase 5 ("Concurrency Hardening", docs/roadmap.md) at the worker-loop
// level: extended/stress variants of the multi-worker scenarios already
// proven at small scale in internal/worker/lease_test.go
// (TestRun_MultipleWorkersProcessSharedJobPool) and
// internal/worker/retry_test.go, now exercising tens of real
// *worker.Worker instances against hundreds of jobs with a realistic mix
// of outcomes (success, retry-then-succeed, permanent failure, and
// simulated crash-and-reclaim), plus graceful shutdown mid-flight. This
// re-verifies TF-INV-002, TF-INV-003, TF-INV-004, and TF-INV-014 at the
// level real deployments actually operate at — a pool of Worker instances
// sharing one *store.Store — not just direct Store calls.
package worker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

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

// TestStress_ManyWorkersProcessLargeMixedJobPool is
// TestRun_MultipleWorkersProcessSharedJobPool at Phase 5 scale: tens of
// concurrent *worker.Worker instances against a large shared pool of jobs
// with a realistic mix of job_types — some always succeed, some fail
// retryably and eventually succeed, some fail permanently — proving every
// job reaches exactly the correct terminal state with no double
// processing and no stranded work, under sustained real contention.
func TestStress_ManyWorkersProcessLargeMixedJobPool(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const numWorkers = 20
	const numSucceed = 90
	const numEventuallySucceed = 60
	const numPermanentFail = 50
	numJobs := numSucceed + numEventuallySucceed + numPermanentFail

	succeedRecorder := testdoubles.NewRecorder(testdoubles.AlwaysSucceed{})
	flaky := testdoubles.NewFlakyThenSucceed(2, nil)
	permanent := testdoubles.NewPermanentFail(nil)

	registry := handler.NewRegistry()
	registry.Register("stress.mixed.succeed", succeedRecorder)
	registry.Register("stress.mixed.retry", flaky)
	registry.Register("stress.mixed.permanent", permanent)

	insertN := func(jobType string, n int, maxAttempts int) {
		for i := 0; i < n; i++ {
			_, err := s.Insert(context.Background(), job.NewParams{
				JobType:                 jobType,
				Payload:                 json.RawMessage(`{}`),
				MaxAttempts:             maxAttempts,
				ExecutionTimeoutSeconds: 30,
			})
			require.NoError(t, err)
		}
	}
	insertN("stress.mixed.succeed", numSucceed, 5)
	insertN("stress.mixed.retry", numEventuallySucceed, 5)
	insertN("stress.mixed.permanent", numPermanentFail, 5)

	workers := make([]*worker.Worker, numWorkers)
	for i := range workers {
		w := worker.New(fmt.Sprintf("mixed-pool-worker-%03d", i), s, registry, 5*time.Millisecond, discardLogger())
		w.SetRetryConfig(retry.Config{BaseDelay: time.Millisecond, MaxBackoff: 10 * time.Millisecond})
		workers[i] = w
	}

	runCtx, runCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *worker.Worker) {
			defer wg.Done()
			_ = w.Run(runCtx)
		}(w)
	}

	require.Eventually(t, func() bool {
		var terminalCount int
		row := db.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM jobs WHERE state IN ('SUCCEEDED', 'DEAD_LETTERED')`)
		require.NoError(t, row.Scan(&terminalCount))
		return terminalCount == numJobs
	}, 20*time.Second, 25*time.Millisecond, "every job must eventually reach a terminal state under sustained multi-worker load")

	runCancel()
	wg.Wait()

	// Exactly-once success execution for the always-succeed pool.
	successExecs := succeedRecorder.Executions()
	require.Len(t, successExecs, numSucceed, "no always-succeed job may execute more than once")
	seen := make(map[string]bool, numSucceed)
	for _, e := range successExecs {
		require.False(t, seen[e.JobID], "job %s executed more than once", e.JobID)
		seen[e.JobID] = true
		require.Equal(t, 1, e.AttemptCount)
	}

	assertAllTerminal(t, db, "stress.mixed.succeed", jobstate.Succeeded, numSucceed)
	assertAllTerminal(t, db, "stress.mixed.retry", jobstate.Succeeded, numEventuallySucceed)
	assertAllTerminal(t, db, "stress.mixed.permanent", jobstate.DeadLettered, numPermanentFail)

	// TF-INV-007 at scale: every job's attempt history is gapless and
	// consistent with its final attempt_count, across the whole pool.
	rows, err := db.QueryContext(context.Background(), `SELECT id, attempt_count FROM jobs`)
	require.NoError(t, err)
	defer rows.Close()
	type idCount struct {
		id    string
		count int
	}
	var all []idCount
	for rows.Next() {
		var ic idCount
		require.NoError(t, rows.Scan(&ic.id, &ic.count))
		all = append(all, ic)
	}
	require.NoError(t, rows.Err())
	require.Len(t, all, numJobs)
	for _, ic := range all {
		var attemptRows int
		row := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM job_attempts WHERE job_id = $1`, ic.id)
		require.NoError(t, row.Scan(&attemptRows))
		require.Equal(t, ic.count, attemptRows, "job %s: job_attempts row count must equal attempt_count", ic.id)
	}
}

// assertAllTerminal checks that every job of jobType reached exactly want,
// and that there are exactly numJobs such rows.
func assertAllTerminal(t *testing.T, db *sql.DB, jobType string, want jobstate.State, numJobs int) {
	t.Helper()
	var count int
	row := db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM jobs WHERE job_type = $1 AND state = $2`, jobType, string(want))
	require.NoError(t, row.Scan(&count))
	require.Equal(t, numJobs, count, "job_type %q: expected all %d jobs to reach state %s", jobType, numJobs, want)

	var other int
	row = db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM jobs WHERE job_type = $1 AND state != $2`, jobType, string(want))
	require.NoError(t, row.Scan(&other))
	require.Zero(t, other, "job_type %q: some jobs did not reach the expected terminal state %s", jobType, want)
}

// TestStress_ManyConcurrentLeaseLossRaces_NoStaleAuthoritativeCompletions
// is the worker-loop-level, many-jobs-at-once generalization of
// TestRunOnce_LeaseLostDuringExecution_SkipsCompletion: many jobs are each
// claimed and held open (via testdoubles.Gated) by a first wave of
// workers; every one of those leases is then force-expired and reclaimed
// concurrently by a second wave; only THEN are the first wave's handlers
// released to finish. TF-INV-003/TF-INV-014 require that none of the
// first wave's completions land — every job's final state must be exactly
// what the second wave produced.
func TestStress_ManyConcurrentLeaseLossRaces_NoStaleAuthoritativeCompletions(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numJobs = 15

	type inFlight struct {
		jobID   string
		started chan struct{}
		proceed chan struct{}
		done    chan struct{}
	}
	registry := handler.NewRegistry()
	inFlights := make([]*inFlight, numJobs)
	firstWaveWorkers := make([]*worker.Worker, numJobs)

	for i := 0; i < numJobs; i++ {
		jobType := fmt.Sprintf("stress.leaseloss.%d", i)
		created, err := s.Insert(ctx, job.NewParams{
			JobType:                 jobType,
			Payload:                 json.RawMessage(`{}`),
			MaxAttempts:             5,
			ExecutionTimeoutSeconds: 1, // short lease -> fast heartbeat interval (~333ms)
		})
		require.NoError(t, err)

		f := &inFlight{jobID: created.ID.String(), started: make(chan struct{}, 1), proceed: make(chan struct{}), done: make(chan struct{})}
		inFlights[i] = f
		registry.Register(jobType, testdoubles.Gated{Started: f.started, Proceed: f.proceed})
		firstWaveWorkers[i] = worker.New(fmt.Sprintf("first-wave-%02d", i), s, registry, 0, discardLogger())
	}

	// Wave 1: every worker claims its own job and starts executing
	// (blocked on Gated.Proceed), all concurrently.
	var wg1 sync.WaitGroup
	for i, w := range firstWaveWorkers {
		wg1.Add(1)
		go func(w *worker.Worker, f *inFlight) {
			defer wg1.Done()
			defer close(f.done)
			_, _ = w.RunOnce(ctx)
		}(w, inFlights[i])
	}
	for _, f := range inFlights {
		<-f.started
	}

	// Force-expire every lease at once, then have a second wave of workers
	// concurrently reclaim all of them.
	_, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'RUNNING'`)
	require.NoError(t, err)

	var wg2 sync.WaitGroup
	reclaimResults := make(chan *job.Job, numJobs)
	for i := 0; i < numJobs; i++ {
		wg2.Add(1)
		workerID := i
		go func() {
			defer wg2.Done()
			j, ok, err := s.Claim(ctx, fmt.Sprintf("second-wave-%02d", workerID))
			require.NoError(t, err)
			require.True(t, ok)
			reclaimResults <- j
		}()
	}
	wg2.Wait()
	close(reclaimResults)

	reclaimed := make(map[string]*job.Job, numJobs)
	for j := range reclaimResults {
		reclaimed[j.ID.String()] = j
	}
	require.Len(t, reclaimed, numJobs, "every in-flight job must be reclaimed exactly once by the second wave")
	for _, j := range reclaimed {
		require.Equal(t, int64(2), j.LeaseGeneration)
	}

	// Second wave completes every job it now legitimately owns,
	// concurrently.
	var wg3 sync.WaitGroup
	for _, j := range reclaimed {
		wg3.Add(1)
		j := j
		go func() {
			defer wg3.Done()
			_, err := s.CompleteSuccess(ctx, j.ID, *j.LeaseOwner, j.LeaseGeneration, nil)
			require.NoError(t, err)
		}()
	}
	wg3.Wait()

	// Only now release every first-wave handler to finish, all at once —
	// the maximally adversarial ordering: first-wave completions race
	// against already-settled state.
	for _, f := range inFlights {
		close(f.proceed)
	}
	for _, f := range inFlights {
		<-f.done
	}
	wg1.Wait()

	for _, f := range inFlights {
		id := f.jobID
		var state string
		var leaseGen int64
		row := db.QueryRowContext(ctx, `SELECT state, lease_generation FROM jobs WHERE id = $1`, id)
		require.NoError(t, row.Scan(&state, &leaseGen))
		require.Equal(t, string(jobstate.Succeeded), state, "job %s: first wave's stale completion must never have landed", id)
		require.Equal(t, int64(2), leaseGen, "job %s: generation must be exactly the second wave's, untouched by the first wave", id)
	}
}

// TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority
// covers "worker shutdown while processing" / "worker shutdown while
// heartbeating": a pool of workers is mid-execution on several
// long-running (Gated) jobs when the pool's context is cancelled. No
// heartbeat goroutine may leak, and no job may be left claiming to be
// SUCCEEDED/DEAD_LETTERED by a worker that never got to report an
// outcome — a cancelled-mid-flight job simply stays RUNNING under its
// original lease until it is later reclaimed normally (ADR-0002: a
// non-crashed worker giving up early has no distinct "release" API in v1
// — recovery is uniformly via lease expiry).
func TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const numWorkers = 10

	registry := handler.NewRegistry()
	proceed := make(chan struct{}) // deliberately never closed: shutdown must not depend on the handler ever finishing
	started := make(chan struct{}, numWorkers)
	registry.Register("stress.shutdown", testdoubles.Gated{Started: started, Proceed: proceed})

	jobIDs := make([]string, 0, numWorkers)
	for i := 0; i < numWorkers; i++ {
		created, err := s.Insert(ctx, job.NewParams{
			JobType:                 "stress.shutdown",
			Payload:                 json.RawMessage(`{}`),
			MaxAttempts:             5,
			ExecutionTimeoutSeconds: 30,
		})
		require.NoError(t, err)
		jobIDs = append(jobIDs, created.ID.String())
	}

	workers := make([]*worker.Worker, numWorkers)
	for i := range workers {
		workers[i] = worker.New(fmt.Sprintf("shutdown-worker-%02d", i), s, registry, 5*time.Millisecond, discardLogger())
	}

	before := runtime.NumGoroutine()

	runCtx, runCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *worker.Worker) {
			defer wg.Done()
			_ = w.Run(runCtx)
		}(w)
	}

	for i := 0; i < numWorkers; i++ {
		<-started
	}

	// Every worker is now mid-execution (and heartbeating, since
	// ExecutionTimeoutSeconds=30 gives a ~10s heartbeat interval — well
	// within this test's window is not required, since we only need the
	// heartbeat GOROUTINE to be running, not for a tick to have fired).
	// Shut the whole pool down while all of them are still blocked.
	runCancel()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		wg.Wait()
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("worker pool did not shut down promptly on context cancellation")
	}

	after := pollGoroutineCount(t, before, 3*time.Second)
	require.LessOrEqual(t, after, before, "cancelling the whole pool must not leak any heartbeat goroutine")

	// Every job must still be exactly RUNNING under generation 1 — no
	// worker fabricated a completion it was never authoritative to make
	// just because it was asked to shut down.
	for _, id := range jobIDs {
		var state string
		var leaseGen int64
		row := db.QueryRowContext(ctx, `SELECT state, lease_generation FROM jobs WHERE id = $1`, id)
		require.NoError(t, row.Scan(&state, &leaseGen))
		require.Equal(t, string(jobstate.Running), state, "job %s must remain RUNNING: shutdown must not fabricate an authoritative outcome", id)
		require.Equal(t, int64(1), leaseGen)
	}

	// The stranded-looking jobs are still perfectly recoverable: force
	// their leases to expire and confirm a fresh worker reclaims and
	// completes each one normally, proving shutdown left no permanent
	// damage (TF-INV-004).
	_, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'RUNNING'`)
	require.NoError(t, err)

	recoveryRegistry := handler.NewRegistry()
	recoveryRegistry.Register("stress.shutdown", testdoubles.AlwaysSucceed{})
	recoveryWorker := worker.New("recovery-worker", s, recoveryRegistry, 0, discardLogger())
	for range jobIDs {
		claimed, err := recoveryWorker.RunOnce(context.Background())
		require.NoError(t, err)
		require.True(t, claimed)
	}

	var succeededCount int
	row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE job_type = 'stress.shutdown' AND state = 'SUCCEEDED'`)
	require.NoError(t, row.Scan(&succeededCount))
	require.Equal(t, numWorkers, succeededCount, "every job orphaned by pool shutdown must still be fully recoverable")

	// Release the original Gated handlers so their goroutines can exit
	// cleanly (they are no longer racing anything meaningful at this
	// point — their worker's execCtx was already cancelled by shutdown).
	close(proceed)
}
