// Integration test against a real PostgreSQL instance (internal/testutil).
package migrate_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/migrations"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

// TestUp_IsIdempotent proves migrations are safe to run every time a
// process starts (as cmd/api and cmd/worker both do): applying them twice
// in a row must not error and must not duplicate schema objects.
func TestUp_IsIdempotent(t *testing.T) {
	db := testutil.DB(t) // testutil.DB already runs migrate.Up once during setup
	ctx := context.Background()

	require.NoError(t, migrate.Up(ctx, db))
	require.NoError(t, migrate.Up(ctx, db))

	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = 1`).Scan(&count))
	require.Equal(t, 1, count, "migration 1 must be recorded exactly once even after Up runs multiple times")
}

// TestUp_CreatesJobsTableWithExpectedConstraints is a schema test: the
// jobs table and its state CHECK / idempotency UNIQUE constraint must
// exist after migrating, independent of any application code path that
// might rely on them.
func TestUp_CreatesJobsTableWithExpectedConstraints(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	var tableExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jobs')`,
	).Scan(&tableExists))
	require.True(t, tableExists)

	var constraintExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'jobs_state_check')`,
	).Scan(&constraintExists))
	require.True(t, constraintExists)

	var indexExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_jobs_idempotency_key')`,
	).Scan(&indexExists))
	require.True(t, indexExists)
}

// TestUp_CreatesJobAttemptsTableWithExpectedConstraints is Phase 2's
// analogue of TestUp_CreatesJobsTableWithExpectedConstraints: the
// job_attempts table (docs/data-model.md, docs/roadmap.md Phase 2 scope)
// and its UNIQUE(job_id, attempt_number) / outcome CHECK constraints must
// exist after migrating.
func TestUp_CreatesJobAttemptsTableWithExpectedConstraints(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	var tableExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'job_attempts')`,
	).Scan(&tableExists))
	require.True(t, tableExists)

	var uniqueExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'job_attempts_attempt_number_unique')`,
	).Scan(&uniqueExists))
	require.True(t, uniqueExists)

	var checkExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'job_attempts_outcome_check')`,
	).Scan(&checkExists))
	require.True(t, checkExists)

	var version int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations WHERE version = 2`).Scan(&version))
	require.Equal(t, 2, version)
}

// TestUp_CreatesWorkflowTablesWithExpectedConstraints is Phase 7's
// analogue of TestUp_CreatesJobAttemptsTableWithExpectedConstraints: the
// workflow_instances/workflow_nodes tables (docs/data-model.md,
// docs/roadmap.md Phase 7 scope) and their state CHECK / uniqueness
// constraints must exist after migrating.
func TestUp_CreatesWorkflowTablesWithExpectedConstraints(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	for _, table := range []string{"workflow_instances", "workflow_nodes"} {
		var tableExists bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&tableExists))
		require.True(t, tableExists, "table %s must exist", table)
	}

	var stateCheckExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_instances_state_check')`,
	).Scan(&stateCheckExists))
	require.True(t, stateCheckExists)

	var keyUniqueExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_nodes_key_unique')`,
	).Scan(&keyUniqueExists))
	require.True(t, keyUniqueExists)

	var jobUniqueExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_nodes_job_unique')`,
	).Scan(&jobUniqueExists))
	require.True(t, jobUniqueExists)

	var version int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations WHERE version = 3`).Scan(&version))
	require.Equal(t, 3, version)
}

// TestUp_UpgradesPhase1SchemaToPhase2 proves migrations upgrade cleanly:
// starting from a database that has ONLY migration 0001 applied (a
// stand-in for an existing Phase 1 deployment), running migrate.Up must
// apply migrations 0002 and 0003 on top of it — creating job_attempts and
// the Phase 7 workflow tables without altering or destroying the existing
// jobs table/data — rather than requiring a fresh database.
func TestUp_UpgradesPhase1SchemaToPhase2(t *testing.T) {
	db := testutil.DB(t) // starts from a fully-migrated DB; we tear down to Phase 1 below
	ctx := context.Background()

	// Roll back to a Phase-1-only schema: drop everything migrations 2
	// and 3 added and their schema_migrations records, leaving migration
	// 1's jobs table (and a row in it, to prove data survives the
	// upgrade) untouched. workflow_nodes/job_attempts must be dropped
	// before jobs would matter, but jobs itself is never dropped here.
	_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS workflow_nodes`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS workflow_instances`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS job_attempts`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (2, 3)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds)
		VALUES (gen_random_uuid(), 'test.phase1.preexisting', '{}', 'QUEUED', 30)`)
	// gen_random_uuid() is a PostgreSQL 13+ built-in (no extension
	// needed); docs/testing-strategy.md's target Postgres versions all
	// support it.
	require.NoError(t, err)

	var preUpgradeCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs`).Scan(&preUpgradeCount))
	require.Equal(t, 1, preUpgradeCount, "test setup: exactly one pre-existing Phase 1 job row expected")

	// This is the actual upgrade under test.
	require.NoError(t, migrate.Up(ctx, db))

	var tableExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'job_attempts')`,
	).Scan(&tableExists))
	require.True(t, tableExists, "migrate.Up must create job_attempts on top of an existing Phase 1 schema")

	var workflowTableExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'workflow_nodes')`,
	).Scan(&workflowTableExists))
	require.True(t, workflowTableExists, "migrate.Up must create workflow_nodes on top of an existing Phase 1 schema")

	var postUpgradeCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs`).Scan(&postUpgradeCount))
	require.Equal(t, 1, postUpgradeCount, "the upgrade must not touch pre-existing jobs data")

	var migratedVersions []int
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		migratedVersions = append(migratedVersions, v)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []int{1, 2, 3, 4}, migratedVersions)
}

// TestUp_UpgradesPhase6SchemaToPhase7 is Phase 7's analogue: starting from
// a database with migrations 0001-0002 already applied (a stand-in for an
// existing Phase 1-6 deployment, none of which ever required a schema
// change beyond migration 0002 — see README's Phase 3/4/6 sections),
// running migrate.Up must apply exactly migration 0003 on top of it,
// creating workflow_instances/workflow_nodes without touching existing
// jobs/job_attempts data.
func TestUp_UpgradesPhase6SchemaToPhase7(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS workflow_nodes`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS workflow_instances`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 3`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, execution_timeout_seconds)
		VALUES (gen_random_uuid(), 'test.phase6.preexisting', '{}', 'QUEUED', 30)`)
	require.NoError(t, err)

	require.NoError(t, migrate.Up(ctx, db))

	var tableExists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'workflow_instances')`,
	).Scan(&tableExists))
	require.True(t, tableExists, "migrate.Up must create workflow_instances on top of an existing Phase 1-6 schema")

	var postUpgradeCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs`).Scan(&postUpgradeCount))
	require.Equal(t, 1, postUpgradeCount, "the upgrade must not touch pre-existing jobs data")

	var version int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations WHERE version = 3`).Scan(&version))
	require.Equal(t, 3, version)
}

