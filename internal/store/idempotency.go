// Phase 4: submission idempotency, per docs/idempotency.md and
// ADR-0004. The unique index that enforces it already existed as of
// migration 0001 (see migrations/0001_create_jobs_table.up.sql's comment:
// "not exercised by Phase 1's API ... but ... costs nothing to have in
// place before Phase 4 wires up the request-side support") -- so that
// phase needed no schema migration, only the application code that uses
// the constraint already sitting there.
//
// Phase 12 re-scoped that index from the global (job_type,
// idempotency_key) to the per-tenant (principal_id, job_type,
// idempotency_key) -- migration 0009, index idx_jobs_idempotency_scoped.
// The mechanism in this file is otherwise unchanged: the INSERT is still
// attempted directly and unconditionally, the conflict is still detected
// by PostgreSQL's own unique index, and the winning row is still re-read
// rather than pre-checked. Only the uniqueness scope moved.
//
// Phase 6 extends InsertIdempotent (not a separate method -- scheduling a
// job is still just an INSERT, per docs/scheduling.md's "Scheduling Is a
// Column, Not a Service") to accept an optional p.ScheduledAt, durably
// recorded as both scheduled_at (audit) and the row's initial eligible_at
// (the live gating timestamp) in the same INSERT statement -- no new
// schema, no new migration: scheduled_at and eligible_at have existed
// since migration 0001, per that migration's comment.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
)

// idempotencyKeyIndexName is the unique index that enforces submission
// idempotency (TF-INV-008, TF-INV-016). Its name is checked explicitly in
// isIdempotencyKeyViolation below -- not just the bare "unique_violation"
// SQLSTATE -- so InsertIdempotent's conflict-recovery path can never
// mistake some other, unrelated unique-constraint violation on jobs (e.g.
// a future constraint) for an idempotency-key collision.
//
// Phase 12 (docs/phase-12-plan.md §5): migration 0009 created, and 0010
// retired, migration 0001's global idx_jobs_idempotency_key in favour of the
// principal-scoped
// idx_jobs_idempotency_scoped on (principal_id, job_type,
// idempotency_key), so this constant MUST name the new index. The two are
// coupled by exact string match against pgErr.ConstraintName: if they ever
// drift apart, InsertIdempotent silently stops recognising idempotency
// conflicts and reports them as generic insert failures instead of
// returning the existing row. This is the single-hardcoded-string coupling
// docs/phase-12-plan.md flagged explicitly, kept in sync here and
// regression-covered by internal/store's idempotency tests.
const idempotencyKeyIndexName = "idx_jobs_idempotency_scoped"

// isIdempotencyKeyViolation reports whether err is a PostgreSQL
// unique-violation (SQLSTATE 23505) against idx_jobs_idempotency_scoped
// specifically.
func isIdempotencyKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == idempotencyKeyIndexName
}

// InsertIdempotent durably creates a new job in QUEUED state. If
// p.IdempotencyKey is nil, this behaves exactly like a plain insert -- a
// fresh job is always created, per docs/idempotency.md: "No
// Idempotency-Key supplied: every POST /jobs call creates a new job."
//
// If p.IdempotencyKey is set, TaskForge guarantees at most one job is
// ever created for that (principal_id, job_type, idempotency_key) triple
// (TF-INV-008, re-scoped by Phase 12 -- two different principals using the
// same job_type and key are two independent submissions, never a false
// duplicate), enforced entirely by idx_jobs_idempotency_scoped
// (TF-INV-016) -- never by
// a preceding "check if it already exists" read, which is what makes the
// guarantee hold under true concurrency, not just sequential retries:
//
//  1. The INSERT is attempted directly and unconditionally, first.
//  2. If it succeeds, this is the first submission with this key --
//     created=true, and the newly committed row is returned.
//  3. If it fails with a unique-violation against
//     idx_jobs_idempotency_scoped, some other submission by the SAME
//     principal with the same (job_type, idempotency_key) has already
//     committed -- created=false,
//     and that existing row is re-read and returned instead. Per
//     docs/idempotency.md's documented v1 decision, the existing row wins
//     unconditionally: this method never compares the new request's
//     payload against the original request that created the row, and
//     never reports a "conflicting key reuse" error of any kind --
//     first-write-wins, no exception, regardless of the job's current
//     state (even a DEAD_LETTERED job's row is still what gets returned;
//     resubmitting under the same key does not create a fresh attempt at
//     it -- see docs/execution-semantics.md on terminal states).
//
// The re-read in step 3 cannot itself fail with "not found": PostgreSQL
// only raises a unique-violation once the conflicting row has actually
// committed. (A concurrent INSERT that merely locked the conflicting key
// and then rolled back produces no violation at all -- this INSERT would
// simply have proceeded and won instead, per ordinary row-lock semantics.)
// Combined with the fact that job rows are never deleted (TF-INV-005:
// terminal states are never reopened, and nothing in this codebase ever
// issues a DELETE against jobs), the row read back in step 3 is
// guaranteed to exist -- so no retry loop is needed here for a race that
// cannot occur.
//
// created's only purpose is observability (see internal/api's "new
// idempotent submission" / "duplicate submission detected" log lines) --
// it must never be used to choose an HTTP status code: per
// docs/idempotency.md, a duplicate submission gets "the same success
// status code the original submission would have produced."
func (s *Store) InsertIdempotent(ctx context.Context, p job.NewParams) (*job.Job, bool, error) {
	_, args := insertJobArgs(p)
	row := s.db.QueryRowContext(ctx, insertJobQuery, args...)
	j, err := scanJob(row)
	if err == nil {
		s.metrics.JobsSubmittedTotal.WithLabelValues(p.JobType).Inc()
		return j, true, nil
	}
	if p.IdempotencyKey == nil || !isIdempotencyKeyViolation(err) {
		return nil, false, fmt.Errorf("store: insert job: %w", err)
	}

	existing, gerr := s.GetByIdempotencyKey(ctx, p.PrincipalID, p.JobType, *p.IdempotencyKey)
	if gerr != nil {
		return nil, false, fmt.Errorf("store: insert job: idempotency conflict, re-read failed: %w", gerr)
	}
	s.metrics.JobsSubmittedTotal.WithLabelValues(p.JobType).Inc()
	s.metrics.IdempotentSubmissionHitsTotal.Inc()
	return existing, false, nil
}

