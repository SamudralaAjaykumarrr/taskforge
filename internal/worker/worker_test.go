// Integration tests against a real PostgreSQL instance (see
// internal/testutil). These are the executable form of SF-001 (normal
// success) and its failure-path analogue for Phase 1's simplified "any
// failure goes straight to DEAD_LETTERED" rule.
package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler/testdoubles"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/worker"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestRunOnce_SF001_NormalSuccess is scenario SF-001: submit, worker
// claims it, handler succeeds, worker reports success. Asserts the exact
// durable state SF-001 specifies: SUCCEEDED, attempt_count = 1.
func TestRunOnce_SF001_NormalSuccess(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 "test.succeed",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.succeed", testdoubles.AlwaysSucceed{})
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)
	require.Equal(t, 1, final.AttemptCount)
	require.NotNil(t, final.TerminalAt)
}

// TestRunOnce_FailurePath is Phase 1's failure scenario: docs/roadmap.md
// specifies failures go straight to DEAD_LETTERED (no RETRY_WAIT yet).
func TestRunOnce_FailurePath(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 "test.fail",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry()
	registry.Register("test.fail", testdoubles.NewAlwaysFail(errors.New("simulated handler failure")))
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, final.State)
	require.Equal(t, 1, final.AttemptCount)
	require.NotNil(t, final.LastError)
	require.Equal(t, "simulated handler failure", *final.LastError)
}

// TestRunOnce_NoHandlerRegistered proves an unrecognized job_type is a
// real, handled Phase 1 failure case (docs/failure-model.md requires every
// assumed failure to have a defined outcome) rather than a panic or a
// silently stuck job.
func TestRunOnce_NoHandlerRegistered(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 "test.unregistered",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	registry := handler.NewRegistry() // nothing registered
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	final, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, final.State)
	require.NotNil(t, final.LastError)
}

// TestRunOnce_SF062_WorkflowNodeWithNoRegisteredHandler is SF-062
// (docs/phase-14-plan.md §6.3/§16): extends
// TestRunOnce_NoHandlerRegistered's plain-job proof to a workflow node's
// underlying job. A workflow node is, underneath, a row in the same jobs
// table claimed and completed through the exact same RunOnce/ErrNoHandler/
// CompleteFailure path -- this test confirms that is genuinely true end to
// end, including TF-INV-012's completion-propagation cascade, not merely
// assumed because both paths happen to call the same store method.
//
// docs/compatibility-policy.md's "PROPOSED: Workflows Surviving
// Deployments" section documents the underlying risk this proves a defined
// (not undefined-behavior) outcome for: a workflow node's job_type handler
// removed or never registered between workflow submission and that node's
// eventual execution dead-letters the node and fails the workflow, exactly
// as an unregistered job_type does for a plain job -- it does not panic,
// hang, or leave the workflow silently stuck RUNNING forever.
func TestRunOnce_SF062_WorkflowNodeWithNoRegisteredHandler(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{
		PrincipalID: testPrincipalID,
		Nodes: []workflow.NodeSpec{
			{NodeKey: "only", JobType: "test.wf.unregistered", Payload: json.RawMessage(`{}`), MaxAttempts: 5, ExecutionTimeoutSeconds: 30},
		},
	})
	require.NoError(t, err)
	require.Len(t, inst.Nodes, 1)
	nodeJobID := inst.Nodes[0].JobID

	registry := handler.NewRegistry() // nothing registered, exactly like the plain-job test above
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	finalJob, err := s.GetByID(ctx, nodeJobID, testAccess)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, finalJob.State,
		"a workflow node's job with no registered handler must dead-letter through the same ErrNoHandler path as a plain job")
	require.NotNil(t, finalJob.LastError)

	finalWorkflow, err := s.GetWorkflow(ctx, inst.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, workflow.Failed, finalWorkflow.State,
		"TF-INV-012's cascade must propagate the dead-lettered node's outcome to the workflow's own terminal state")
}

// TestRunOnce_NothingToClaim proves the steady-state "no eligible job"
// outcome is reported cleanly (claimed=false, err=nil), not as an error.
func TestRunOnce_NothingToClaim(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	registry := handler.NewRegistry()
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	require.False(t, claimed)
}

// TestRunOnce_HandlerRecordsExactlyOneExecution proves that, on the
// documented Phase 1 happy path (no crash, no retry), a job's handler is
// invoked exactly once — this is what "at-least-once" degenerates to when
// nothing goes wrong, and is the baseline the crash-induced duplicate
// scenario (SF-004, out of Phase 1's scope) is contrasted against.
func TestRunOnce_HandlerRecordsExactlyOneExecution(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 "test.recorded",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
	})
	require.NoError(t, err)

	recorder := testdoubles.NewRecorder(nil)
	registry := handler.NewRegistry()
	registry.Register("test.recorded", recorder)
	w := worker.New("worker-1", s, registry, 0, discardLogger())

	claimed, err := w.RunOnce(ctx)
	require.NoError(t, err)
	require.True(t, claimed)

	executions := recorder.Executions()
	require.Len(t, executions, 1)
	require.Equal(t, created.ID.String(), executions[0].JobID)
	require.Equal(t, 1, executions[0].AttemptCount)
}