// TestMigration0004_BackfillsTerminalAttemptCount proves migration 0004's
// actual data semantics (internal/invariant's checkNoAttemptAfterTerminal
// depends on this column being backfilled correctly for every
// pre-existing terminal row -- see that package's doc comment), not just
// that the migration count/version bumped. Starting from a pre-0004
// schema/data state built the same way this file's other Upgrade tests
// do (drop what a later migration added, delete its schema_migrations
// row, insert data as if it predated that migration), this applies
// migration 0004 through the real migrate.Up mechanism and asserts its
// exact backfill contract:
//
//   - a terminal row with attempt_count > 0 backfills to that same value
//   - a terminal row with attempt_count = 0 (the zero-attempt
//     cancellation shape) backfills to 0, not left NULL
//   - a non-terminal row is left NULL, not backfilled to anything
//
// It also proves migrate.Up (and therefore migration 0004 specifically)
// remains safe to run twice in a row, per TestUp_IsIdempotent's existing
// contract, and separately runs the actual embedded 0004 down.sql content
// to prove it removes the column -- using the same embedded migrations.Files
// this package already depends on, not a new migration-down framework
// (this project's migrate.Up has no corresponding Down function to call;
// see this test's second half for why the down SQL is applied directly).
func TestMigration0004_BackfillsTerminalAttemptCount(t *testing.T) {
	db := testutil.DB(t) // starts fully migrated; rolled back to pre-0004 below
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `ALTER TABLE jobs DROP COLUMN IF EXISTS terminal_attempt_count`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 4`)
	require.NoError(t, err)

	terminalWithAttempts := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, attempt_count, max_attempts, execution_timeout_seconds, terminal_at, last_error, last_error_class)
		VALUES ($1, 'test.migration0004.terminal_with_attempts', '{}', 'SUCCEEDED', 3, 5, 30, now(), NULL, NULL)`,
		terminalWithAttempts)
	require.NoError(t, err)

	terminalZeroAttempts := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, attempt_count, max_attempts, execution_timeout_seconds, terminal_at)
		VALUES ($1, 'test.migration0004.terminal_zero_attempts', '{}', 'CANCELLED', 0, 5, 30, now())`,
		terminalZeroAttempts)
	require.NoError(t, err)

	nonTerminal := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, payload, state, attempt_count, max_attempts, execution_timeout_seconds)
		VALUES ($1, 'test.migration0004.nonterminal', '{}', 'QUEUED', 0, 5, 30)`,
		nonTerminal)
	require.NoError(t, err)

	// This is the actual upgrade under test.
	require.NoError(t, migrate.Up(ctx, db))

	var columnExists bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'terminal_attempt_count')`,
	).Scan(&columnExists))
	require.True(t, columnExists, "migration 0004 must add jobs.terminal_attempt_count")

	assertTerminalAttemptCount := func(id uuid.UUID, want sql.NullInt64, msg string) {
		t.Helper()
		var got sql.NullInt64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT terminal_attempt_count FROM jobs WHERE id = $1`, id).Scan(&got))
		require.Equal(t, want, got, msg)
	}
	assertTerminalAttemptCount(terminalWithAttempts, sql.NullInt64{Int64: 3, Valid: true},
		"a terminal row with attempt_count=3 must backfill terminal_attempt_count to 3")
	assertTerminalAttemptCount(terminalZeroAttempts, sql.NullInt64{Int64: 0, Valid: true},
		"a terminal row with attempt_count=0 (zero-attempt cancellation) must backfill terminal_attempt_count to 0, not leave it NULL")
	assertTerminalAttemptCount(nonTerminal, sql.NullInt64{},
		"a non-terminal row must NOT be backfilled -- terminal_attempt_count stays NULL until this job actually becomes terminal")

	var version int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = 4`).Scan(&version))
	require.Equal(t, 4, version, "migration 0004 must be recorded in schema_migrations")

	// Idempotency: re-running Up (as every cmd/api and cmd/worker startup
	// does) must not error and must not re-backfill or duplicate the
	// schema_migrations row -- migration 0004's own backfill UPDATE is
	// itself already guarded by `WHERE terminal_attempt_count IS NULL`,
	// but this proves the whole Up path, not just that one WHERE clause.
	require.NoError(t, migrate.Up(ctx, db))
	var version4Count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = 4`).Scan(&version4Count))
	require.Equal(t, 1, version4Count, "migration 4 must be recorded exactly once even after Up runs multiple times")
	assertTerminalAttemptCount(terminalWithAttempts, sql.NullInt64{Int64: 3, Valid: true},
		"re-running Up must not disturb an already-backfilled value")

	// Documented limitation (must not be contradicted by this test): a
	// terminal row that was ALREADY illegitimately reopened before this
	// migration ran cannot have its true original attempt_count
	// reconstructed from existing durable data -- the backfill above can
	// only record "attempt_count as of the backfill," which is exactly
	// what terminalWithAttempts's assertion above proves it does, not
	// some reconstructed historical value. No further assertion is
	// possible for that case; it is a genuine, permanent limitation of
	// migrating pre-existing data, not a bug in the backfill.

	// Prove the embedded 0004 down.sql content actually removes the
	// column -- applied directly (this project's migrate package has no
	// Down function to call; see this test's doc comment). This is run
	// against db, which per docs/testing-strategy.md may be a real,
	// PERSISTENT, cross-package-shared PostgreSQL instance (whenever
	// TASKFORGE_TEST_DATABASE_URL is set -- e.g. in CI), not a disposable
	// one scoped to this test alone -- so the drop below MUST be undone
	// before this test returns, via t.Cleanup registered before the drop
	// runs (so it still fires even if a later assertion in this block
	// fails t.FailNow()). Leaving the column dropped while
	// schema_migrations still records version 4 as applied would silently
	// break every other package's tests for the rest of this process
	// (migrate.Up sees version 4 already recorded and never reapplies
	// 0004's up.sql to restore it) -- exactly the failure mode this
	// comment exists to prevent a future edit from reintroducing.
	t.Cleanup(func() {
		upSQL, err := migrations.Files.ReadFile("0004_add_terminal_attempt_count.up.sql")
		require.NoError(t, err)
		_, err = db.ExecContext(context.Background(), string(upSQL))
		require.NoError(t, err, "must restore jobs.terminal_attempt_count for every other test sharing this database")
	})

	downSQL, err := migrations.Files.ReadFile("0004_add_terminal_attempt_count.down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(downSQL))
	require.NoError(t, err)

	var columnExistsAfterDown bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'terminal_attempt_count')`,
	).Scan(&columnExistsAfterDown))
	require.False(t, columnExistsAfterDown, "0004's down migration must remove jobs.terminal_attempt_count")
}

// TestFiles_ExactlyFourMigrationsEmbedded is a light guard against a
// migration file being accidentally left out of, or duplicated in, the
// embedded set — mostly useful as a canary if migrations/embed.go's glob
// pattern is ever changed.
func TestFiles_ExactlyFourMigrationsEmbedded(t *testing.T) {
	entries, err := migrations.Files.ReadDir(".")
	require.NoError(t, err)

	var upFiles int
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > 7 && e.Name()[len(e.Name())-7:] == ".up.sql" {
			upFiles++
		}
	}
	require.Equal(t, 4, upFiles)
}
