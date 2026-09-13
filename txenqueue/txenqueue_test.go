// Integration tests against a real PostgreSQL instance (see
// internal/testutil), per docs/testing-strategy.md: persistence and
// transaction-atomicity correctness are never asserted against a mock.
//
// These tests prove docs/enterprise-roadmap.md Phase 11's core claim: a
// caller-owned pgx.Tx used for both a business-data write and
// txenqueue.EnqueueTx either commits both together or neither commits --
// same-PostgreSQL-transaction atomicity, nothing more (no cross-database
// claim is made or tested here; see docs/transactional-enqueue.md).
package txenqueue_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/txenqueue"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

// newPool opens a native pgx connection pool against dsn -- the same
// PostgreSQL instance backing testutil.DB/DSN -- so a pgx.Tx obtained from
// it is a real, caller-owned transaction of exactly the kind
// txenqueue.EnqueueTx is designed for.
func newPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// newVerifyStore opens a plain, independent *sql.DB (via database/sql,
// exactly as cmd/api and cmd/worker do) against the same dsn, wrapped in an
// ordinary *store.Store -- used to prove a transactionally-enqueued job,
// once committed, is visible and claimable through TaskForge's normal
// engine, not merely present in whatever connection created it.
func newVerifyStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return store.New(db)
}

// setupBusinessTable creates (idempotently) a throwaway table standing in
// for "the caller's own business data, in the same PostgreSQL database" --
// deliberately unrelated to any TaskForge-owned table, since this
// package's whole point is working alongside a caller's schema it knows
// nothing about. Each test uses a fresh uuid per row, so no truncation
// between tests is needed.
func setupBusinessTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS txenqueue_test_business_rows (id uuid PRIMARY KEY, note text NOT NULL)`)
	require.NoError(t, err)
}

func businessRowExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var note string
	err := pool.QueryRow(context.Background(), `SELECT note FROM txenqueue_test_business_rows WHERE id = $1`, id).Scan(&note)
	if err == pgx.ErrNoRows {
		return false
	}
	require.NoError(t, err)
	return true
}

func jobTypeCount(t *testing.T, pool *pgxpool.Pool, jobType string) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE job_type = $1`, jobType).Scan(&count))
	return count
}

// TestEnqueueTx_Commit_BusinessDataAndJobBothDurable_ClaimableThroughNormalEngine
// is Section 2.A of docs/enterprise-roadmap.md Phase 11's proof
// obligations: within one caller-owned transaction, a business write and a
// transactional job enqueue both become durable on commit, and the job is
// claimable through TaskForge's ordinary engine (internal/store.Claim) --
// not a parallel table or execution path.
func TestEnqueueTx_Commit_BusinessDataAndJobBothDurable_ClaimableThroughNormalEngine(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	setupBusinessTable(t, pool)
	verify := newVerifyStore(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	businessID := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO txenqueue_test_business_rows (id, note) VALUES ($1, $2)`, businessID, "order-created")
	require.NoError(t, err)

	j, created, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{
		JobType: "test.tx.commit",
		Payload: json.RawMessage(`{"k":"v"}`),
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, j)

	require.NoError(t, tx.Commit(ctx))

	require.True(t, businessRowExists(t, pool, businessID), "business row must be durable after commit")

	fetched, err := verify.GetByID(ctx, j.ID)
	require.NoError(t, err)
	require.Equal(t, jobstate.Queued, fetched.State)

	claimed, ok, err := verify.Claim(ctx, "test-worker")
	require.NoError(t, err)
	require.True(t, ok, "the committed job must be claimable through the ordinary claim query")
	require.Equal(t, j.ID, claimed.ID)
	require.Equal(t, jobstate.Running, claimed.State)
}

// TestEnqueueTx_Rollback_NoBusinessDataNoJob is Section 2.B: within one
// caller-owned transaction, a business write plus a transactional enqueue,
// rolled back, leaves neither durable.
func TestEnqueueTx_Rollback_NoBusinessDataNoJob(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	setupBusinessTable(t, pool)
	verify := newVerifyStore(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	businessID := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO txenqueue_test_business_rows (id, note) VALUES ($1, $2)`, businessID, "order-created")
	require.NoError(t, err)

	j, _, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{JobType: "test.tx.rollback"})
	require.NoError(t, err)
	require.NotNil(t, j)

	require.NoError(t, tx.Rollback(ctx))

	require.False(t, businessRowExists(t, pool, businessID), "business row must not survive rollback")

	_, err = verify.GetByID(ctx, j.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "job must not survive rollback")
}

