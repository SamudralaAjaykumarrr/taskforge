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
	// ScheduledAt is Phase 6's optional future-execution request, per
	// docs/scheduling.md. Omitted or null means "run as soon as
	// possible" (unchanged Phase 1-5 behavior). encoding/json parses this
	// as RFC 3339 automatically -- a malformed value fails at
	// dec.Decode's DisallowUnknownFields JSON parse in CreateJob below,
	// before validateCreateJobRequest ever runs, with a 400 response.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
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
	ScheduledAt             *time.Time      `json:"scheduled_at,omitempty"`
	CancelRequested         bool            `json:"cancel_requested,omitempty"`
	CancelRequestedAt       *time.Time      `json:"cancel_requested_at,omitempty"`
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
		ScheduledAt:             j.ScheduledAt,
		CancelRequested:         j.CancelRequested,
		CancelRequestedAt:       j.CancelRequestedAt,
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

	// Phase 8: the idempotency key itself is never logged (only whether
	// one was supplied) -- docs/roadmap.md's Idempotency Observability
	// guidance: "Never emit the raw idempotency key into metrics labels.
	// Avoid logging it unless docs explicitly permit safe
	// redaction/hashing," which docs/idempotency.md does not.
	if idemKey != nil {
		if created {
			h.logger.Info("new idempotent submission", "event", "submission", "job_id", j.ID.String(), "job_type", j.JobType, "state", string(j.State), "had_idempotency_key", true)
		} else {
			h.logger.Info("duplicate submission detected; returning existing job", "event", "duplicate_submission_hit", "job_id", j.ID.String(), "job_type", j.JobType, "state", string(j.State), "had_idempotency_key", true)
		}
	} else {
		h.logger.Info("job submitted", "event", "submission", "job_id", j.ID.String(), "job_type", j.JobType, "state", string(j.State), "had_idempotency_key", false)
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
		ScheduledAt:             req.ScheduledAt,
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

// CancelJob handles POST /jobs/{id}/cancel, per docs/worker-protocol.md's
// documented contract:
//
//   - QUEUED/RETRY_WAIT: transitions directly to CANCELLED (no worker
//     involved, no race).
//   - RUNNING: durably records the request (cancel_requested = true);
//     the response reports "cancellation requested, not yet confirmed" —
//     the caller must poll GET /jobs/{id} to observe the eventual
//     outcome (CANCELLED, or a completion that won the race per
//     TF-INV-010).
//   - Already terminal: idempotent no-op, response reports the job's
//     actual terminal state.
//
// This is a three-step cascade rather than a read-then-act check: each
// step is itself a single fenced, conditional UPDATE (internal/store's
// CancelQueuedOrRetryWait / RequestCancellation), so there is no
// check-then-act race window — if a step's guard does not match the
// job's *current* state, it is because a genuinely different state
// already applies (possibly changed concurrently by a claim or a
// worker's own completion call), not because this handler read a stale
// snapshot. The final GetByID only ever supplies the response body for
// an already-resolved (terminal, or genuinely nonexistent) job.
func (h *Handlers) CancelJob(w http.ResponseWriter, r *http.Request) {
	idParam := r.PathValue("id")
	id, err := uuid.Parse(idParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a valid UUID")
		return
	}

	// Phase 8: which branch of this cascade resolved the request is
	// itself diagnostically useful (docs/observability.md: "Cancellation
	// requested / cancellation race outcome (which side won)") --
	// path distinguishes an uncontested pre-claim cancellation from a
	// request pending a running worker's acknowledgement from a no-op
	// against an already-terminal (or nonexistent) job.
	path := "direct"
	j, err := h.store.CancelQueuedOrRetryWait(r.Context(), id)
	if errors.Is(err, store.ErrStaleTransition) {
		path = "requested_pending_worker"
		j, err = h.store.RequestCancellation(r.Context(), id)
	}
	if errors.Is(err, store.ErrStaleTransition) {
		// Neither QUEUED/RETRY_WAIT nor RUNNING matched: the job is
		// already terminal (or does not exist at all) — report reality
		// rather than a generic rejection, per the documented contract.
		path = "already_terminal"
		j, err = h.store.GetByID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
	}
	if err != nil {
		h.logger.Error("failed to cancel job", "error", err, "job_id", id)
		writeError(w, http.StatusInternalServerError, "failed to cancel job")
		return
	}
	h.logger.Info("cancellation requested", "event", "cancellation_requested",
		"job_id", id.String(), "job_type", j.JobType, "state", string(j.State), "path", path)

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
