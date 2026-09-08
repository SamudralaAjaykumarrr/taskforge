// Phase 7: durable workflow/DAG persistence, per docs/workflows.md and
// TF-INV-012. This file adds exactly three durable operations
// (CreateWorkflow, GetWorkflow, CancelWorkflow) plus the internal
// dependency-propagation mechanism (propagateWorkflowTransition) invoked
// from every existing job-completion path that can land a job on a
// terminal state (CompleteSuccess, CompleteFailure,
// completeRetryableOutcome's exhaustion branch, CompleteCancelled,
// CancelQueuedOrRetryWait, and the Lazy Dead-Letter Sweep) — all in
// complete.go, retry.go, cancellation.go, and claim.go respectively.
//
// No new claiming mechanism, lease mechanism, retry mechanism, or
// execution engine is introduced: a workflow node is, from the worker's
// point of view, an ordinary job row. Dependency gating is implemented
// entirely as a jobs.eligible_at value (blockedEligibleAt below) that the
// existing claim query already treats as "not yet eligible" — see
// migration 0003's comment for why this sentinel, not PostgreSQL's
// 'infinity' timestamptz, was chosen.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

// blockedEligibleAt is the fixed sentinel used to keep a workflow node's
// underlying job durably QUEUED but never claimable until every one of
// its dependencies is satisfied. It is a concrete, portable timestamp
// value (year 9999) deliberately far enough in the future that it will
// never be reached in practice, rather than PostgreSQL's special
// 'infinity' timestamptz value — see migration 0003's comment for why.
var blockedEligibleAt = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

// uuidArrayLiteral renders ids as a PostgreSQL array-literal string (e.g.
// "{id1,id2}", or "{}" for an empty slice) suitable for a `$n::uuid[]`
// query parameter. This sidesteps any question of whether the database/
// sql driver in use (pgx/v5's stdlib wrapper) natively marshals
// []uuid.UUID as a PostgreSQL array — PostgreSQL's own array input
// function parses this text form identically regardless of protocol, so
// passing a plain string parameter with an explicit cast is portable and
// requires no driver-specific array support on either the write or read
// side (see parseUUIDArrayLiteral for the read side, which similarly
// reads the column back with an explicit ::text cast).
func uuidArrayLiteral(ids []uuid.UUID) string {
	if len(ids) == 0 {
		return "{}"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// parseUUIDArrayLiteral parses a PostgreSQL array-literal string (as
// produced by casting a uuid[] column to ::text) back into a slice of
// uuid.UUID. The empty array "{}" parses to a nil slice.
func parseUUIDArrayLiteral(s string) ([]uuid.UUID, error) {
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]uuid.UUID, 0, len(parts))
	for _, p := range parts {
		id, err := uuid.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("parse uuid array literal: %w", err)
		}
		out = append(out, id)
	}
	return out, nil
}

// logWorkflowFinalized logs a workflow's terminal-state transition, per
// propagateWorkflowTransition's doc comment: callers invoke this ONLY
// after their own transaction (which is what actually finalized the
// workflow) has committed. A no-op if finalState is "" (this call did
// not finalize a workflow -- either it is not yet complete, or a
// concurrent call already did).
func (s *Store) logWorkflowFinalized(workflowID uuid.UUID, finalState string) {
	if finalState == "" {
		return
	}
	s.logger.Info("workflow finalized",
		"event", "workflow_finalized", "workflow_id", workflowID.String(), "state", finalState)
}

