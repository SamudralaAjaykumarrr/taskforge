// Phase 11 (docs/enterprise-roadmap.md "Transactional Enqueue & API
// Contract Hardening"): InsertTx is the pgx.Tx-based counterpart to
// InsertIdempotent (idempotency.go) -- it inserts a job using a
// caller-owned, caller-managed pgx.Tx (an interface in pgx/v5, not a
// pointer type) instead of this Store's own connection pool, so a caller
// whose business data lives in the same PostgreSQL database can enqueue a
// job as part of their own transaction: the business write and the job
// insert commit together, or neither does. See the txenqueue package for
// the small, deliberate public Go surface built on top of this function --
// this file itself is not that public surface (docs/enterprise-roadmap.md
// is explicit that internal/store cannot be one: it is Go-enforced
// unimportable outside this module).
//
// Transaction-ownership discipline (docs/enterprise-roadmap.md "Transaction
// Boundary Safety"): InsertTx never calls Commit or Rollback on the tx
// argument itself, and never opens a second connection or a second
// top-level transaction. The only Begin/Commit/Rollback calls in this file
// are against the *pseudo-nested* transaction pgx.Tx.Begin returns when
// called on an already-open Tx -- pgx implements this via a PostgreSQL
// SAVEPOINT (see pgx/v5's tx.go: "Begin starts a pseudo nested transaction
// implemented with a savepoint"), never a second real transaction and
// never the caller's own tx. Committing that savepoint releases it;
// rolling it back issues "ROLLBACK TO SAVEPOINT", which un-aborts the
// caller's outer transaction after a failed statement without ending it --
// this is the standard PostgreSQL idiom for "attempt an INSERT, recover
// from a unique-constraint conflict, keep going in the same transaction"
// that InsertIdempotent's plain-pool version gets for free (each of its
// calls is its own separate implicit transaction, so a failed INSERT never
// poisons a later statement); inside a caller-supplied, already-open
// transaction the same recovery requires an explicit savepoint, or the
// caller's entire transaction would be left in PostgreSQL's aborted state
// by the very first idempotency-key conflict.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
)

// ErrTransactionRetry is InsertTx's narrow internal signal that the
// caller-owned tx cannot be resolved within its current PostgreSQL
// snapshot/serialization order and must be rolled back and retried in
// full by the caller -- never partially retried by this package, and
// never retried automatically. Two distinct conditions map to it:
//
//   - A PostgreSQL serialization failure or deadlock (SQLSTATE 40001 /
//     40P01) reported directly against the INSERT itself -- possible under
//     REPEATABLE READ/SERIALIZABLE even without an idempotency-key
//     conflict (e.g. write skew).
//   - An idempotency-key uniqueness conflict whose winning row cannot be
//     read back within this transaction's own snapshot (see
//     getByIdempotencyKeyTx's caller in InsertTx below): under READ
//     COMMITTED each statement gets a fresh snapshot, so the fallback
//     SELECT always sees an already-committed conflicting row -- but under
//     REPEATABLE READ/SERIALIZABLE, a transaction's snapshot is fixed at
//     (or before) its first statement, so a row committed by a *different*
//     transaction after that point is invisible to this one even though
//     PostgreSQL's own unique-index enforcement (which is not governed by
//     snapshot visibility) already detected the conflict. This is not "the
//     job doesn't exist" (ErrNotFound's ordinary meaning) -- the job does
//     exist, durably, in another committed transaction; this transaction
//     simply cannot see it and must not be told it can. See
//     docs/transactional-enqueue.md "Isolation level and idempotency
//     conflicts".
//
// This is deliberately not exported as part of any public API surface --
// internal/store is unimportable outside this module by construction; the
// txenqueue package maps it onto its own small public error contract (see
// txenqueue/errors.go) rather than re-exporting it directly.
var ErrTransactionRetry = errors.New("store: transaction must be rolled back and retried")

// isSerializationConflict reports whether err is a PostgreSQL serialization
// failure (40001) or deadlock (40P01) -- both conditions PostgreSQL expects
// the client to resolve by retrying the whole transaction from the start,
// never by retrying a single statement in place.
func isSerializationConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

