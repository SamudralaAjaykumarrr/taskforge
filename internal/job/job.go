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
	ID uuid.UUID
	// PrincipalID is the API caller that submitted this job (Phase 12,
	// migrations 0006-0009 -- NOT NULL). Every principal-scoped read and
	// cancellation in internal/store matches on this column inside the
	// same statement that does the work, never as a separate check
	// (docs/phase-12-plan.md §4a).
	PrincipalID             uuid.UUID
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
	// QueueName is Phase 13's named-queue column (docs/phase-13-plan.md
	// §7). Every job carries one, defaulting to "default" for every
	// pre-Phase-13 row and for any submission that does not name one
	// explicitly -- see internal/job.ValidateSubmission.
	QueueName string
}

// IsTerminal reports whether the job has reached a state with no legal
// outbound transition (TF-INV-005).
func (j *Job) IsTerminal() bool {
	return jobstate.IsTerminal(j.State)
}

// NewParams holds the caller-supplied fields for submitting a new job.
// Fields not listed here (priority, cancellation) are non-goals per
// docs/roadmap.md and are left at their schema defaults (see
// internal/store.Insert). IdempotencyKey was added in Phase 4 (see
// internal/store.InsertIdempotent, docs/idempotency.md) -- a nil value
// means no key was supplied, and every such submission creates a new job.
// ScheduledAt was added in Phase 6 (see internal/store.InsertIdempotent,
// docs/scheduling.md) -- a nil value means "run as soon as possible"
// (eligible_at defaults to the insert-time now(), unchanged from Phase
// 1-5); a non-nil value is durably recorded as both scheduled_at (caller
// intent, immutable, audit-only) and the job's initial eligible_at (the
// live gating timestamp the claim query actually consults).
type NewParams struct {
	// PrincipalID is the authenticated API caller this job is submitted
	// on behalf of (Phase 12, docs/phase-12-plan.md §5). It is
	// mandatory: jobs.principal_id is NOT NULL (migrations 0008/0009), so a
	// zero value is rejected by PostgreSQL rather than silently stored,
	// and nothing anywhere resolves an unset value to
	// principal.SystemPrincipalID -- that identity is assigned by
	// migration 0007's backfill alone.
	//
	// It is typed uuid.UUID rather than internal/principal's own type to
	// keep internal/job free of a dependency on the identity package: a
	// job row records WHICH principal owns it, and needs to know nothing
	// else about principals.
	//
	// It is populated exactly once per submission, from the
	// authenticated principal.AccessContext, and never from a request
	// body, query parameter, or header (docs/phase-12-plan.md §6a,
	// verification point 6).
	PrincipalID             uuid.UUID
	JobType                 string
	Payload                 json.RawMessage
	MaxAttempts             int
	ExecutionTimeoutSeconds int
	IdempotencyKey          *string
	ScheduledAt             *time.Time
	// QueueName is Phase 13's optional named-queue field
	// (docs/phase-13-plan.md §8). Populated by ValidateSubmission, which
	// defaults an empty/unsupplied value to "default" -- every call site
	// that constructs NewParams directly (internal/store/workflow.go) must
	// apply the same default, never leave this empty, since the INSERT
	// always supplies an explicit value rather than relying on the
	// schema's own DEFAULT 'default' (jobs.queue_name is NOT NULL with no
	// volatile default-dependent behavior once a value is explicitly
	// bound).
	QueueName string
}

// Failure classes recorded in last_error_class, per docs/data-model.md.
const (
	ErrorClassPermanent = "PERMANENT"
	ErrorClassRetryable = "RETRYABLE"
)
