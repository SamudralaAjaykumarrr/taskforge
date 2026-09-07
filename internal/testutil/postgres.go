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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
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

// DB returns an open, migrated *sql.DB for integration tests, with the
// jobs table truncated so the test starts from an empty, deterministic
// state (docs/testing-strategy.md: "Tests must be deterministic and
// independently runnable.").
func DB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv(envDatabaseURL)
	if dsn == "" {
		var err error
		dsn, err = startEmbedded()
		if err != nil {
			t.Fatalf("testutil: could not obtain a test database: %v", err)
		}
	}

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
	// job_attempts references jobs via a foreign key, so it must be
	// truncated in the same statement (or first) — TRUNCATE jobs alone
	// fails once that constraint exists (Phase 2, migration 0002).
	if _, err := db.ExecContext(ctx, `TRUNCATE TABLE job_attempts, jobs`); err != nil {
		t.Fatalf("testutil: truncate jobs/job_attempts tables: %v", err)
	}

	return db
}
