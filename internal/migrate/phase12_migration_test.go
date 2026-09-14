// Phase 12 migration proofs (docs/phase-12-plan.md §11, verification
// points 1, 2 and 13): legacy-row backfill, NOT NULL convergence,
// principal-scoped uniqueness, the honest (deliberately failing) 0009 down
// migration, and the lock-behaviour tests for the whole 0006-0009 sequence
// -- measured through migrate.Up, which is the only path that actually
// ships.
//
// These run against real PostgreSQL and, where they need a pre-Phase-12
// database, produce one by actually applying the 0005-0010 down migrations
// rather than by hand-crafting a schema that might not match what a real
// deployment has.
package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// applyDown executes one .down.sql file from the embedded set.
func applyDown(t *testing.T, db *sql.DB, name string) error {
	t.Helper()
	sqlText, err := migrations.Files.ReadFile(name)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), string(sqlText))
	return err
}

// TestMigration0005_SeedsImmutableSystemPrincipal is OD-1's foundation:
// migration 0005 must seed the exact, documented, well-known UUID that
// principal.SystemPrincipalID names in Go, so no code path ever needs a
// runtime lookup and migration 0007's backfill always has a valid FK
// target.
func TestMigration0005_SeedsImmutableSystemPrincipal(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	var kind, displayName string
	var revokedAt sql.NullTime
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT kind, display_name, revoked_at FROM principals WHERE id = $1`,
		principal.SystemPrincipalID).Scan(&kind, &displayName, &revokedAt))

	require.Equal(t, string(principal.KindCaller), kind)
	require.False(t, revokedAt.Valid, "the system principal is never revoked")
	require.Contains(t, displayName, "system",
		"the display name must make clear this is not a real caller")

	// The Go constant and the seeded row must be the same value. If they
	// ever drift, backfill silently points at a row that does not exist.
	require.Equal(t, "00000000-0000-0000-0000-000000000001", principal.SystemPrincipalID.String())
}

// TestMigration0005_RejectsWorkerPrincipalKind is OD-3 at the schema
// level: there is no 'worker' principal kind, and the CHECK constraint
// prevents one being introduced by a stray INSERT even if application code
// were changed to allow it.
func TestMigration0005_RejectsWorkerPrincipalKind(t *testing.T) {
	db := testutil.DB(t)
	_, err := db.Exec(`INSERT INTO principals (id, kind, display_name) VALUES ($1, 'worker', 'a worker')`, uuid.New())
	require.Error(t, err,
		"OD-3: worker identity is a PostgreSQL role, never an application principal")

	for _, kind := range []string{string(principal.KindCaller), string(principal.KindAdmin)} {
		_, err := db.Exec(`INSERT INTO principals (id, kind, display_name) VALUES ($1, $2, 'ok')`, uuid.New(), kind)
		require.NoError(t, err, "%q must be a legal kind", kind)
	}
}

// TestMigrations0005To0010_BackfillLegacyRowsAndConverge is verification
// point 1's full proof, run against a genuinely pre-Phase-12 database.
//
// It rewinds to a pre-Phase-12 schema, inserts rows exactly as a Phase 1-11
// deployment would have (with no principal at all), then runs the real
// migrate.Up and asserts every documented outcome: every legacy row
// resolves to the system principal, principal_id is provably NOT NULL on
// both tables, the old global idempotency index is gone, and the new
// scoped one is enforcing.
func TestMigrations0005To0010_BackfillLegacyRowsAndConverge(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	dropPhase12Schema(t, db)

	// Legacy rows, inserted the pre-Phase-12 way.
	legacyJob, legacyIdemJob, legacyWorkflow := uuid.New(), uuid.New(), uuid.New()
	_, err := db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds)
		VALUES ($1, 'test.p12.legacy', '{}', 'QUEUED', 30)`, legacyJob)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
		VALUES ($1, 'test.p12.legacy.idem', '{}', 'QUEUED', 30, 'legacy-key')`, legacyIdemJob)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, state) VALUES ($1, 'RUNNING')`, legacyWorkflow)
	require.NoError(t, err)

	// The real upgrade.
	require.NoError(t, migrate.Up(ctx, db))

	t.Run("every legacy job row is backfilled to the system principal", func(t *testing.T) {
		for _, id := range []uuid.UUID{legacyJob, legacyIdemJob} {
			var owner uuid.UUID
			require.NoError(t, db.QueryRowContext(ctx, `SELECT principal_id FROM jobs WHERE id = $1`, id).Scan(&owner))
			require.Equal(t, principal.SystemPrincipalID, owner)
		}
	})

	t.Run("the legacy workflow row is backfilled too", func(t *testing.T) {
		var owner uuid.UUID
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT principal_id FROM workflow_instances WHERE id = $1`, legacyWorkflow).Scan(&owner))
		require.Equal(t, principal.SystemPrincipalID, owner)
	})

	t.Run("no row anywhere is left with a NULL principal", func(t *testing.T) {
		for _, table := range []string{"jobs", "workflow_instances"} {
			var nulls int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT count(*) FROM `+table+` WHERE principal_id IS NULL`).Scan(&nulls))
			require.Zero(t, nulls, "%s must have no NULL principal_id after migration", table)
		}
	})

	t.Run("principal_id has reached its NOT NULL steady state", func(t *testing.T) {
		// The constraint is enforced, not merely declared: a NULL insert
		// is rejected by the database itself.
		_, err := db.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds)
			VALUES ($1, NULL, 'test.p12.nullprincipal', '{}', 'QUEUED', 30)`, uuid.New())
		require.Error(t, err, "a NULL principal_id must be rejected -- OD-1 forbids NULL as a compatibility mechanism")

		_, err = db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, principal_id, state) VALUES ($1, NULL, 'RUNNING')`, uuid.New())
		require.Error(t, err)

		// And the constraints are VALIDATED, not left NOT VALID -- an
		// unvalidated constraint would not have proved the existing rows
		// conform.
		for _, name := range []string{"jobs_principal_id_not_null", "workflow_instances_principal_id_not_null"} {
			var validated bool
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT convalidated FROM pg_constraint WHERE conname = $1`, name).Scan(&validated))
			require.True(t, validated,
				"%s must be VALIDATED, not left NOT VALID -- otherwise pre-existing rows were never checked", name)
		}
	})

	t.Run("the old global idempotency index is gone and the scoped one enforces", func(t *testing.T) {
		require.False(t, indexExists(t, db, "idx_jobs_idempotency_key"))
		require.True(t, indexExists(t, db, "idx_jobs_idempotency_scoped"))

		other := testutil.NewPrincipal(t, db, principal.KindCaller, "post-migration tenant")

		// A different principal may reuse the legacy row's key...
		_, err := db.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
			VALUES ($1, $2, 'test.p12.legacy.idem', '{}', 'QUEUED', 30, 'legacy-key')`, uuid.New(), other)
		require.NoError(t, err,
			"a legacy (system-principal) row must not block a real tenant from using the same key")

		// ...but not reuse its own.
		_, err = db.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
			VALUES ($1, $2, 'test.p12.legacy.idem', '{}', 'QUEUED', 30, 'legacy-key')`, uuid.New(), other)
		require.Error(t, err, "the scoped index must still enforce uniqueness within one principal")

		// Nor may the system principal's own legacy key be duplicated.
		_, err = db.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
			VALUES ($1, $2, 'test.p12.legacy.idem', '{}', 'QUEUED', 30, 'legacy-key')`,
			uuid.New(), principal.SystemPrincipalID)
		require.Error(t, err,
			"backfilled legacy rows participate in the scoped constraint exactly like any other row")
	})
}

