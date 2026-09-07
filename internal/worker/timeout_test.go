// Phase 6 worker-loop level execution-timeout tests: proving
// execution_timeout_seconds is enforced as a fixed, per-attempt ceiling
// distinct from the lease (which the heartbeat loop keeps renewing
// throughout), per docs/execution-semantics.md "Timeout Semantics" and
// this task's explicit "timeout vs lease" adversarial requirements.
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

// TestRunOnce_ExecutionTimeout_FiresIndependentlyOfLeaseRenewal is the
// direct proof that closes docs/failure-model.md F9's previously-open
// gap: a cooperative handler (testdoubles.Gated already checks
// ctx.Done()) that runs past execution_timeout_seconds is cancelled and
// reports TIMED_OUT, even though the LEASE itself is still valid
// throughout (heartbeats kept renewing it right up to the moment the
// fixed execution-timeout deadline fired) — proving the two mechanisms
// are genuinely independent, not the same check twice.
func TestRunOnce_ExecutionTimeout_FiresIndependentlyOfLeaseRenewal(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.timeout.fires",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 2, // ~666ms heartbeat interval; 2s fixed execution-timeout ceiling
	})
	require.NoError(t, err)

	proceed := make(chan struct{}) // never closed: the handler must be stopped by the timeout, not finish on its own
	registry := handler.NewRegistry()
	registry.Register("test.timeout.fires", testdoubles.Gated{Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, final.State, "a timeout with attempts remaining schedules a retry, exactly like any other retryable outcome")
	require.NotNil(t, final.LastErrorClass)
	require.Equal(t, "TIMEOUT", *final.LastErrorClass)
	require.Equal(t, int64(1), final.LeaseGeneration, "the lease itself must never have been lost or reclaimed -- heartbeats kept it valid the whole time")

	var outcome string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT outcome FROM job_attempts WHERE job_id = $1`, created.ID).Scan(&outcome))
	require.Equal(t, "TIMED_OUT", outcome)
}

// TestRunOnce_ExecutionTimeout_HandlerIgnoresContext_StillReportsTimeout
// proves the same documented cooperative-cancellation limitation for the
// timeout path specifically ("handler ignores cancellation"): a handler
// that never checks ctx keeps running past the deadline from its own
// point of view, but the worker still correctly classifies the outcome
// as TIMED_OUT once the handler eventually returns (here, reporting
// success, which is nonetheless overridden by the observed timeout —
// see reportTimeout's doc comment).
func TestRunOnce_ExecutionTimeout_HandlerIgnoresContext_StillReportsTimeout(t *testing.T) {
	s := storeForTimeoutTest(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.timeout.ignored",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 1, // 1s fixed ceiling
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.timeout.ignored", sleepIgnoringContext{sleep: 1500 * time.Millisecond}) // ignores ctx, outlives the 1s ceiling
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, final.State)
	require.Equal(t, "TIMEOUT", *final.LastErrorClass,
		"the worker cannot forcibly stop an uncooperative handler, but must still correctly classify the outcome once it returns")
}

// TestRunOnce_ExecutionTimeout_HandlerFinishesJustWithinBudget_Succeeds
// is the negative complement: a handler that finishes just BEFORE its
// execution_timeout_seconds ceiling succeeds normally — proving the
// mechanism does not fire prematurely or introduce any race against a
// handler that is about to legitimately finish in time.
func TestRunOnce_ExecutionTimeout_HandlerFinishesJustWithinBudget_Succeeds(t *testing.T) {
	s := storeForTimeoutTest(t)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.timeout.withinbudget",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 3,
	})
	require.NoError(t, err)

	proceed := make(chan struct{})
	registry := handler.NewRegistry()
	registry.Register("test.timeout.withinbudget", testdoubles.Gated{Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	runDone := make(chan struct{})
	var claimed bool
	var runErr error
	go func() {
		defer close(runDone)
		claimed, runErr = w.RunOnce(ctx)
	}()

	time.Sleep(1500 * time.Millisecond) // well under the 3s ceiling
	close(proceed)

	<-runDone
	require.NoError(t, runErr)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
}

// TestRunOnce_ExecutionTimeout_ExhaustionDeadLetters proves TF-INV-006
// holds identically on the timeout path at the worker-loop level:
// repeated timeouts exhaust max_attempts and dead-letter, never
// exceeding the configured ceiling.
func TestRunOnce_ExecutionTimeout_ExhaustionDeadLetters(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.timeout.exhaust.worker",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             2,
		ExecutionTimeoutSeconds: 1,
	})
	require.NoError(t, err)

	proceed := make(chan struct{}) // never closed on either attempt
	registry := handler.NewRegistry()
	registry.Register("test.timeout.exhaust.worker", testdoubles.Gated{Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	for attempt := 1; attempt <= 2; attempt++ {
		claimed, err := w.RunOnce(ctx)
		require.NoError(t, err)
		require.True(t, claimed)

		if attempt < 2 {
			forceSetEligibleAtWT(t, db, created.ID, -time.Second)
		}
	}

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, final.State)
	require.NotNil(t, final.TerminalAt)
	require.Equal(t, "TIMEOUT", *final.LastErrorClass)
}

// TestRunOnce_ExecutionTimeout_FiresAfterLeaseAlreadyLost proves
// adversarial case #13 ("timeout fires after worker lost ownership"):
// once the lease has already been reclaimed by another worker, this
// worker's subsequent execution-timeout firing must not attempt any
// completion call at all -- lease loss is detected and takes priority
// (dispositionLeaseLost), so no stale CompleteTimeout call is ever made.
func TestRunOnce_ExecutionTimeout_FiresAfterLeaseAlreadyLost(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.timeout.vs.leaselost",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 2, // ~666ms heartbeat interval, 2s ceiling
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{}) // never closed: handler stopped only by the (lost) lease/timeout signal
	registry := handler.NewRegistry()
	registry.Register("test.timeout.vs.leaselost", testdoubles.Gated{Started: started, Proceed: proceed})
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

	<-runDone // worker-1's heartbeat loop discovers the lost lease at its next tick, well before the 2s timeout would otherwise fire
	require.NoError(t, runErr)
	require.True(t, claimed)

	// worker-2 owns generation 2 and completes normally -- proving
	// worker-1 never raced it with a stale CompleteTimeout call.
	completed, err := s.CompleteSuccess(ctx, reclaimed.ID, "worker-2", reclaimed.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)
}

// storeForTimeoutTest is a small local alias so this file's tests read
// consistently with the rest of the package's newStore-style helpers
// without redeclaring one across files.
func storeForTimeoutTest(t *testing.T) *store.Store {
	t.Helper()
	return store.New(testutil.DB(t))
}
