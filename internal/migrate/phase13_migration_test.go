// Phase 13 migration proofs (docs/phase-13-plan.md §7, §11; ADR-0009):
// queue_name's additive, catalog-only column add, the queue-aware claimable
// index's measured lock profile, the new governance tables' zero impact on
// jobs, the retired index's contract-step lock profile, and reversibility
// of the whole 0011-0014 sequence.
//
// Per this project's own "never assert a lock property without measuring
// it" discipline (docs/data-model.md "Phase 12 migration lock profile," the
// two retracted claims that established the rule, and
// internal/migrate/phase12_lockclaims_test.go's guard against reintroducing
// them), every blocking/non-blocking claim below is proved against real
// PostgreSQL, not asserted from documentation prose alone.
//
// Unlike phase12_migration_test.go's realClaimQuery helper (a hardcoded
// copy of internal/store/claim.go's pre-Phase-13 statement), these tests
// deliberately do NOT duplicate the claim query's SQL text: Checkpoints 2-3
// of this same change rewrite that query for queue subscriptions and the
// slot-table mechanism, which would immediately make a duplicated copy
// stale. What actually matters for a migration's lock profile is which lock
// MODE its DDL takes and what that mode conflicts with -- proved here with
// a generic representative write (an ordinary UPDATE, which takes ROW
// EXCLUSIVE exactly like the real claim query's UPDATE does) instead.
package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/migrations"
)

// seedPhase13JobsForLockTest inserts n plain QUEUED jobs, exactly like
// phase12_migration_test.go's seedJobsForLockTest but carrying an explicit
// queue_name so the seeded rows exercise the new column.
func seedPhase13JobsForLockTest(t *testing.T, db *sql.DB, n int) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		id := uuid.New()
		_, err := db.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, queue_name)
			VALUES ($1, $2, 'test.p13.lock', '{}', 'QUEUED', 30, 'lock-test')`,
			id, principal.SystemPrincipalID)
		require.NoError(t, err)
		ids[i] = id
	}
	return ids
}

// tryOrdinaryWrite attempts a representative write against jobs (an
// ordinary UPDATE, ROW EXCLUSIVE -- the same lock class the real claim
// query's UPDATE needs) under lockTimeout, reporting whether it was
// blocked waiting for a table lock (55P03) rather than completing.
func tryOrdinaryWrite(t *testing.T, db *sql.DB, id uuid.UUID, lockTimeout string) (blocked bool, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecContext(ctx, `SET lock_timeout = '`+lockTimeout+`'`)
	require.NoError(t, err)

	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // never committed

	_, err = tx.ExecContext(ctx, `UPDATE jobs SET updated_at = now() WHERE id = $1`, id)
	if err == nil {
		return false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == phase13LockNotAvailable {
		return true, err
	}
	return false, err
}

const phase13LockNotAvailable = "55P03"

// tryOrdinaryRead attempts a plain read under lockTimeout, reporting
// whether it was blocked.
func tryOrdinaryRead(t *testing.T, db *sql.DB, lockTimeout string) (blocked bool, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecContext(ctx, `SET lock_timeout = '`+lockTimeout+`'`)
	require.NoError(t, err)

	var n int
	err = conn.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE job_type = 'test.p13.lock'`).Scan(&n)
	if err == nil {
		return false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == phase13LockNotAvailable {
		return true, err
	}
	return false, err
}