// TestMigrations0005To0008_AreDataSafeReversible is verification point
// 13's first half: the additive migrations round-trip. Up, down, and up
// again must leave the same durable state, with no data loss.
func TestMigrations0005To0008_AreDataSafeReversible(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	// dropPhase12Schema below already registers the restore.

	dropPhase12Schema(t, db)

	jobID := uuid.New()
	_, err := db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds)
		VALUES ($1, 'test.p12.roundtrip', '{}', 'QUEUED', 30)`, jobID)
	require.NoError(t, err)

	// Up.
	require.NoError(t, migrate.Up(ctx, db))
	var owner uuid.UUID
	require.NoError(t, db.QueryRowContext(ctx, `SELECT principal_id FROM jobs WHERE id = $1`, jobID).Scan(&owner))
	require.Equal(t, principal.SystemPrincipalID, owner)

	// Down, newest first (0010's down is safe here, since no cross-principal
	// key reuse exists yet).
	require.NoError(t, applyDown(t, db, "0010_drop_global_idempotency_index.down.sql"))
	require.NoError(t, applyDown(t, db, "0009_validate_principal_id_and_scope_idempotency.down.sql"))
	require.NoError(t, applyDown(t, db, "0008_require_principal_id.down.sql"))
	require.NoError(t, applyDown(t, db, "0007_backfill_principal_id.down.sql"))
	require.NoError(t, applyDown(t, db, "0006_add_principal_id_columns.down.sql"))
	require.NoError(t, applyDown(t, db, "0005_create_principals_and_api_keys.down.sql"))
	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (5, 6, 7, 8, 9)`)
	require.NoError(t, err)

	// The job row itself survived the rollback untouched.
	var jobType string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT job_type FROM jobs WHERE id = $1`, jobID).Scan(&jobType))
	require.Equal(t, "test.p12.roundtrip", jobType, "rolling back must not lose job data")

	// Up again: the same backfill result, reproducibly.
	require.NoError(t, migrate.Up(ctx, db))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT principal_id FROM jobs WHERE id = $1`, jobID).Scan(&owner))
	require.Equal(t, principal.SystemPrincipalID, owner,
		"re-running 0006-0007 must reproduce the identical backfill")
}

