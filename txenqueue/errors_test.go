// Phase 11 audit-fix tests (see docs/transactional-enqueue.md "Error
// contract" and "Isolation level and idempotency conflicts"): the small,
// deliberate public error contract EnqueueTx now guarantees, proven
// against real PostgreSQL -- never a mock, per docs/testing-strategy.md.
package txenqueue_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/txenqueue"
)

// TestEnqueueTx_NilTransaction_NoPanicReturnsInvalidTransactionError is the
// exact-scope regression this audit requires: an ordinary nil pgx.Tx
// interface value (the common real-world mistake -- a caller forgetting to
// check pool.Begin's own error before passing its zero-value tx through)
// must never reach store.InsertTx, which would panic dereferencing it. No
// real PostgreSQL connection is needed for this case -- validation and the
// nil check both happen before any statement would be issued.
func TestEnqueueTx_NilTransaction_NoPanicReturnsInvalidTransactionError(t *testing.T) {
	st := txenqueue.New()

	var tx pgx.Tx // ordinary nil interface, not a custom typed-nil implementation
	require.NotPanics(t, func() {
		j, created, err := st.EnqueueTx(context.Background(), tx, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: "test.tx.niltx"})
		require.Error(t, err)
		require.ErrorIs(t, err, txenqueue.ErrInvalidTransaction)
		require.Nil(t, j)
		require.False(t, created)
	})
}

// TestEnqueueTx_ValidationFailure_ClassifiesAsErrInvalidRequest proves
// req-validation failures are always classifiable as ErrInvalidRequest via
// errors.Is, while still carrying the same safe, human-readable validation
// detail POST /jobs's 400 body would show -- the public contract requires
// both: classifiable, and not silently opaque for an ordinary caller
// mistake.
func TestEnqueueTx_ValidationFailure_ClassifiesAsErrInvalidRequest(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, err = st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: "   "})
	require.Error(t, err)
	require.ErrorIs(t, err, txenqueue.ErrInvalidRequest)
	require.Contains(t, err.Error(), "job_type is required", "the safe validation detail must still be present")
}

// TestEnqueueTx_UnexpectedInsertFailure_ClassifiesAsErrEnqueueFailed_NoLeak
// proves an unexpected PostgreSQL insert failure that is neither a unique
// (idempotency-key) violation nor a serialization/deadlock condition --
// here, a job_type value containing a NUL byte, which PostgreSQL's TEXT
// type cannot represent (SQLSTATE 22021 / a client-side encoding
// rejection, depending on where it is caught) -- is classified as the
// generic ErrEnqueueFailed, and that the returned error's text contains
// none of the underlying failure's raw detail.
func TestEnqueueTx_UnexpectedInsertFailure_ClassifiesAsErrEnqueueFailed_NoLeak(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, err = st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: "bad\x00type"})
	require.Error(t, err)
	require.ErrorIs(t, err, txenqueue.ErrEnqueueFailed)
	require.False(t, errors.Is(err, txenqueue.ErrInvalidRequest), "a NUL byte is not rejected by internal/job.ValidateSubmission -- this must fail at the database, not validation")

	for _, leak := range []string{"SQLSTATE", "22021", "store:", "pq:", "0x00", "PostgreSQL"} {
		require.NotContains(t, err.Error(), leak, "error text must not leak raw PostgreSQL/internal/store detail")
	}
}

// TestEnqueueTx_ReadCommitted_DuplicateAcrossSeparateTransactions_ResolvesToExistingRow
// is the READ COMMITTED baseline this audit requires: under PostgreSQL's
// default isolation level (in effect for every pre-Phase-11 codepath and
// for pool.Begin's default here), a second, wholly separate transaction
// calling EnqueueTx with a key an already-committed transaction used
// always resolves cleanly to that existing row (created=false, no error,
// the SAME job_id) -- because each READ COMMITTED statement gets a fresh
// snapshot, so the fallback re-read (internal/store's getByIdempotencyKeyTx)
// always sees the winning commit. This is the passing case
// TestEnqueueTx_RepeatableRead_IdempotencyConflictOutsideSnapshot_MapsToMustRetry
// below deliberately contrasts with.
func TestEnqueueTx_ReadCommitted_DuplicateAcrossSeparateTransactions_ResolvesToExistingRow(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	const jobType = "test.tx.idem.read_committed"
	key := "read-committed-cross-tx"

	txA, err := pool.Begin(ctx)
	require.NoError(t, err)
	jA, createdA, err := st.EnqueueTx(ctx, txA, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: jobType, IdempotencyKey: &key})
	require.NoError(t, err)
	require.True(t, createdA)
	require.NoError(t, txA.Commit(ctx))

	txB, err := pool.Begin(ctx) // default isolation: READ COMMITTED
	require.NoError(t, err)
	jB, createdB, err := st.EnqueueTx(ctx, txB, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: jobType, IdempotencyKey: &key})
	require.NoError(t, err, "READ COMMITTED must always resolve a cross-transaction idempotency conflict, never require a retry")
	require.False(t, createdB)
	require.Equal(t, jA.ID, jB.ID)
	require.NoError(t, txB.Commit(ctx))

	require.Equal(t, 1, jobTypeCount(t, pool, jobType))
}

