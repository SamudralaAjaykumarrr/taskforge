// Package workflow is the pure, database-free representation of Phase 7's
// workflow/DAG model, per docs/workflows.md: workflow-level state
// (distinct from the job-level state machine in internal/jobstate, per
// that document's explicit "separate state space" note) and DAG
// structural validation (docs/workflows.md's dependency model, TF-INV-012).
//
// Persistence lives in internal/store (internal/store/workflow.go), which
// builds workflow_instances/workflow_nodes rows from the types here and
// drives dependency-satisfaction propagation through the existing
// internal/jobstate-governed jobs table -- exactly as docs/workflows.md
// requires: "No new claiming mechanism is introduced for workflow nodes."
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
)

// State is a workflow-instance-level state, per docs/workflows.md: "a
// separate state space from the job-level state enum ... a workflow's
// FAILED means 'at least one required node dead-lettered or was
// cancelled, causing the workflow to not complete successfully.'"
type State string

const (
	Running   State = "RUNNING"
	Succeeded State = "SUCCEEDED"
	Failed    State = "FAILED"
	Cancelled State = "CANCELLED"
)

// IsTerminal reports whether s has no legal outbound transition. Mirrors
// internal/jobstate.IsTerminal's role for the job-level state machine, but
// for the workflow-level one -- see docs/workflows.md's "Workflow-Level
// Cancellation" and the "Terminal workflow states must not reopen"
// requirement this package's callers (internal/store) enforce via a
// guarded `WHERE state = 'RUNNING'` UPDATE.
func IsTerminal(s State) bool {
	switch s {
	case Succeeded, Failed, Cancelled:
		return true
	default:
		return false
	}
}

// NodeSpec is one caller-submitted node in a workflow graph at submission
// time, keyed by a caller-chosen NodeKey (unique within the submission)
// rather than a server-generated ID -- this is what lets DependsOn
// reference sibling nodes before any database identifiers exist, per
// docs/workflows.md's model ("workflow_nodes: one row per node ...
// referencing ... a list of predecessor node IDs").
type NodeSpec struct {
	NodeKey                 string
	JobType                 string
	Payload                 json.RawMessage
	MaxAttempts             int
	ExecutionTimeoutSeconds int
	ScheduledAt             *time.Time
	DependsOn               []string
}

// GraphSpec is a full workflow submission: every node in one logical DAG.
type GraphSpec struct {
	Nodes []NodeSpec
}

// Sentinel reasons wrapped by InvalidGraphError. These are graph-structure
// concerns only (docs/workflows.md's dependency model) -- ordinary job
// field validation (job_type presence/length, payload JSON-validity,
// max_attempts/execution_timeout_seconds bounds) is the API layer's
// concern, exactly as it already is for a plain POST /jobs submission
// (internal/api.validateCreateJobRequest), not duplicated here.
var (
	ErrEmptyGraph          = errors.New("workflow: nodes must contain at least one entry")
	ErrMissingNodeKey      = errors.New("workflow: node_key is required")
	ErrDuplicateNodeKey    = errors.New("workflow: duplicate node_key")
	ErrMissingJobType      = errors.New("workflow: job_type is required")
	ErrDuplicateDependency = errors.New("workflow: a node lists the same dependency more than once")
	ErrUnknownDependency   = errors.New("workflow: depends_on references an unknown node_key")
	ErrSelfDependency      = errors.New("workflow: a node cannot depend on itself")
	ErrCycle               = errors.New("workflow: dependency graph contains a cycle")
)

// InvalidGraphError wraps every ValidateGraph rejection so callers
// (internal/api) can distinguish "malformed submission" (400 Bad Request)
// from an internal/store failure (500) with a single errors.As check,
// without enumerating every sentinel reason above individually -- mirrors
// internal/handler's classifiedError pattern.
type InvalidGraphError struct {
	err error
}

func (e *InvalidGraphError) Error() string { return e.err.Error() }
func (e *InvalidGraphError) Unwrap() error { return e.err }

func invalid(reason error, format string, args ...any) *InvalidGraphError {
	return &InvalidGraphError{err: fmt.Errorf("%w: %s", reason, fmt.Sprintf(format, args...))}
}