// TestEnqueueTx_SuccessfulEnqueueThenLaterCallerRollback_NoOrphanJob is
// Section 2.D: a successful EnqueueTx, followed by more caller work and a
// later caller-initiated rollback, must leave no orphaned job -- ordering
// (enqueue first, business write second) must not matter.
func TestEnqueueTx_SuccessfulEnqueueThenLaterCallerRollback_NoOrphanJob(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	setupBusinessTable(t, pool)
	verify := newVerifyStore(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	j, created, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{JobType: "test.tx.rollback.after.enqueue"})
	require.NoError(t, err)
	require.True(t, created)

	businessID := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO txenqueue_test_business_rows (id, note) VALUES ($1, $2)`, businessID, "later-business-write")
	require.NoError(t, err)

	// Simulates the caller's own later decision to abort (e.g. a
	// downstream business-logic failure) after the enqueue already
	// "succeeded" within the transaction.
	require.NoError(t, tx.Rollback(ctx))

	require.False(t, businessRowExists(t, pool, businessID))
	_, err = verify.GetByID(ctx, j.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a successful EnqueueTx must not leave an orphan job once the caller rolls back")
}

// TestEnqueueTx_FailsOnClosedTransaction_NoFalseSuccessNoOrphanJob is
// Section 2.C: if the enqueue itself fails inside the caller's
// transaction, EnqueueTx must report the failure (never a false success),
// transaction ownership remains with the caller, and no job is
// independently committed. A transaction pgx has already closed (e.g. by
// an earlier Rollback, standing in for some other failure that ended it)
// is used here as a deterministic way to force InsertTx's own statement to
// fail.
func TestEnqueueTx_FailsOnClosedTransaction_NoFalseSuccessNoOrphanJob(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx)) // close the transaction up front

	const jobType = "test.tx.closed"
	_, _, err = st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{JobType: jobType})
	require.Error(t, err, "EnqueueTx must never report success against a transaction it cannot actually use")
	require.ErrorIs(t, err, txenqueue.ErrInvalidTransaction, "a closed tx must classify as the public invalid-transaction error")
	for _, leak := range []string{"pgx", "SQLSTATE", "store:", "conn"} {
		require.NotContains(t, err.Error(), leak, "error text must not leak internal/pgx detail")
	}

	require.Equal(t, 0, jobTypeCount(t, pool, jobType), "no job may exist after a failed enqueue")
}

// TestEnqueueTx_ValidationFailure_NoJobCreated_TxRemainsUsable proves
// invalid input is rejected before tx is touched at all: no job is
// created, and the caller's transaction is left completely usable for
// whatever else the caller wants to do with it (including simply
// continuing and committing their own unrelated work).
func TestEnqueueTx_ValidationFailure_NoJobCreated_TxRemainsUsable(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	_, _, err = st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{JobType: "   "})
	require.Error(t, err)

	var one int
	require.NoError(t, tx.QueryRow(ctx, `SELECT 1`).Scan(&one), "tx must still be usable after a validation failure")
	require.Equal(t, 1, one)
	require.NoError(t, tx.Rollback(ctx))

	require.Equal(t, 0, jobTypeCount(t, pool, "   "))
}

// TestEnqueueTx_IdempotencyKey_DuplicateWithinSameTransaction proves
// TF-INV-008/TF-INV-016 hold inside a single caller-owned transaction: a
// second EnqueueTx call with the same (JobType, IdempotencyKey) inside the
// SAME transaction returns the first call's job, created=false, and
// commits to exactly one row.
func TestEnqueueTx_IdempotencyKey_DuplicateWithinSameTransaction(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	key := "dup-key-same-tx"
	req := txenqueue.EnqueueRequest{JobType: "test.tx.idem.sametx", IdempotencyKey: &key}

	j1, created1, err := st.EnqueueTx(ctx, tx, req)
	require.NoError(t, err)
	require.True(t, created1)

	j2, created2, err := st.EnqueueTx(ctx, tx, req)
	require.NoError(t, err)
	require.False(t, created2, "second call with the same key inside the same tx must not create a second job")
	require.Equal(t, j1.ID, j2.ID)

	require.NoError(t, tx.Commit(ctx))

	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key = $2`,
		"test.tx.idem.sametx", key).Scan(&count))
	require.Equal(t, 1, count)
}

