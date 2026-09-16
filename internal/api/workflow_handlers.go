// Phase 7: the workflow HTTP surface -- POST /workflows, GET
// /workflows/{id}, POST /workflows/{id}/cancel -- per docs/workflows.md.
// Every existing job endpoint (POST /jobs, GET /jobs/{id}, POST
// /jobs/{id}/cancel) is unchanged by this file: a workflow node's
// underlying job remains fully visible and manageable through those
// endpoints too, since it is an ordinary job row (see
// internal/store/workflow.go).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

// createWorkflowNodeRequest is one node in a POST /workflows body. Like
// createJobRequest it deliberately carries no principal_id/tenant_id field
// and must never gain one (docs/phase-12-plan.md §6a, verification point
// 6) -- the owning principal is taken once, in CreateWorkflow, from the
// authenticated AccessContext.
type createWorkflowNodeRequest struct {
	NodeKey                 string          `json:"node_key"`
	JobType                 string          `json:"job_type"`
	Payload                 json.RawMessage `json:"payload"`
	MaxAttempts             *int            `json:"max_attempts,omitempty"`
	ExecutionTimeoutSeconds *int            `json:"execution_timeout_seconds,omitempty"`
	ScheduledAt             *time.Time      `json:"scheduled_at,omitempty"`
	DependsOn               []string        `json:"depends_on,omitempty"`
}

type createWorkflowRequest struct {
	Nodes []createWorkflowNodeRequest `json:"nodes"`
	// QueueName is Phase 13's optional named-queue field
	// (docs/phase-13-plan.md §8), applied to the whole workflow -- every
	// node's underlying job shares it, the same way every node shares the
	// workflow's PrincipalID. Omitted or empty defaults to "default".
	QueueName *string `json:"queue_name,omitempty"`
}

type workflowNodeResponse struct {
	NodeKey        string   `json:"node_key"`
	JobID          string   `json:"job_id"`
	DependsOn      []string `json:"depends_on,omitempty"`
	State          string   `json:"state"`
	AttemptCount   int      `json:"attempt_count"`
	LastError      *string  `json:"last_error,omitempty"`
	LastErrorClass *string  `json:"last_error_class,omitempty"`
}