// InsertTx behaves exactly like (*Store).InsertIdempotent (same validation
// expectations on p, same durable row shape, same idempotency semantics --
// see docs/idempotency.md) except that the INSERT (and, when
// p.IdempotencyKey collides with an already-committed row, the fallback
// re-read) runs against tx instead of any Store's pool. It is a
// package-level function, not a *Store method, because it needs no Store
// state at all (no pool, no metrics, no logger) -- every operation it
// performs runs through the caller-supplied tx -- which lets the txenqueue
// package call it without needing to construct or hold a *store.Store.
//
// Ownership: tx is never committed or rolled back by this function --
// success or failure, the caller decides tx's own fate. The returned
// *job.Job, if non-nil and err is nil, reflects a row that has been
// INSERTed within tx but is only durable once the caller commits tx; a
// caller that rolls back tx after a successful InsertTx call will find no
// such job exists (TF-INV-013, extended to this entry point -- see
// docs/invariants.md and the txenqueue package's integration tests for the
// commit/rollback proof).
//
// created reports whether this call's own INSERT is what created the row
// (true) or whether an idempotency-key conflict against a row some other,
// already-committed transaction created was found instead (false) -- same
// meaning as InsertIdempotent's created, and, like it, must never be used
// to choose a caller-visible status code (docs/idempotency.md).
//
// If p.IdempotencyKey is nil, this is a single INSERT directly against tx
// -- no savepoint is opened, because a fresh, randomly generated job id can
// only fail to insert for a genuine anomaly (not a condition this method
// is expected to recover from), and any such error is returned to the
// caller exactly as it is -- correctly leaving tx in whatever state
// PostgreSQL itself put it in, for the caller to inspect and roll back.
func InsertTx(ctx context.Context, tx pgx.Tx, p job.NewParams) (*job.Job, bool, error) {
	_, args := insertJobArgs(p)

	if p.IdempotencyKey == nil {
		j, err := scanJob(tx.QueryRow(ctx, insertJobQuery, args...))
		if err != nil {
			if isSerializationConflict(err) {
				return nil, false, fmt.Errorf("store: insert job (tx): %w", ErrTransactionRetry)
			}
			return nil, false, fmt.Errorf("store: insert job (tx): %w", err)
		}
		return j, true, nil
	}

	// A savepoint isolates the INSERT attempt: if it fails with an
	// idempotency-key unique-violation, rolling back to this savepoint
	// (not tx itself) clears PostgreSQL's aborted-transaction state so the
	// fallback re-read below can still run inside tx.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store: insert job (tx): open savepoint: %w", err)
	}

	j, err := scanJob(sp.QueryRow(ctx, insertJobQuery, args...))
	if err == nil {
		if cerr := sp.Commit(ctx); cerr != nil {
			return nil, false, fmt.Errorf("store: insert job (tx): release savepoint: %w", cerr)
		}
		return j, true, nil
	}

	if !isIdempotencyKeyViolation(err) {
		if isSerializationConflict(err) {
			// A genuine serialization failure/deadlock, not an
			// idempotency-key conflict -- the savepoint rollback below
			// still applies (it un-aborts tx for the caller's own
			// inspection), but the classification the caller needs is
			// "retry the whole transaction," not "here is the raw error."
			if rerr := sp.Rollback(ctx); rerr != nil {
				return nil, false, fmt.Errorf("store: insert job (tx): %w (savepoint rollback also failed: %v)", ErrTransactionRetry, rerr)
			}
			return nil, false, fmt.Errorf("store: insert job (tx): %w", ErrTransactionRetry)
		}
		// Some other failure (not an idempotency conflict this method
		// knows how to recover from). Roll back only the savepoint, so
		// tx's own state reflects nothing more than "the caller's prior
		// statements, if any, are unaffected" -- tx itself is left open
		// for the caller to decide whether to retry, continue, or roll
		// back entirely.
		if rerr := sp.Rollback(ctx); rerr != nil {
			return nil, false, fmt.Errorf("store: insert job (tx): %w (savepoint rollback also failed: %v)", err, rerr)
		}
		return nil, false, fmt.Errorf("store: insert job (tx): %w", err)
	}

	if rerr := sp.Rollback(ctx); rerr != nil {
		return nil, false, fmt.Errorf("store: insert job (tx): rollback to savepoint: %w", rerr)
	}

	existing, gerr := getByIdempotencyKeyTx(ctx, tx, p.PrincipalID, p.JobType, *p.IdempotencyKey)
	if gerr != nil {
		if errors.Is(gerr, ErrNotFound) {
			// The INSERT lost a uniqueness race against a row some other,
			// already-committed transaction created, but this
			// transaction's own MVCC snapshot cannot see it (see
			// ErrTransactionRetry's doc comment above) -- this is not
			// "the job doesn't exist," so ErrNotFound itself must never
			// reach the caller here.
			return nil, false, fmt.Errorf("store: insert job (tx): idempotency conflict not visible in this transaction's snapshot: %w", ErrTransactionRetry)
		}
		return nil, false, fmt.Errorf("store: insert job (tx): idempotency conflict, re-read failed: %w", gerr)
	}
	return existing, false, nil
}

// getByIdempotencyKeyTx is GetByIdempotencyKey's pgx.Tx-scoped
// counterpart, used only by InsertTx's conflict-recovery path above -- it
// must run inside the same tx as the failed INSERT, not against the pool,
// so it sees that transaction's own uncommitted-but-still-visible-to-itself
// view of the database.
//
// Under READ COMMITTED (PostgreSQL's default, and every isolation level
// this codebase used before Phase 11's pgx.Tx entry point), this SELECT
// always additionally sees the already-committed conflicting row from
// whichever other transaction won the race, per PostgreSQL's MVCC rules --
// each statement in a READ COMMITTED transaction gets a fresh snapshot. It
// is the caller InsertTx's responsibility, NOT this function's, to
// distinguish that ordinary case from REPEATABLE READ/SERIALIZABLE, where
// this SELECT's ErrNotFound can mean "the conflicting row exists and is
// durably committed, but this transaction's fixed snapshot predates that
// commit and cannot see it" rather than "no such job" -- see
// ErrTransactionRetry's doc comment.
func getByIdempotencyKeyTx(ctx context.Context, tx pgx.Tx, principalID uuid.UUID, jobType, key string) (*job.Job, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE principal_id = $1 AND job_type = $2 AND idempotency_key = $3`,
		principalID, jobType, key)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job by idempotency key (tx): %w", err)
	}
	return j, nil
}
