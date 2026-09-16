// Phase 13 privilege audit (docs/phase-13-plan.md §9, §15; ADR-0009):
// deploy/postgres-roles.sql's new taskforge_retention role, and the
// governance-table grants added to taskforge_api/taskforge_worker.
// Mirrors phase12_roles_test.go's methodology exactly -- ask PostgreSQL
// itself, not the script's text -- extended to the three tables/roles this
// phase adds.
package migrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestPostgresRoleScript_TaskforgeRetentionIsTheNarrowestRole is the core
// least-privilege proof for the new role: SELECT+DELETE on exactly the
// four retention-eligible tables, and nothing else -- no access to
// credentials, no access to governance/rate-limit state, no INSERT/UPDATE
// anywhere (the sweeper only ever reads and deletes).
func TestPostgresRoleScript_TaskforgeRetentionIsTheNarrowestRole(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)

	t.Run("can SELECT and DELETE on exactly the four retention-eligible tables", func(t *testing.T) {
		for _, table := range []string{"jobs", "job_attempts", "workflow_instances", "workflow_nodes"} {
			require.True(t, hasPrivilege(t, db, "taskforge_retention", table, "SELECT"), "taskforge_retention needs SELECT on %s", table)
			require.True(t, hasPrivilege(t, db, "taskforge_retention", table, "DELETE"), "taskforge_retention needs DELETE on %s", table)
			require.False(t, hasPrivilege(t, db, "taskforge_retention", table, "INSERT"), "taskforge_retention must not INSERT into %s -- it only prunes", table)
			require.False(t, hasPrivilege(t, db, "taskforge_retention", table, "UPDATE"), "taskforge_retention must not UPDATE %s -- it only prunes", table)
		}
	})

	t.Run("has no access to credentials or governance/rate-limit state", func(t *testing.T) {
		for _, table := range []string{"principals", "api_keys", "queue_state", "queue_limits", "queue_slots", "rate_limit_buckets"} {
			for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
				require.False(t, hasPrivilege(t, db, "taskforge_retention", table, priv),
					"taskforge_retention must have NO %s on %s -- it is scoped to exactly the four retention tables", priv, table)
			}
		}
	})

	t.Run("cannot create schema objects", func(t *testing.T) {
		var canCreate bool
		require.NoError(t, db.QueryRow(`SELECT has_schema_privilege('taskforge_retention', 'public', 'CREATE')`).Scan(&canCreate))
		require.False(t, canCreate)
	})
}