// TestEnqueueTx_RepeatableRead_IdempotencyConflictOutsideSnapshot_MapsToMustRetry
// is the REPEATABLE READ proof this audit specifically requires: B's
// snapshot is fixed (by its first statement) BEFORE A commits the
// conflicting idempotency-key row. B's own INSERT still loses the
// uniqueness race (PostgreSQL's unique-index enforcement checks the latest
// committed data, not a transaction's MVCC snapshot), but B's fallback
// re-read -- constrained to run inside B's own already-fixed snapshot --
// cannot see A's now-committed row. The old behavior (fixed by this audit)
// would have let store.ErrNotFound leak out of EnqueueTx here, wrongly
// implying "no such job" rather than "this transaction must retry."
func TestEnqueueTx_RepeatableRead_IdempotencyConflictOutsideSnapshot_MapsToMustRetry(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	const jobType = "test.tx.idem.repeatable_read"
	key := "repeatable-read-snapshot-conflict"

	txB, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	defer func() { _ = txB.Rollback(ctx) }()

	// Fix B's REPEATABLE READ snapshot (Postgres fixes it at the first
	// statement, not at BEGIN) strictly before A commits below.
	var one int
	require.NoError(t, txB.QueryRow(ctx, `SELECT 1`).Scan(&one))

	txA, err := pool.Begin(ctx)
	require.NoError(t, err)
	jA, createdA, err := st.EnqueueTx(ctx, txA, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: jobType, IdempotencyKey: &key})
	require.NoError(t, err)
	require.True(t, createdA)
	require.NoError(t, txA.Commit(ctx))

	_, _, err = st.EnqueueTx(ctx, txB, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: jobType, IdempotencyKey: &key})
	require.Error(t, err, "B's fixed snapshot cannot see A's post-snapshot commit -- this must not be reported as success")
	require.ErrorIs(t, err, txenqueue.ErrMustRetryTransaction)
	require.False(t, errors.Is(err, store.ErrNotFound), "must never surface store.ErrNotFound's \"job doesn't exist\" meaning -- the job exists, just outside this snapshot")
	for _, leak := range []string{"SQLSTATE", "pgx", "store:", "ErrNoRows"} {
		require.NotContains(t, err.Error(), leak, "error text must not leak raw internal/store/pgx detail")
	}

	require.NoError(t, txB.Rollback(ctx))

	// A's row is the only durable one -- B never created a second row, and
	// B's failed attempt left no trace.
	require.Equal(t, 1, jobTypeCount(t, pool, jobType))
	require.Equal(t, jA.ID, mustGetByIdempotencyKey(t, pool, jobType, key))
}

// TestEnqueueTx_Serializable_IdempotencyConflictOutsideSnapshot_MapsToMustRetry
// exercises the analogous case at SERIALIZABLE: SERIALIZABLE's snapshot
// visibility rules for an ordinary read are the same as REPEATABLE READ's
// (the additional serializable-only guarantees are about detecting
// dependency cycles between concurrent transactions, not about widening
// what a fixed snapshot can see), so the same "fallback re-read outside
// the snapshot" condition arises here too and must be classified
// identically -- ErrMustRetryTransaction, never store.ErrNotFound. This
// test does not attempt to construct a genuine write-skew serialization
// failure (SQLSTATE 40001) -- doing so deterministically requires a more
// elaborate concurrent read/write dependency cycle than this fixture's
// scope calls for ("where applicable"); the idempotency-conflict path is
// the concrete, deterministic SERIALIZABLE proof obligation this phase
// requires.
func TestEnqueueTx_Serializable_IdempotencyConflictOutsideSnapshot_MapsToMustRetry(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	const jobType = "test.tx.idem.serializable"
	key := "serializable-snapshot-conflict"

	txB, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	require.NoError(t, err)
	defer func() { _ = txB.Rollback(ctx) }()

	var one int
	require.NoError(t, txB.QueryRow(ctx, `SELECT 1`).Scan(&one))

	txA, err := pool.Begin(ctx)
	require.NoError(t, err)
	jA, createdA, err := st.EnqueueTx(ctx, txA, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: jobType, IdempotencyKey: &key})
	require.NoError(t, err)
	require.True(t, createdA)
	require.NoError(t, txA.Commit(ctx))

	_, _, err = st.EnqueueTx(ctx, txB, txenqueue.EnqueueRequest{PrincipalID: testPrincipalID, JobType: jobType, IdempotencyKey: &key})
	require.Error(t, err)
	require.ErrorIs(t, err, txenqueue.ErrMustRetryTransaction)
	require.False(t, errors.Is(err, store.ErrNotFound))
	for _, leak := range []string{"SQLSTATE", "pgx", "store:", "ErrNoRows"} {
		require.NotContains(t, err.Error(), leak, "error text must not leak raw internal/store/pgx detail")
	}

	require.NoError(t, txB.Rollback(ctx))
	require.Equal(t, 1, jobTypeCount(t, pool, jobType))
	require.Equal(t, jA.ID, mustGetByIdempotencyKey(t, pool, jobType, key))
}

// mustGetByIdempotencyKey reads jobs.id directly (bypassing txenqueue/
// internal/store entirely) to independently confirm exactly which row is
// durable, for tests above that need to assert against it after their own
// transactions have already resolved.
func mustGetByIdempotencyKey(t *testing.T, pool *pgxpool.Pool, jobType, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT id FROM jobs WHERE job_type = $1 AND idempotency_key = $2`, jobType, key).Scan(&id))
	return id
}
