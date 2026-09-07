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
	"errors"
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
// treated as a failure of that attempt. Per docs/retry-semantics.md
// ("Retryable vs. Permanent Failures"), a Handler that wants a failure
// retried MUST wrap it with Retryable; a failure wrapped with Permanent (or
// not wrapped at all) dead-letters immediately without consuming further
// attempts (see Classify below for the exact default). TaskForge does not
// and will not infer retryability from string matching or error type.
type Handler interface {
	Execute(ctx context.Context, j *job.Job) (Result, error)
}

// FailureClass distinguishes a retryable attempt failure from a permanent
// one, per docs/retry-semantics.md. TaskForge trusts this classification
// entirely -- it has no visibility into the semantics of the handler's own
// side effect and cannot infer retryability itself (a handler that
// misclassifies a failure will simply exhaust max_attempts and dead-letter
// normally, per that document -- a handler-authoring concern, not a gap in
// TaskForge).
type FailureClass string

const (
	// ClassRetryable is a failure the handler believes may succeed on a
	// future attempt (e.g. a downstream 503, a transient network error).
	ClassRetryable FailureClass = "RETRYABLE"
	// ClassPermanent is a failure the handler believes retrying will not
	// fix (e.g. a 400 indicating the payload itself is invalid). It is
	// also the default classification for an error a handler did not
	// explicitly wrap with Retryable or Permanent -- see Classify.
	ClassPermanent FailureClass = "PERMANENT"
)

// classifiedError wraps a Handler's failure with an explicit retry
// classification. Unexported: constructed only via Retryable/Permanent,
// inspected only via Classify, so a handler cannot forge a class value
// outside this package's two constructors.
type classifiedError struct {
	class FailureClass
	err   error
}

func (e *classifiedError) Error() string { return e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

// Retryable marks err as a failure the handler believes may succeed on a
// future attempt. A nil err returns nil (mirrors fmt.Errorf/errors.New
// conventions: wrapping "no error" is still "no error").
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{class: ClassRetryable, err: err}
}

// Permanent marks err as a failure the handler believes will not be fixed
// by retrying. A nil err returns nil. Wrapping with Permanent is never
// required for correctness (see Classify's default), but is useful for
// making a handler's intent explicit and self-documenting.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{class: ClassPermanent, err: err}
}

// Classify extracts the failure class from a Handler's returned error,
// per docs/retry-semantics.md. It walks err's Unwrap chain (via errors.As)
// so a handler may add its own context with fmt.Errorf("...: %w", ...)
// around a Retryable/Permanent-wrapped cause without losing the
// classification. The returned error is always the original err, with all
// wrapping intact, so no message context is discarded by classification.
//
// An error not wrapped via Retryable or Permanent anywhere in its chain is
// treated as ClassPermanent: docs/retry-semantics.md requires the handler
// to explicitly opt in to a retry, and an unclassified error must not be
// silently retried forever by default -- it dead-letters immediately
// instead, which is also the safe, backward-compatible behavior for a
// Handler written before Phase 3 introduced this distinction.
func Classify(err error) (FailureClass, error) {
	var ce *classifiedError
	if errors.As(err, &ce) {
		return ce.class, err
	}
	return ClassPermanent, err
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
