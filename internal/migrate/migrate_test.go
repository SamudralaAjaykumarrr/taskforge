// Integration test against a real PostgreSQL instance (internal/testutil).
package migrate_test

import (
	"context"
	"testing"

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

// TestUp_UpgradesPhase1SchemaToPhase2 proves migrations upgrade cleanly:
// starting from a database that has ONLY migration 0001 applied (a
// stand-in for an existing Phase 1 deployment), running migrate.Up must
// apply exactly migration 0002 on top of it — creating job_attempts
// without altering or destroying the existing jobs table/data — rather
// than requiring a fresh database.
func TestUp_UpgradesPhase1SchemaToPhase2(t *testing.T) {
	db := testutil.DB(t) // starts from a fully-migrated DB; we tear down to Phase 1 below
	ctx := context.Background()

	// Roll back to a Phase-1-only schema: drop job_attempts and the
	// migration 2 record, leaving migration 1's jobs table (and a row in
	// it, to prove data survives the upgrade) untouched.
	_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS job_attempts`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 2`)
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
	require.Equal(t, []int{1, 2}, migratedVersions)
}

// TestFiles_ExactlyTwoMigrationsEmbedded is a light guard against a
// migration file being accidentally left out of, or duplicated in, the
// embedded set — mostly useful as a canary if migrations/embed.go's glob
// pattern is ever changed.
func TestFiles_ExactlyTwoMigrationsEmbedded(t *testing.T) {
	entries, err := migrations.Files.ReadDir(".")
	require.NoError(t, err)

	var upFiles int
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > 7 && e.Name()[len(e.Name())-7:] == ".up.sql" {
			upFiles++
		}
	}
	require.Equal(t, 2, upFiles)
}