// TestMigration0010_Down_FailsLoudlyOnDivergentData is verification point
// 13's honest-rollback proof, and it asserts a FAILURE is the correct
// behaviour.
//
// Once two principals share a (job_type, idempotency_key) pair -- the
// entire point of 0010 having shipped -- recreating the old global unique
// index is impossible without discarding one tenant's job. TaskForge
// cannot make that choice, so the down migration must fail with a
// PostgreSQL uniqueness violation rather than silently "succeeding" by
// some other means.
//
// It also asserts the failure is atomic: a failed rollback leaves the
// database exactly as 0010 left it -- the scoped index still in place, the
// global one still absent -- not half-reverted with neither index present.
func TestMigration0010_Down_FailsLoudlyOnDivergentData(t *testing.T) {
	db := testutil.DB(t)
	restorePhase12Schema(t, db)
	ctx := context.Background()

	a := testutil.NewPrincipal(t, db, principal.KindCaller, "divergent tenant A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "divergent tenant B")

	const jobType = "test.p12.down.divergent"
	const key = "both-tenants-key"
	for _, p := range []uuid.UUID{a, b} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
			VALUES ($1, $2, $3, '{}', 'QUEUED', 30, $4)`, uuid.New(), p, jobType, key)
		require.NoError(t, err, "post-0010, two tenants sharing one key is legal -- that is the point")
	}

	err := applyDown(t, db, "0010_drop_global_idempotency_index.down.sql")
	require.Error(t, err,
		"0010's down must fail loudly once real divergent data exists -- it is documented as "+
			"forward-fix-preferred, and silently discarding one tenant's job would be far worse")
	require.True(t,
		strings.Contains(err.Error(), "23505") || strings.Contains(strings.ToLower(err.Error()), "unique"),
		"the failure must be PostgreSQL's own uniqueness violation, not some substituted error: %v", err)

	// Atomic: nothing was reverted.
	require.True(t, indexExists(t, db, "idx_jobs_idempotency_scoped"),
		"a failed rollback must leave the scoped index in place, not a half-reverted schema")
	require.False(t, indexExists(t, db, "idx_jobs_idempotency_key"))

	var stillValidated bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT convalidated FROM pg_constraint WHERE conname = 'jobs_principal_id_not_null'`).Scan(&stillValidated))
	require.True(t, stillValidated, "the NOT NULL constraint must survive a failed rollback too")
}

