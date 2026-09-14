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
	"log/slog"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
)

// Store is a PostgreSQL-backed job repository.
type Store struct {
	db      *sql.DB
	metrics *metrics.Metrics
	logger  *slog.Logger
}

// Option configures optional Store dependencies. See WithMetrics and
// WithLogger.
type Option func(*Store)

// WithMetrics attaches m as the Store's metrics recorder (Phase 8, per
// docs/observability.md). Every metric recorded by internal/store is
// recorded against m -- production wiring (cmd/api, cmd/worker) shares
// one *metrics.Metrics instance across the Store and the process's
// /metrics HTTP endpoint; tests pass an isolated metrics.New() to assert
// against without cross-test interference. If never supplied, New
// attaches a private, unregistered-anywhere metrics.New() instance, so
// every Store is always metrics-safe (no nil check needed at any call
// site) even in the many existing tests that call store.New(db) alone.
func WithMetrics(m *metrics.Metrics) Option {
	return func(s *Store) { s.metrics = m }
}

// WithLogger attaches l as the Store's structured logger (Phase 8), used
// only for the handful of lifecycle events Store uniquely observes and
// no caller can reconstruct: a genuine lease-expiry reclaim (as opposed
// to an ordinary post-backoff retry claim, which advances
// lease_generation identically -- see Claim's doc comment) and the lazy
// dead-letter sweep's silent DEAD_LETTERED transitions (which have no
// worker "claim" to attribute them to at all). Every other lifecycle
// event is logged by internal/worker and internal/api, which have
// handler-execution context Store does not. Defaults to slog.Default()
// if never supplied.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// New wraps an already-open *sql.DB. The caller owns the DB's lifecycle
// (including running migrations before first use). opts configures
// optional Phase 8 observability dependencies (WithMetrics, WithLogger);
// omitting them yields a fully functional Store with private,
// unregistered defaults -- every existing store.New(db) call site
// continues to compile and behave identically.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, metrics: metrics.New(), logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// jobColumns is table-qualified because Claim's RETURNING clause runs in
// the scope of a query that also selects from a "candidate" CTE — an
// unqualified column list there would be ambiguous wherever both relations
// share a column name (e.g. "id"). Qualifying unconditionally keeps a
// single column list usable by every query in this file.
const jobColumns = `
	jobs.id, jobs.principal_id, jobs.job_type, jobs.payload, jobs.state, jobs.priority, jobs.created_at, jobs.updated_at, jobs.eligible_at,
	jobs.scheduled_at, jobs.lease_owner, jobs.lease_generation, jobs.lease_expires_at, jobs.heartbeat_at,
	jobs.attempt_count, jobs.max_attempts, jobs.execution_timeout_seconds, jobs.cancel_requested,
	jobs.cancel_requested_at, jobs.idempotency_key, jobs.last_error, jobs.last_error_class,
	jobs.result_metadata, jobs.terminal_at, jobs.version`

// principalScopeClause is Phase 12's ownership predicate
// (docs/phase-12-plan.md §4a), rendered for the two positional parameters
// an authz value binds. It is appended to the WHERE clause of every
// statement that reads or mutates a caller-owned row, so authorization is
// part of the same statement that does the work rather than a separate
// check preceding it.
//
// Why this shape, and not a handler-level "read the row, check the owner,
// then call the existing store method":
//
//   - Every mutating cancellation path in this package is a fenced,
//     conditional UPDATE whose WHERE clause already carries every
//     condition that must hold for the operation to be valid (id, state,
//     and for worker-side calls lease_owner/lease_generation) -- the
//     idiom ADR-0002 and TF-INV-003/010/014 are built on. Adding
//     principal_id is one more predicate on that same statement.
//   - A handler-level pre-check would reintroduce exactly the
//     check-then-act TOCTOU window this codebase deliberately avoids (see
//     idempotency.go: the idempotency guarantee is real specifically
//     because the INSERT is attempted directly, never a check-then-act
//     read). Worse, internal/api.CancelJob's first action is already a
//     MUTATING call -- so a pre-check placed "before writing the
//     response" would have let an unauthorized cancel flip
//     cancel_requested on another principal's RUNNING job before any
//     ownership check ran.
//   - It makes "found but not yours" and "does not exist" the same
//     outcome by construction, at the SQL level: neither matches, so both
//     produce ErrNotFound/ErrStaleTransition through the identical code
//     path. No handler needs to remember to collapse them, and there is
//     no second error type to keep in sync (G2, verification point 9).
//
// The admin bypass is bound as a boolean parameter rather than branching
// into a second query, so there is exactly one statement per operation
// regardless of who is calling.
func principalScopeClause(isAdminParam, principalParam int) string {
	return fmt.Sprintf("($%d::boolean OR jobs.principal_id = $%d)", isAdminParam, principalParam)
}

