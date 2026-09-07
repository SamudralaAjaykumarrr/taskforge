// Package store is the sole persistence layer for TaskForge jobs. Every
// method here maps directly to a transition or read described in
// docs/worker-protocol.md and docs/execution-semantics.md. PostgreSQL is
// the only datastore involved (ADR-0001); there is no in-memory cache or
// fallback path, and no method here ever reports success before the
// underlying statement has committed (TF-INV-001).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
)

// Store is a PostgreSQL-backed job repository.
type Store struct {
	db *sql.DB
}

// New wraps an already-open *sql.DB. The caller owns the DB's lifecycle
// (including running migrations before first use).
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// jobColumns is table-qualified because Claim's RETURNING clause runs in
// the scope of a query that also selects from a "candidate" CTE — an
// unqualified column list there would be ambiguous wherever both relations
// share a column name (e.g. "id"). Qualifying unconditionally keeps a
// single column list usable by every query in this file.
const jobColumns = `
	jobs.id, jobs.job_type, jobs.payload, jobs.state, jobs.priority, jobs.created_at, jobs.updated_at, jobs.eligible_at,
	jobs.scheduled_at, jobs.lease_owner, jobs.lease_generation, jobs.lease_expires_at, jobs.heartbeat_at,
	jobs.attempt_count, jobs.max_attempts, jobs.execution_timeout_seconds, jobs.cancel_requested,
	jobs.cancel_requested_at, jobs.idempotency_key, jobs.last_error, jobs.last_error_class,
	jobs.result_metadata, jobs.terminal_at, jobs.version`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (*job.Job, error) {
	var (
		j              job.Job
		state          string
		scheduledAt    sql.NullTime
		leaseOwner     sql.NullString
		leaseExpiresAt sql.NullTime
		heartbeatAt    sql.NullTime
		cancelReqAt    sql.NullTime
		idempotencyKey sql.NullString
		lastError      sql.NullString
		lastErrorClass sql.NullString
		resultMetadata []byte
		terminalAt     sql.NullTime
	)

	err := row.Scan(
		&j.ID, &j.JobType, &j.Payload, &state, &j.Priority, &j.CreatedAt, &j.UpdatedAt, &j.EligibleAt,
		&scheduledAt, &leaseOwner, &j.LeaseGeneration, &leaseExpiresAt, &heartbeatAt,
		&j.AttemptCount, &j.MaxAttempts, &j.ExecutionTimeoutSeconds, &j.CancelRequested,
		&cancelReqAt, &idempotencyKey, &lastError, &lastErrorClass,
		&resultMetadata, &terminalAt, &j.Version,
	)
	if err != nil {
		return nil, err
	}

	j.State = jobstate.State(state)
	if scheduledAt.Valid {
		j.ScheduledAt = &scheduledAt.Time
	}
	if leaseOwner.Valid {
		j.LeaseOwner = &leaseOwner.String
	}
	if leaseExpiresAt.Valid {
		j.LeaseExpiresAt = &leaseExpiresAt.Time
	}
	if heartbeatAt.Valid {
		j.HeartbeatAt = &heartbeatAt.Time
	}
	if cancelReqAt.Valid {
		j.CancelRequestedAt = &cancelReqAt.Time
	}
	if idempotencyKey.Valid {
		j.IdempotencyKey = &idempotencyKey.String
	}
	if lastError.Valid {
		j.LastError = &lastError.String
	}
	if lastErrorClass.Valid {
		j.LastErrorClass = &lastErrorClass.String
	}
	if resultMetadata != nil {
		j.ResultMetadata = resultMetadata
	}
	if terminalAt.Valid {
		j.TerminalAt = &terminalAt.Time
	}

	return &j, nil
}