// CreateWorkflow durably and atomically creates a workflow instance, every
// one of its nodes' underlying jobs, and the workflow_nodes rows linking
// them, per docs/workflows.md's model — all inside a single PostgreSQL
// transaction, so a caller never observes a workflow that exists with
// only some of its nodes, or nodes whose dependency edges are missing
// (the "ATOMIC WORKFLOW CREATION" requirement). g is validated with
// workflow.ValidateGraph BEFORE the transaction is opened (before any SQL
// is issued at all), per the "DAG VALIDATION" requirement that an invalid
// submission never reaches durable state and is never acknowledged.
//
// A node with no dependencies (a root node) is inserted immediately
// eligible (eligible_at = COALESCE(its own scheduled_at, now())) — an
// ordinary claimable QUEUED job from the moment this transaction commits.
// A node with one or more dependencies is inserted with eligible_at =
// blockedEligibleAt, made eligible later by propagateWorkflowTransition
// once every one of its dependencies' jobs has reached SUCCEEDED (see
// that function's doc comment for the exact algorithm, including the
// concurrent-fan-in locking argument).
func (s *Store) CreateWorkflow(ctx context.Context, g workflow.GraphSpec) (*workflow.Instance, error) {
	if err := workflow.ValidateGraph(g); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: create workflow: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	workflowID := uuid.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_instances (id, state) VALUES ($1, 'RUNNING')`, workflowID); err != nil {
		return nil, fmt.Errorf("store: create workflow: insert instance: %w", err)
	}

	nodeIDs := make(map[string]uuid.UUID, len(g.Nodes))
	for _, n := range g.Nodes {
		nodeIDs[n.NodeKey] = uuid.New()
	}

	jobTypes := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		jobID := uuid.New()
		blocked := len(n.DependsOn) > 0
		if err := insertWorkflowNodeJob(ctx, tx, jobID, n, blocked); err != nil {
			return nil, fmt.Errorf("store: create workflow: insert node %q job: %w", n.NodeKey, err)
		}
		jobTypes = append(jobTypes, n.JobType)

		depIDs := make([]uuid.UUID, 0, len(n.DependsOn))
		for _, dep := range n.DependsOn {
			depIDs = append(depIDs, nodeIDs[dep])
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_nodes (id, workflow_instance_id, node_key, job_id, depends_on)
			VALUES ($1, $2, $3, $4, $5::uuid[])`,
			nodeIDs[n.NodeKey], workflowID, n.NodeKey, jobID, uuidArrayLiteral(depIDs),
		); err != nil {
			return nil, fmt.Errorf("store: create workflow: insert node %q: %w", n.NodeKey, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: create workflow: commit: %w", err)
	}

	// Phase 8: each workflow node's underlying job is a submission like
	// any other (docs/observability.md's taskforge_jobs_submitted_total),
	// recorded only now that the whole atomic creation has actually
	// committed. Workflow creation has no idempotency-key contract (see
	// this file's package doc comment), so
	// taskforge_idempotent_submission_hits_total never applies here.
	for _, jt := range jobTypes {
		s.metrics.JobsSubmittedTotal.WithLabelValues(jt).Inc()
	}
	s.logger.Info("workflow created",
		"event", "workflow_created", "workflow_id", workflowID.String(), "node_count", len(g.Nodes))

	return s.GetWorkflow(ctx, workflowID)
}

