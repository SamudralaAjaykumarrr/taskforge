// Phase 12 privilege audit (G4, docs/phase-12-plan.md §15 stage 6;
// docs/enterprise-roadmap.md Phase 12 "Least-privilege PostgreSQL roles").
//
// deploy/postgres-roles.sql is operator provisioning, not a migration, so
// nothing in the normal startup path exercises it. That makes it exactly
// the kind of artifact that rots silently: a future migration adds a table,
// nobody re-runs the script, and the first symptom is a permission error in
// production. These tests apply the real script against the real schema and
// assert the privilege DIFFERENCE that is the whole point of having two
// roles -- not merely that the script parses.
//
// It lives in internal/migrate because the script's correctness is a
// function of the migrated schema, and this package already owns "does the
// schema look the way the documents say."
package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/migrations"
)

// applyRoleScript runs deploy/postgres-roles.sql against the test database
// and drops the roles again afterwards (PostgreSQL roles are cluster-wide
// and would otherwise outlive this test binary's other tests).
func applyRoleScript(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "postgres-roles.sql"))
	require.NoError(t, err, "deploy/postgres-roles.sql must exist")

	t.Cleanup(func() {
		for _, role := range []string{"taskforge_api", "taskforge_worker", "taskforge_retention"} {
			// DROP OWNED first: a role holding privileges cannot be
			// dropped while any grant still references it.
			if _, err := db.ExecContext(context.Background(), `DROP OWNED BY `+role); err != nil {
				t.Logf("cleanup: drop owned by %s: %v", role, err)
			}
			if _, err := db.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+role); err != nil {
				t.Logf("cleanup: drop role %s: %v", role, err)
			}
		}
	})

	_, err = db.ExecContext(ctx, string(raw))
	require.NoError(t, err, "deploy/postgres-roles.sql must apply cleanly against a migrated database")
}

