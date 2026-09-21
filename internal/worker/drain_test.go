// Phase 14 in-process proofs for the opt-in graceful-drain redesign
// (docs/phase-14-plan.md §6.4, §19 OD-3). These exercise
// internal/worker.Worker directly -- exactly like every other test in
// this package -- because their entire purpose is proving claims about
// the Go package's own API (the default, un-opted-in contract is
// unchanged), not about cmd/worker's process-level behavior. The real,
// separately-compiled cmd/worker OS-process proofs (SF-064, SF-065,
// SF-069) live under test/procs, since they need to send real signals to
// a real binary.
package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler/testdoubles"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
)

// TestRunOnce_SF064a_DefaultWorkerCancelsExecutionImmediately is SF-064a
// (docs/phase-14-plan.md §6.4 "Required regression proof" item 2, §16):
// a *Worker that never calls SetDrainTimeout -- the state every Worker
// constructed by worker.New is already in -- must still cancel an
// in-flight handler's context the instant Run's/RunOnce's own context is
// cancelled, with no grace period at all. This is exercised directly,
// independent of TestStress_WorkerPoolGracefulShutdown_*'s larger,
// multi-worker stress scenario, so the zero-value default is proven on
// its own rather than only incidentally.
func TestRunOnce_SF064a_DefaultWorkerCancelsExecutionImmediately(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 "test.sf064a.default_cancel",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30, // long enough that only the outer cancellation, never the execution timeout, could plausibly end this attempt
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{}) // deliberately never closed: only cancellation may end this attempt
	registry := handler.NewRegistry()
	registry.Register("test.sf064a.default_cancel", testdoubles.Gated{Started: started, Proceed: proceed})

	w := worker.New("worker-1", s, registry, 0, discardLogger())
	// SetDrainTimeout is deliberately never called: this is the default,
	// un-opted-in contract under test.

	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = w.RunOnce(runCtx)
	}()
	<-started

	cancelledAt := time.Now()
	cancel()

	select {
	case <-runDone:
	case <-time.After(1 * time.Second):
		t.Fatal("RunOnce did not return promptly after the outer context was cancelled -- " +
			"a default, un-opted-in Worker must cancel in-flight execution immediately, with no drain grace period")
	}
	require.Less(t, time.Since(cancelledAt), 1*time.Second,
		"cancellation of an un-opted-in Worker's in-flight handler must be prompt (sub-second), not eventual")

	final, err := s.GetByID(context.Background(), created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, final.State,
		"a cancelled-mid-flight job with no drain configured must remain RUNNING, exactly as before Phase 14 -- "+
			"no fabricated completion is ever reported for a cancelled-and-abandoned attempt")
}

// TestRunOnce_SF069InProcess_DrainTimeoutExpiryReportsNoCompletion_LeaseReclaimable
// is the in-process half of SF-069 (docs/phase-14-plan.md §6.4 "Required
// regression proof" item 4, §19 OD-3's core safety claim): once a
// Worker's configured drain timeout elapses before an in-flight attempt
// finishes, RunOnce must return without ever calling any Complete*
// method, leaving the job's row exactly as it was -- RUNNING, under this
// worker's own lease -- so it is reclaimable through the ordinary,
// unmodified TF-INV-004 path once lease_expires_at passes. The real
// cmd/worker OS-process version of this proof (a real SIGTERM, a real
// separately-run binary) lives under test/procs.
func TestRunOnce_SF069InProcess_DrainTimeoutExpiryReportsNoCompletion_LeaseReclaimable(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 "test.sf069.drain_timeout",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{}) // never closed: the handler only stops via context cancellation, after the drain timeout
	registry := handler.NewRegistry()
	registry.Register("test.sf069.drain_timeout", testdoubles.Gated{Started: started, Proceed: proceed})

	w := worker.New("draining-worker", s, registry, 0, discardLogger())
	w.SetDrainTimeout(200 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = w.RunOnce(runCtx)
	}()
	<-started
	cancel() // simulates SIGTERM: the poll-gating context is cancelled while the job is in flight

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("RunOnce did not return after its configured drain timeout elapsed")
	}

	stranded, err := s.GetByID(context.Background(), created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.Running, stranded.State,
		"a drain-timeout expiry must not write any completion -- the job's row must be left exactly as it was")
	require.Equal(t, int64(1), stranded.LeaseGeneration)
	require.Equal(t, 1, stranded.AttemptCount, "the drain-timeout path must not have advanced or recorded a new attempt")

	// Claim (not completion) is what writes job_attempts' started_at row;
	// what the drain-timeout path must never do is fill in that row's
	// outcome/finished_at, which only a Complete* call does.
	var finishedAt *time.Time
	var outcome *string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT finished_at, outcome FROM job_attempts WHERE job_id = $1`, created.ID).Scan(&finishedAt, &outcome))
	require.Nil(t, finishedAt, "a drain-timeout expiry must not finalize the in-flight attempt row")
	require.Nil(t, outcome, "a drain-timeout expiry must not record any outcome for the in-flight attempt")

	// TF-INV-004: once the lease expires, a fresh worker reclaims and
	// completes the job normally, through the existing, unmodified path
	// -- proving the drain-timeout expiry left no permanent damage.
	_, err = db.ExecContext(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, created.ID)
	require.NoError(t, err)

	recoveryRegistry := handler.NewRegistry()
	recoveryRegistry.Register("test.sf069.drain_timeout", testdoubles.AlwaysSucceed{})
	recoveryWorker := worker.New("recovery-worker", s, recoveryRegistry, 0, discardLogger())
	claimed, err := recoveryWorker.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(context.Background(), created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State,
		"the job stranded by drain-timeout expiry must still be fully recoverable via ordinary lease-expiry reclaim")
	require.Equal(t, int64(2), final.LeaseGeneration)

	close(proceed) // let the original Gated handler's goroutine exit cleanly
}