// insertWorkflowNodeJob inserts the underlying jobs row for one workflow
// node, directly (not via InsertIdempotent — workflow node submission has
// no documented Idempotency-Key contract of its own in docs/workflows.md,
// so none is invented here per the task's "do not invent an undocumented
// contract" instruction; workflow creation is not idempotent in v1). The
// eligible_at CASE keeps the "blocked vs. immediately eligible" decision
// inside PostgreSQL's own now(), never an application-local clock read,
// consistent with docs/failure-model.md's Clock Model — blockedEligibleAt
// itself is a fixed constant, not a clock reading, so passing it as a
// literal parameter does not reintroduce a clock-skew dependency.
func insertWorkflowNodeJob(ctx context.Context, tx *sql.Tx, jobID uuid.UUID, n workflow.NodeSpec, blocked bool) error {
	var scheduledAt sql.NullTime
	if n.ScheduledAt != nil {
		scheduledAt = sql.NullTime{Time: *n.ScheduledAt, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, max_attempts, execution_timeout_seconds, scheduled_at, eligible_at)
		VALUES ($1, $2, $3, 'QUEUED', $4, $5, $6, CASE WHEN $7 THEN $8 ELSE COALESCE($6, now()) END)`,
		jobID, n.JobType, []byte(n.Payload), n.MaxAttempts, n.ExecutionTimeoutSeconds, scheduledAt, blocked, blockedEligibleAt,
	)
	return err
}

// GetWorkflow returns the current durable representation of a workflow
// instance and all of its nodes (each joined with its underlying job's
// current state), or ErrNotFound if no such workflow exists. Like
// GetByID, this is a plain read: no locking, no side effects.
func (s *Store) GetWorkflow(ctx context.Context, id uuid.UUID) (*workflow.Instance, error) {
	var inst workflow.Instance
	var state string
	var cancelReqAt, terminalAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, state, cancel_requested, cancel_requested_at, created_at, updated_at, terminal_at
		FROM workflow_instances WHERE id = $1`, id,
	).Scan(&inst.ID, &state, &inst.CancelRequested, &cancelReqAt, &inst.CreatedAt, &inst.UpdatedAt, &terminalAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get workflow %s: %w", id, err)
	}
	inst.State = workflow.State(state)
	if cancelReqAt.Valid {
		inst.CancelRequestedAt = &cancelReqAt.Time
	}
	if terminalAt.Valid {
		inst.TerminalAt = &terminalAt.Time
	}

	nodes, err := loadWorkflowNodes(ctx, s.db, id)
	if err != nil {
		return nil, fmt.Errorf("store: get workflow %s: %w", id, err)
	}
	inst.Nodes = nodes
	return &inst, nil
}

// nodeQuerier is satisfied by *sql.DB and *sql.Tx.
type nodeQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadWorkflowNodes reads every workflow_nodes row for workflowID, joined
// with its underlying job's current state, and resolves each node's
// predecessor IDs back to their node_key strings (for API friendliness —
// callers submitted dependencies by key, so responses echo the same
// vocabulary rather than raw internal UUIDs).
func loadWorkflowNodes(ctx context.Context, q nodeQuerier, workflowID uuid.UUID) ([]workflow.Node, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT wn.id, wn.node_key, wn.job_id, wn.depends_on::text,
			j.state, j.attempt_count, j.last_error, j.last_error_class
		FROM workflow_nodes wn JOIN jobs j ON j.id = wn.job_id
		WHERE wn.workflow_instance_id = $1
		ORDER BY wn.created_at ASC, wn.id ASC`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("load workflow nodes: %w", err)
	}
	defer rows.Close()

	type rawNode struct {
		node   workflow.Node
		depIDs []uuid.UUID
	}
	var raws []rawNode
	keyByID := make(map[uuid.UUID]string)

	for rows.Next() {
		var n workflow.Node
		var depText, stateStr string
		var lastError, lastErrorClass sql.NullString
		if err := rows.Scan(&n.ID, &n.NodeKey, &n.JobID, &depText, &stateStr, &n.AttemptCount, &lastError, &lastErrorClass); err != nil {
			return nil, fmt.Errorf("load workflow nodes: scan: %w", err)
		}
		n.JobState = jobstate.State(stateStr)
		if lastError.Valid {
			n.LastError = &lastError.String
		}
		if lastErrorClass.Valid {
			n.LastErrorClass = &lastErrorClass.String
		}
		depIDs, err := parseUUIDArrayLiteral(depText)
		if err != nil {
			return nil, fmt.Errorf("load workflow nodes: %w", err)
		}
		keyByID[n.ID] = n.NodeKey
		raws = append(raws, rawNode{node: n, depIDs: depIDs})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load workflow nodes: %w", err)
	}

	out := make([]workflow.Node, 0, len(raws))
	for _, r := range raws {
		n := r.node
		n.DependsOnIDs = r.depIDs
		keys := make([]string, 0, len(r.depIDs))
		for _, depID := range r.depIDs {
			keys = append(keys, keyByID[depID])
		}
		n.DependsOn = keys
		out = append(out, n)
	}
	return out, nil
}

// CancelWorkflow implements docs/workflows.md's "Workflow-Level
// Cancellation": cancelling a workflow_instance cancels every one of its
// currently non-terminal nodes' underlying jobs via the exact same
// per-job primitives an ordinary POST /jobs/{id}/cancel uses
// (CancelQueuedOrRetryWait for QUEUED/RETRY_WAIT — which includes a
// dependency-blocked node, since a blocked node's job is QUEUED —  and
// RequestCancellation for RUNNING), never a new workflow-wide cancellation
// primitive. TF-INV-010's race rule applies independently to each node,
// exactly as it does for a standalone job.
//
// If the workflow is already terminal, this is an idempotent no-op that
// reports the workflow's actual current state, mirroring
// docs/worker-protocol.md's job-level cancel contract.
func (s *Store) CancelWorkflow(ctx context.Context, id uuid.UUID) (*workflow.Instance, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancel_requested = true,
			cancel_requested_at = COALESCE(cancel_requested_at, now()),
			updated_at = now()
		WHERE id = $1 AND state = 'RUNNING'`, id)
	if err != nil {
		return nil, fmt.Errorf("store: cancel workflow %s: record request: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.logger.Info("workflow cancellation requested", "event", "workflow_cancellation_requested", "workflow_id", id.String())
	}

	inst, err := s.GetWorkflow(ctx, id)
	if err != nil {
		return nil, err
	}
	if workflow.IsTerminal(inst.State) {
		return inst, nil
	}

	for _, n := range inst.Nodes {
		switch n.JobState {
		case jobstate.Queued, jobstate.RetryWait:
			if _, cerr := s.CancelQueuedOrRetryWait(ctx, n.JobID); cerr != nil && !errors.Is(cerr, ErrStaleTransition) {
				return nil, fmt.Errorf("store: cancel workflow %s: node %q: %w", id, n.NodeKey, cerr)
			}
		case jobstate.Running:
			if _, cerr := s.RequestCancellation(ctx, n.JobID); cerr != nil && !errors.Is(cerr, ErrStaleTransition) {
				return nil, fmt.Errorf("store: cancel workflow %s: node %q: %w", id, n.NodeKey, cerr)
			}
		}
	}

	return s.GetWorkflow(ctx, id)
}

