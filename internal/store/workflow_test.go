// Phase 7 integration tests against a real PostgreSQL instance, per
// docs/testing-strategy.md: workflow/DAG correctness is never asserted
// against a mock. See docs/workflows.md and TF-INV-012.
package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

func wfNode(key string, deps ...string) workflow.NodeSpec {
	return workflow.NodeSpec{
		NodeKey:                 key,
		JobType:                 "test.workflow.node",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		DependsOn:               deps,
	}
}

// diamondSpec builds the canonical A -> B, A -> C, B+C -> D diamond this
// task's quality gate names explicitly.
func diamondSpec() workflow.GraphSpec {
	return workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
		wfNode("C", "A"),
		wfNode("D", "B", "C"),
	}}
}

func nodeByKey(t *testing.T, inst *workflow.Instance, key string) workflow.Node {
	t.Helper()
	for _, n := range inst.Nodes {
		if n.NodeKey == key {
			return n
		}
	}
	t.Fatalf("node %q not found in workflow %s", key, inst.ID)
	return workflow.Node{}
}

func TestCreateWorkflow_LinearChain_RootEligibleDependentsBlocked(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
		wfNode("C", "B"),
	}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	require.Equal(t, workflow.Running, inst.State)
	require.Len(t, inst.Nodes, 3)

	a := nodeByKey(t, inst, "A")
	b := nodeByKey(t, inst, "B")
	c := nodeByKey(t, inst, "C")
	require.Equal(t, jobstate.Queued, a.JobState)
	require.Equal(t, jobstate.Queued, b.JobState)
	require.Equal(t, jobstate.Queued, c.JobState)
	require.Empty(t, a.DependsOn)
	require.Equal(t, []string{"A"}, b.DependsOn)
	require.Equal(t, []string{"B"}, c.DependsOn)

	// Root activation: only A is claimable right now.
	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)

	// Nothing else eligible: B and C remain blocked.
	_, ok, err = s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.False(t, ok, "B/C must not be claimable before A succeeds")
}

