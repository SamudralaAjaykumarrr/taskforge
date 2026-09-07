// Phase 6 worker-loop level cancellation tests: cooperative and
// uncooperative handler behavior under a durable cancellation request,
// per docs/execution-semantics.md "Cancellation and Timeouts" and this
// task's explicit requirement to prove (not merely assert) that
// TaskForge can request cooperative cancellation but cannot guarantee
// physical termination of arbitrary handler code.
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

// TestRunOnce_CooperativeHandler_AcknowledgesCancellation proves the
// documented happy path: a handler that checks ctx.Done()
// (testdoubles.Gated already does this) stops promptly once the worker's
// heartbeat loop observes a durable cancellation request and cancels the
// handler's context, and the worker acknowledges CANCELLED.
func TestRunOnce_CooperativeHandler_AcknowledgesCancellation(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.cancel.cooperative",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 2, // ~666ms heartbeat interval
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{}) // never closed: the handler must stop via ctx, not via Proceed
	registry := handler.NewRegistry()
	registry.Register("test.cancel.cooperative", testdoubles.Gated{Started: started, Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	runDone := make(chan struct{})
	var claimed bool
	var runErr error
	go func() {
		defer close(runDone)
		claimed, runErr = w.RunOnce(ctx)
	}()

	<-started
	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	<-runDone
	require.NoError(t, runErr)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, final.State)
	require.NotNil(t, final.TerminalAt)

	var outcome string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT outcome FROM job_attempts WHERE job_id = $1`, created.ID).Scan(&outcome))
	require.Equal(t, "CANCELLED", outcome)
}

// sleepIgnoringContext is a Handler that deliberately does NOT check
// ctx.Done() -- it sleeps for a fixed real duration regardless of
// cancellation, then reports success. It exists specifically to prove
// TaskForge's documented limitation: a handler that ignores context
// cancellation cannot be physically stopped, but the worker still
// correctly reports the durable outcome (CANCELLED, since the
// cancellation was observed) once the handler eventually returns.
type sleepIgnoringContext struct {
	started chan<- struct{}
	sleep   time.Duration
}

func (h sleepIgnoringContext) Execute(_ context.Context, _ *job.Job) (handler.Result, error) {
	if h.started != nil {
		h.started <- struct{}{}
	}
	time.Sleep(h.sleep) // deliberately ignores ctx cancellation
	return handler.Result{}, nil
}

// TestRunOnce_HandlerIgnoresCancellation_StillAcknowledgesCancelled
// proves the documented cooperative-cancellation limitation concretely:
// the handler never checks ctx and completes "successfully" from its own
// point of view, but because the worker observed a durable cancellation
// request before the handler returned, the worker still reports
// CANCELLED, not SUCCEEDED — the side effect (here, nothing observable,
// but see the SF-004-style companion tests for a real side-effect
// double) is not undone, exactly as documented.
func TestRunOnce_HandlerIgnoresCancellation_StillAcknowledgesCancelled(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.cancel.uncooperative",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 2, // ~666ms heartbeat interval
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	registry := handler.NewRegistry()
	registry.Register("test.cancel.uncooperative", sleepIgnoringContext{started: started, sleep: 1200 * time.Millisecond})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	runDone := make(chan struct{})
	var claimed bool
	var runErr error
	go func() {
		defer close(runDone)
		claimed, runErr = w.RunOnce(ctx)
	}()

	<-started
	// Request cancellation immediately after the handler starts, well
	// before the first heartbeat tick (~666ms) and well before the
	// handler's own 1200ms sleep completes -- guaranteeing the worker
	// observes it before the handler returns.
	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)

	<-runDone
	require.NoError(t, runErr)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, final.State,
		"a durable cancellation observed during execution must win even when the handler itself ignored ctx and reported success")
}

// TestRunOnce_LeaseLostTakesPriorityOverPendingCancellation proves
// precedence: if the lease is lost (reclaimed by another worker) before
// this worker's heartbeat loop would otherwise acknowledge a pending
// cancellation, the worker must not attempt ANY completion call
// (including a cancellation acknowledgement) — a stale generation is no
// more authoritative for acknowledging cancellation than for reporting
// success (TF-INV-003/014).
func TestRunOnce_LeaseLostTakesPriorityOverPendingCancellation(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		JobType:                 "test.cancel.vs.leaselost",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 1, // ~333ms heartbeat interval
	})
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})
	registry := handler.NewRegistry()
	registry.Register("test.cancel.vs.leaselost", testdoubles.Gated{Started: started, Proceed: proceed})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	runDone := make(chan struct{})
	var claimed bool
	var runErr error
	go func() {
		defer close(runDone)
		claimed, runErr = w.RunOnce(ctx)
	}()

	<-started

	// A cancellation is requested, but before worker-1's heartbeat loop
	// gets a chance to observe it, its lease is force-expired and
	// genuinely reclaimed by a simulated second worker.
	_, err = s.RequestCancellation(ctx, created.ID)
	require.NoError(t, err)
	forceExpireLeaseWT(t, db, created.ID)
	reclaimed, ok, err := s.Claim(ctx, "worker-2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)
	require.False(t, reclaimed.CancelRequested, "cancel_requested resets on reclaim, per docs/worker-protocol.md")

	time.Sleep(500 * time.Millisecond) // let worker-1's heartbeat loop notice the lost lease
	close(proceed)

	<-runDone
	require.NoError(t, runErr)
	require.True(t, claimed)

	// worker-2 legitimately owns generation 2 and can complete normally
	// -- proving worker-1 never raced it with a stale cancellation
	// acknowledgement.
	completed, err := s.CompleteSuccess(ctx, reclaimed.ID, "worker-2", reclaimed.LeaseGeneration, nil)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, completed.State)
}