// TestPostgresRoleScript_ApiAndWorkerGovernanceTableGrants proves the
// asymmetric split ADR-0009's implementation depends on: the API server
// upserts queue_state and reads/refills rate-limit state; the worker reads
// and holds/releases queue_slots and updates queue_state's fairness
// column; NEITHER role ever consults queue_limits from the claim path
// (the worker has no grant on it at all), and neither may DELETE any of
// the four new tables -- slot-count reconciliation is owner-privileged
// DML via cmd/taskforge-admin, never a runtime code path.
func TestPostgresRoleScript_ApiAndWorkerGovernanceTableGrants(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)

	t.Run("taskforge_api", func(t *testing.T) {
		require.True(t, hasPrivilege(t, db, "taskforge_api", "queue_state", "SELECT"))
		require.True(t, hasPrivilege(t, db, "taskforge_api", "queue_state", "INSERT"))
		require.False(t, hasPrivilege(t, db, "taskforge_api", "queue_state", "UPDATE"),
			"only the claim path (taskforge_worker) advances last_claimed_at")
		require.True(t, hasPrivilege(t, db, "taskforge_api", "queue_limits", "SELECT"))
		require.False(t, hasPrivilege(t, db, "taskforge_api", "queue_limits", "INSERT"),
			"queue_limits is operator-tool-managed (cmd/taskforge-admin under the owner role)")
		require.True(t, hasPrivilege(t, db, "taskforge_api", "rate_limit_buckets", "SELECT"))
		require.True(t, hasPrivilege(t, db, "taskforge_api", "rate_limit_buckets", "INSERT"))
		require.True(t, hasPrivilege(t, db, "taskforge_api", "rate_limit_buckets", "UPDATE"))
		require.False(t, hasPrivilege(t, db, "taskforge_api", "queue_slots", "SELECT"),
			"the API server has no reason to touch the concurrency slot table directly")
	})

	t.Run("taskforge_worker", func(t *testing.T) {
		require.True(t, hasPrivilege(t, db, "taskforge_worker", "queue_slots", "SELECT"))
		require.True(t, hasPrivilege(t, db, "taskforge_worker", "queue_slots", "UPDATE"))
		require.False(t, hasPrivilege(t, db, "taskforge_worker", "queue_slots", "INSERT"),
			"slot provisioning is owner-privileged DML via cmd/taskforge-admin, never the claim path")
		require.True(t, hasPrivilege(t, db, "taskforge_worker", "queue_state", "SELECT"))
		require.True(t, hasPrivilege(t, db, "taskforge_worker", "queue_state", "INSERT"))
		require.True(t, hasPrivilege(t, db, "taskforge_worker", "queue_state", "UPDATE"))
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			require.False(t, hasPrivilege(t, db, "taskforge_worker", "queue_limits", priv),
				"taskforge_worker must have NO %s on queue_limits -- \"does this queue have capacity\" "+
					"is answered entirely from queue_slots", priv)
		}
	})

	t.Run("neither role may DELETE any Phase 13 table", func(t *testing.T) {
		for _, role := range []string{"taskforge_api", "taskforge_worker"} {
			for _, table := range []string{"queue_state", "queue_limits", "queue_slots", "rate_limit_buckets"} {
				require.False(t, hasPrivilege(t, db, role, table, "DELETE"),
					"%s must not be able to DELETE from %s", role, table)
			}
		}
	})
}

// TestMigrateUp_RunsUnderTaskforgeRetentionRole is
// TestMigrateUp_RunsUnderLeastPrivilegeRoles's counterpart for the new
// role: any process wired up under taskforge_retention (the retention
// sweeper) must be able to start against an already-migrated database
// exactly like cmd/api and cmd/worker do.
func TestMigrateUp_RunsUnderTaskforgeRetentionRole(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)
	setRolePasswords(t, db)

	roleDB := openAsRole(t, "taskforge_retention")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, migrate.Up(ctx, roleDB))
	require.NoError(t, migrate.Up(ctx, roleDB), "a second startup must also succeed")
}

// TestPostgresRoleScript_TaskforgeRetentionCanActuallyDeleteATerminalJob is
// the deployment-path proof, not just the catalog: connect as the role and
// perform a representative retention delete for real, exactly as
// internal/retention's sweeper will.
func TestPostgresRoleScript_TaskforgeRetentionCanActuallyDeleteATerminalJob(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)
	setRolePasswords(t, db)
	ctx := context.Background()

	id := uuid.New()
	_, err := db.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds, terminal_at, terminal_attempt_count)
		VALUES ($1, $2, 'test.p13.retention_role', '{}', 'SUCCEEDED', 30, now() - interval '1 hour', 0)`,
		id, principal.SystemPrincipalID)
	require.NoError(t, err)

	roleDB := openAsRole(t, "taskforge_retention")
	_, err = roleDB.ExecContext(ctx, `DELETE FROM jobs WHERE id = $1`, id)
	require.NoError(t, err, "taskforge_retention must be able to delete a terminal job row")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&count))
	require.Zero(t, count)

	// And it cannot insert one, or read/touch credentials.
	_, err = roleDB.ExecContext(ctx, `
		INSERT INTO jobs (id, principal_id, job_type, payload, state, execution_timeout_seconds)
		VALUES ($1, $2, 'test.p13.retention_role_insert', '{}', 'QUEUED', 30)`,
		uuid.New(), principal.SystemPrincipalID)
	require.Error(t, err, "taskforge_retention must not be able to INSERT")

	_, err = roleDB.QueryContext(ctx, `SELECT 1 FROM api_keys LIMIT 1`)
	require.Error(t, err, "taskforge_retention must not be able to read api_keys")
}