// dependentNode is one direct dependent discovered by dependentsOf: its
// own workflow_nodes.id, the ID of the job it wraps, and its own full
// dependency list (needed to re-evaluate whether ALL of its dependencies
// are now satisfied, not just the one that just transitioned).
type dependentNode struct {
	id        uuid.UUID
	jobID     uuid.UUID
	dependsOn []uuid.UUID
}

// dependentsOf returns every workflow_nodes row that lists nodeID as one
// of its dependencies, ordered by id for a deterministic, consistent lock
// order (see resolveDependent's row-locking comment).
func dependentsOf(ctx context.Context, tx *sql.Tx, nodeID uuid.UUID) ([]dependentNode, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, job_id, depends_on::text
		FROM workflow_nodes
		WHERE depends_on @> ARRAY[$1]::uuid[]
		ORDER BY id ASC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("dependents of %s: %w", nodeID, err)
	}
	defer rows.Close()

	var out []dependentNode
	for rows.Next() {
		var d dependentNode
		var depText string
		if err := rows.Scan(&d.id, &d.jobID, &depText); err != nil {
			return nil, fmt.Errorf("dependents of %s: scan: %w", nodeID, err)
		}
		deps, err := parseUUIDArrayLiteral(depText)
		if err != nil {
			return nil, fmt.Errorf("dependents of %s: %w", nodeID, err)
		}
		d.dependsOn = deps
		out = append(out, d)
	}
	return out, rows.Err()
}

