// Phase 4: submission idempotency, per docs/idempotency.md and
// ADR-0004. The (job_type, idempotency_key) unique index
// (idx_jobs_idempotency_key) already exists as of migration 0001 (see
// migrations/0001_create_jobs_table.up.sql's comment: "not exercised by
// Phase 1's API ... but ... costs nothing to have in place before Phase 4
// wires up the request-side support") -- so this phase needs no schema
// migration, only the application code that uses the constraint already
// sitting there.
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

// idempotencyKeyIndexName is the unique index migration 0001 creates on
// (job_type, idempotency_key) WHERE idempotency_key IS NOT NULL
// (TF-INV-008, TF-INV-016). Its name is checked explicitly in
// isIdempotencyKeyViolation below -- not just the bare "unique_violation"
// SQLSTATE -- so InsertIdempotent's conflict-recovery path can never
// mistake some other, unrelated unique-constraint violation on jobs (e.g.
// a future constraint) for an idempotency-key collision.
const idempotencyKeyIndexName = "idx_jobs_idempotency_key"

// isIdempotencyKeyViolation reports whether err is a PostgreSQL
// unique-violation (SQLSTATE 23505) against idx_jobs_idempotency_key
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
// ever created for that (job_type, idempotency_key) pair (TF-INV-008),
// enforced entirely by idx_jobs_idempotency_key (TF-INV-016) -- never by
// a preceding "check if it already exists" read, which is what makes the
// guarantee hold under true concurrency, not just sequential retries:
//
//  1. The INSERT is attempted directly and unconditionally, first.
//  2. If it succeeds, this is the first submission with this key --
//     created=true, and the newly committed row is returned.
//  3. If it fails with a unique-violation against
//     idx_jobs_idempotency_key, some other submission with the same
//     (job_type, idempotency_key) has already committed -- created=false,
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
	id := uuid.New()
	var idemKey sql.NullString
	if p.IdempotencyKey != nil {
		idemKey = sql.NullString{String: *p.IdempotencyKey, Valid: true}
	}

	row := s.db.QueryRowContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, max_attempts, execution_timeout_seconds, idempotency_key)
		VALUES ($1, $2, $3, 'QUEUED', $4, $5, $6)
		RETURNING `+jobColumns,
		id, p.JobType, p.Payload, p.MaxAttempts, p.ExecutionTimeoutSeconds, idemKey,
	)
	j, err := scanJob(row)
	if err == nil {
		return j, true, nil
	}
	if p.IdempotencyKey == nil || !isIdempotencyKeyViolation(err) {
		return nil, false, fmt.Errorf("store: insert job: %w", err)
	}

	existing, gerr := s.GetByIdempotencyKey(ctx, p.JobType, *p.IdempotencyKey)
	if gerr != nil {
		return nil, false, fmt.Errorf("store: insert job: idempotency conflict, re-read failed: %w", gerr)
	}
	return existing, false, nil
}

// GetByIdempotencyKey returns the job durably mapped to (jobType, key), or
// ErrNotFound if no job has ever been submitted with that key under that
// job_type. This is a plain, unlocked read -- see docs/idempotency.md's
// scope note: "(job_type, idempotency_key) ... there is no global
// idempotency namespace," so the same key value under a different
// job_type is an entirely unrelated lookup.
func (s *Store) GetByIdempotencyKey(ctx context.Context, jobType, key string) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_type = $1 AND idempotency_key = $2`, jobType, key)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job by idempotency key: %w", err)
	}
	return j, nil
}
