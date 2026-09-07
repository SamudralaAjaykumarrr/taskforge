package workflow_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

func node(key string, deps ...string) workflow.NodeSpec {
	return workflow.NodeSpec{
		NodeKey:                 key,
		JobType:                 "test.job",
		Payload:                 json.RawMessage(`{}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		DependsOn:               deps,
	}
}

func TestValidateGraph_EmptyGraphRejected(t *testing.T) {
	err := workflow.ValidateGraph(workflow.GraphSpec{})
	require.Error(t, err)
	require.ErrorIs(t, err, workflow.ErrEmptyGraph)
	var invalid *workflow.InvalidGraphError
	require.ErrorAs(t, err, &invalid)
}

func TestValidateGraph_LinearChainAccepted(t *testing.T) {
	// A -> B -> C
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A"),
		node("B", "A"),
		node("C", "B"),
	}}
	require.NoError(t, workflow.ValidateGraph(g))
}

func TestValidateGraph_DiamondAccepted(t *testing.T) {
	//   A
	//  / \
	// B   C
	//  \ /
	//   D
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A"),
		node("B", "A"),
		node("C", "A"),
		node("D", "B", "C"),
	}}
	require.NoError(t, workflow.ValidateGraph(g))
}

func TestValidateGraph_FanOutAccepted(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A"),
		node("B", "A"),
		node("C", "A"),
		node("D", "A"),
	}}
	require.NoError(t, workflow.ValidateGraph(g))
}

func TestValidateGraph_MissingNodeKeyRejected(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{node("")}}
	err := workflow.ValidateGraph(g)
	require.ErrorIs(t, err, workflow.ErrMissingNodeKey)
}

func TestValidateGraph_MissingJobTypeRejected(t *testing.T) {
	n := node("A")
	n.JobType = "  "
	err := workflow.ValidateGraph(workflow.GraphSpec{Nodes: []workflow.NodeSpec{n}})
	require.ErrorIs(t, err, workflow.ErrMissingJobType)
}

func TestValidateGraph_DuplicateNodeKeyRejected(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{node("A"), node("A")}}
	err := workflow.ValidateGraph(g)
	require.ErrorIs(t, err, workflow.ErrDuplicateNodeKey)
}

func TestValidateGraph_SelfDependencyRejected(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{node("A", "A")}}
	err := workflow.ValidateGraph(g)
	require.ErrorIs(t, err, workflow.ErrSelfDependency)
}

func TestValidateGraph_UnknownDependencyRejected(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{node("A", "ghost")}}
	err := workflow.ValidateGraph(g)
	require.ErrorIs(t, err, workflow.ErrUnknownDependency)
}

func TestValidateGraph_DuplicateDependencyRejected(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A"),
		node("B", "A", "A"),
	}}
	err := workflow.ValidateGraph(g)
	require.ErrorIs(t, err, workflow.ErrDuplicateDependency)
}

func TestValidateGraph_DirectCycleRejected(t *testing.T) {
	// A -> B -> A
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A", "B"),
		node("B", "A"),
	}}
	err := workflow.ValidateGraph(g)
	require.ErrorIs(t, err, workflow.ErrCycle)
}

func TestValidateGraph_LongerCycleRejected(t *testing.T) {
	// A -> B -> C -> A
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A", "C"),
		node("B", "A"),
		node("C", "B"),
	}}
	err := workflow.ValidateGraph(g)
	require.ErrorIs(t, err, workflow.ErrCycle)
}

func TestValidateGraph_CycleDetectionIsDeterministic(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A", "C"),
		node("B", "A"),
		node("C", "B"),
	}}
	var first error
	for i := 0; i < 20; i++ {
		err := workflow.ValidateGraph(g)
		require.Error(t, err)
		if first == nil {
			first = err
		} else {
			require.Equal(t, first.Error(), err.Error(), "cycle detection must report the same cycle every time for the same input")
		}
	}
}

func TestValidateGraph_SingleRootNodeAccepted(t *testing.T) {
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{node("solo")}}
	require.NoError(t, workflow.ValidateGraph(g))
}

func TestValidateGraph_DisconnectedComponentsAccepted(t *testing.T) {
	// Two independent chains in one submission: A->B, and C (root, alone).
	g := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		node("A"),
		node("B", "A"),
		node("C"),
	}}
	require.NoError(t, workflow.ValidateGraph(g))
}

func TestIsTerminal(t *testing.T) {
	require.False(t, workflow.IsTerminal(workflow.Running))
	require.True(t, workflow.IsTerminal(workflow.Succeeded))
	require.True(t, workflow.IsTerminal(workflow.Failed))
	require.True(t, workflow.IsTerminal(workflow.Cancelled))
}

func TestInvalidGraphError_UnwrapsToSentinel(t *testing.T) {
	err := workflow.ValidateGraph(workflow.GraphSpec{Nodes: []workflow.NodeSpec{node("A", "A")}})
	require.True(t, errors.Is(err, workflow.ErrSelfDependency))
	var invalid *workflow.InvalidGraphError
	require.True(t, errors.As(err, &invalid))
}