// predecessorStates returns the current job-level state of every node in
// nodeIDs, via a single query (no per-predecessor round trip).
func predecessorStates(ctx context.Context, tx *sql.Tx, nodeIDs []uuid.UUID) ([]string, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT j.state
		FROM workflow_nodes wn JOIN jobs j ON j.id = wn.job_id
		WHERE wn.id = ANY($1::uuid[])`, uuidArrayLiteral(nodeIDs))
	if err != nil {
		return nil, fmt.Errorf("predecessor states: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, len(nodeIDs))
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return nil, fmt.Errorf("predecessor states: scan: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// resolveDependent re-evaluates one dependent node's readiness against
// the CURRENT (as of this call, inside the caller's open transaction)
// state of every one of its predecessors, and applies exactly one of
// three outcomes:
//
//   - Any predecessor DEAD_LETTERED or CANCELLED: the dependent is itself
//     cancelled (docs/workflows.md's Failure Propagation table — a failed
//     or cancelled prerequisite cancels dependents by default, regardless
//     of the OTHER predecessors' state). Returns cascaded=true so the
//     caller's BFS continues into this node's own dependents.
//   - Every predecessor SUCCEEDED: the dependent's job is activated
//     (eligible_at advanced off blockedEligibleAt to
//     COALESCE(scheduled_at, now())) — an ordinary claimable job from
//     this point on. Does not cascade further: activation is not a
//     terminal event, so this node's own dependents cannot yet be
//     evaluated (TF-INV-012's fan-in AND semantics).
//   - Otherwise (still waiting on at least one non-terminal predecessor):
//     no change.
//
// Concurrent-fan-in correctness: the very first statement below takes a
// row lock on the dependent's own job (SELECT ... FOR UPDATE), which is
// what makes "two predecessors of the same fan-in node complete at
// exactly the same instant" resolve correctly rather than both
// transactions concluding "not all predecessors have succeeded yet" and
// neither activating the dependent. Whichever of the two predecessors'
// completion transactions acquires this lock SECOND is guaranteed — by
// PostgreSQL's MVCC snapshot semantics under READ COMMITTED, combined
// with this lock forcing the two transactions to serialize on this
// specific row — to see the other predecessor's already-committed
// terminal state when predecessorStates below runs, and so correctly
// performs the activation (or cascade) that the transaction which
// acquired the lock FIRST could not yet see was warranted. See
// docs/workflows.md's "Dependency Satisfaction Semantics" and the
// TF-INV-012 concurrent-fan-in test coverage in
// internal/store/workflow_test.go.
//
// The `state = 'QUEUED'` guard on both possible UPDATEs makes this
// idempotent under retry/redundant invocation (e.g. a workflow-wide
// cancellation directly cancelling a node that a concurrent cascade has
// already cancelled) and is safe by construction: a node can only be
// found here in a state other than QUEUED if it was already resolved by
// an earlier, already-committed call — see the doc comment on
// propagateWorkflowTransition for why a dependent can never legitimately
// be RUNNING or terminal before all of its predecessors have resolved.
func resolveDependent(ctx context.Context, tx *sql.Tx, dep dependentNode) (cascaded bool, err error) {
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = $1 FOR UPDATE`, dep.jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve dependent %s: lock job: %w", dep.jobID, err)
	}
	if state != string(jobstate.Queued) {
		return false, nil
	}

	predStates, err := predecessorStates(ctx, tx, dep.dependsOn)
	if err != nil {
		return false, fmt.Errorf("resolve dependent %s: %w", dep.jobID, err)
	}

	failed := false
	allSucceeded := len(predStates) > 0
	for _, ps := range predStates {
		switch jobstate.State(ps) {
		case jobstate.DeadLettered, jobstate.Cancelled:
			failed = true
		case jobstate.Succeeded:
			// satisfied; keep checking the rest
		default:
			allSucceeded = false
		}
	}

	switch {
	case failed:
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs
			SET state = 'CANCELLED', terminal_at = now(), updated_at = now(), version = version + 1
			WHERE id = $1 AND state = 'QUEUED'`, dep.jobID)
		if err != nil {
			return false, fmt.Errorf("resolve dependent %s: cancel: %w", dep.jobID, err)
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	case allSucceeded:
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs
			SET eligible_at = COALESCE(scheduled_at, now()), updated_at = now(), version = version + 1
			WHERE id = $1 AND state = 'QUEUED' AND eligible_at = $2`, dep.jobID, blockedEligibleAt)
		if err != nil {
			return false, fmt.Errorf("resolve dependent %s: activate: %w", dep.jobID, err)
		}
		return false, nil
	default:
		return false, nil
	}
}