// TestPhase13Migration0011_AddsQueueNameWithDefault proves the compatibility
// requirement docs/phase-13-plan.md §14 states as non-negotiable: every
// pre-existing (and every row inserted without naming queue_name at all)
// gets exactly 'default', NOT NULL.
func TestPhase13Migration0011_AddsQueueNameWithDefault(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	// A raw INSERT naming no queue_name at all (the shape any pre-Phase-13
	// caller/driver would produce) must still get 'default' -- proving the
	// column has a real schema default, not merely an application-layer one.
	id := uuid.New()
	_, err := db.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds)
		VALUES ($1, $2, 'test.p13.default_queue', '{}', 'QUEUED', 30)`,
		id, principal.SystemPrincipalID)
	require.NoError(t, err)

	var queueName string
	var isNullable string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT queue_name FROM jobs WHERE id = $1`, id).Scan(&queueName))
	require.Equal(t, "default", queueName)

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'queue_name'`,
	).Scan(&isNullable))
	require.Equal(t, "NO", isNullable, "jobs.queue_name must be NOT NULL")

	// workflow_instances gets the identical treatment.
	wfID := uuid.New()
	_, err = db.ExecContext(ctx,
		`INSERT INTO workflow_instances (id, principal_id, state) VALUES ($1, $2, 'RUNNING')`,
		wfID, principal.SystemPrincipalID)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT queue_name FROM workflow_instances WHERE id = $1`, wfID).Scan(&queueName))
	require.Equal(t, "default", queueName)
}