// TestMigration0010_Down_SucceedsWhenNoDivergentDataExists is the other
// half of the same contract: the rollback is not unconditionally broken.
// On a database where no two principals have collided yet, it works.
func TestMigration0010_Down_SucceedsWhenNoDivergentDataExists(t *testing.T) {
	db := testutil.DB(t)
	restorePhase12Schema(t, db)
	ctx := context.Background()

	p := testutil.NewPrincipal(t, db, principal.KindCaller, "single tenant")
	_, err := db.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, idempotency_key)
		VALUES ($1, $2, 'test.p12.down.clean', '{}', 'QUEUED', 30, 'only-key')`, uuid.New(), p)
	require.NoError(t, err)

	require.NoError(t, applyDown(t, db, "0010_drop_global_idempotency_index.down.sql"))
	require.True(t, indexExists(t, db, "idx_jobs_idempotency_key"),
		"with no divergent data the global index can be rebuilt, so this rollback step works")
	require.NoError(t, applyDown(t, db, "0009_validate_principal_id_and_scope_idempotency.down.sql"))
	require.False(t, indexExists(t, db, "idx_jobs_idempotency_scoped"))
}

// ---------------------------------------------------------------------
// Lock behaviour, measured through migrate.Up (HIGH-2)
// ---------------------------------------------------------------------
//
// The previous version of this file asserted the non-blocking property by
// running a bare `ALTER TABLE ... VALIDATE CONSTRAINT` standalone, outside
// any transaction, against a constraint it had added itself in a separate
// statement. That is not how any migration ever runs. An independent review
// measured the real path and found the opposite of what README.md and
// docs/data-model.md claimed: because internal/migrate wraps each .up.sql in
// ONE transaction and PostgreSQL releases locks only at commit, ADD
// CONSTRAINT's ACCESS EXCLUSIVE lock was held across VALIDATE's full-table
// scan -- and, worse, ADD COLUMN's was held across the entire backfill.
//
// The migrations were restructured (0006 columns, 0007 backfill, 0008
// constraint, 0009 validate + index swap) so that no ACCESS EXCLUSIVE lock is
// ever held across a full-table scan or a full-table write.
//
// A SECOND independent review then measured THIS file's own claims and found
// them overstated in turn. The tests here previously asserted that migration
// 0009 "does not block the claim path", using a SELECT-only stand-in query
// described in a comment as "byte-for-byte the locking clause the real worker
// claim query uses". It is not. internal/store/claim.go's claimQuery is
//
//	WITH candidate AS (SELECT ... FOR UPDATE SKIP LOCKED) UPDATE jobs ...
//
// -- an UPDATE, which takes ROW EXCLUSIVE, which CONFLICTS with the SHARE that
// 0009's CREATE INDEX statements take. Measured against the real 0009 file on
// a 3M-row / 426 MB jobs table, the real claim query hit lock_timeout
// (SQLSTATE 55P03) instead of claiming. The old test passed anyway, because
// its stand-in query took only ROW SHARE, which does not conflict.
//
// So the tests below now assert only what PostgreSQL actually delivers:
//
//   - 0007 (the backfill) takes ROW EXCLUSIVE and genuinely does not block
//     reads or the real claim query.
//   - 0009 takes no ACCESS EXCLUSIVE lock and genuinely does not block reads,
//     and completes even while a worker holds a claim-shaped row lock.
//   - 0009 DOES block every write to jobs, the real claim query included --
//     asserted directly, as behaviour to be relied on and scheduled around
//     rather than denied.
//
// Every probe uses realClaimQuery below, which is the actual statement.

// applyThroughVersion applies every Phase 12 migration up to and including
// version, recording each in schema_migrations, so the test can then hand
// the NEXT migration -- and only it -- to the real migrate.Up and observe
// that migration's lock behaviour in isolation.
func applyThroughVersion(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	ctx := context.Background()

	entries, err := migrations.Files.ReadDir(".")
	require.NoError(t, err)

	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		v, err := strconv.ParseInt(strings.SplitN(name, "_", 2)[0], 10, 64)
		require.NoError(t, err)
		if v > version {
			continue
		}
		var already int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE version = $1`, v).Scan(&already))
		if already > 0 {
			continue
		}
		sqlText, err := migrations.Files.ReadFile(name)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(sqlText))
		require.NoError(t, err, "applying %s", name)
		_, err = db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, v, name)
		require.NoError(t, err)
	}
}

// holdClaimShapedRowLock opens a transaction that holds a row lock on jobs of
// the kind a worker holds mid-claim (ROW SHARE on the table, plus a row-level
// lock), and returns a function that releases it. It models a worker that is
// part-way through claiming when a migration starts -- not the claim
// statement's own table-level lock, which is ROW EXCLUSIVE (see
// realClaimQuery).
func holdClaimShapedRowLock(t *testing.T, db *sql.DB) (lockedID uuid.UUID, release func()) {
	t.Helper()
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT id FROM jobs ORDER BY id LIMIT 1 FOR UPDATE`).Scan(&lockedID),
		"the test needs at least one jobs row to lock")

	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return lockedID, release
}

// seedJobsForLockTest inserts enough rows that the backfill and the
// validation scan are real work rather than no-ops.
func seedJobsForLockTest(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := db.ExecContext(ctx, `
			INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds)
			VALUES ($1, 'test.p12.lock', '{}', 'QUEUED', 30)`, uuid.New())
		require.NoError(t, err)
	}
}

// markVersionApplied records version as already applied WITHOUT running it,
// so a following migrate.Up applies exactly the migrations the test wants to
// observe and stops there. Paired with unmarkVersion below.
//
// Driving one migration per Up run is what makes these lock tests
// deterministic: otherwise Up races ahead into the next file, and a
// catalog-only ACCESS EXCLUSIVE statement there queues for its lock and
// stalls every subsequent reader BEHIND it -- which would make the test
// fail for a reason that has nothing to do with the statement under test.
func markVersionApplied(t *testing.T, db *sql.DB, version int64, name string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)
		 ON CONFLICT (version) DO NOTHING`, version, name)
	require.NoError(t, err)
}