func TestCreateWorkflow_AtomicCreation_InvalidGraphNeverPersisted(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	// A -> B -> A: a cycle, rejected by workflow.ValidateGraph before any
	// SQL is issued.
	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A", "B"),
		wfNode("B", "A"),
	}}
	_, err := s.CreateWorkflow(ctx, spec)
	require.Error(t, err)
	var invalid *workflow.InvalidGraphError
	require.ErrorAs(t, err, &invalid)

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances`).Scan(&count))
	require.Equal(t, 0, count, "an invalid graph must never create a workflow_instances row")
}

// TestCreateWorkflow_RollbackLeavesNoPartialState is this phase's
// TF-INV-013-style fault-injection test for workflow creation: forcing a
// transaction to fail partway through (a duplicate job_id violating
// workflow_nodes_job_unique, injected directly, bypassing
// store.CreateWorkflow's own Go-level correctness) must leave NO
// workflow_instances, workflow_nodes, or jobs rows behind — not a
// workflow that exists with only some of its nodes.
func TestCreateWorkflow_RollbackLeavesNoPartialState(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	workflowID := uuid.New()
	jobID := uuid.New()

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_instances (id, state) VALUES ($1, 'RUNNING')`, workflowID)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, max_attempts, execution_timeout_seconds, eligible_at)
		VALUES ($1, 'test.rollback', '{}', 'QUEUED', 5, 30, now())`, jobID)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_nodes (id, workflow_instance_id, node_key, job_id, depends_on)
		VALUES ($1, $2, 'A', $3, '{}')`, uuid.New(), workflowID, jobID)
	require.NoError(t, err)

	// Force failure: workflow_nodes_job_unique forbids a second node
	// wrapping the same job_id.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_nodes (id, workflow_instance_id, node_key, job_id, depends_on)
		VALUES ($1, $2, 'A-dup', $3, '{}')`, uuid.New(), workflowID, jobID)
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	var wfCount, jobCount, nodeCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances WHERE id = $1`, workflowID).Scan(&wfCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, jobID).Scan(&jobCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_nodes WHERE workflow_instance_id = $1`, workflowID).Scan(&nodeCount))
	require.Equal(t, 0, wfCount, "rolled-back workflow_instances row must not exist")
	require.Equal(t, 0, jobCount, "rolled-back jobs row must not exist")
	require.Equal(t, 0, nodeCount, "rolled-back workflow_nodes rows must not exist")
}

func TestFanOut_AllChildrenIndependentlyEligibleAfterParentSucceeds(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
		wfNode("C", "A"),
		wfNode("D", "A"),
	}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a := nodeByKey(t, inst, "A")

	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)

	_, ok, err = s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.False(t, ok, "B/C/D must not be claimable before A succeeds")

	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	claimedIDs := map[uuid.UUID]bool{}
	for i := 0; i < 3; i++ {
		j, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		require.True(t, ok, "expected B, C, and D all independently claimable, got %d", i)
		claimedIDs[j.ID] = true
	}
	require.Len(t, claimedIDs, 3, "no duplicate node job claimed twice")

	inst2 := nodeByKey(t, inst, "B") // sanity: node identity unchanged
	require.NotEqual(t, uuid.Nil, inst2.JobID)

	_, ok, err = s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.False(t, ok, "nothing left eligible")
}

func TestFanIn_ChildBlockedUntilAllPredecessorsSucceed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, diamondSpec())
	require.NoError(t, err)
	a, b, c, d := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B"), nodeByKey(t, inst, "C"), nodeByKey(t, inst, "D")

	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	// B and C now eligible; claim + succeed B only.
	var bClaimed, cClaimed *jobRow
	for i := 0; i < 2; i++ {
		j, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		require.True(t, ok)
		switch j.ID {
		case b.JobID:
			bClaimed = &jobRow{id: j.ID, gen: j.LeaseGeneration}
		case c.JobID:
			cClaimed = &jobRow{id: j.ID, gen: j.LeaseGeneration}
		}
	}
	require.NotNil(t, bClaimed)
	require.NotNil(t, cClaimed)

	_, err = s.CompleteSuccess(ctx, bClaimed.id, "w1", bClaimed.gen, nil)
	require.NoError(t, err)

	// D must NOT be eligible yet -- only one of its two predecessors has
	// succeeded (TF-INV-012's AND fan-in semantics).
	got, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.False(t, ok, "D must not be claimable with only B succeeded, got %v", got)

	_, err = s.CompleteSuccess(ctx, cClaimed.id, "w1", cClaimed.gen, nil)
	require.NoError(t, err)

	got, ok, err = s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok, "D must become claimable once both B and C have succeeded")
	require.Equal(t, d.JobID, got.ID)
}

type jobRow struct {
	id  uuid.UUID
	gen int64
}

// TestConcurrentFanIn_BothPredecessorsCompleteSimultaneously is the
// TF-INV-012 "concurrent dependency completion" adversarial case: B and C
// (D's two predecessors) report success from two real, concurrently
// executing goroutines, synchronized to commit as close to simultaneously
// as possible via a start barrier. D must become eligible exactly once
// (not zero times -- a lost-update bug -- and not claimed twice).
func TestConcurrentFanIn_BothPredecessorsCompleteSimultaneously(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, diamondSpec())
	require.NoError(t, err)
	a, b, c, d := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B"), nodeByKey(t, inst, "C"), nodeByKey(t, inst, "D")

	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)

	var bClaim, cClaim jobRow
	for i := 0; i < 2; i++ {
		j, ok, err := s.Claim(ctx, "wpool")
		require.NoError(t, err)
		require.True(t, ok)
		switch j.ID {
		case b.JobID:
			bClaim = jobRow{id: j.ID, gen: j.LeaseGeneration}
		case c.JobID:
			cClaim = jobRow{id: j.ID, gen: j.LeaseGeneration}
		}
	}
	require.NotEqual(t, uuid.Nil, bClaim.id)
	require.NotEqual(t, uuid.Nil, cClaim.id)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := s.CompleteSuccess(ctx, bClaim.id, "wpool", bClaim.gen, nil)
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := s.CompleteSuccess(ctx, cClaim.id, "wpool", cClaim.gen, nil)
		errCh <- err
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	// D must be claimable exactly once.
	claimedCount := 0
	for i := 0; i < 5; i++ {
		j, ok, err := s.Claim(ctx, "wpool")
		require.NoError(t, err)
		if !ok {
			break
		}
		require.Equal(t, d.JobID, j.ID)
		claimedCount++
	}
	require.Equal(t, 1, claimedCount, "D must become eligible exactly once despite concurrent predecessor completion")
}

func TestRetryingPredecessor_DoesNotUnblockDependent(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
	}}
	spec.Nodes[0].MaxAttempts = 3
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B")

	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)

	// A long backoff (never a real sleep-dependent short window, per
	// docs/testing-strategy.md's determinism requirement) so A is
	// unambiguously still RETRY_WAIT, not eligible, when we check B below.
	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "w1", claimed.LeaseGeneration, "transient", time.Hour)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)

	// A retrying predecessor must not unblock B.
	_, ok, err = s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.False(t, ok, "B must remain blocked while A is only RETRY_WAIT")

	// Fast-forward A's backoff window deterministically (DB-time
	// manipulation, not a real sleep) and retry it to success.
	forceSetEligibleAt(t, db, a.JobID, -time.Second)
	claimed2, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed2.ID)
	_, err = s.CompleteSuccess(ctx, claimed2.ID, "w1", claimed2.LeaseGeneration, nil)
	require.NoError(t, err)

	got, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, b.JobID, got.ID, "B becomes claimable only once A's SUCCESSFUL retry commits")
}

func TestDeadLetteredPredecessor_CancelsDependentsTransitively(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// A -> B -> C: dead-lettering A must cascade CANCELLED to both B and C.
	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
		wfNode("C", "B"),
	}}
	spec.Nodes[0].MaxAttempts = 1
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b, c := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B"), nodeByKey(t, inst, "C")

	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)

	_, err = s.CompleteFailure(ctx, claimed.ID, "w1", claimed.LeaseGeneration, "boom", "PERMANENT")
	require.NoError(t, err)

	aAfter, err := s.GetByID(ctx, a.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, aAfter.State)

	bAfter, err := s.GetByID(ctx, b.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, bAfter.State, "B must cascade-cancel when its only predecessor dead-letters")

	cAfter, err := s.GetByID(ctx, c.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, cAfter.State, "C must transitively cascade-cancel through B")

	wf, err := s.GetWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Failed, wf.State)
	require.NotNil(t, wf.TerminalAt)
}

func TestDeadLetteredPredecessor_ExhaustionViaRetryPath_CancelsDependents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
	}}
	spec.Nodes[0].MaxAttempts = 1
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B")

	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)

	// max_attempts=1, so a retryable failure exhausts immediately and
	// dead-letters via completeRetryableOutcome's exhaustion branch, not
	// CompleteFailure.
	result, err := s.CompleteRetryableFailure(ctx, claimed.ID, "w1", claimed.LeaseGeneration, "boom", time.Second)
	require.NoError(t, err)
	require.Equal(t, jobstate.DeadLettered, result.State)

	bAfter, err := s.GetByID(ctx, b.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, bAfter.State, "B must cascade-cancel when A dead-letters via retry exhaustion, not just via CompleteFailure")
}

func TestCancelledPredecessor_CancelsDependents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
	}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B")

	// A is QUEUED, not yet claimed: direct cancellation.
	_, err = s.CancelQueuedOrRetryWait(ctx, a.JobID)
	require.NoError(t, err)

	bAfter, err := s.GetByID(ctx, b.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Cancelled, bAfter.State, "B must cascade-cancel when A is cancelled directly, per docs/workflows.md's Failure Propagation table")
}

func TestWorkflowState_SucceedsWhenEveryNodeSucceeds(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, diamondSpec())
	require.NoError(t, err)

	// Drive the whole diamond to completion via ordinary claim/succeed
	// cycles.
	for completed := 0; completed < 4; {
		j, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		if !ok {
			t.Fatalf("workflow stalled after %d of 4 nodes completed", completed)
		}
		_, err = s.CompleteSuccess(ctx, j.ID, "w1", j.LeaseGeneration, nil)
		require.NoError(t, err)
		completed++
	}

	wf, err := s.GetWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Succeeded, wf.State)
	require.NotNil(t, wf.TerminalAt)
	for _, n := range wf.Nodes {
		require.Equal(t, jobstate.Succeeded, n.JobState)
	}
}

func TestWorkflowState_TerminalCannotReopen(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("solo")}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	solo := nodeByKey(t, inst, "solo")

	j, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, solo.JobID, j.ID)
	_, err = s.CompleteSuccess(ctx, j.ID, "w1", j.LeaseGeneration, nil)
	require.NoError(t, err)

	wf, err := s.GetWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Succeeded, wf.State)
	terminalAt := *wf.TerminalAt

	// Cancelling an already-succeeded workflow must be a no-op: state and
	// terminal_at never change.
	wf2, err := s.CancelWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Succeeded, wf2.State, "a terminal workflow state must never reopen or be overwritten")
	require.True(t, terminalAt.Equal(*wf2.TerminalAt))
}

func TestCancelWorkflow_QueuedAndBlockedNodesCancelledImmediately(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, diamondSpec())
	require.NoError(t, err)

	wf, err := s.CancelWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	for _, n := range wf.Nodes {
		require.Equal(t, jobstate.Cancelled, n.JobState, "node %q", n.NodeKey)
	}
	require.Equal(t, workflow.Cancelled, wf.State)
	require.True(t, wf.CancelRequested)
	require.NotNil(t, wf.CancelRequestedAt)
}

func TestCancelWorkflow_RunningNodeRequestedNotYetConfirmed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("solo")}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	solo := nodeByKey(t, inst, "solo")

	j, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, solo.JobID, j.ID)

	wf, err := s.CancelWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Running, wf.State, "workflow stays RUNNING until the RUNNING node's cancellation is acknowledged")
	n := nodeByKey(t, &workflow.Instance{Nodes: wf.Nodes}, "solo")
	require.Equal(t, jobstate.Running, n.JobState)

	// Worker acknowledges.
	_, err = s.CompleteCancelled(ctx, j.ID, "w1", j.LeaseGeneration)
	require.NoError(t, err)

	wf2, err := s.GetWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Cancelled, wf2.State)
}

// TestCancelWorkflow_MixedNodeStates exercises this task's exact required
// case list for workflow cancellation: "unstarted nodes, scheduled nodes,
// RETRY_WAIT nodes, RUNNING nodes, completed nodes" all within one
// workflow. "blocked" (an unstarted node, gated on a dependency that
// deliberately never completes during this test) and "scheduled" (not
// yet eligible per its own future scheduled_at) are never claimed by
// construction, so this test claims exactly the three remaining
// independent roots ("retrying", "running", "done") and dispatches each
// to its target state by matching the claimed job's identity, rather
// than assuming any particular claim order among tied-priority roots.
func TestCancelWorkflow_MixedNodeStates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	future := time.Now().Add(time.Hour)
	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("retrying"),
		wfNode("running"),
		wfNode("done"),
		wfNode("scheduled"),
		wfNode("blocked", "running"),
	}}
	spec.Nodes[3].ScheduledAt = &future
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	retrying := nodeByKey(t, inst, "retrying")
	running := nodeByKey(t, inst, "running")
	done := nodeByKey(t, inst, "done")

	handled := map[uuid.UUID]bool{}
	for i := 0; i < 3; i++ {
		j, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		require.True(t, ok)
		switch j.ID {
		case retrying.JobID:
			_, err = s.CompleteRetryableFailure(ctx, j.ID, "w1", j.LeaseGeneration, "transient", time.Hour)
			require.NoError(t, err)
		case running.JobID:
			// Leave RUNNING -- never completed.
		case done.JobID:
			_, err = s.CompleteSuccess(ctx, j.ID, "w1", j.LeaseGeneration, nil)
			require.NoError(t, err)
		default:
			t.Fatalf("unexpected job claimed: %s", j.ID)
		}
		require.NoError(t, err)
		handled[j.ID] = true
	}
	require.True(t, handled[retrying.JobID])
	require.True(t, handled[running.JobID])
	require.True(t, handled[done.JobID])

	wf, err := s.CancelWorkflow(ctx, inst.ID)
	require.NoError(t, err)

	byKey := map[string]workflow.Node{}
	for _, n := range wf.Nodes {
		byKey[n.NodeKey] = n
	}
	require.Equal(t, jobstate.Cancelled, byKey["blocked"].JobState, "unstarted/dependency-blocked node cancels directly")
	require.Equal(t, jobstate.Cancelled, byKey["scheduled"].JobState, "not-yet-eligible scheduled node cancels directly")
	require.Equal(t, jobstate.Cancelled, byKey["retrying"].JobState, "RETRY_WAIT node cancels directly")
	require.Equal(t, jobstate.Running, byKey["running"].JobState, "RUNNING node's cancellation is requested, not yet confirmed")
	require.Equal(t, jobstate.Succeeded, byKey["done"].JobState, "already-completed node is untouched")

	// The RUNNING node's job must have cancel_requested set.
	runningJob, err := s.GetByID(ctx, running.JobID)
	require.NoError(t, err)
	require.True(t, runningJob.CancelRequested)

	// Workflow itself is not yet terminal: one node is still RUNNING.
	require.Equal(t, workflow.Running, wf.State)
}

// TestStaleGeneration_CannotUnblockDependents is this task's named
// critical test: Worker A executes node X, loses its lease, Worker B
// reclaims X and succeeds it, and Worker A's stale, late success report
// for X must not (in fact, cannot even reach the point of trying to)
// unblock X's dependents a second time -- and, more fundamentally, must
// be rejected outright so it can never have caused the unblock on its
// own.
func TestStaleGeneration_CannotUnblockDependents(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("X"),
		wfNode("Y", "X"),
	}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	x, y := nodeByKey(t, inst, "X"), nodeByKey(t, inst, "Y")

	claimedA, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, x.JobID, claimedA.ID)
	staleGen := claimedA.LeaseGeneration

	// Worker A's lease expires; Worker B reclaims and succeeds X.
	forceExpireLease(t, db, x.JobID)
	claimedB, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, x.JobID, claimedB.ID)
	require.Greater(t, claimedB.LeaseGeneration, staleGen)

	_, err = s.CompleteSuccess(ctx, claimedB.ID, "worker-B", claimedB.LeaseGeneration, nil)
	require.NoError(t, err)

	yAfterLegit, err := s.GetByID(ctx, y.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Queued, yAfterLegit.State)
	require.True(t, yAfterLegit.EligibleAt.Before(time.Now().Add(time.Minute)), "Y must be genuinely activated by B's real success")

	// Worker A, unaware, now reports success for the SAME job under its
	// stale generation.
	_, err = s.CompleteSuccess(ctx, x.JobID, "worker-A", staleGen, nil)
	require.ErrorIs(t, err, store.ErrStaleTransition, "a stale generation's completion must be rejected outright")

	// X's state (from B) and Y's activation are unaffected by A's
	// rejected call.
	xFinal, err := s.GetByID(ctx, x.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, xFinal.State)
	require.Equal(t, claimedB.LeaseGeneration, xFinal.LeaseGeneration)
}

// TestStaleGeneration_LateFailureCannotCancelDependents is the mirror
// case: a stale worker's late FAILURE report (after the job was already
// reclaimed and succeeded by another worker) must not cascade-cancel
// dependents -- it must simply be rejected, leaving the already-succeeded
// job's real activation of its dependents untouched.
func TestStaleGeneration_LateFailureCannotCancelDependents(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("X"),
		wfNode("Y", "X"),
	}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	x, y := nodeByKey(t, inst, "X"), nodeByKey(t, inst, "Y")

	claimedA, ok, err := s.Claim(ctx, "worker-A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, x.JobID, claimedA.ID)
	staleGen := claimedA.LeaseGeneration

	forceExpireLease(t, db, x.JobID)
	claimedB, ok, err := s.Claim(ctx, "worker-B")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CompleteSuccess(ctx, claimedB.ID, "worker-B", claimedB.LeaseGeneration, nil)
	require.NoError(t, err)

	// Worker A's stale, late PERMANENT failure report must be rejected,
	// not silently accepted and cascaded.
	_, err = s.CompleteFailure(ctx, x.JobID, "worker-A", staleGen, "too late", "PERMANENT")
	require.ErrorIs(t, err, store.ErrStaleTransition)

	yAfter, err := s.GetByID(ctx, y.JobID)
	require.NoError(t, err)
	require.NotEqual(t, jobstate.Cancelled, yAfter.State, "Y must not be cancelled by a rejected stale failure report")
}

func TestRestart_WorkflowProgressSurvivesFreshStoreInstance(t *testing.T) {
	db := testutil.DB(t)
	s1 := store.New(db)
	ctx := context.Background()

	inst, err := s1.CreateWorkflow(ctx, diamondSpec())
	require.NoError(t, err)
	a := nodeByKey(t, inst, "A")

	j, ok, err := s1.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, j.ID)
	_, err = s1.CompleteSuccess(ctx, j.ID, "w1", j.LeaseGeneration, nil)
	require.NoError(t, err)

	// A brand-new *store.Store sharing only the database (standing in for
	// a fully restarted process fleet, per SF-018's shape) must correctly
	// see B and C as claimable, with no in-memory workflow coordinator
	// state anywhere to lose.
	s2 := store.New(db)
	claimedIDs := map[uuid.UUID]bool{}
	for i := 0; i < 2; i++ {
		j, ok, err := s2.Claim(ctx, "w2")
		require.NoError(t, err)
		require.True(t, ok)
		claimedIDs[j.ID] = true
	}
	b, c := nodeByKey(t, inst, "B"), nodeByKey(t, inst, "C")
	require.True(t, claimedIDs[b.JobID])
	require.True(t, claimedIDs[c.JobID])

	wf, err := s2.GetWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Running, wf.State)
}

func TestSchedulingInteraction_ActivationRespectsFutureSchedule(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	future := time.Now().Add(time.Hour)
	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
	}}
	spec.Nodes[1].ScheduledAt = &future
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B")

	j, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, j.ID)
	_, err = s.CompleteSuccess(ctx, j.ID, "w1", j.LeaseGeneration, nil)
	require.NoError(t, err)

	// B's dependency is satisfied, but its own scheduled_at is still an
	// hour away -- dependency satisfaction must not bypass scheduling.
	_, ok, err = s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.False(t, ok, "B must wait for its own scheduled_at even though its dependency is satisfied")

	bJob, err := s.GetByID(ctx, b.JobID)
	require.NoError(t, err)
	require.WithinDuration(t, future, bJob.EligibleAt, time.Second, "activation must set eligible_at to the node's own scheduled_at, not now()")
}

func TestSchedulingInteraction_PastScheduleActivatesImmediately(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B", "A"),
	}}
	spec.Nodes[1].ScheduledAt = &past
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B")

	j, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, j.ID)
	_, err = s.CompleteSuccess(ctx, j.ID, "w1", j.LeaseGeneration, nil)
	require.NoError(t, err)

	got, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok, "a scheduled_at already in the past must not further delay activation")
	require.Equal(t, b.JobID, got.ID)
}

func TestGetWorkflow_NotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.GetWorkflow(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestCancelWorkflow_NotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.CancelWorkflow(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestGetWorkflow_DependsOnResolvedToNodeKeys(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	inst, err := s.CreateWorkflow(ctx, diamondSpec())
	require.NoError(t, err)

	d := nodeByKey(t, inst, "D")
	require.ElementsMatch(t, []string{"B", "C"}, d.DependsOn)
}

// TestMultiWorker_FanOutClaimedSafelyUnderContention is this phase's
// multi-worker fan-out safety test: N workers race to claim B/C/D-style
// fan-out children the instant they become eligible; no child is ever
// claimed twice, and every eligible child is claimed by exactly one
// worker.
func TestMultiWorker_FanOutClaimedSafelyUnderContention(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const fanOut = 8
	nodes := []workflow.NodeSpec{wfNode("root")}
	for i := 0; i < fanOut; i++ {
		nodes = append(nodes, wfNode(uuid.New().String(), "root"))
	}
	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: nodes})
	require.NoError(t, err)
	root := nodeByKey(t, inst, "root")

	j, ok, err := s.Claim(ctx, "w0")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, root.JobID, j.ID)
	_, err = s.CompleteSuccess(ctx, j.ID, "w0", j.LeaseGeneration, nil)
	require.NoError(t, err)

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[uuid.UUID]int{}
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for {
				j, ok, err := s.Claim(ctx, uuid.New().String())
				require.NoError(t, err)
				if !ok {
					return
				}
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	require.Len(t, claimed, fanOut, "every fan-out child must be claimed exactly once, by exactly one worker")
	for id, n := range claimed {
		require.Equal(t, 1, n, "job %s claimed %d times", id, n)
	}
}

func TestCreateWorkflow_MissingDependencyRejectedByStore(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("A", "ghost")}})
	require.Error(t, err)
	require.ErrorIs(t, err, workflow.ErrUnknownDependency)
}

func TestCreateWorkflow_DuplicateNodeKeyRejectedByStore(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("A"), wfNode("A")}})
	require.Error(t, err)
	require.ErrorIs(t, err, workflow.ErrDuplicateNodeKey)
}

// TestMixedRetryAndSuccess_FanInWaitsForBothOutcomes is adversarial case
// #5 from this task's audit list: one fan-in predecessor retries while
// the other succeeds. The dependent must remain blocked until the
// retrying predecessor ALSO eventually succeeds — a mix of "succeeded"
// and "retrying" is not "all succeeded."
func TestMixedRetryAndSuccess_FanInWaitsForBothOutcomes(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		wfNode("A"),
		wfNode("B"),
		wfNode("D", "A", "B"),
	}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b, d := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B"), nodeByKey(t, inst, "D")

	var aClaim, bClaim jobRow
	for i := 0; i < 2; i++ {
		j, ok, err := s.Claim(ctx, "w1")
		require.NoError(t, err)
		require.True(t, ok)
		switch j.ID {
		case a.JobID:
			aClaim = jobRow{id: j.ID, gen: j.LeaseGeneration}
		case b.JobID:
			bClaim = jobRow{id: j.ID, gen: j.LeaseGeneration}
		}
	}
	require.NotEqual(t, uuid.Nil, aClaim.id)
	require.NotEqual(t, uuid.Nil, bClaim.id)

	// A retries (long backoff, deterministically not yet eligible); B
	// succeeds outright.
	result, err := s.CompleteRetryableFailure(ctx, aClaim.id, "w1", aClaim.gen, "transient", time.Hour)
	require.NoError(t, err)
	require.Equal(t, jobstate.RetryWait, result.State)
	_, err = s.CompleteSuccess(ctx, bClaim.id, "w1", bClaim.gen, nil)
	require.NoError(t, err)

	// D must still be blocked: A has not succeeded, only retried.
	_, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.False(t, ok, "D must remain blocked while A is RETRY_WAIT, even though B already succeeded")

	// A's retry eventually succeeds too.
	forceSetEligibleAt(t, db, a.JobID, -time.Second)
	aRetryClaim, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, aRetryClaim.ID)
	_, err = s.CompleteSuccess(ctx, aRetryClaim.ID, "w1", aRetryClaim.LeaseGeneration, nil)
	require.NoError(t, err)

	got, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok, "D becomes claimable only once BOTH A and B have reached SUCCEEDED")
	require.Equal(t, d.JobID, got.ID)
}

// TestFaultInjection_SuccessAndPropagationRollbackTogether is adversarial
// cases #10 and #11: because a predecessor's success transition and its
// dependency-propagation step run inside ONE transaction
// (propagateWorkflowTransition is called before that transaction
// commits — see complete.go's CompleteSuccess), "worker crashes after
// the success decision but before the downstream propagation commits" is
// not a distinct window that can occur: either both happen, or (if the
// transaction aborts for any reason, injected here directly) neither
// does. This test forces exactly that abort and proves neither the
// predecessor's SUCCEEDED transition nor the dependent's activation
// survive it.
func TestFaultInjection_SuccessAndPropagationRollbackTogether(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("A"), wfNode("B", "A")}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	a, b := nodeByKey(t, inst, "A"), nodeByKey(t, inst, "B")

	claimed, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.JobID, claimed.ID)

	beforeA, err := s.GetByID(ctx, a.JobID)
	require.NoError(t, err)
	beforeB, err := s.GetByID(ctx, b.JobID)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)

	// Replicate CompleteSuccess's own fenced UPDATE for A.
	_, err = tx.ExecContext(ctx, `
		UPDATE jobs SET state='SUCCEEDED', lease_owner=NULL, lease_expires_at=NULL, terminal_at=now(), updated_at=now(), version=version+1
		WHERE id=$1 AND lease_owner=$2 AND lease_generation=$3 AND state='RUNNING'`,
		claimed.ID, "w1", claimed.LeaseGeneration)
	require.NoError(t, err)

	// Replicate propagateWorkflowTransition's activation of B, in the
	// SAME transaction.
	_, err = tx.ExecContext(ctx, `
		UPDATE jobs SET eligible_at = COALESCE(scheduled_at, now()), updated_at = now(), version = version + 1
		WHERE id = $1 AND state = 'QUEUED'`, b.JobID)
	require.NoError(t, err)

	// Force the transaction to abort before commit (a stand-in for a
	// crashed connection, a constraint violation, or any other
	// mid-transaction failure).
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'NOT_A_REAL_STATE' WHERE id = $1`, claimed.ID)
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	afterA, err := s.GetByID(ctx, a.JobID)
	require.NoError(t, err)
	afterB, err := s.GetByID(ctx, b.JobID)
	require.NoError(t, err)

	require.Equal(t, beforeA.State, afterA.State, "A must remain RUNNING -- the success decision must not survive a rolled-back transaction")
	require.Equal(t, beforeA.Version, afterA.Version)
	require.Equal(t, beforeB.State, afterB.State)
	require.True(t, beforeB.EligibleAt.Equal(afterB.EligibleAt), "B's activation must not survive a rolled-back predecessor transaction")
	require.Equal(t, beforeB.Version, afterB.Version)

	// A is still legitimately RUNNING and can be completed for real
	// afterward -- the rollback did not corrupt or strand it.
	_, err = s.CompleteSuccess(ctx, claimed.ID, "w1", claimed.LeaseGeneration, nil)
	require.NoError(t, err)
	finalB, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, b.JobID, finalB.ID)
}