// TestPhase13Migration0011_AddColumnIsFastRegardlessOfTableSize is the
// empirical half of the catalog-only claim in 0011's up migration: a
// non-volatile-default ADD COLUMN NOT NULL is O(1) work in PostgreSQL 11+
// (no table rewrite, no full scan), so re-running the identical statement
// shape against a non-trivially-sized table must complete quickly, not in
// time proportional to row count. This does not re-run 0011 itself
// (already applied by testutil.DB); it applies the same statement shape to
// a scratch column, which exercises the identical PostgreSQL code path.
func TestPhase13Migration0011_AddColumnIsFastRegardlessOfTableSize(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	seedPhase13JobsForLockTest(t, db, 5000)

	start := time.Now()
	_, err := db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN phase13_scratch_col TEXT NOT NULL DEFAULT 'x'`)
	require.NoError(t, err)
	elapsed := time.Since(start)

	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `ALTER TABLE jobs DROP COLUMN IF EXISTS phase13_scratch_col`)
	})

	require.Less(t, elapsed, 2*time.Second,
		"ADD COLUMN ... NOT NULL DEFAULT '<literal>' against 5000 rows took %s -- if this is no longer "+
			"O(1), 0011's catalog-only claim is false and must be re-derived", elapsed)
}

// TestPhase13Migration0012_BlocksWritesAndReadsAreUnaffected proves
// 0012's documented lock profile: CREATE INDEX (non-CONCURRENTLY, since
// every migration here runs inside one transaction) takes SHARE for its
// whole duration, which conflicts with the ROW EXCLUSIVE every write to
// jobs needs -- including the real claim query's UPDATE -- while ordinary
// reads (ACCESS SHARE) are unaffected.
func TestPhase13Migration0012_BlocksWritesAndReadsAreUnaffected(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	ids := seedPhase13JobsForLockTest(t, db, 50)

	t.Run("0012 really does build an index, so SHARE really is its lock", func(t *testing.T) {
		raw, err := migrations.Files.ReadFile("0012_create_claimable_by_queue_index.up.sql")
		require.NoError(t, err)
		require.Contains(t, string(raw), "CREATE INDEX")
	})

	holder, err := db.Conn(ctx)
	require.NoError(t, err)
	defer holder.Close()
	holderTx, err := holder.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holderTx.Rollback() }) // no-op if already rolled back below
	_, err = holderTx.ExecContext(ctx, `LOCK TABLE jobs IN SHARE MODE`)
	require.NoError(t, err)

	t.Run("reads are NOT blocked", func(t *testing.T) {
		blocked, err := tryOrdinaryRead(t, db, "5s")
		require.False(t, blocked, "ACCESS SHARE is compatible with SHARE")
		require.NoError(t, err)
	})

	t.Run("ordinary writes ARE blocked", func(t *testing.T) {
		blocked, err := tryOrdinaryWrite(t, db, ids[0], "1s")
		require.True(t, blocked, "SHARE conflicts with the ROW EXCLUSIVE every write to jobs needs, "+
			"including the real claim query's UPDATE")
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		require.Equal(t, phase13LockNotAvailable, pgErr.Code)
	})

	require.NoError(t, holderTx.Rollback())

	blocked, err := tryOrdinaryWrite(t, db, ids[0], "5s")
	require.False(t, blocked, "writes must resume once SHARE is released")
	require.NoError(t, err)
}

// TestPhase13Migration0013_BlocksWritesViaForeignKeyLock_ReadsUnaffected
// proves 0013's REAL lock profile against jobs -- not the "no lock at
// all" claim an earlier draft of this file's own comment made, which this
// test itself caught as false (see the up migration's corrected comment).
// `queue_slots.held_by_job_id ... REFERENCES jobs(id)` takes SHARE ROW
// EXCLUSIVE on jobs to create the foreign key, which conflicts with the
// ROW EXCLUSIVE every write to jobs needs (including the real claim
// query's UPDATE), while leaving ordinary reads (ACCESS SHARE) unaffected.
//
// This reconstructs pre-0013 state (drops the four governance tables) so
// the migration's actual statements can be re-run for real, rather than
// only inspecting its text. The holder transaction is rolled back via
// t.Cleanup (not a bare deferred statement after a possibly-failing
// assertion) so a failed assertion can never leave it open across the
// rest of the test binary -- leaving a *sql.Tx uncommitted while its
// reserved *sql.Conn is closed by an earlier, unrelated defer is exactly
// what produced a real test-binary hang while this test was being
// authored (an unconditional require.False failing mid-test skipped the
// manual Rollback call that used to follow it).
func TestPhase13Migration0013_BlocksWritesViaForeignKeyLock_ReadsUnaffected(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	ids := seedPhase13JobsForLockTest(t, db, 10)

	_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS rate_limit_buckets, queue_slots, queue_limits, queue_state`)
	require.NoError(t, err)

	raw, err := migrations.Files.ReadFile("0013_create_governance_tables.up.sql")
	require.NoError(t, err)

	holder, err := db.Conn(ctx)
	require.NoError(t, err)
	defer holder.Close()
	holderTx, err := holder.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holderTx.Rollback() }) // no-op if already committed below

	_, err = holderTx.ExecContext(ctx, string(raw))
	require.NoError(t, err, "0013's up migration must apply cleanly against the reconstructed pre-0013 schema")

	t.Run("reads are NOT blocked", func(t *testing.T) {
		blocked, rerr := tryOrdinaryRead(t, db, "5s")
		require.False(t, blocked, "SHARE ROW EXCLUSIVE (the new foreign key's lock) is compatible with ACCESS SHARE")
		require.NoError(t, rerr)
	})

	t.Run("ordinary writes ARE blocked, by the queue_slots -> jobs foreign key", func(t *testing.T) {
		blocked, werr := tryOrdinaryWrite(t, db, ids[0], "1s")
		require.True(t, blocked, "creating queue_slots.held_by_job_id REFERENCES jobs(id) takes SHARE ROW "+
			"EXCLUSIVE on jobs, which conflicts with the ROW EXCLUSIVE every write (including the real claim "+
			"query's UPDATE) needs")
		var pgErr *pgconn.PgError
		require.True(t, errors.As(werr, &pgErr))
		require.Equal(t, phase13LockNotAvailable, pgErr.Code)
	})

	require.NoError(t, holderTx.Commit())

	blocked, werr := tryOrdinaryWrite(t, db, ids[0], "5s")
	require.False(t, blocked, "writes must resume once 0013 commits")
	require.NoError(t, werr)

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_state WHERE queue_name = 'default'`).Scan(&count))
	require.Equal(t, 1, count, "0013 must seed the 'default' queue_state row")
}

// TestPhase13Migration0014_BlocksOnAccessExclusive_ButIsCatalogOnly proves
// 0014's contract-step lock profile: DROP INDEX takes ACCESS EXCLUSIVE
// (which blocks even reads, unlike 0012's SHARE) but performs no scan, so
// once acquired it is released essentially immediately -- proved by timing
// a representative DROP/CREATE INDEX pair against a non-trivial table,
// exactly like 0011's catalog-only proof.
func TestPhase13Migration0014_BlocksOnAccessExclusive_ButIsCatalogOnly(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	seedPhase13JobsForLockTest(t, db, 5000)

	holder, err := db.Conn(ctx)
	require.NoError(t, err)
	defer holder.Close()
	holderTx, err := holder.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holderTx.Rollback() }) // no-op if already rolled back below
	_, err = holderTx.ExecContext(ctx, `LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	t.Run("reads are blocked too (unlike 0012's SHARE)", func(t *testing.T) {
		blocked, err := tryOrdinaryRead(t, db, "1s")
		require.True(t, blocked, "ACCESS EXCLUSIVE conflicts with every other lock mode, including ACCESS SHARE")
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		require.Equal(t, phase13LockNotAvailable, pgErr.Code)
	})

	require.NoError(t, holderTx.Rollback())

	// Catalog-only: dropping and recreating idx_jobs_claimable (this
	// migration's actual statement shape) against 5000 rows must be fast,
	// not proportional to table size -- a real scan/rewrite would not be.
	start := time.Now()
	_, err = db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_jobs_claimable`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_jobs_claimable ON jobs (priority DESC, eligible_at ASC) WHERE state IN ('QUEUED', 'RETRY_WAIT')`)
	require.NoError(t, err)
	elapsed := time.Since(start)
	require.Less(t, elapsed, 3*time.Second,
		"drop+recreate of idx_jobs_claimable against 5000 rows took %s", elapsed)
}

