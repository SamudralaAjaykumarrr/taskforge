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
	IdempotencyKey          *string         `json:"idempotency_key,omitempty"`
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
		IdempotencyKey:          j.IdempotencyKey,
		LastError:               j.LastError,
		LastErrorClass:          j.LastErrorClass,
		ResultMetadata:          j.ResultMetadata,
		TerminalAt:              j.TerminalAt,
	}
}

// idempotencyKeyHeader is the request header docs/worker-protocol.md's API
// Contract and docs/idempotency.md document: "The client supplies an
// Idempotency-Key header." http.Header.Get canonicalizes the name, so
// lookups are case-insensitive regardless of how the client wrote it.
const idempotencyKeyHeader = "Idempotency-Key"

// CreateJob handles POST /jobs. Per TF-INV-001 and ADR-0006, the 201
// response is written if and only if internal/store.InsertIdempotent
// returns successfully — which itself only happens after the INSERT
// transaction has committed (whether that INSERT created a new row or lost
// a race and fell through to re-reading an already-committed one — either
// way, a durable, committed row is guaranteed to exist before any response
// is written). Any error (validation already having passed) results in a
// 5xx response and no success acknowledgement is ever sent.
//
// Per docs/idempotency.md, a duplicate submission (same job_type +
// Idempotency-Key as an existing job) returns that existing job's current
// representation with the SAME 2xx status a fresh submission would have
// produced — never a different status code, and never a comparison against
// the new request's payload.
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

	idemKey, verr := parseIdempotencyKey(r.Header.Get(idempotencyKeyHeader))
	if verr != "" {
		writeError(w, http.StatusBadRequest, verr)
		return
	}
	params.IdempotencyKey = idemKey

	j, created, err := h.store.InsertIdempotent(r.Context(), params)
	if err != nil {
		h.logger.Error("failed to insert job", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to durably persist job")
		return
	}

	if idemKey != nil {
		if created {
			h.logger.Info("new idempotent submission", "job_id", j.ID.String(), "job_type", j.JobType, "idempotency_key", *idemKey)
		} else {
			h.logger.Info("duplicate submission detected; returning existing job", "job_id", j.ID.String(), "job_type", j.JobType, "idempotency_key", *idemKey)
		}
	}

	writeJSON(w, http.StatusCreated, toJobResponse(j))
}

// parseIdempotencyKey validates the optional Idempotency-Key request
// header per docs/idempotency.md. A missing header, or one that is empty
// or all-whitespace after trimming, is treated as "no key supplied" — not
// a validation error — mirroring docs/idempotency.md's "No
// Idempotency-Key supplied: every POST /jobs call creates a new job" (an
// empty header value is operationally indistinguishable from a caller who
// did not mean to send one). This empty-header rule, and the
// MaxIdempotencyKeyLength bound below, are Phase 4 implementation
// decisions not separately specified in docs/idempotency.md — documented
// here, in docs/idempotency.md's "Implementation Notes," and in README's
// Phase 4 section, rather than decided silently, per the task's
// requirement not to invent an undocumented public contract.
func parseIdempotencyKey(raw string) (*string, string) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return nil, ""
	}
	if len(key) > MaxIdempotencyKeyLength {
		return nil, fmt.Sprintf("Idempotency-Key must be at most %d characters", MaxIdempotencyKeyLength)
	}
	return &key, ""
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
