// Package testutil provides a real, disposable PostgreSQL database for
// integration tests, per docs/testing-strategy.md: "PostgreSQL
// integration tests run against a real PostgreSQL instance ... not an
// in-memory substitute or a mocked driver."
//
// Two modes are supported:
//
//   - TASKFORGE_TEST_DATABASE_URL is set (typical in CI, which runs a
//     Postgres service container): tests connect to that database
//     directly. The caller is responsible for pointing this at a
//     throwaway database.
//   - TASKFORGE_TEST_DATABASE_URL is unset (typical for local development
//     without Docker): this package starts a real, temporary PostgreSQL
//     server via github.com/fergusstrange/embedded-postgres (a portable
//     binary distribution, no root/Docker required) for the lifetime of
//     the test binary. Every package that calls DB must call RunMain from
//     its own TestMain so that server is explicitly stopped before the
//     test binary exits — embedded-postgres's server process otherwise
//     detaches from this process and is not reaped automatically.
//
// Either way, tests exercise actual PostgreSQL — real transactions,
// FOR UPDATE SKIP LOCKED, and constraint enforcement — never a mock.
// Embedded-postgres is justified here specifically because Docker is not
// guaranteed to be available in every environment TaskForge is developed
// or reviewed in (it is not available in the sandbox this project was
// initially built in), and it still yields a real, disposable, per-test-
// binary PostgreSQL instance rather than a developer's permanent database.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
)

const envDatabaseURL = "TASKFORGE_TEST_DATABASE_URL"

var (
	embeddedOnce     sync.Once
	embeddedDSN      string
	embeddedErr      error
	embeddedInstance *embeddedpostgres.EmbeddedPostgres
	embeddedTempDir  string
)

// startEmbedded lazily starts one embedded PostgreSQL instance shared by
// every test in this test binary (starting a fresh server per test would
// be needlessly slow); isolation between individual tests is achieved by
// truncating tables in DB, not by a fresh server per test.
//
// The port AND the runtime/data directories are unique per process
// (rather than embedded-postgres's shared default under
// ~/.embedded-postgres-go/extracted), because `go test ./...` may run
// multiple packages' test binaries as concurrent processes, and two
// embedded-postgres instances sharing a runtime directory would race on
// extracting binaries into it. The downloaded binary archive itself is
// still cached at embedded-postgres's default location and reused across
// runs; run the integration suite with `go test -p 1 ./...` the very
// first time (cold cache) to avoid a concurrent-download race — see
// docs/testing-strategy.md and the project README.
func startEmbedded() (string, error) {
	embeddedOnce.Do(func() {
		port, err := freePort()
		if err != nil {
			embeddedErr = fmt.Errorf("testutil: find free port: %w", err)
			return
		}

		runtimeDir, err := os.MkdirTemp("", "taskforge-embedded-postgres-*")
		if err != nil {
			embeddedErr = fmt.Errorf("testutil: create runtime dir: %w", err)
			return
		}

		pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Port(port).
			RuntimePath(filepath.Join(runtimeDir, "runtime")).
			DataPath(filepath.Join(runtimeDir, "data")).
			Logger(nil))

		if err := pg.Start(); err != nil {
			embeddedErr = fmt.Errorf("testutil: start embedded postgres: %w", err)
			return
		}

		embeddedInstance = pg
		embeddedTempDir = runtimeDir
		embeddedDSN = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	})
	return embeddedDSN, embeddedErr
}

// stopEmbedded stops the embedded PostgreSQL instance started by
// startEmbedded, if one was started. It is a no-op if TASKFORGE_TEST_DATABASE_URL
// was used instead, or if no test in this binary ever called DB.
func stopEmbedded() {
	if embeddedInstance == nil {
		return
	}
	_ = embeddedInstance.Stop()
	if embeddedTempDir != "" {
		_ = os.RemoveAll(embeddedTempDir)
	}
}

// RunMain must be called from a TestMain(m *testing.M) in every package
// that calls DB, so that an embedded PostgreSQL instance started for this
// test binary is explicitly stopped afterward:
//
//	func TestMain(m *testing.M) { testutil.RunMain(m) }
//
// Without this, embedded-postgres's server subprocess detaches from the
// test binary's process and would otherwise be leaked (left running)
// after the test binary exits.
func RunMain(m *testing.M) {
	code := m.Run()
	stopEmbedded()
	os.Exit(code)
}

func freePort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}

