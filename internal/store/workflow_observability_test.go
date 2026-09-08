// Phase 8: workflow observability -- submission metrics for each node's
// underlying job, and the "workflow created"/"workflow finalized"
// structured logs Store uniquely observes (see internal/store/workflow.go's
// logWorkflowFinalized doc comment for why this is logging, not a new
// metric: docs/observability.md defines no workflow-specific metric, and
// docs/roadmap.md's cardinality guidance forbids using workflow_id as a
// metric label -- workflow diagnostics belong in structured logs).
package store_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

func newStoreWithMetricsAndLog(t *testing.T) (*store.Store, *metrics.Metrics, *bytes.Buffer) {
	t.Helper()
	db := testutil.DB(t)
	m := metrics.New()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	return store.New(db, store.WithMetrics(m), store.WithLogger(logger)), m, &buf
}

// TestWorkflowMetrics_CreateWorkflow_SubmitsOneJobPerNode proves each
// workflow node's underlying job is counted in
// taskforge_jobs_submitted_total exactly like a standalone job
// submission (SF-019/SF-022's diamond shape, reused here purely for
// metric assertions -- no new scenario is introduced).
func TestWorkflowMetrics_CreateWorkflow_SubmitsOneJobPerNode(t *testing.T) {
	s, m, buf := newStoreWithMetricsAndLog(t)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, diamondSpec())
	require.NoError(t, err)
	require.Len(t, inst.Nodes, 4)

	require.Equal(t, float64(4), counterValue(t, m.JobsSubmittedTotal.WithLabelValues("test.workflow.node")))
	require.Equal(t, float64(0), counterValue(t, m.IdempotentSubmissionHitsTotal),
		"workflow creation has no idempotency-key contract (docs/workflows.md)")

	logOutput := buf.String()
	require.Contains(t, logOutput, `"event":"workflow_created"`)
	require.Contains(t, logOutput, `"workflow_id":"`+inst.ID.String()+`"`)
	require.Contains(t, logOutput, `"node_count":4`)
}

// TestWorkflowLogging_FinalizedExactlyOnceOnSuccess proves
// "workflow_finalized" is logged exactly once, by whichever completion
// call actually finalized the workflow -- not once per node, and not
// before the workflow is durably SUCCEEDED.
func TestWorkflowLogging_FinalizedExactlyOnceOnSuccess(t *testing.T) {
	s, _, buf := newStoreWithMetricsAndLog(t)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("A"), wfNode("B", "A")}})
	require.NoError(t, err)

	require.NotContains(t, buf.String(), `"event":"workflow_finalized"`)

	a := nodeByKey(t, inst, "A")
	claimedA, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimedA.ID)
	_, err = s.CompleteSuccess(ctx, claimedA.ID, "worker-A", claimedA.LeaseGeneration, nil)
	require.NoError(t, err)
	require.NotContains(t, buf.String(), `"event":"workflow_finalized"`, "B has not succeeded yet")

	claimedB, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimedB.ID, "worker-B", claimedB.LeaseGeneration, nil)
	require.NoError(t, err)

	logOutput := buf.String()
	require.Equal(t, 1, strings.Count(logOutput, `"event":"workflow_finalized"`),
		"finalization must be logged exactly once")
	require.Contains(t, logOutput, `"workflow_id":"`+inst.ID.String()+`"`)
	require.Contains(t, logOutput, `"state":"SUCCEEDED"`)
}

// TestWorkflowLogging_FinalizedOnceOnCascadeFailure proves the same
// exactly-once property when a permanent failure cascades a whole
// downstream chain to CANCELLED -- the workflow's own finalization log
// still fires exactly once, at FAILED, not once per cascaded node.
func TestWorkflowLogging_FinalizedOnceOnCascadeFailure(t *testing.T) {
	s, _, buf := newStoreWithMetricsAndLog(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("A"), wfNode("B", "A"), wfNode("C", "B")}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)

	a := nodeByKey(t, inst, "A")
	claimedA, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimedA.ID)
	_, err = s.CompleteFailure(ctx, claimedA.ID, "worker-A", claimedA.LeaseGeneration, "boom", "PERMANENT")
	require.NoError(t, err)

	logOutput := buf.String()
	require.Equal(t, 1, strings.Count(logOutput, `"event":"workflow_finalized"`))
	require.Contains(t, logOutput, `"state":"FAILED"`)
}