// workflowPrincipalScopeClause is principalScopeClause's counterpart for
// statements against workflow_instances.
func workflowPrincipalScopeClause(isAdminParam, principalParam int) string {
	return fmt.Sprintf("($%d::boolean OR workflow_instances.principal_id = $%d)", isAdminParam, principalParam)
}

// scopeArgs returns the two positional arguments principalScopeClause and
// workflowPrincipalScopeClause consume, in order.
func scopeArgs(authz principal.AccessContext) []any {
	return []any{authz.IsAdmin, authz.PrincipalID}
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// queryRower is satisfied by both *sql.DB and *sql.Tx, letting transition
// (and anything built on it) run against either a bare connection or an
// already-open transaction without duplicating query logic.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// jobScanFields holds every scan destination for a jobColumns row. It is
// factored out of scanJob so Claim's RETURNING clause (jobColumns plus two
// extra "candidate" columns — see claim.go) can reuse the exact same
// destination list instead of duplicating twenty-plus Scan arguments.
type jobScanFields struct {
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
}

func (f *jobScanFields) dest() []any {
	return []any{
		&f.j.ID, &f.j.PrincipalID, &f.j.JobType, &f.j.Payload, &f.state, &f.j.Priority, &f.j.CreatedAt, &f.j.UpdatedAt, &f.j.EligibleAt,
		&f.scheduledAt, &f.leaseOwner, &f.j.LeaseGeneration, &f.leaseExpiresAt, &f.heartbeatAt,
		&f.j.AttemptCount, &f.j.MaxAttempts, &f.j.ExecutionTimeoutSeconds, &f.j.CancelRequested,
		&f.cancelReqAt, &f.idempotencyKey, &f.lastError, &f.lastErrorClass,
		&f.resultMetadata, &f.terminalAt, &f.j.Version,
	}
}

func (f *jobScanFields) materialize() *job.Job {
	j := f.j
	j.State = jobstate.State(f.state)
	if f.scheduledAt.Valid {
		j.ScheduledAt = &f.scheduledAt.Time
	}
	if f.leaseOwner.Valid {
		j.LeaseOwner = &f.leaseOwner.String
	}
	if f.leaseExpiresAt.Valid {
		j.LeaseExpiresAt = &f.leaseExpiresAt.Time
	}
	if f.heartbeatAt.Valid {
		j.HeartbeatAt = &f.heartbeatAt.Time
	}
	if f.cancelReqAt.Valid {
		j.CancelRequestedAt = &f.cancelReqAt.Time
	}
	if f.idempotencyKey.Valid {
		j.IdempotencyKey = &f.idempotencyKey.String
	}
	if f.lastError.Valid {
		j.LastError = &f.lastError.String
	}
	if f.lastErrorClass.Valid {
		j.LastErrorClass = &f.lastErrorClass.String
	}
	if f.resultMetadata != nil {
		j.ResultMetadata = f.resultMetadata
	}
	if f.terminalAt.Valid {
		j.TerminalAt = &f.terminalAt.Time
	}
	return &j
}

func scanJob(row rowScanner) (*job.Job, error) {
	var f jobScanFields
	if err := row.Scan(f.dest()...); err != nil {
		return nil, err
	}
	return f.materialize(), nil
}

// Insert durably creates a new job in QUEUED state and returns the row
// exactly as committed. This is a single INSERT statement, so it is a
// single implicit PostgreSQL transaction: the call either returns a fully
// committed row, or returns an error and the row does not exist at all —
// there is no partially-applied intermediate state (TF-INV-013), and
// callers (see internal/api) must not report submission success to a
// caller until this method returns without error (TF-INV-001).
//
// Insert is a thin wrapper around InsertIdempotent (see idempotency.go)
// that discards the "created" flag: every existing caller predates Phase
// 4 and never sets p.IdempotencyKey, so this preserves Insert's exact
// pre-Phase-4 behavior and signature unchanged.
func (s *Store) Insert(ctx context.Context, p job.NewParams) (*job.Job, error) {
	j, _, err := s.InsertIdempotent(ctx, p)
	return j, err
}

// GetByID returns the current durable row for id, scoped to authz's
// principal. It is a plain read: no locking, no side effects.
//
// Phase 12: a row that exists but belongs to a different, non-admin
// principal does not match this statement's WHERE clause, so it is
// reported as ErrNotFound -- byte-for-byte the same outcome, through the
// same code path, as an id that has never existed (G2, verification point
// 9). An admin principal (principal.KindAdmin) matches every row, which is
// the single documented exception.
func (s *Store) GetByID(ctx context.Context, id uuid.UUID, authz principal.AccessContext) (*job.Job, error) {
	args := append([]any{id}, scopeArgs(authz)...)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = $1 AND `+principalScopeClause(2, 3), args...)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job %s: %w", id, err)
	}
	return j, nil
}

// transition is the single choke point every fenced state-changing UPDATE
// in this package runs through: it (a) rejects, before issuing any SQL, a
// (from, to) pair that internal/jobstate says is illegal per
// docs/execution-semantics.md, and (b) maps "the UPDATE's WHERE clause
// matched zero rows" to the explicit ErrStaleTransition rather than a bare
// sql.ErrNoRows, so callers cannot mistake a rejected transition for "job
// not found". This is what docs/worker-protocol.md calls "The Fencing
// Guarantee, Stated Precisely" — see also TF-INV-003, TF-INV-005,
// TF-INV-014, TF-INV-015.
//
// q is a queryRower so this can run against either the bare *sql.DB
// (Heartbeat, a single-statement fenced update) or an open *sql.Tx
// (CompleteSuccess/CompleteFailure, which also write job_attempts in the
// same transaction — see complete.go).
func (s *Store) transition(ctx context.Context, q queryRower, from, to jobstate.State, query string, args ...any) (*job.Job, error) {
	if !jobstate.IsValidTransition(from, to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	j, err := scanTransitionResult(ctx, q, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: transition %s -> %s: %w", from, to, err)
	}
	return j, nil
}

// scanTransitionResult runs query (a fenced UPDATE ... RETURNING) and maps
// "affected zero rows" to ErrStaleTransition, per docs/worker-protocol.md's
// fencing guarantee. It is the low-level primitive transition uses after
// its single-destination legality check; internal/store's
// CompleteRetryableFailure (see retry.go) calls this directly because its
// single UPDATE statement can legally land on either of two destination
// states (RETRY_WAIT or DEAD_LETTERED, chosen by a SQL CASE expression, not
// by the caller), so the fixed single-`to` shape transition assumes does
// not fit -- callers of this function are responsible for validating
// whichever (from, to) pairs their query can actually produce before
// issuing it.
func scanTransitionResult(ctx context.Context, q queryRower, query string, args ...any) (*job.Job, error) {
	row := q.QueryRowContext(ctx, query, args...)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrStaleTransition
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}