// insertJobQuery is the INSERT shared by the pool-based InsertIdempotent
// above and the pgx.Tx-based InsertTx (tx.go, Phase 11) -- both paths must
// create a durable jobs row with identical shape, so there is exactly one
// copy of this statement.
const insertJobQuery = `
	INSERT INTO jobs (id, principal_id, job_type, payload, state, max_attempts, execution_timeout_seconds, idempotency_key, scheduled_at, eligible_at)
	VALUES ($1, $8, $2, $3, 'QUEUED', $4, $5, $6, $7, COALESCE($7, now()))
	RETURNING ` + jobColumns

// insertJobArgs generates a fresh job id and builds insertJobQuery's
// positional arguments from p, shared by InsertIdempotent and InsertTx.
//
// eligible_at is COALESCE'd to now() (both evaluated by PostgreSQL's own
// clock, per docs/failure-model.md's Clock Model) rather than left at the
// schema's now()-default column, so a caller-supplied scheduled_at is what
// actually gates claim eligibility -- per docs/scheduling.md: "scheduled_at
// ... Set to now() (or scheduled_at) at submission time." scheduled_at
// itself is stored unmodified (NULL when not supplied) as the immutable
// audit record of the caller's original request, per that same document's
// field separation from the live, retry-advanced eligible_at.
func insertJobArgs(p job.NewParams) (uuid.UUID, []any) {
	id := uuid.New()
	var idemKey sql.NullString
	if p.IdempotencyKey != nil {
		idemKey = sql.NullString{String: *p.IdempotencyKey, Valid: true}
	}
	var scheduledAt sql.NullTime
	if p.ScheduledAt != nil {
		scheduledAt = sql.NullTime{Time: *p.ScheduledAt, Valid: true}
	}
	// Phase 12: p.PrincipalID is passed straight through with no
	// defaulting of any kind. A zero value is NOT rewritten to
	// principal.SystemPrincipalID (or to anything else) -- it is sent to
	// PostgreSQL as the all-zero UUID, which has no principals row, so the
	// insert fails on the foreign key. Failing loudly on an unattributed
	// submission is the point: there is no silent fallback identity
	// anywhere on this path (docs/phase-12-plan.md §6b, OD-1/OD-4).
	return id, []any{id, p.JobType, p.Payload, p.MaxAttempts, p.ExecutionTimeoutSeconds, idemKey, scheduledAt, p.PrincipalID}
}

// GetByIdempotencyKey returns the job durably mapped to (principalID,
// jobType, key), or ErrNotFound if that principal has never submitted a
// job with that key under that job_type. This is a plain, unlocked read --
// see docs/idempotency.md's scope note: "(job_type, idempotency_key) ...
// there is no global idempotency namespace," so the same key value under a
// different job_type is an entirely unrelated lookup.
//
// Phase 12 (G7): the lookup is principal-scoped, matching
// idx_jobs_idempotency_scoped exactly. This is load-bearing, not
// cosmetic -- InsertIdempotent's conflict-recovery path calls it to
// re-read "the row that won the race", and a globally-scoped read here
// would let principal A's insert conflict-recover onto principal B's row
// and hand B's job back to A. Two tenants choosing the same job_type and
// Idempotency-Key are two independent submissions, end to end
// (docs/enterprise-roadmap.md Phase 12's "two different tenants ... must
// not collide").
//
// It takes principalID rather than an AccessContext because it is not an
// authorization decision: it is the tenant half of a compound key. There
// is deliberately no admin bypass -- an admin reading another principal's
// job does so by id (GetByID), never by guessing at their idempotency
// keys.
func (s *Store) GetByIdempotencyKey(ctx context.Context, principalID uuid.UUID, jobType, key string) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE principal_id = $1 AND job_type = $2 AND idempotency_key = $3`,
		principalID, jobType, key)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job by idempotency key: %w", err)
	}
	return j, nil
}