func unmarkVersion(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`DELETE FROM schema_migrations WHERE version = $1`, version)
	require.NoError(t, err)
}

// realClaimQuery is internal/store/claim.go's claimQuery. Only the RETURNING
// list is shortened (RETURNING affects no lock), and the candidate CTE is
// narrowed to this test's seeded rows.
//
// Using the REAL statement is the entire point of this helper's existence. A
// previous revision used only the candidate CTE's "SELECT ... FOR UPDATE SKIP
// LOCKED", which takes ROW SHARE -- and ROW SHARE does not conflict with the
// SHARE that CREATE INDEX takes, so every probe passed while the real claim
// path was in fact blocked. The statement below is an UPDATE and takes ROW
// EXCLUSIVE, which is what a worker actually needs and what actually
// conflicts.
const realClaimQuery = `
	WITH candidate AS (
		SELECT id, state AS old_state, attempt_count AS old_attempt_count
		FROM jobs
		WHERE job_type = 'test.p12.lock'
		  AND (
				(state IN ('QUEUED', 'RETRY_WAIT') AND eligible_at <= now())
				OR (state = 'RUNNING' AND lease_expires_at < now() AND attempt_count < max_attempts)
			  )
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
	RETURNING jobs.id`

// tryRealClaim runs realClaimQuery on its own connection under the supplied
// lock_timeout, rolls back whatever it claimed, and reports whether the
// statement was blocked waiting for a table lock (SQLSTATE 55P03) rather than
// completing.
//
// Returning zero rows counts as "not blocked": SKIP LOCKED means a claim that
// finds every candidate row individually locked returns empty promptly, which
// is correct claim behaviour, not a stall.
func tryRealClaim(t *testing.T, db *sql.DB, lockTimeout string) (blocked bool, err error) {
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
	defer tx.Rollback() //nolint:errcheck // the claim is never committed

	var id uuid.UUID
	err = tx.QueryRowContext(ctx, realClaimQuery, "test-worker").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // skipped every locked row, promptly
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable {
		return true, err
	}
	return false, err
}

// lockNotAvailable is PostgreSQL's SQLSTATE for a statement cancelled because
// it exceeded lock_timeout while waiting for a lock.
const lockNotAvailable = "55P03"

// requireClaimPathUnblocked fails if the REAL claim query cannot run promptly.
func requireClaimPathUnblocked(t *testing.T, db *sql.DB, why string) {
	t.Helper()
	blocked, err := tryRealClaim(t, db, "5s")
	require.False(t, blocked, why)
	require.NoError(t, err, why)
}