// TestCancelWorkflow_RaceLostByCancellation_WorkflowStaysSucceeded is a
// regression test for a real defect this task's adversarial audit
// (case #13: "workflow cancellation races with node completion") caught
// during Phase 7 development: finalizeWorkflowIfComplete originally let
// workflow_instances.cancel_requested unconditionally force the
// workflow's terminal state to CANCELLED, even when every node had
// actually reached SUCCEEDED because its completion legitimately won the
// TF-INV-010 race against a cancellation that only *requested*
// cancellation of a RUNNING node. Fixed to check "did every node
// succeed" before consulting cancel_requested at all.
func TestCancelWorkflow_RaceLostByCancellation_WorkflowStaysSucceeded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{wfNode("solo")}}
	inst, err := s.CreateWorkflow(ctx, spec)
	require.NoError(t, err)
	solo := nodeByKey(t, inst, "solo")

	j, ok, err := s.Claim(ctx, "w1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, solo.JobID, j.ID)

	// Cancellation is requested while the node is RUNNING...
	wf, err := s.CancelWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Running, wf.State)
	require.True(t, wf.CancelRequested)

	// ...but the worker's completion commits before any acknowledgement
	// of the cancellation -- TF-INV-010's "first durable terminal write
	// wins" rule means this SUCCEEDED transition is entirely legitimate.
	_, err = s.CompleteSuccess(ctx, j.ID, "w1", j.LeaseGeneration, nil)
	require.NoError(t, err)

	final, err := s.GetByID(ctx, solo.JobID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Succeeded, final.State)

	wfFinal, err := s.GetWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Succeeded, wfFinal.State, "a workflow whose cancellation lost every race must be SUCCEEDED, never CANCELLED, once every node has actually succeeded")
}

