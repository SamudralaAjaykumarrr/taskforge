// Package testdoubles provides deterministic Handler implementations used
// by state-machine and worker tests: a handler must be able to always
// succeed, always fail, or record what it was asked to execute, without
// relying on any real external side effect.
package testdoubles

import (
	"context"
	"errors"
	"sync"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/handler"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
)

// AlwaysSucceed is a Handler that always reports success with no result
// metadata.
type AlwaysSucceed struct{}

func (AlwaysSucceed) Execute(context.Context, *job.Job) (handler.Result, error) {
	return handler.Result{}, nil
}

// AlwaysFail is a Handler that always fails with a fixed error.
type AlwaysFail struct {
	Err error
}

// NewAlwaysFail returns an AlwaysFail with a default error if err is nil.
func NewAlwaysFail(err error) AlwaysFail {
	if err == nil {
		err = errors.New("testdoubles: always fail")
	}
	return AlwaysFail{Err: err}
}

func (h AlwaysFail) Execute(context.Context, *job.Job) (handler.Result, error) {
	return handler.Result{}, h.Err
}

// Recorder is a Handler that records every job it was asked to execute,
// in call order, and delegates the actual outcome to an inner handler
// (AlwaysSucceed by default). Safe for concurrent use.
type Recorder struct {
	mu       sync.Mutex
	executed []RecordedExecution
	inner    handler.Handler
}

// RecordedExecution captures the identity of one Execute call.
type RecordedExecution struct {
	JobID        string
	JobType      string
	AttemptCount int
}

// NewRecorder returns a Recorder that delegates to inner. If inner is nil,
// it delegates to AlwaysSucceed.
func NewRecorder(inner handler.Handler) *Recorder {
	if inner == nil {
		inner = AlwaysSucceed{}
	}
	return &Recorder{inner: inner}
}

func (r *Recorder) Execute(ctx context.Context, j *job.Job) (handler.Result, error) {
	r.mu.Lock()
	r.executed = append(r.executed, RecordedExecution{
		JobID:        j.ID.String(),
		JobType:      j.JobType,
		AttemptCount: j.AttemptCount,
	})
	r.mu.Unlock()
	return r.inner.Execute(ctx, j)
}

// Executions returns a copy of every recorded execution, in call order.
func (r *Recorder) Executions() []RecordedExecution {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedExecution, len(r.executed))
	copy(out, r.executed)
	return out
}