// requireReadUnblocked runs a plain read under a short deadline. This is the
// assertion that fails outright against the pre-fix migrations: ACCESS
// EXCLUSIVE blocks even ACCESS SHARE, so a simple count timed out while the
// backfill ran.
func requireReadUnblocked(t *testing.T, db *sql.DB, wantRows int, why string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM jobs WHERE job_type = 'test.p12.lock'`).Scan(&n), why)
	require.Equal(t, wantRows, n)
}

// TestPhase12Migrations_BackfillDoesNotBlockReadsOrTheClaimPath drives
// migration 0007 -- the backfill -- through the real migrate.Up while a
// concurrent transaction holds a claim-shaped row lock on jobs.
//
// 0007 and 0009 are the two Phase 12 migrations whose duration scales with
// table size. 0007 is the one that genuinely does not block writes: a plain
// UPDATE takes ROW EXCLUSIVE, which does not conflict with itself, so the
// real claim query (also an UPDATE, also ROW EXCLUSIVE) runs throughout. 0009
// is the one that does block -- see
// TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery.
//
// The property proved is about the TABLE lock: while the backfill runs,
// ordinary reads complete promptly and the real claim query still returns
// promptly. Before the restructure both blocked outright, because the
// backfill shared a transaction with 0006's ADD COLUMN and therefore ran
// under ACCESS EXCLUSIVE -- a table-wide outage for the backfill's entire
// duration, while README.md claimed the opposite.
//
// The property deliberately NOT claimed is that the backfill never waits at
// all. It writes every row, so it takes ordinary row-level locks and waits
// behind a worker holding one of them. That is bounded MVCC contention on
// one row, the claim query skips such rows by design (SKIP LOCKED), and
// pretending otherwise would be the same kind of false claim this test
// exists to prevent.
func TestPhase12Migrations_BackfillDoesNotBlockReadsOrTheClaimPath(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	dropPhase12Schema(t, db)
	seedJobsForLockTest(t, db, 300)

	// Everything up to and including the column addition, uncontended.
	applyThroughVersion(t, db, 6)

	// Make this Up run apply exactly 0007, the backfill.
	for _, m := range []struct {
		v    int64
		name string
	}{
		{8, "0008_require_principal_id.up.sql"},
		{9, "0009_validate_principal_id_and_scope_idempotency.up.sql"},
		{10, "0010_drop_global_idempotency_index.up.sql"},
	} {
		markVersionApplied(t, db, m.v, m.name)
	}

	// A worker-shaped transaction holding a row lock, as the claim query
	// does between its SELECT ... FOR UPDATE SKIP LOCKED and its commit.
	_, release := holdClaimShapedRowLock(t, db)

	upCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- migrate.Up(upCtx, db) }()

	// While the backfill is in flight, reads and the claim path must both
	// keep working. Under the old structure these timed out.
	for i := 0; i < 15; i++ {
		requireReadUnblocked(t, db, 300,
			"a concurrent read must never be blocked while the backfill runs -- "+
				"the backfill takes ROW EXCLUSIVE, which does not conflict with ACCESS SHARE")
		requireClaimPathUnblocked(t, db,
			"the worker claim query must never be blocked while the backfill runs -- "+
				"ROW EXCLUSIVE does not conflict with ROW SHARE, and SKIP LOCKED handles the rows")
		time.Sleep(20 * time.Millisecond)
	}

	// Releasing the row lock lets the backfill finish the row it was
	// waiting on.
	release()
	require.NoError(t, <-done, "migration 0007 must complete once the row lock is released")

	var backfilled int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM jobs WHERE principal_id = $1`, principal.SystemPrincipalID).Scan(&backfilled))
	require.Equal(t, 300, backfilled, "every legacy row must end up owned by the system principal")

	// Let the rest of the sequence run for real, so the shared database is
	// left fully migrated for the cleanup hook.
	for _, v := range []int64{8, 9, 10} {
		unmarkVersion(t, db, v)
	}
	require.NoError(t, migrate.Up(ctx, db))
}

// TestPhase12Migrations_0009TakesNoAccessExclusive_ReadsAndCompletionContinue
// is what the restructure actually bought, measured on the path that ships.
//
// 0009 is split away from 0008's ADD CONSTRAINT and 0010's DROP INDEX so that
// its full-table VALIDATE scan and its three index builds run under NO ACCESS
// EXCLUSIVE lock. The observable consequences are narrow and this test asserts
// exactly them, and nothing more:
//
//   - ordinary reads are never blocked (ACCESS SHARE is compatible with both
//     SHARE UPDATE EXCLUSIVE and SHARE), and
//   - 0009 itself COMPLETES even while a worker holds a claim-shaped row lock,
//     rather than deadlocking or queueing behind it.
//
// What this test deliberately does NOT assert is that the claim path keeps
// working -- it does not. See
// TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery, which asserts the
// blocking directly. A previous revision of this test asserted the opposite
// using a SELECT-only stand-in for the claim query and passed while the real
// claim path was blocked.
func TestPhase12Migrations_0009TakesNoAccessExclusive_ReadsAndCompletionContinue(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	dropPhase12Schema(t, db)
	seedJobsForLockTest(t, db, 300)

	// Everything up to and including the NOT VALID constraint, uncontended.
	applyThroughVersion(t, db, 8)

	var validatedBefore bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT convalidated FROM pg_constraint WHERE conname = 'jobs_principal_id_not_null'`).Scan(&validatedBefore))
	require.False(t, validatedBefore, "test setup: the constraint must start NOT VALID")

	// Make this Up run apply exactly 0009 -- the scan and the index builds.
	markVersionApplied(t, db, 10, "0010_drop_global_idempotency_index.up.sql")

	_, release := holdClaimShapedRowLock(t, db)

	upCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- migrate.Up(upCtx, db) }()

	// Reads must keep working for the whole of 0009. This is the assertion
	// that fails outright if any ACCESS EXCLUSIVE statement is ever folded
	// back into this file, because ACCESS EXCLUSIVE blocks even ACCESS SHARE.
	for i := 0; i < 15; i++ {
		requireReadUnblocked(t, db, 300,
			"a concurrent read must never be blocked by migration 0009 -- it takes no ACCESS EXCLUSIVE lock")
		time.Sleep(20 * time.Millisecond)
	}

	// 0009 must COMPLETE while the row lock is still held -- not merely avoid
	// blocking readers.
	require.NoError(t, <-done,
		"migration 0009 must complete in full while a claim-shaped row lock is held")

	var validated bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT convalidated FROM pg_constraint WHERE conname = 'jobs_principal_id_not_null'`).Scan(&validated))
	require.True(t, validated,
		"VALIDATE CONSTRAINT must have completed under the held row lock")
	require.True(t, indexExists(t, db, "idx_jobs_idempotency_scoped"),
		"0009's index builds must also complete under the held row lock")
	require.True(t, indexExists(t, db, "idx_jobs_principal_id"))

	// And once 0009 has committed and released SHARE, the real claim query
	// works again -- the block is for the migration's duration, not permanent.
	release()
	requireClaimPathUnblocked(t, db,
		"once 0009 commits and releases SHARE, the real claim query must work again")

	// 0010's DROP INDEX is catalog-only but needs ACCESS EXCLUSIVE, so it
	// must wait for any conflicting lock like all DDL. That is documented
	// deployment behaviour, not a defect -- and it is why it is a separate
	// file.
	unmarkVersion(t, db, 10)
	require.NoError(t, migrate.Up(ctx, db))
	require.False(t, indexExists(t, db, "idx_jobs_idempotency_key"))
}

// TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery is the corrected
// HIGH-2 regression test. It asserts the blocking behaviour as a FACT to be
// scheduled around, rather than denying it.
//
// Mechanism, asserted rather than described: 0009's CREATE INDEX statements
// take SHARE on jobs; SHARE conflicts with ROW EXCLUSIVE; and every write to
// jobs needs ROW EXCLUSIVE -- including the worker claim query, which is an
// UPDATE (internal/store/claim.go), not the SELECT its candidate CTE makes it
// look like.
//
// The probe holds SHARE explicitly rather than racing a live migrate.Up. That
// is deliberate and makes the test deterministic instead of dependent on
// 0009's duration against a 300-row table: LOCK TABLE ... IN SHARE MODE takes
// byte-for-byte the same lock mode CREATE INDEX takes, so the conflict proved
// here is exactly the conflict a real deployment hits. The companion
// assertion below pins that 0009 really does contain CREATE INDEX statements,
// so the two halves cannot drift apart.
func TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	dropPhase12Schema(t, db)
	seedJobsForLockTest(t, db, 50)
	applyThroughVersion(t, db, 10)

	t.Run("0009 really does build indexes, so SHARE really is its lock", func(t *testing.T) {
		raw, err := migrations.Files.ReadFile("0009_validate_principal_id_and_scope_idempotency.up.sql")
		require.NoError(t, err)
		var code []string
		for _, line := range strings.Split(string(raw), "\n") {
			if trimmed := strings.TrimSpace(line); !strings.HasPrefix(trimmed, "--") {
				code = append(code, line)
			}
		}
		require.Contains(t, strings.ToUpper(strings.Join(code, "\n")), "CREATE INDEX",
			"this test's premise is that 0009 builds indexes under SHARE; if it no longer does, "+
				"re-derive the lock profile in docs/data-model.md rather than deleting this test")
	})

	// Hold exactly the lock CREATE INDEX holds, for the duration 0009 would
	// hold it (internal/migrate keeps it until the file commits).
	holder, err := db.Conn(ctx)
	require.NoError(t, err)
	defer holder.Close()
	holderTx, err := holder.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = holderTx.ExecContext(ctx, `LOCK TABLE jobs IN SHARE MODE`)
	require.NoError(t, err)

	t.Run("reads are NOT blocked", func(t *testing.T) {
		requireReadUnblocked(t, db, 50,
			"ACCESS SHARE is compatible with SHARE, so reads continue through 0009")
	})

	t.Run("the REAL worker claim query IS blocked", func(t *testing.T) {
		blocked, err := tryRealClaim(t, db, "1s")
		require.True(t, blocked,
			"migration 0009 takes SHARE, which conflicts with the ROW EXCLUSIVE the real claim "+
				"query needs -- workers CANNOT claim while it runs. If this assertion ever fails, "+
				"do not weaken it: re-derive the lock profile and correct README.md, "+
				"docs/data-model.md and 0009's own header, which all now state that it blocks")
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		require.Equal(t, lockNotAvailable, pgErr.Code,
			"the claim must fail by waiting for a table lock, not for some unrelated reason: %v", err)
	})

	t.Run("ordinary writes are blocked too", func(t *testing.T) {
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		defer conn.Close()
		_, err = conn.ExecContext(ctx, `SET lock_timeout = '1s'`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `
			INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds)
			VALUES ($1, $2, 'test.p12.lock', '{}', 'QUEUED', 30)`, uuid.New(), principal.SystemPrincipalID)
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr), "expected a PostgreSQL error, got %v", err)
		require.Equal(t, lockNotAvailable, pgErr.Code,
			"POST /jobs is blocked by 0009 for the same reason the claim query is")
	})

	// Releasing it restores both.
	require.NoError(t, holderTx.Rollback())
	requireClaimPathUnblocked(t, db, "the claim query must work again once SHARE is released")
}