// TestLargeDAG_ExecutesCorrectlyUnderMultipleWorkers is adversarial case
// #18: a large but CI-safe DAG (5 layers, ~30 nodes) executes correctly
// to completion under several concurrent, real workers, with no
// duplicate claim and no stalled node.
func TestLargeDAG_ExecutesCorrectlyUnderMultipleWorkers(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const layers = 5
	const perLayer = 6
	var nodes []workflow.NodeSpec
	prevLayer := []string{"root"}
	nodes = append(nodes, wfNode("root"))
	for l := 0; l < layers; l++ {
		var thisLayer []string
		for i := 0; i < perLayer; i++ {
			key := uuid.New().String()
			nodes = append(nodes, wfNode(key, prevLayer...))
			thisLayer = append(thisLayer, key)
		}
		prevLayer = thisLayer
	}
	// A final sink depending on the last layer (fan-in of perLayer nodes).
	nodes = append(nodes, wfNode("sink", prevLayer...))

	inst, err := s.CreateWorkflow(ctx, workflow.GraphSpec{Nodes: nodes})
	require.NoError(t, err)
	require.Len(t, inst.Nodes, 1+layers*perLayer+1)

	total := len(inst.Nodes)
	const workers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[uuid.UUID]int{}
	errCh := make(chan error, workers)

	// Termination is driven by global completion progress (the shared
	// claimed count reaching every node in the DAG), not a per-worker
	// idle counter -- a worker that finds nothing claimable keeps
	// retrying as long as the DAG as a whole is still incomplete, so a
	// slow neighbor mid-transaction can never cause another worker to
	// give up prematurely.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		workerID := uuid.New().String()
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				done := len(claimed) >= total
				mu.Unlock()
				if done {
					return
				}
				j, ok, err := s.Claim(ctx, workerID)
				if err != nil {
					errCh <- err
					return
				}
				if !ok {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
				_, err = s.CompleteSuccess(ctx, j.ID, workerID, j.LeaseGeneration, nil)
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("TestLargeDAG_ExecutesCorrectlyUnderMultipleWorkers: workers did not finish within 30s -- possible stall")
	}
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	require.Len(t, claimed, len(inst.Nodes), "every node in the DAG must be claimed exactly once")
	for id, n := range claimed {
		require.Equal(t, 1, n, "job %s claimed %d times", id, n)
	}

	wf, err := s.GetWorkflow(ctx, inst.ID)
	require.NoError(t, err)
	require.Equal(t, workflow.Succeeded, wf.State)
	for _, n := range wf.Nodes {
		require.Equal(t, jobstate.Succeeded, n.JobState, "node %q", n.NodeKey)
	}
}