// hasPrivilege asks PostgreSQL itself whether role holds privilege on
// table -- the authoritative answer, not an inference from the script's
// text.
func hasPrivilege(t *testing.T, db *sql.DB, role, table, privilege string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT has_table_privilege($1, $2, $3)`, role, table, privilege).Scan(&ok))
	return ok
}

// TestPostgresRoleScript_AppliesCleanlyAndGrantsTheDocumentedPrivileges is
// the audit itself.
func TestPostgresRoleScript_AppliesCleanlyAndGrantsTheDocumentedPrivileges(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)

	t.Run("the API role can do its job", func(t *testing.T) {
		for _, c := range []struct{ table, priv string }{
			{"jobs", "SELECT"}, {"jobs", "INSERT"}, {"jobs", "UPDATE"},
			{"workflow_instances", "SELECT"}, {"workflow_instances", "INSERT"}, {"workflow_instances", "UPDATE"},
			{"workflow_nodes", "SELECT"}, {"workflow_nodes", "INSERT"},
			{"job_attempts", "SELECT"},
			// It authenticates callers, so it must be able to read
			// credentials.
			{"principals", "SELECT"}, {"api_keys", "SELECT"},
		} {
			require.True(t, hasPrivilege(t, db, "taskforge_api", c.table, c.priv),
				"taskforge_api needs %s on %s", c.priv, c.table)
		}
	})

	t.Run("the API role cannot mint, revoke, or rewrite credentials", func(t *testing.T) {
		// This is the containment that matters: a compromised API server
		// can verify credentials but cannot create one, un-revoke one,
		// extend an expiry, or rewrite a secret_hash.
		for _, c := range []struct{ table, priv string }{
			{"api_keys", "INSERT"},
			{"principals", "INSERT"}, {"principals", "UPDATE"},
		} {
			require.False(t, hasPrivilege(t, db, "taskforge_api", c.table, c.priv),
				"taskforge_api must NOT have %s on %s -- key lifecycle is operator tooling", c.priv, c.table)
		}

		// last_used_at is the single column it may write, and the grant is
		// column-scoped rather than table-wide.
		var canWriteLastUsed, canWriteSecretHash, canWriteRevokedAt bool
		require.NoError(t, db.QueryRow(
			`SELECT has_column_privilege('taskforge_api', 'api_keys', 'last_used_at', 'UPDATE'),
			        has_column_privilege('taskforge_api', 'api_keys', 'secret_hash', 'UPDATE'),
			        has_column_privilege('taskforge_api', 'api_keys', 'revoked_at', 'UPDATE')`,
		).Scan(&canWriteLastUsed, &canWriteSecretHash, &canWriteRevokedAt))
		require.True(t, canWriteLastUsed, "best-effort telemetry must be writable")
		require.False(t, canWriteSecretHash, "a compromised API server must not be able to rewrite a stored secret hash")
		require.False(t, canWriteRevokedAt, "a compromised API server must not be able to un-revoke a key")
	})

	t.Run("the worker role can do its job", func(t *testing.T) {
		for _, c := range []struct{ table, priv string }{
			{"jobs", "SELECT"}, {"jobs", "UPDATE"},
			{"job_attempts", "SELECT"}, {"job_attempts", "INSERT"}, {"job_attempts", "UPDATE"},
			{"workflow_instances", "SELECT"}, {"workflow_instances", "UPDATE"},
			{"workflow_nodes", "SELECT"},
		} {
			require.True(t, hasPrivilege(t, db, "taskforge_worker", c.table, c.priv),
				"taskforge_worker needs %s on %s", c.priv, c.table)
		}
	})

	t.Run("the worker role has no access to credentials at all", func(t *testing.T) {
		// OD-3: worker identity is the database role itself, so a worker
		// never needs to look a principal or an API key up -- and must not
		// be able to.
		for _, table := range []string{"principals", "api_keys"} {
			for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
				require.False(t, hasPrivilege(t, db, "taskforge_worker", table, priv),
					"taskforge_worker must have NO %s on %s", priv, table)
			}
		}
	})

	t.Run("neither role may DELETE anything", func(t *testing.T) {
		for _, role := range []string{"taskforge_api", "taskforge_worker"} {
			for _, table := range []string{"jobs", "job_attempts", "workflow_instances", "workflow_nodes", "principals", "api_keys"} {
				require.False(t, hasPrivilege(t, db, role, table, "DELETE"),
					"%s must not be able to DELETE from %s -- no TaskForge code path issues one", role, table)
			}
		}
	})

	t.Run("the two roles' privileges genuinely differ", func(t *testing.T) {
		// The containment value of least-privilege roles comes entirely
		// from the privilege difference, not from having two passwords
		// (docs/security-model.md §2). Assert a real difference exists in
		// both directions, so a future edit that quietly widens one role to
		// match the other is caught.
		require.True(t, hasPrivilege(t, db, "taskforge_api", "api_keys", "SELECT"))
		require.False(t, hasPrivilege(t, db, "taskforge_worker", "api_keys", "SELECT"))

		require.True(t, hasPrivilege(t, db, "taskforge_worker", "job_attempts", "INSERT"))
		require.False(t, hasPrivilege(t, db, "taskforge_api", "job_attempts", "INSERT"))
	})

	t.Run("neither role may create schema objects", func(t *testing.T) {
		for _, role := range []string{"taskforge_api", "taskforge_worker"} {
			var canCreate bool
			require.NoError(t, db.QueryRow(
				`SELECT has_schema_privilege($1, 'public', 'CREATE')`, role).Scan(&canCreate))
			require.False(t, canCreate,
				"%s must not be able to create objects -- migrations run as the owner, not as these roles", role)
		}
	})
}

// TestPostgresRoleScript_IsIdempotent proves the script is safe to re-run,
// which it must be: it has to be re-applied after any future migration that
// adds a table, and an operator should not have to reason about whether the
// roles already exist.
func TestPostgresRoleScript_IsIdempotent(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)

	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "postgres-roles.sql"))
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), string(raw))
	require.NoError(t, err, "deploy/postgres-roles.sql must be safe to re-run")

	require.True(t, hasPrivilege(t, db, "taskforge_api", "jobs", "SELECT"))
	require.False(t, hasPrivilege(t, db, "taskforge_worker", "api_keys", "SELECT"))
}

// createTableRE extracts the table name from a migration's CREATE TABLE
// statement, tolerating the "IF NOT EXISTS" every migrations/*.up.sql file
// uses (see the grep in migrationDefinedTables's comment).
var createTableRE = regexp.MustCompile(`(?i)CREATE TABLE(?:\s+IF NOT EXISTS)?\s+([a-zA-Z_][a-zA-Z0-9_]*)`)

// migrationDefinedTables returns every table a migrations/*.up.sql file
// creates, read from the embedded migration source itself -- NOT from the
// live database's pg_tables catalog.
//
// That distinction matters: internal/testutil.DB may point at a database
// shared with other packages' test binaries (TASKFORGE_TEST_DATABASE_URL,
// which CI sets to one Postgres service container for the whole job; a
// local run without it gets a fresh embedded-postgres instance per test
// binary and never shares a database at all). Other packages create their
// own throwaway tables directly against that shared "public" schema --
// txenqueue/txenqueue_test.go's setupBusinessTable creates
// txenqueue_test_business_rows to stand in for a caller's own business
// table, and deliberately never drops it (see its doc comment). Enumerating
// pg_tables would make this test's pass/fail depend on which other
// packages' tests happened to run against the same physical database
// first -- exactly the CI-only flake this test hit: passes locally (every
// package gets its own throwaway database), fails in CI (one shared
// database, so a fixture table created by an unrelated package leaks into
// the scan and "is unreachable by both roles" because no role was ever
// meant to reach it).
//
// The migrations are the actual definition of "the schema TaskForge has,"
// so this reads that definition directly instead of asking a catalog that
// can contain tables no migration ever created.
func migrationDefinedTables(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	require.NoError(t, err)

	seen := make(map[string]bool)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		content, err := migrations.Files.ReadFile(e.Name())
		require.NoError(t, err)
		for _, m := range createTableRE.FindAllStringSubmatch(string(content), -1) {
			seen[strings.ToLower(m[1])] = true
		}
	}

	tables := make([]string, 0, len(seen))
	for name := range seen {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	return tables
}

// TestPostgresRoleScript_CoversEveryTableTheSchemaHas is the rot guard: if
// a future migration adds a table and nobody updates the provisioning
// script, at least one role will have no privilege on it at all and this
// fails -- instead of the first symptom being a permission error in
// production.
func TestPostgresRoleScript_CoversEveryTableTheSchemaHas(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)

	tables := migrationDefinedTables(t)
	require.NotEmpty(t, tables)

	for _, table := range tables {
		apiCan := hasPrivilege(t, db, "taskforge_api", table, "SELECT")
		workerCan := hasPrivilege(t, db, "taskforge_worker", table, "SELECT")
		require.True(t, apiCan || workerCan,
			"table %q is unreachable by BOTH least-privilege roles -- deploy/postgres-roles.sql "+
				"has not been updated for the migration that added it", table)
	}
}

// ---------------------------------------------------------------------
// The deployment path itself, not just the catalog
// ---------------------------------------------------------------------
//
// Everything above asks PostgreSQL what privileges the two roles hold.
// That is necessary and it is not sufficient: an independent review found
// every grant correct and the documented deployment still completely
// broken, because nothing ever connected AS either role and ran the code
// cmd/api and cmd/worker actually run at startup.
//
// The tests below close that gap. They open a real connection as
// taskforge_api / taskforge_worker and invoke the real migrate.Up.

// rolePassword is set on both roles by setRolePasswords so these tests can
// log in. deploy/postgres-roles.sql ships deliberately non-working
// placeholders ("CHANGE_ME_api"), which an operator is told to replace;
// replacing them here is the same step, not a relaxation of the script.
const rolePassword = "phase12-role-audit-password"

func setRolePasswords(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, role := range []string{"taskforge_api", "taskforge_worker", "taskforge_retention"} {
		_, err := db.ExecContext(context.Background(),
			`ALTER ROLE `+role+` PASSWORD '`+rolePassword+`'`)
		require.NoError(t, err, "set a usable password on %s", role)
	}
}

// openAsRole opens a connection to the same test database as one of the
// least-privilege roles, exactly as cmd/api or cmd/worker would be
// configured in the deployment deploy/postgres-roles.sql describes.
func openAsRole(t *testing.T, role string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testutil.DSNAsRole(t, role, rolePassword))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx),
		"%s must be able to connect -- it is the role the deployment guide names", role)
	return db
}

// TestMigrateUp_RunsUnderLeastPrivilegeRoles is the HIGH-1 regression test.
//
// cmd/api and cmd/worker both call migrate.Up unconditionally on startup
// (cmd/api/main.go, cmd/worker/main.go). deploy/postgres-roles.sql's own
// USAGE section step 4 says to point them at taskforge_api and
// taskforge_worker. Those two facts together are the deployment, and they
// must actually work against an already-migrated database.
//
// Before the fix this failed with "permission denied for schema public",
// because migrate.Up's ensureMigrationsTable issued CREATE TABLE IF NOT
// EXISTS unconditionally and PostgreSQL performs the schema CREATE ACL
// check BEFORE the IF NOT EXISTS short-circuit -- so a role correctly
// denied CREATE could not start even when there was nothing whatsoever to
// create. Up now probes the catalog first and issues no DDL when the
// ledger already exists.
//
// This is deliberately NOT fixed by granting the roles CREATE: the whole
// point of G4 is that neither role may change the schema
// (TestPostgresRoleScript_AppliesCleanlyAndGrantsTheDocumentedPrivileges's
// "neither role may create schema objects" subtest still asserts exactly
// that, and still passes).
func TestMigrateUp_RunsUnderLeastPrivilegeRoles(t *testing.T) {
	db := testutil.DB(t) // fully migrated, as the owner
	applyRoleScript(t, db)
	setRolePasswords(t, db)

	for _, role := range []string{"taskforge_api", "taskforge_worker"} {
		t.Run(role, func(t *testing.T) {
			roleDB := openAsRole(t, role)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			require.NoError(t, migrate.Up(ctx, roleDB),
				"migrate.Up must succeed as %s against an already-migrated database -- "+
					"cmd/api and cmd/worker call it on every startup", role)

			// Idempotent: a restart must behave identically.
			require.NoError(t, migrate.Up(ctx, roleDB),
				"a second startup under %s must also succeed", role)
		})
	}

	// And the privilege boundary is intact: neither role gained CREATE.
	for _, role := range []string{"taskforge_api", "taskforge_worker"} {
		var canCreate bool
		require.NoError(t, db.QueryRow(
			`SELECT has_schema_privilege($1, 'public', 'CREATE')`, role).Scan(&canCreate))
		require.False(t, canCreate,
			"%s must still be unable to create schema objects -- the fix is in migrate.Up, "+
				"not in a widened grant", role)
	}
}

// TestMigrateUp_UnderLeastPrivilegeRole_CannotApplyAPendingMigration is the
// other half of the same contract, and the one that proves the fix did not
// simply make migrate.Up permissive.
//
// A least-privilege process must start cleanly when there is nothing to do,
// and must still fail LOUDLY when there genuinely is a pending schema
// change -- because applying schema changes is an owner-privileged,
// separate deployment step by design. Silently skipping a pending migration
// would be far worse than the startup failure this replaces: the process
// would come up and run against a schema it was not built for.
func TestMigrateUp_UnderLeastPrivilegeRole_CannotApplyAPendingMigration(t *testing.T) {
	db := testutil.DB(t)
	applyRoleScript(t, db)
	setRolePasswords(t, db)
	ctx := context.Background()

	// Make the newest migration look un-applied, so migrate.Up has real
	// work to do. Restored in cleanup regardless of outcome.
	var version int64
	var name string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT version, name FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &name))
	_, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)
			 ON CONFLICT (version) DO NOTHING`, version, name)
	})

	roleDB := openAsRole(t, "taskforge_api")
	err = migrate.Up(ctx, roleDB)
	require.Error(t, err,
		"a least-privilege role must not be able to apply a pending migration -- "+
			"schema changes are an owner-privileged deployment step (G4)")
	// SQLSTATE 42501 is PostgreSQL's insufficient_privilege class. Asserting
	// the class rather than a message keeps this robust across the two
	// different wordings PostgreSQL uses here ("permission denied for
	// schema ..." for a CREATE, "must be owner of table ..." for an ALTER)
	// while still proving the rejection came from PostgreSQL's own
	// authorization check and not from a silent skip in our own code.
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr),
		"the failure must be PostgreSQL's own error, not one synthesised by migrate: %v", err)
	require.Equal(t, "42501", pgErr.Code,
		"the failure must be an insufficient_privilege error, not some unrelated failure: %v", err)

	// Nothing was recorded, so the failure is loud and repeatable rather
	// than leaving the ledger half-written.
	var recorded int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = $1`, version).Scan(&recorded))
	require.Zero(t, recorded, "a failed migration must record nothing (TF-INV-013)")
}