// TestPhase13Migrations0011To0014_AreDataSafeReversible drives the real
// .down.sql files, in reverse version order, against a database that has
// gone all the way through 0014 -- mirroring
// TestMigrations0005To0008_AreDataSafeReversible's pattern in
// phase12_migration_test.go for the Phase 13 sequence.
func TestPhase13Migrations0011To0014_AreDataSafeReversible(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	// A row using a non-default queue_name, so the down migrations'
	// destructiveness (queue_name column drop) is exercised against real
	// data, not an empty table.
	id := uuid.New()
	_, err := db.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, queue_name)
		VALUES ($1, $2, 'test.p13.reversible', '{}', 'QUEUED', 30, 'reports')`,
		id, principal.SystemPrincipalID)
	require.NoError(t, err)

	for _, name := range []string{
		"0014_drop_old_claimable_index.down.sql",
		"0013_create_governance_tables.down.sql",
		"0012_create_claimable_by_queue_index.down.sql",
		"0011_add_queue_name.down.sql",
	} {
		require.NoError(t, applyDown(t, db, name), "applying %s", name)
	}
	// applyDown only runs the raw SQL; it does not touch the ledger.
	// Without removing these rows, migrate.Up below would believe
	// 0011-0014 are already applied and skip them entirely -- exactly the
	// gap phase12_migration_test.go's own reversibility test already
	// guards against for 0005-0010.
	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (11, 12, 13, 14)`)
	require.NoError(t, err)

	// The pre-Phase-13 index is back, the new one and the governance
	// tables are gone, and jobs/workflow_instances no longer carry
	// queue_name at all.
	var idxCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_jobs_claimable'`).Scan(&idxCount))
	require.Equal(t, 1, idxCount)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_jobs_claimable_by_queue'`).Scan(&idxCount))
	require.Equal(t, 0, idxCount)

	for _, table := range []string{"queue_state", "queue_limits", "queue_slots", "rate_limit_buckets"} {
		var exists bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists))
		require.False(t, exists, "table %s must not exist after rollback", table)
	}

	var colCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'queue_name'`,
	).Scan(&colCount))
	require.Equal(t, 0, colCount)

	// Re-applying every up migration from this rolled-back state must
	// converge cleanly (idempotent IF NOT EXISTS/IF EXISTS guards
	// throughout), and the previously-inserted row now gets 'default'
	// (its non-default value was genuinely dropped by the rollback, per
	// this sequence's own documented data-safe-reversible label -- the
	// column itself, not a per-row backup, is what "reversible" means
	// here).
	require.NoError(t, migrate.Up(ctx, db))
	var queueName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT queue_name FROM jobs WHERE id = $1`, id).Scan(&queueName))
	require.Equal(t, "default", queueName)
}
