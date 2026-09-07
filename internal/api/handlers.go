package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

type createJobRequest struct {
	JobType                 string          `json:"job_type"`
	Payload                 json.RawMessage `json:"payload"`
	MaxAttempts             *int            `json:"max_attempts,omitempty"`
	ExecutionTimeoutSeconds *int            `json:"execution_timeout_seconds,omitempty"`
}

type jobResponse struct {
	ID                      string          `json:"id"`
	JobType                 string          `json:"job_type"`
	State                   string          `json:"state"`
	AttemptCount            int             `json:"attempt_count"`
	MaxAttempts             int             `json:"max_attempts"`
	ExecutionTimeoutSeconds int             `json:"execution_timeout_seconds"`
	CreatedAt               time.Time       `json:"created_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
	EligibleAt              time.Time       `json:"eligible_at"`
	LastError               *string         `json:"last_error,omitempty"`
	LastErrorClass          *string         `json:"last_error_class,omitempty"`
	ResultMetadata          json.RawMessage `json:"result_metadata,omitempty"`
	TerminalAt              *time.Time      `json:"terminal_at,omitempty"`
}

func toJobResponse(j *job.Job) jobResponse {
	return jobResponse{
		ID:                      j.ID.String(),
		JobType:                 j.JobType,
		State:                   string(j.State),
		AttemptCount:            j.AttemptCount,
		MaxAttempts:             j.MaxAttempts,
		ExecutionTimeoutSeconds: j.ExecutionTimeoutSeconds,
		CreatedAt:               j.CreatedAt,
		UpdatedAt:               j.UpdatedAt,
		EligibleAt:              j.EligibleAt,
		LastError:               j.LastError,
		LastErrorClass:          j.LastErrorClass,
		ResultMetadata:          j.ResultMetadata,
		TerminalAt:              j.TerminalAt,
	}
}

// CreateJob handles POST /jobs. Per TF-INV-001 and ADR-0006, the 201
// response is written if and only if internal/store.Insert returns
// successfully — which itself only happens after the INSERT transaction
// has committed. Any error from Insert (validation already having passed)
// results in a 5xx response and no success acknowledgement is ever sent.
func (h *Handlers) CreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	params, verr := validateCreateJobRequest(req)
	if verr != "" {
		writeError(w, http.StatusBadRequest, verr)
		return
	}

	j, err := h.store.Insert(r.Context(), params)
	if err != nil {
		h.logger.Error("failed to insert job", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to durably persist job")
		return
	}

	writeJSON(w, http.StatusCreated, toJobResponse(j))
}

func validateCreateJobRequest(req createJobRequest) (job.NewParams, string) {
	jobType := strings.TrimSpace(req.JobType)
	if jobType == "" {
		return job.NewParams{}, "job_type is required"
	}
	if len(jobType) > 255 {
		return job.NewParams{}, "job_type must be at most 255 characters"
	}

	payload := req.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	} else if !json.Valid(payload) {
		return job.NewParams{}, "payload must be valid JSON"
	}

	maxAttempts := DefaultMaxAttempts
	if req.MaxAttempts != nil {
		if *req.MaxAttempts < 1 {
			return job.NewParams{}, "max_attempts must be at least 1"
		}
		maxAttempts = *req.MaxAttempts
	}

	executionTimeout := DefaultExecutionTimeoutSeconds
	if req.ExecutionTimeoutSeconds != nil {
		if *req.ExecutionTimeoutSeconds < 1 {
			return job.NewParams{}, "execution_timeout_seconds must be at least 1"
		}
		executionTimeout = *req.ExecutionTimeoutSeconds
	}

	return job.NewParams{
		JobType:                 jobType,
		Payload:                 payload,
		MaxAttempts:             maxAttempts,
		ExecutionTimeoutSeconds: executionTimeout,
	}, ""
}

// GetJob handles GET /jobs/{id}. It is a plain read against durable state
// — no locking, no side effects — per docs/worker-protocol.md.
func (h *Handlers) GetJob(w http.ResponseWriter, r *http.Request) {
	idParam := r.PathValue("id")
	id, err := uuid.Parse(idParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a valid UUID")
		return
	}

	j, err := h.store.GetByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		h.logger.Error("failed to read job", "error", err, "job_id", id)
		writeError(w, http.StatusInternalServerError, "failed to read job")
		return
	}

	writeJSON(w, http.StatusOK, toJobResponse(j))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
