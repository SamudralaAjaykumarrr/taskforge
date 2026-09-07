// Package job defines the durable job record, matching the jobs table in
// docs/data-model.md column for column. It holds no behavior of its own
// beyond simple accessors — the state machine lives in internal/jobstate,
// and persistence lives in internal/store.
package job

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
)

// Job is the full durable row for a single logical unit of work.
type Job struct {
	ID                      uuid.UUID
	JobType                 string
	Payload                 json.RawMessage
	State                   jobstate.State
	Priority                int16
	CreatedAt               time.Time
	UpdatedAt               time.Time
	EligibleAt              time.Time
	ScheduledAt             *time.Time
	LeaseOwner              *string
	LeaseGeneration         int64
	LeaseExpiresAt          *time.Time
	HeartbeatAt             *time.Time
	AttemptCount            int
	MaxAttempts             int
	ExecutionTimeoutSeconds int
	CancelRequested         bool
	CancelRequestedAt       *time.Time
	IdempotencyKey          *string
	LastError               *string
	LastErrorClass          *string
	ResultMetadata          json.RawMessage
	TerminalAt              *time.Time
	Version                 int64
}

// IsTerminal reports whether the job has reached a state with no legal
// outbound transition (TF-INV-005).
func (j *Job) IsTerminal() bool {
	return jobstate.IsTerminal(j.State)
}

// NewParams holds the caller-supplied fields for submitting a new job.
// Fields not listed here (priority, scheduled_at, idempotency_key,
// cancellation) are Phase 1 non-goals per docs/roadmap.md and are left at
// their schema defaults (see internal/store.Insert).
type NewParams struct {
	JobType                 string
	Payload                 json.RawMessage
	MaxAttempts             int
	ExecutionTimeoutSeconds int
}

// Failure classes recorded in last_error_class, per docs/data-model.md.
const (
	ErrorClassPermanent = "PERMANENT"
	ErrorClassRetryable = "RETRYABLE"
)
