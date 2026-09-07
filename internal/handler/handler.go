// Package handler defines the job execution contract. It is deliberately
// minimal: a Handler executes one job and returns a result or an error.
// There is no plugin system, no dynamic loading, and no handler
// configuration beyond registering a Go value under a job_type string —
// exactly enough to let a worker dispatch to different logic per job_type
// and to let tests supply deterministic handlers.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
)

// Result is what a Handler returns on success. Metadata, if non-nil, is
// stored verbatim in the job's result_metadata column.
type Result struct {
	Metadata json.RawMessage
}

// Handler executes a single job attempt. A returned error is always
// treated as a failure of that attempt; Phase 1 has no distinction between
// retryable and permanent failure (see internal/worker) because
// docs/roadmap.md's Phase 1 state machine sends every failure straight to
// DEAD_LETTERED.
type Handler interface {
	Execute(ctx context.Context, j *job.Job) (Result, error)
}

// Registry maps job_type to the Handler responsible for executing it.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

// Register associates jobType with h. Registering the same jobType twice
// replaces the previous handler.
func (r *Registry) Register(jobType string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[jobType] = h
}

// Lookup returns the handler registered for jobType, if any.
func (r *Registry) Lookup(jobType string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[jobType]
	return h, ok
}

// ErrNoHandler is wrapped into the error internal/worker records as a
// permanent failure when a claimed job's job_type has no registered
// handler.
type ErrNoHandler struct {
	JobType string
}

func (e ErrNoHandler) Error() string {
	return fmt.Sprintf("handler: no handler registered for job_type %q", e.JobType)
}
