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

	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

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
		Nodes:             nodes,
	}
}

// validateWorkflowNodeRequest applies exactly the same per-node field
// rules internal/api.validateCreateJobRequest applies to a standalone
// POST /jobs submission (job_type presence/length, payload JSON
// validity, max_attempts/execution_timeout_seconds bounds and defaults)
// -- these are ordinary job-field concerns, not DAG structure, so they
// live here rather than in internal/workflow.ValidateGraph (which is
// reserved for graph-shape validation: node_key uniqueness,
// self-dependency, unknown dependency, cycles). node_key and depends_on
// are passed through as-is for internal/workflow.ValidateGraph to check.
func validateWorkflowNodeRequest(req createWorkflowNodeRequest) (workflow.NodeSpec, string) {
	nodeKey := strings.TrimSpace(req.NodeKey)
	if nodeKey == "" {
		return workflow.NodeSpec{}, "node_key is required"
	}

	jobType := strings.TrimSpace(req.JobType)
	if jobType == "" {
		return workflow.NodeSpec{}, fmt.Sprintf("node %q: job_type is required", nodeKey)
	}
	if len(jobType) > 255 {
		return workflow.NodeSpec{}, fmt.Sprintf("node %q: job_type must be at most 255 characters", nodeKey)
	}

	payload := req.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	} else if !json.Valid(payload) {
		return workflow.NodeSpec{}, fmt.Sprintf("node %q: payload must be valid JSON", nodeKey)
	}

	maxAttempts := DefaultMaxAttempts
	if req.MaxAttempts != nil {
		if *req.MaxAttempts < 1 {
			return workflow.NodeSpec{}, fmt.Sprintf("node %q: max_attempts must be at least 1", nodeKey)
		}
		maxAttempts = *req.MaxAttempts
	}

	executionTimeout := DefaultExecutionTimeoutSeconds
	if req.ExecutionTimeoutSeconds != nil {
		if *req.ExecutionTimeoutSeconds < 1 {
			return workflow.NodeSpec{}, fmt.Sprintf("node %q: execution_timeout_seconds must be at least 1", nodeKey)
		}
		executionTimeout = *req.ExecutionTimeoutSeconds
	}

	return workflow.NodeSpec{
		NodeKey:                 nodeKey,
		JobType:                 jobType,
		Payload:                 payload,
		MaxAttempts:             maxAttempts,
		ExecutionTimeoutSeconds: executionTimeout,
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
	var req createWorkflowRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if len(req.Nodes) == 0 {
		writeError(w, http.StatusBadRequest, "nodes must contain at least one entry")
		return
	}

	spec := workflow.GraphSpec{Nodes: make([]workflow.NodeSpec, 0, len(req.Nodes))}
	for _, n := range req.Nodes {
		ns, verr := validateWorkflowNodeRequest(n)
		if verr != "" {
			writeError(w, http.StatusBadRequest, verr)
			return
		}
		spec.Nodes = append(spec.Nodes, ns)
	}

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

	h.logger.Info("workflow created", "workflow_id", wf.ID.String(), "node_count", len(wf.Nodes))
	writeJSON(w, http.StatusCreated, toWorkflowResponse(wf))
}

// GetWorkflow handles GET /workflows/{id}. Like GetJob, this is a plain
// read against durable state -- no locking, no side effects -- covering
// both the workflow_instances row and every node's current, live job
// state (joined fresh on every call, never cached).
func (h *Handlers) GetWorkflow(w http.ResponseWriter, r *http.Request) {
	idParam := r.PathValue("id")
	id, err := uuid.Parse(idParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a valid UUID")
		return
	}

	wf, err := h.store.GetWorkflow(r.Context(), id)
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
func (h *Handlers) CancelWorkflow(w http.ResponseWriter, r *http.Request) {
	idParam := r.PathValue("id")
	id, err := uuid.Parse(idParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a valid UUID")
		return
	}

	wf, err := h.store.CancelWorkflow(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if err != nil {
		h.logger.Error("failed to cancel workflow", "error", err, "workflow_id", id)
		writeError(w, http.StatusInternalServerError, "failed to cancel workflow")
		return
	}

	writeJSON(w, http.StatusOK, toWorkflowResponse(wf))
}