// resolveDSN returns the DSN to use for this test binary's PostgreSQL
// instance: TASKFORGE_TEST_DATABASE_URL if set, otherwise a lazily-started
// embedded-postgres instance shared by the whole binary (see
// startEmbedded). Shared by DB and DSN below.
func resolveDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(envDatabaseURL)
	if dsn != "" {
		return dsn
	}
	dsn, err := startEmbedded()
	if err != nil {
		t.Fatalf("testutil: could not obtain a test database: %v", err)
	}
	return dsn
}

// DB returns an open, migrated *sql.DB for integration tests, with the
// jobs table truncated so the test starts from an empty, deterministic
// state (docs/testing-strategy.md: "Tests must be deterministic and
// independently runnable.").
func DB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := resolveDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("testutil: open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("testutil: ping database: %v", err)
	}
	if err := migrate.Up(ctx, db); err != nil {
		t.Fatalf("testutil: run migrations: %v", err)
	}
	// job_attempts and workflow_nodes both reference jobs via a foreign
	// key (migrations 0002 and 0003), and workflow_nodes also references
	// workflow_instances — all four must be truncated in the same
	// statement (or in dependency order) — TRUNCATE jobs alone fails once
	// these constraints exist.
	//
	// Phase 12 adds api_keys to the same statement (nothing references
	// it, but credentials must not leak between tests) and principals
	// afterwards, in dependency order: jobs and workflow_instances both
	// reference principals, so they have to be emptied first.
	if _, err := db.ExecContext(ctx, `TRUNCATE TABLE job_attempts, workflow_nodes, workflow_instances, jobs, api_keys`); err != nil {
		t.Fatalf("testutil: truncate tables: %v", err)
	}
	// The system principal (migration 0005's seed row) is deliberately
	// preserved: it is schema, not test data, and migration 0007's
	// backfill contract depends on it existing. Everything else a
	// previous test created is removed.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM principals WHERE id <> $1`, principal.SystemPrincipalID); err != nil {
		t.Fatalf("testutil: reset principals: %v", err)
	}

	return db
}

// DSN returns a DSN pointing at the same fresh, migrated, empty-tables
// PostgreSQL instance DB provides -- but as a connection string rather
// than an open *sql.DB, for tests that need to open their own native pgx
// connection or pool (e.g. to obtain a real pgx.Tx -- see the txenqueue
// package's integration tests) instead of going through database/sql.
// Migration and truncation are performed via a throwaway *sql.DB that is
// closed before this function returns; the caller owns the lifecycle of
// whatever it opens against the returned DSN.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := resolveDSN(t)
	DB(t) // migrates and truncates; t.Cleanup already closes the *sql.DB this opens
	return dsn
}

// NewPrincipal creates a real principals row and returns its id, for
// tests that need durable, distinct caller identities (Phase 12). kind is
// principal.KindCaller or principal.KindAdmin.
//
// Tests whose subject predates Phase 12 -- the Phase 1-11 suite -- do not
// call this: they attribute their jobs to principal.SystemPrincipalID,
// which migration 0005 always seeds. That is a deliberate choice, not an
// oversight: those tests are about state transitions, leases, retries, and
// workflows, and giving each of them a bespoke principal would add setup
// noise without testing anything Phase 12's own suite does not already
// cover directly.
func NewPrincipal(t *testing.T, db *sql.DB, kind principal.Kind, displayName string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO principals (id, kind, display_name) VALUES ($1, $2, $3)`,
		id, string(kind), displayName)
	if err != nil {
		t.Fatalf("testutil: create principal: %v", err)
	}
	return id
}

// DSNAsRole returns this test binary's PostgreSQL DSN rewritten to connect
// as role/password instead of as the owner the rest of the suite uses.
//
// It exists for the Phase 12 least-privilege role audit
// (internal/migrate/phase12_roles_test.go): asserting privileges with
// has_table_privilege proves what the catalog says, but only an actual
// connection as taskforge_api/taskforge_worker -- running the same
// migrate.Up that cmd/api and cmd/worker run on startup -- proves the
// documented deployment path actually works. That gap is exactly what an
// independent review found: every grant was correct and the process still
// could not start.
//
// It deliberately does NOT migrate or truncate anything: the caller
// already holds an owner-scoped *sql.DB from DB for that. The returned DSN
// points at the same database.
func DSNAsRole(t *testing.T, role, password string) string {
	t.Helper()
	u, err := url.Parse(resolveDSN(t))
	if err != nil {
		t.Fatalf("testutil: parse test DSN: %v", err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}
