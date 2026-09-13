package txenqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

// Public error contract (Phase 11, docs/enterprise-roadmap.md's public-API
// error-leakage requirement). EnqueueTx never returns a raw
// internal/store or pgx/PostgreSQL error to the caller -- every non-nil
// error it returns is classifiable, via errors.Is, as exactly one of the
// four sentinels below, or is exactly context.Canceled/
// context.DeadlineExceeded (the bare stdlib sentinel, not an
// internal/store-wrapped error that merely satisfies errors.Is against it
// -- audit finding: an earlier version of classifyStoreErr returned the
// wrapped error unchanged here, which could leak a "store: ..." prefix or
// other internal/store wording through Error()) so a caller's existing
// ctx-cancellation checks keep working. The underlying database error text
// (SQL, SQLSTATE, PostgreSQL server detail, internal/store's own
// "store: ..." wording) is deliberately never included in any error this
// package returns.
//
// That discarded detail is NOT preserved anywhere by this package: txenqueue
// logs nothing itself, and does not hand the underlying error to a caller
// by any other means -- it is simply dropped at this public boundary. A
// caller that wants the raw underlying failure logged on TaskForge's own
// server side must do so itself, from its own error (same as for any other
// Go error) -- this package provides no such observability today. That gap
// -- no server-side record of the specific persistence failure a sanitized
// txenqueue error corresponds to -- is an explicit, deferred observability
// gap, not something this phase claims is already covered elsewhere.
var (
	// ErrInvalidRequest means req itself failed TaskForge's ordinary
	// submission validation (internal/job.ValidateSubmission -- the same
	// rules POST /jobs and POST /workflows apply). tx is never touched:
	// no statement is issued against it, so it remains exactly as usable
	// as it was before this call. Error() additionally carries the
	// specific validation reason (e.g. "job_type is required") --
	// unlike the other three sentinels, this text is always safe to
	// surface to an end caller, because it is the same stable,
	// public-facing validation wording POST /jobs already returns in a
	// 400 body.
	ErrInvalidRequest = errors.New("txenqueue: invalid request")

	// ErrInvalidTransaction means tx itself cannot be used to enqueue a
	// job: it is nil, or it is a pgx.Tx PostgreSQL/pgx has already ended
	// (an earlier Commit or Rollback, or a prior failed statement that
	// closed it). No statement is issued in this case either. The
	// caller's tx (if it still exists as a Go value) must not be reused
	// as though this call had never happened -- if it is not already
	// nil/closed, the caller is still responsible for deciding whether to
	// roll it back, exactly as documented for any other EnqueueTx error.
	ErrInvalidTransaction = errors.New("txenqueue: transaction is nil or unusable")

	// ErrMustRetryTransaction means PostgreSQL could not resolve this
	// EnqueueTx call within tx's current transaction: either a genuine
	// serialization failure/deadlock (possible under REPEATABLE
	// READ/SERIALIZABLE), or an idempotency-key conflict whose winning
	// row exists, durably committed, but is not visible within tx's own
	// snapshot (also REPEATABLE READ/SERIALIZABLE-specific -- see
	// docs/transactional-enqueue.md "Isolation level and idempotency
	// conflicts"). In both cases tx may already be left in PostgreSQL's
	// aborted state by the failed statement.
	//
	// txenqueue never retries anything itself -- no second connection, no
	// hidden retry loop, no partial retry of tx. The caller must roll
	// back tx and retry its ENTIRE business transaction from the
	// beginning (open a new tx, redo every statement, call EnqueueTx
	// again) -- calling EnqueueTx again on the same tx after this error
	// is not a valid recovery, because tx itself may already be aborted.
	ErrMustRetryTransaction = errors.New("txenqueue: caller transaction must be rolled back and retried")

	// ErrEnqueueFailed is the generic public classification for any other
	// persistence failure this package does not more specifically
	// classify above -- an unexpected PostgreSQL error, a savepoint
	// operation failure, or any other internal/store failure. As with
	// the other sentinels, the underlying error's text is deliberately
	// not included.
	ErrEnqueueFailed = errors.New("txenqueue: enqueue failed")
)

// classifyStoreErr maps an error returned by internal/store.InsertTx (or by
// the pgx.Tx interface itself) onto txenqueue's four-sentinel public error
// contract above, discarding the underlying error's text so no SQL,
// SQLSTATE, constraint name, or other internal/store/PostgreSQL detail
// reaches a public caller. A context cancellation/deadline is reported as
// exactly the bare context.Canceled/context.DeadlineExceeded sentinel --
// not err itself -- so callers can keep using errors.Is against them
// directly (per Go's usual ctx-cancellation contract) without also
// receiving whatever internal/store wrapping happened to accompany it (e.g.
// InsertTx's "store: insert job (tx): ..." prefix); audit finding: an
// earlier version returned err unchanged here, which satisfies errors.Is
// but can still leak that wrapping through Error().
func classifyStoreErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, pgx.ErrTxCommitRollback) {
		return fmt.Errorf("%w", ErrInvalidTransaction)
	}
	if errors.Is(err, store.ErrTransactionRetry) {
		return fmt.Errorf("%w", ErrMustRetryTransaction)
	}
	return fmt.Errorf("%w", ErrEnqueueFailed)
}