// TestEnqueueTx_IdempotencyKey_RollbackThenReuseSameKey_Succeeds proves a
// key used by a transaction that later rolled back is fully available for
// reuse -- the rolled-back attempt never durably existed, so a later,
// separate transaction using the same key is a genuine first insert
// (created=true), not a duplicate hit.
func TestEnqueueTx_IdempotencyKey_RollbackThenReuseSameKey_Succeeds(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	key := "reuse-after-rollback"
	jobType := "test.tx.idem.reuse"

	tx1, err := pool.Begin(ctx)
	require.NoError(t, err)
	j1, created1, err := st.EnqueueTx(ctx, tx1, txenqueue.EnqueueRequest{JobType: jobType, IdempotencyKey: &key})
	require.NoError(t, err)
	require.True(t, created1)
	require.NoError(t, tx1.Rollback(ctx))

	tx2, err := pool.Begin(ctx)
	require.NoError(t, err)
	j2, created2, err := st.EnqueueTx(ctx, tx2, txenqueue.EnqueueRequest{JobType: jobType, IdempotencyKey: &key})
	require.NoError(t, err)
	require.True(t, created2, "the key must be fully reusable once the first attempt rolled back")
	require.NotEqual(t, j1.ID, j2.ID)
	require.NoError(t, tx2.Commit(ctx))

	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key = $2`,
		jobType, key).Scan(&count))
	require.Equal(t, 1, count)
}

// TestEnqueueTx_IdempotencyKey_ConcurrentTransactions_ExactlyOneCreates
// extends the same-transaction idempotency proof to true concurrency
// across separate transactions racing on the same key -- in the spirit of
// internal/store/idempotency_test.go's
// TestInsertIdempotent_ConcurrentDuplicateSubmissions_SF005, but exercised
// through separate, concurrently-committing pgx transactions rather than
// the pool path, per docs/enterprise-roadmap.md Phase 11's proof
// obligation ("under concurrent commit/rollback races").
//
// Renamed (Phase 11 audit finding) from
// "...ExactlyOneCommits": every one of the n transactions below that
// reaches tx.Commit actually does commit successfully -- what is true, and
// what this test proves, is that exactly one of them is the transaction
// whose own INSERT created the durable row (created=true); every other
// commits too, but resolves idempotently to that same already-existing
// row (created=false). "ExactlyOneCommits" was therefore a misleading
// name -- it is not the case that only one caller's transaction commits.
func TestEnqueueTx_IdempotencyKey_ConcurrentTransactions_ExactlyOneCreates(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	st := txenqueue.New()

	const n = 10
	const jobType = "test.tx.idem.concurrent"
	key := "concurrent-tx-key"

	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	createdFlags := make([]bool, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			j, created, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{JobType: jobType, IdempotencyKey: &key})
			if err != nil {
				_ = tx.Rollback(ctx)
				errs[i] = err
				return
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				errs[i] = cerr
				return
			}
			ids[i] = j.ID
			createdFlags[i] = created
		}(i)
	}
	wg.Wait()

	firstCreated := 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i], "every caller must observe the same job_id")
		if createdFlags[i] {
			firstCreated++
		}
	}
	require.Equal(t, 1, firstCreated, "exactly one transaction's INSERT must have won the race")
	require.Equal(t, 1, jobTypeCount(t, pool, jobType))
}

// TestEnqueueTx_ConcurrentCommitRollbackRace_OnlyCommittedJobsExist is
// docs/enterprise-roadmap.md Phase 11's headline concurrency proof
// obligation: many transactions, each enqueuing its own (uniquely keyed)
// job and independently choosing to commit or roll back, leave exactly the
// committed jobs durable and none of the rolled-back ones -- concurrent
// commit/rollback races do not corrupt or leak state, in the same spirit as
// Phase 4's idempotency concurrency tests.
func TestEnqueueTx_ConcurrentCommitRollbackRace_OnlyCommittedJobsExist(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	verify := newVerifyStore(t, dsn)
	st := txenqueue.New()

	const n = 20
	const jobType = "test.tx.race"

	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	wantCommit := make([]bool, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wantCommit[i] = i%2 == 0
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			j, _, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{JobType: jobType})
			if err != nil {
				_ = tx.Rollback(ctx)
				errs[i] = err
				return
			}
			ids[i] = j.ID
			if wantCommit[i] {
				errs[i] = tx.Commit(ctx)
			} else {
				errs[i] = tx.Rollback(ctx)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		_, err := verify.GetByID(ctx, ids[i])
		if wantCommit[i] {
			require.NoError(t, err, "committed job %d must exist", i)
		} else {
			require.ErrorIs(t, err, store.ErrNotFound, "rolled-back job %d must not exist", i)
		}
	}

	require.Equal(t, n/2, jobTypeCount(t, pool, jobType), "exactly the committed half must be durable")
}