// ValidateGraph checks GraphSpec structural validity per docs/workflows.md
// and the Phase 7 "DAG Validation" contract, entirely in memory, before
// any durable state is created (internal/store.CreateWorkflow calls this
// before opening a transaction) -- an invalid submission never reaches
// PostgreSQL at all, per the requirement that validation "happen before
// acknowledging an invalid workflow" and not "rely on workers discovering
// graph corruption later."
//
// Checked, in order: the graph is non-empty; every node has a non-empty,
// unique node_key and a non-empty job_type; every depends_on entry
// references a node_key that exists in this same submission, is not the
// node's own key (self-dependency), and is not repeated within one node's
// own depends_on list; and the resulting dependency graph contains no
// directed cycle. Cycle detection is a deterministic depth-first walk in
// submission order (both across nodes and within each node's own
// depends_on list), so the same invalid graph always reports the same
// cycle, not merely "some" cycle.
func ValidateGraph(g GraphSpec) error {
	if len(g.Nodes) == 0 {
		return invalid(ErrEmptyGraph, "got 0")
	}

	byKey := make(map[string]NodeSpec, len(g.Nodes))
	order := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		key := strings.TrimSpace(n.NodeKey)
		if key == "" {
			return invalid(ErrMissingNodeKey, "every node requires a non-empty node_key")
		}
		if _, exists := byKey[key]; exists {
			return invalid(ErrDuplicateNodeKey, "%q", key)
		}
		if strings.TrimSpace(n.JobType) == "" {
			return invalid(ErrMissingJobType, "node %q", key)
		}
		byKey[key] = n
		order = append(order, key)
	}

	for _, key := range order {
		n := byKey[key]
		seen := make(map[string]bool, len(n.DependsOn))
		for _, dep := range n.DependsOn {
			if dep == key {
				return invalid(ErrSelfDependency, "node %q", key)
			}
			if seen[dep] {
				return invalid(ErrDuplicateDependency, "node %q lists %q more than once", key, dep)
			}
			seen[dep] = true
			if _, ok := byKey[dep]; !ok {
				return invalid(ErrUnknownDependency, "node %q depends on %q", key, dep)
			}
		}
	}

	if cyclePath := findCycle(order, byKey); cyclePath != "" {
		return invalid(ErrCycle, "%s", cyclePath)
	}

	return nil
}

// findCycle runs a deterministic three-color DFS over the dependency
// graph (edges point from a node to each of its predecessors, per
// DependsOn) and returns a human-readable description of the first cycle
// found (in submission order), or "" if the graph is acyclic. Edge
// direction is irrelevant to whether a cycle exists, so walking
// "dependent -> predecessor" edges finds exactly the same cycles as
// walking the reverse would.
func findCycle(order []string, byKey map[string]NodeSpec) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(order))
	var stack []string

	var visit func(key string) string
	visit = func(key string) string {
		color[key] = gray
		stack = append(stack, key)
		for _, dep := range byKey[key].DependsOn {
			switch color[dep] {
			case white:
				if path := visit(dep); path != "" {
					return path
				}
			case gray:
				return cyclePathString(stack, dep)
			}
		}
		stack = stack[:len(stack)-1]
		color[key] = black
		return ""
	}

	for _, key := range order {
		if color[key] == white {
			if path := visit(key); path != "" {
				return path
			}
		}
	}
	return ""
}

// cyclePathString renders the cycle found on stack, starting from where
// closeAt (the node the DFS just found already on the stack, i.e. gray)
// first appears, back through to the current top of stack, and closing
// the loop back to closeAt -- e.g. "A -> B -> C -> A".
func cyclePathString(stack []string, closeAt string) string {
	start := 0
	for i, k := range stack {
		if k == closeAt {
			start = i
			break
		}
	}
	segment := append(append([]string{}, stack[start:]...), closeAt)
	return strings.Join(segment, " -> ")
}

// Instance is the durable workflow_instances row plus its resolved nodes
// -- the read model internal/store's workflow methods return, and
// internal/api serializes for POST/GET /workflows responses.
type Instance struct {
	ID                uuid.UUID
	State             State
	CancelRequested   bool
	CancelRequestedAt *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	TerminalAt        *time.Time
	Nodes             []Node
}

// Node is one workflow_nodes row joined with its underlying job's current
// state -- enough for a caller to see per-node progress without a
// separate GET /jobs/{id} call per node.
type Node struct {
	ID             uuid.UUID
	NodeKey        string
	JobID          uuid.UUID
	DependsOn      []string // predecessor node_keys, resolved from the stored predecessor node IDs
	DependsOnIDs   []uuid.UUID
	JobState       jobstate.State
	AttemptCount   int
	LastError      *string
	LastErrorClass *string
}