// propagateWorkflowTransition runs inside the SAME transaction as the
// caller's own fenced job-state UPDATE, immediately after that UPDATE has
// been confirmed to land on a terminal state — see the call sites in
// complete.go (CompleteSuccess, CompleteFailure), retry.go
// (completeRetryableOutcome's exhaustion branch), cancellation.go
// (CompleteCancelled, CancelQueuedOrRetryWait), and claim.go (the Lazy
// Dead-Letter Sweep). If jobID is not a workflow node's underlying job,
// this is a no-op (an ordinary standalone job never touches
// workflow_nodes).
//
// Because activation (in resolveDependent) requires that a node's job be
// RUNNING/terminal only reachable via claim, and claim only ever selects
// eligible_at <= now() rows, a dependent whose eligible_at is still
// blockedEligibleAt (i.e., not yet activated) can never have been
// claimed — so a dependent discovered by this function's BFS is always
// found either still blocked (state QUEUED, eligible_at =
// blockedEligibleAt) or, in the redundant-invocation case (e.g. two
// predecessors failing concurrently, or a workflow-wide cancellation
// racing an organic cascade), already resolved by an earlier call in this
// same transaction or an already-committed one. It is never found
// RUNNING or otherwise mid-flight, because reaching RUNNING requires
// having first been activated, which requires every dependency to have
// already reached SUCCEEDED — directly contradicting "this call was
// triggered by one of its dependencies reaching a non-SUCCEEDED terminal
// state."
//
// After the cascade (activation of newly-satisfied dependents, and
// transitive cancellation of dependents downstream of a failure)
// completes, this finalizes the owning workflow_instances row's terminal
// state if every one of its nodes has now reached a terminal job state
// (see finalizeWorkflowIfComplete) — guarded so a workflow's terminal
// state, once set, is never reopened (mirrors TF-INV-005's job-level
// guarantee at the workflow level).
//
// Phase 8: returns the workflow's id and, if this call is what finalized
// it, its resulting terminal state ("" otherwise) — every caller uses
// this ONLY to log "workflow completed/failed/cancelled"
// (event="workflow_finalized") after ITS OWN transaction has committed
// (never from within this function, which runs mid-transaction: a log
// line asserting a workflow finalized must not be emitted before that
// fact is actually durable). Per-node activation/cascade-cancellation
// events are not surfaced this way — see docs/observability.md's
// "Implementation Notes" for why that finer-grained logging was left
// out of Phase 8's scope (it would require threading a similar
// commit-deferred event list through every one of this function's five
// call sites for a purely diagnostic, non-metric benefit; per-node
// history remains queryable via GET /workflows/{id} and job_attempts).
func (s *Store) propagateWorkflowTransition(ctx context.Context, tx *sql.Tx, jobID uuid.UUID, newState jobstate.State) (workflowID uuid.UUID, finalState string, err error) {
	if newState != jobstate.Succeeded && newState != jobstate.DeadLettered && newState != jobstate.Cancelled {
		return uuid.UUID{}, "", nil
	}

	var nodeID uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT id, workflow_instance_id FROM workflow_nodes WHERE job_id = $1`, jobID).Scan(&nodeID, &workflowID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.UUID{}, "", nil
	}
	if err != nil {
		return uuid.UUID{}, "", fmt.Errorf("propagate workflow transition: lookup node for job %s: %w", jobID, err)
	}

	queue := []uuid.UUID{nodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		deps, err := dependentsOf(ctx, tx, current)
		if err != nil {
			return uuid.UUID{}, "", fmt.Errorf("propagate workflow transition: %w", err)
		}
		for _, d := range deps {
			cascaded, err := resolveDependent(ctx, tx, d)
			if err != nil {
				return uuid.UUID{}, "", fmt.Errorf("propagate workflow transition: %w", err)
			}
			if cascaded {
				queue = append(queue, d.id)
			}
		}
	}

	finalState, err = finalizeWorkflowIfComplete(ctx, tx, workflowID)
	if err != nil {
		return uuid.UUID{}, "", fmt.Errorf("propagate workflow transition: %w", err)
	}
	return workflowID, finalState, nil
}

// finalizeWorkflowIfComplete sets workflow_instances.state to a terminal
// value once every one of its nodes' underlying jobs has reached a
// terminal job state: SUCCEEDED throughout means workflow SUCCEEDED; any
// node DEAD_LETTERED or CANCELLED means workflow FAILED per
// docs/workflows.md ("a workflow-level summary concept ... at least one
// required node dead-lettered or was cancelled") — UNLESS the workflow
// itself was explicitly, durably asked to cancel
// (workflow_instances.cancel_requested), in which case the terminal state
// is CANCELLED instead of FAILED, distinguishing "the operator cancelled
// this workflow" from "this workflow failed organically via failure
// propagation," even though both paths mechanically drive every node
// through the same job-level CANCELLED/DEAD_LETTERED states.
//
// The final UPDATE's `WHERE state = 'RUNNING'` guard makes this
// idempotent and, combined with the fact that no code path in this
// package ever transitions workflow_instances out of a terminal state,
// enforces that a workflow's terminal state, once set, is never reopened.
//
// Phase 8: returns the terminal state this call itself set ("" if the
// workflow is not yet complete, or was already finalized by an earlier
// call), so propagateWorkflowTransition's caller can log the event only
// once, only by the call that actually caused it, and only after its own
// transaction commits.
func finalizeWorkflowIfComplete(ctx context.Context, tx *sql.Tx, workflowID uuid.UUID) (string, error) {
	var total, nonTerminal, failedOrCancelled int
	err := tx.QueryRowContext(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE j.state NOT IN ('SUCCEEDED','DEAD_LETTERED','CANCELLED')),
			count(*) FILTER (WHERE j.state IN ('DEAD_LETTERED','CANCELLED'))
		FROM workflow_nodes wn JOIN jobs j ON j.id = wn.job_id
		WHERE wn.workflow_instance_id = $1`, workflowID,
	).Scan(&total, &nonTerminal, &failedOrCancelled)
	if err != nil {
		return "", fmt.Errorf("finalize workflow %s: %w", workflowID, err)
	}
	if total == 0 || nonTerminal > 0 {
		return "", nil
	}

	// If literally every node SUCCEEDED, the workflow SUCCEEDED --
	// unconditionally, even if cancel_requested is set. This matters for
	// TF-INV-010: a workflow-level cancellation only *requests*
	// cancellation of a RUNNING node (RequestCancellation), and per
	// TF-INV-010's "first durable terminal write wins" rule, that node's
	// own completion may legitimately win the race and reach SUCCEEDED
	// anyway. Checking failedOrCancelled BEFORE consulting
	// cancel_requested (rather than letting cancel_requested
	// unconditionally override the outcome) is what keeps a workflow
	// whose cancellation lost every race correctly SUCCEEDED, not
	// incorrectly CANCELLED.
	finalState := "SUCCEEDED"
	if failedOrCancelled > 0 {
		finalState = "FAILED"

		var cancelRequested bool
		if err := tx.QueryRowContext(ctx, `SELECT cancel_requested FROM workflow_instances WHERE id = $1`, workflowID).Scan(&cancelRequested); err != nil {
			return "", fmt.Errorf("finalize workflow %s: read cancel_requested: %w", workflowID, err)
		}
		if cancelRequested {
			finalState = "CANCELLED"
		}
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET state = $2, terminal_at = now(), updated_at = now()
		WHERE id = $1 AND state = 'RUNNING'`, workflowID, finalState)
	if err != nil {
		return "", fmt.Errorf("finalize workflow %s: %w", workflowID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("finalize workflow %s: rows affected: %w", workflowID, err)
	}
	if n == 0 {
		// Already finalized by an earlier call (redundant invocation --
		// e.g. two predecessors failing concurrently) -- not an error,
		// just nothing new for this call to report.
		return "", nil
	}
	return finalState, nil
}