type workflowResponse struct {
	ID                string                 `json:"id"`
	State             string                 `json:"state"`
	CancelRequested   bool                   `json:"cancel_requested,omitempty"`
	CancelRequestedAt *time.Time             `json:"cancel_requested_at,omitempty"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
	TerminalAt        *time.Time             `json:"terminal_at,omitempty"`
	QueueName         string                 `json:"queue_name"`
	Nodes             []workflowNodeResponse `json:"nodes"`
}

func toWorkflowResponse(wf *workflow.Instance) workflowResponse {
	nodes := make([]workflowNodeResponse, 0, len(wf.Nodes))
	for _, n := range wf.Nodes {
		nodes = append(nodes, workflowNodeResponse{
			NodeKey:        n.NodeKey,
			JobID:          n.JobID.String(),
			DependsOn:      n.DependsOn,
			State:          string(n.JobState),
			AttemptCount:   n.AttemptCount,
			LastError:      n.LastError,
			LastErrorClass: n.LastErrorClass,
		})
	}
	return workflowResponse{
		ID:                wf.ID.String(),
		State:             string(wf.State),
		CancelRequested:   wf.CancelRequested,
		CancelRequestedAt: wf.CancelRequestedAt,
		CreatedAt:         wf.CreatedAt,
		UpdatedAt:         wf.UpdatedAt,
		TerminalAt:        wf.TerminalAt,
		QueueName:         wf.QueueName,
		Nodes:             nodes,
	}
}

// validateWorkflowNodeRequest applies exactly the same per-node field
// rules a standalone POST /jobs submission gets (job_type presence/length,
// payload JSON validity, max_attempts/execution_timeout_seconds bounds and
// defaults) -- these are ordinary job-field concerns, not DAG structure, so
// they live here rather than in internal/workflow.ValidateGraph (which is
// reserved for graph-shape validation: node_key uniqueness,
// self-dependency, unknown dependency, cycles). node_key and depends_on
// are passed through as-is for internal/workflow.ValidateGraph to check.
//
// As of Phase 11 (docs/enterprise-roadmap.md), the job-field rules
// themselves are delegated to internal/job.ValidateSubmission -- the same
// function validateCreateJobRequest (handlers.go) and the direct-Go
// transactional enqueue API (txenqueue) use -- only the node_key presence
// check and the "node %q: ..." error-message prefix are specific to this
// call site.
func validateWorkflowNodeRequest(req createWorkflowNodeRequest) (workflow.NodeSpec, string) {
	nodeKey := strings.TrimSpace(req.NodeKey)
	if nodeKey == "" {
		return workflow.NodeSpec{}, "node_key is required"
	}

	params, err := job.ValidateSubmission(req.JobType, req.Payload, req.MaxAttempts, req.ExecutionTimeoutSeconds, nil)
	if err != nil {
		return workflow.NodeSpec{}, fmt.Sprintf("node %q: %s", nodeKey, err.Error())
	}

	return workflow.NodeSpec{
		NodeKey:                 nodeKey,
		JobType:                 params.JobType,
		Payload:                 params.Payload,
		MaxAttempts:             params.MaxAttempts,
		ExecutionTimeoutSeconds: params.ExecutionTimeoutSeconds,
		ScheduledAt:             req.ScheduledAt,
		DependsOn:               req.DependsOn,
	}, ""
}

// CreateWorkflow handles POST /workflows. Per the "DAG VALIDATION" and
// "ATOMIC WORKFLOW CREATION" requirements: every node's fields are
// validated (this function), then the full graph's structure is
// validated (internal/workflow.ValidateGraph, invoked by
// internal/store.CreateWorkflow before it opens any transaction), and
// only once both pass does a single atomic transaction create the
// workflow_instances row, every node's underlying jobs row, and every
// workflow_nodes row together. A 201 response is written if and only if
// that transaction has committed (TF-INV-001's submission-acknowledgement
// guarantee, extended to workflow creation) -- an invalid submission
// never touches the database and is never acknowledged as created.
func (h *Handlers) CreateWorkflow(w http.ResponseWriter, r *http.Request) {
	authz, ok := h.accessContext(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)

	var req createWorkflowRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body must be at most %d bytes", MaxRequestBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if len(req.Nodes) == 0 {
		writeError(w, http.StatusBadRequest, "nodes must contain at least one entry")
		return
	}

	// PrincipalID is set here, once, from the verified credential --
	// internal/store.CreateWorkflow writes it to the workflow_instances
	// row and to every node's underlying jobs row in one transaction, so
	// a workflow and its nodes are always owned by the same principal.
	queueName, qerr := job.ValidateQueueName(req.QueueName)
	if qerr != nil {
		writeError(w, http.StatusBadRequest, qerr.Error())
		return
	}

	spec := workflow.GraphSpec{PrincipalID: authz.PrincipalID, QueueName: queueName, Nodes: make([]workflow.NodeSpec, 0, len(req.Nodes))}
	for _, n := range req.Nodes {
		ns, verr := validateWorkflowNodeRequest(n)
		if verr != "" {
			writeError(w, http.StatusBadRequest, verr)
			return
		}
		spec.Nodes = append(spec.Nodes, ns)
	}

	release, ok := h.admitSubmission(w, r, authz.PrincipalID.String(), queueName)
	if !ok {
		return
	}
	defer release()

	wf, err := h.store.CreateWorkflow(r.Context(), spec)
	if err != nil {
		var invalid *workflow.InvalidGraphError
		if errors.As(err, &invalid) {
			writeError(w, http.StatusBadRequest, invalid.Error())
			return
		}
		h.logger.Error("failed to create workflow", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to durably persist workflow")
		return
	}

	h.logger.Info("workflow created", "event", "workflow_submitted",
		"actor", authz.PrincipalID.String(), "workflow_id", wf.ID.String(), "node_count", len(wf.Nodes))
	writeJSON(w, http.StatusCreated, toWorkflowResponse(wf))
}

// GetWorkflow handles GET /workflows/{id}. Like GetJob, this is a plain
// read against durable state -- no locking, no side effects -- covering
// both the workflow_instances row and every node's current, live job
// state (joined fresh on every call, never cached).
//
// Phase 12: principal-scoped in SQL. Another principal's workflow is
// ErrNotFound from the store and reported through the same branch as a
// nonexistent id, so the 404 is byte-identical -- and because the instance
// row must match before its nodes are loaded, a non-owned workflow's node
// list (which would disclose job ids and job types) is never read.
func (h *Handlers) GetWorkflow(w http.ResponseWriter, r *http.Request) {
	authz, ok := h.accessContext(w, r)
	if !ok {
		return
	}

	idParam := r.PathValue("id")
	id, err := uuid.Parse(idParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a valid UUID")
		return
	}

	wf, err := h.store.GetWorkflow(r.Context(), id, authz)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if err != nil {
		h.logger.Error("failed to read workflow", "error", err, "workflow_id", id)
		writeError(w, http.StatusInternalServerError, "failed to read workflow")
		return
	}

	writeJSON(w, http.StatusOK, toWorkflowResponse(wf))
}

// CancelWorkflow handles POST /workflows/{id}/cancel, per
// docs/workflows.md's "Workflow-Level Cancellation": cancels every
// currently non-terminal node's underlying job via the same per-job
// cancellation primitives POST /jobs/{id}/cancel uses (see
// internal/store.CancelWorkflow), and reports the workflow's resulting
// state -- which may not yet be CANCELLED if any node was RUNNING at the
// moment of the request (that node's cancellation is only requested, not
// yet confirmed, exactly as for a standalone job; poll GET
// /workflows/{id} to observe the eventual outcome). Calling this on an
// already-terminal workflow is an idempotent no-op reporting its actual
// terminal state.
//
// Phase 12 (docs/phase-12-plan.md §7): the ownership predicate sits on
// internal/store.CancelWorkflow's first statement AND on the read that
// follows it, so a non-owned workflow returns before the per-node
// cancellation loop is reached -- its node jobs are never touched, not
// just its top-level row.
func (h *Handlers) CancelWorkflow(w http.ResponseWriter, r *http.Request) {
	authz, ok := h.accessContext(w, r)
	if !ok {
		return
	}

	idParam := r.PathValue("id")
	id, err := uuid.Parse(idParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a valid UUID")
		return
	}

	wf, err := h.store.CancelWorkflow(r.Context(), id, authz)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if err != nil {
		h.logger.Error("failed to cancel workflow", "error", err, "workflow_id", id)
		writeError(w, http.StatusInternalServerError, "failed to cancel workflow")
		return
	}

	h.logger.Info("workflow cancellation requested", "event", "workflow_cancel_requested",
		"actor", authz.PrincipalID.String(), "workflow_id", wf.ID.String(), "state", string(wf.State))

	writeJSON(w, http.StatusOK, toWorkflowResponse(wf))
}