// Insert durably creates a new job in QUEUED state and returns the row
// exactly as committed. This is a single INSERT statement, so it is a
// single implicit PostgreSQL transaction: the call either returns a fully
// committed row, or returns an error and the row does not exist at all —
// there is no partially-applied intermediate state (TF-INV-013), and
// callers (see internal/api) must not report submission success to a
// caller until this method returns without error (TF-INV-001).
func (s *Store) Insert(ctx context.Context, p job.NewParams) (*job.Job, error) {
	id := uuid.New()
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, max_attempts, execution_timeout_seconds)
		VALUES ($1, $2, $3, 'QUEUED', $4, $5)
		RETURNING `+jobColumns,
		id, p.JobType, p.Payload, p.MaxAttempts, p.ExecutionTimeoutSeconds,
	)
	j, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("store: insert job: %w", err)
	}
	return j, nil
}

// GetByID returns the current durable row for id. It is a plain read: no
// locking, no side effects.
func (s *Store) GetByID(ctx context.Context, id uuid.UUID) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job %s: %w", id, err)
	}
	return j, nil
}

// Claim atomically finds and takes ownership of at most one eligible job
// for workerID, transitioning it QUEUED -> RUNNING.
//
// This is deliberately the Phase 1 SUBSET of the general claim query in
// docs/worker-protocol.md: it only considers state = 'QUEUED' rows.
// docs/roadmap.md restricts Phase 1's state machine to
// "QUEUED -> RUNNING -> SUCCEEDED and a simple RUNNING -> DEAD_LETTERED on
// any failure (no retry machinery, no RETRY_WAIT yet)" — so there is never
// a RETRY_WAIT row to reclaim, and Phase 1 has no lease-expiration/reclaim
// mechanism at all (that is Phase 2 scope: "Do not implement yet: ...
// lease expiration"). Extending the WHERE clause to also match expired-
// lease RUNNING rows, as the full worker-protocol query does, would
// silently implement Phase 2's reclaim behavior ahead of schedule. A
// RUNNING job whose worker crashed before completing therefore stays
// RUNNING until a human resubmits a new job — an explicitly accepted
// Phase 1 limitation (see docs/roadmap.md Phase 1 completion criteria).
//
// The second return value is false (with a nil error) when there is
// nothing eligible to claim right now — that is a normal, expected
// outcome, not a failure.
func (s *Store) Claim(ctx context.Context, workerID string) (*job.Job, bool, error) {
	if !jobstate.IsValidTransition(jobstate.Queued, jobstate.Running) {
		return nil, false, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, jobstate.Queued, jobstate.Running)
	}

	row := s.db.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT id
			FROM jobs
			WHERE state = 'QUEUED' AND eligible_at <= now()
			ORDER BY priority DESC, eligible_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE jobs
		SET state = 'RUNNING',
			lease_owner = $1,
			lease_generation = jobs.lease_generation + 1,
			lease_expires_at = now() + make_interval(secs => jobs.execution_timeout_seconds),
			heartbeat_at = now(),
			attempt_count = jobs.attempt_count + 1,
			updated_at = now(),
			cancel_requested = false,
			cancel_requested_at = NULL,
			version = jobs.version + 1
		FROM candidate
		WHERE jobs.id = candidate.id
		RETURNING `+jobColumns,
		workerID,
	)

	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: claim: %w", err)
	}
	return j, true, nil
}

// CompleteSuccess fences on (id, leaseOwner, leaseGeneration, state =
// RUNNING) and transitions the job RUNNING -> SUCCEEDED, per
// docs/worker-protocol.md "Completion / Success". If the guard does not
// match — the job is not RUNNING, or is RUNNING under a different lease —
// this returns ErrStaleTransition and leaves the row untouched
// (TF-INV-003).
func (s *Store) CompleteSuccess(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, resultMetadata []byte) (*job.Job, error) {
	return s.transition(ctx, jobstate.Running, jobstate.Succeeded, `
		UPDATE jobs
		SET state = 'SUCCEEDED',
			lease_owner = NULL,
			lease_expires_at = NULL,
			result_metadata = $4,
			terminal_at = now(),
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING'
		RETURNING `+jobColumns,
		id, leaseOwner, leaseGeneration, resultMetadata,
	)
}

// CompleteFailure fences the same way as CompleteSuccess and transitions
// RUNNING -> DEAD_LETTERED unconditionally.
//
// This is Phase 1's documented simplification of
// docs/worker-protocol.md's "Retryable Failure" / "Permanent Failure"
// queries: docs/roadmap.md states Phase 1 has "no retry machinery, no
// RETRY_WAIT yet — a failure goes straight to DEAD_LETTERED", regardless
// of attempt_count or error classification. Phase 3 introduces the real
// RETRY_WAIT branch and attempt-budget check.
func (s *Store) CompleteFailure(ctx context.Context, id uuid.UUID, leaseOwner string, leaseGeneration int64, errMessage, errClass string) (*job.Job, error) {
	return s.transition(ctx, jobstate.Running, jobstate.DeadLettered, `
		UPDATE jobs
		SET state = 'DEAD_LETTERED',
			lease_owner = NULL,
			lease_expires_at = NULL,
			last_error = $4,
			last_error_class = $5,
			terminal_at = now(),
			updated_at = now(),
			version = version + 1
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND state = 'RUNNING'
		RETURNING `+jobColumns,
		id, leaseOwner, leaseGeneration, errMessage, errClass,
	)
}

// transition is the single choke point every fenced state-changing UPDATE
// in this package runs through: it (a) rejects, before issuing any SQL, a
// (from, to) pair that internal/jobstate says is illegal per
// docs/execution-semantics.md, and (b) maps "the UPDATE's WHERE clause
// matched zero rows" to the explicit ErrStaleTransition rather than a bare
// sql.ErrNoRows, so callers cannot mistake a rejected transition for "job
// not found". This is what docs/worker-protocol.md calls "The Fencing
// Guarantee, Stated Precisely" — see also TF-INV-003, TF-INV-005,
// TF-INV-014.
func (s *Store) transition(ctx context.Context, from, to jobstate.State, query string, args ...any) (*job.Job, error) {
	if !jobstate.IsValidTransition(from, to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}

	row := s.db.QueryRowContext(ctx, query, args...)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrStaleTransition
	}
	if err != nil {
		return nil, fmt.Errorf("store: transition %s -> %s: %w", from, to, err)
	}
	return j, nil
}
