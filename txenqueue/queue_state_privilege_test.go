// Phase 13 correction pass (independent review finding H2): EnqueueTx's
// underlying insert (internal/store.insertJobQuery, shared by InsertTx and
// InsertIdempotent) is now a data-modifying CTE that also performs
// `INSERT INTO queue_state (queue_name) ... ON CONFLICT (queue_name) DO
// NOTHING` in the SAME statement and transaction as the job row itself
// (docs/phase-13-plan.md §7; deploy/postgres-roles.sql's own comment on
// taskforge_api's queue_state grant). This is a real, breaking change to
// the minimum PostgreSQL privilege an EXTERNAL txenqueue integrator's own
// custom database role needs -- distinct from taskforge_api/taskforge_worker,
// which internal/migrate/phase13_roles_test.go already covers.
//
// This test proves both halves of docs/transactional-enqueue.md's new
// "Required database privilege" section directly against real PostgreSQL,
// under a role that holds only what a pre-Phase-13 integrator would have
// granted themselves (SELECT/INSERT on jobs, SELECT on job_attempts) plus
// CONNECT/USAGE -- never taskforge_api itself, which is a different,
// broader-scoped role covered elsewhere:
//
//  1. Necessary: without SELECT+INSERT on queue_state, EnqueueTx fails,
//     its error classifies as ErrEnqueueFailed (never a raw PostgreSQL/SQL
//     leak, per this package's existing error-leakage discipline), and the
//     caller's tx is left usable to roll back -- no partial,
//     half-privileged write survives.
//  2. Sufficient: granting exactly SELECT+INSERT on queue_state (nothing
//     broader -- no UPDATE or DELETE) is enough for EnqueueTx to succeed,
//     proving the documented minimum is not merely necessary but also
//     complete. SELECT is required, not merely INSERT, because PostgreSQL's
//     own `INSERT ... ON CONFLICT` (the upsert insertJobQuery uses for
//     queue_state) checks the conflict target against existing rows even
//     for DO NOTHING with no inference/exclusion clause, which requires
//     SELECT on the table -- confirmed directly against real PostgreSQL
//     while writing this test (INSERT alone produces "permission denied
//     for table queue_state"), not assumed from the ON CONFLICT DO NOTHING
//     clause's own read of "no SET/WHERE, so surely no SELECT."
package txenqueue_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/txenqueue"
)

const (
	externalIntegratorRole     = "txenqueue_external_integrator_test_role"
	externalIntegratorPassword = "txenqueue_external_integrator_test_password"
)

// createExternalIntegratorRole provisions a role holding exactly what a
// pre-Phase-13 external integrator would have granted themselves to use
// EnqueueTx before this phase existed: CONNECT/USAGE, and SELECT/INSERT on
// jobs (the only table EnqueueTx's pre-Phase-13 insert ever touched),
// SELECT on job_attempts (read-back, not written by this package, but a
// plausible pre-existing grant). It deliberately does NOT grant anything
// on queue_state -- that is exactly the gap this test proves.
func createExternalIntegratorRole(t *testing.T, ownerDB *sql.DB) {
	t.Helper()
	ctx := context.Background()

	_, err := ownerDB.ExecContext(ctx,
		`DROP OWNED BY `+externalIntegratorRole)
	if err == nil {
		_, _ = ownerDB.ExecContext(ctx, `DROP ROLE IF EXISTS `+externalIntegratorRole)
	}
	_, err = ownerDB.ExecContext(ctx,
		`CREATE ROLE `+externalIntegratorRole+` LOGIN PASSWORD '`+externalIntegratorPassword+`'`)
	require.NoError(t, err)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = ownerDB.ExecContext(cctx, `REVOKE ALL ON queue_state FROM `+externalIntegratorRole)
		_, _ = ownerDB.ExecContext(cctx, `DROP OWNED BY `+externalIntegratorRole)
		_, _ = ownerDB.ExecContext(cctx, `DROP ROLE IF EXISTS `+externalIntegratorRole)
	})

	stmts := []string{
		`GRANT CONNECT ON DATABASE ` + currentDatabase(t, ownerDB) + ` TO ` + externalIntegratorRole,
		`GRANT USAGE ON SCHEMA public TO ` + externalIntegratorRole,
		`GRANT SELECT, INSERT ON jobs TO ` + externalIntegratorRole,
		`GRANT SELECT ON job_attempts TO ` + externalIntegratorRole,
	}
	for _, stmt := range stmts {
		_, err := ownerDB.ExecContext(ctx, stmt)
		require.NoError(t, err, "setting up external-integrator test role: %s", stmt)
	}
}