// TestPhase12Migrations_NoLongRunningStatementSharesAFileWithExclusiveDDL
// pins the structural rule the runtime tests above measure: because
// internal/migrate holds every lock a file takes until that file commits, a
// statement whose cost scales with table size must never share a file with
// an ALTER TABLE or a DROP INDEX -- i.e. no ACCESS EXCLUSIVE lock may be held
// across a full-table scan or write. That is the whole of what the split
// buys; it does not make 0009 non-blocking.
//
// This is the rule a future edit is most likely to break -- by "tidying"
// these small migrations back into one -- and the runtime tests would then
// fail only under concurrency, which is a slower and more confusing signal
// than this.
func TestPhase12Migrations_NoLongRunningStatementSharesAFileWithExclusiveDDL(t *testing.T) {
	read := func(name string) string {
		raw, err := migrations.Files.ReadFile(name)
		require.NoError(t, err)
		// Strip comments so the prose explaining these rules does not trip
		// assertions about the SQL itself.
		var code []string
		for _, line := range strings.Split(string(raw), "\n") {
			if trimmed := strings.TrimSpace(line); !strings.HasPrefix(trimmed, "--") {
				code = append(code, line)
			}
		}
		return strings.ToUpper(strings.Join(code, "\n"))
	}

	// ACCESS EXCLUSIVE files: catalog-only, no scan, no long statement.
	for _, name := range []string{
		"0006_add_principal_id_columns.up.sql",
		"0008_require_principal_id.up.sql",
		"0010_drop_global_idempotency_index.up.sql",
	} {
		code := read(name)
		require.NotContains(t, code, "UPDATE JOBS",
			"%s takes ACCESS EXCLUSIVE; a backfill here would run under it", name)
		require.NotContains(t, code, "CREATE INDEX",
			"%s takes ACCESS EXCLUSIVE; an index build here would run under it", name)
		require.NotContains(t, code, "VALIDATE CONSTRAINT",
			"%s takes ACCESS EXCLUSIVE; a validation scan here would run under it", name)
	}

	// The backfill: no DDL of any kind may share its transaction.
	backfill := read("0007_backfill_principal_id.up.sql")
	require.Contains(t, backfill, "UPDATE JOBS")
	require.NotContains(t, backfill, "ALTER TABLE",
		"the backfill must not share a transaction with any ALTER TABLE")
	require.NotContains(t, backfill, "CREATE INDEX")
	require.NotContains(t, backfill, "DROP INDEX")

	// The scan-and-build file: no ACCESS EXCLUSIVE statement may join it.
	validate := read("0009_validate_principal_id_and_scope_idempotency.up.sql")
	require.Contains(t, validate, "VALIDATE CONSTRAINT")
	require.Contains(t, validate, "CREATE UNIQUE INDEX")
	require.NotContains(t, validate, "ADD CONSTRAINT",
		"VALIDATE CONSTRAINT must not share a transaction with ADD CONSTRAINT -- that is precisely "+
			"what made the documented non-blocking guarantee false")
	require.NotContains(t, validate, "ADD COLUMN")
	require.NotContains(t, validate, "DROP INDEX",
		"a DROP INDEX here would make the whole scan's transaction queue for ACCESS EXCLUSIVE "+
			"at the end, stalling every new reader behind it")
}
