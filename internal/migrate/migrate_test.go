// Integration test against a real PostgreSQL instance (internal/testutil).
package migrate_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
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