func currentDatabase(t *testing.T, db *sql.DB) string {
	t.Helper()
	var name string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&name))
	return name
}

// TestEnqueueTx_ExternalIntegratorRole_RequiresQueueStateInsert is finding
// H2's regression test.
func TestEnqueueTx_ExternalIntegratorRole_RequiresQueueStateInsert(t *testing.T) {
	ownerDSN := testutil.DSN(t)
	ownerDB, err := sql.Open("pgx", ownerDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ownerDB.Close() })

	createExternalIntegratorRole(t, ownerDB)
	principalID := testutil.NewPrincipal(t, ownerDB, principal.KindCaller, "external-integrator")

	roleDSN := testutil.DSNAsRole(t, externalIntegratorRole, externalIntegratorPassword)
	rolePool, err := pgxpool.New(context.Background(), roleDSN)
	require.NoError(t, err)
	t.Cleanup(rolePool.Close)

	req := txenqueue.EnqueueRequest{
		PrincipalID: principalID,
		JobType:     "test.txenqueue.h2.privilege",
		Payload:     json.RawMessage(`{}`),
	}

	t.Run("fails without SELECT+INSERT on queue_state, classified as ErrEnqueueFailed, tx still rollback-able", func(t *testing.T) {
		tx, err := rolePool.Begin(context.Background())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()

		_, _, err = txenqueue.New().EnqueueTx(context.Background(), tx, req)
		require.Error(t, err, "a role holding only pre-Phase-13 grants (no privilege on queue_state) must not be able to enqueue")
		require.True(t, errors.Is(err, txenqueue.ErrEnqueueFailed),
			"the failure must classify as the generic persistence-failure sentinel, exactly like any other unexpected store error")
		require.NotContains(t, err.Error(), "queue_state", "the error text must never leak the underlying table/permission/SQL detail (this package's existing error-leakage discipline)")
		require.NotContains(t, err.Error(), "permission denied")

		// The caller's tx must still be explicitly rollback-able -- this
		// package never leaves the caller unable to clean up after its
		// own error.
		require.NoError(t, tx.Rollback(context.Background()))
	})

	t.Run("succeeds once granted exactly SELECT+INSERT on queue_state, nothing broader", func(t *testing.T) {
		_, err := ownerDB.ExecContext(context.Background(), `GRANT SELECT, INSERT ON queue_state TO `+externalIntegratorRole)
		require.NoError(t, err)

		// Prove the grant really is minimal: no UPDATE or DELETE on
		// queue_state was extended alongside it.
		for _, priv := range []string{"UPDATE", "DELETE"} {
			var has bool
			require.NoError(t, ownerDB.QueryRowContext(context.Background(),
				`SELECT has_table_privilege($1, 'queue_state', $2)`, externalIntegratorRole, priv).Scan(&has))
			require.False(t, has, "the documented minimum grant is SELECT+INSERT only -- %s must not be present", priv)
		}

		tx, err := rolePool.Begin(context.Background())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()

		job, created, err := txenqueue.New().EnqueueTx(context.Background(), tx, req)
		require.NoError(t, err, "SELECT+INSERT on queue_state must be sufficient -- no broader grant should be required")
		require.True(t, created)
		require.NotEqual(t, job.ID.String(), "")

		require.NoError(t, tx.Commit(context.Background()))
	})
}
