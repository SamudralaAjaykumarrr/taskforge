// Package migrate applies the SQL files embedded from migrations/ in
// deterministic, filename order and records which have been applied in a
// schema_migrations table. It intentionally does not depend on any
// third-party migration framework: Phase 1 needs only "apply every
// not-yet-applied *.up.sql file, in order, once" and that is a small
// amount of code to own directly and review.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/SamudralaAjaykumarrr/taskforge/migrations"
)

var files = migrations.Files

const migrationsDir = "."

// advisoryLockKey is an arbitrary, fixed PostgreSQL advisory lock ID used
// to serialize migration runs. cmd/api and cmd/worker each call Up on
// startup, and both may start at the same instant (e.g. `docker compose
// up`) against a brand-new database; without this lock, two concurrent
// `CREATE TABLE IF NOT EXISTS schema_migrations` calls can race inside
// PostgreSQL's own catalog (a well-known PG gotcha: concurrent DDL
// creating the same object can raise "duplicate key value violates
// unique constraint \"pg_type_typname_nsp_index\"" even though each
// statement is individually guarded by IF NOT EXISTS). Session-scoped
// pg_advisory_lock serializes the whole migration run across processes
// without needing a real row to lock.
const advisoryLockKey = 0x7461736b666f7267 // "taskforg" as hex, arbitrary but stable

type migration struct {
	version int64
	name    string
	upSQL   string
}

// execQuerier is satisfied by both *sql.DB and *sql.Conn, letting the
// helpers below run against a single reserved connection (required for
// the session-scoped advisory lock to mean anything) without duplicating
// their bodies.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Up applies every migration whose version is not yet recorded in
// schema_migrations, in ascending version order, each inside its own
// transaction (TF-INV-013: a failed migration leaves the schema exactly as
// it was before that migration started). The whole run is serialized
// across concurrently-starting processes via a session-scoped advisory
// lock (see advisoryLockKey).
func Up(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: reserve connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, int64(advisoryLockKey)); err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(advisoryLockKey))
	}()

	if err := ensureMigrationsTable(ctx, conn); err != nil {
		return fmt.Errorf("migrate: ensure schema_migrations: %w", err)
	}

	all, err := loadMigrations()
	if err != nil {
		return fmt.Errorf("migrate: load migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return fmt.Errorf("migrate: read applied versions: %w", err)
	}

	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", m.name, err)
		}
	}
	return nil
}

func ensureMigrationsTable(ctx context.Context, db execQuerier) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     BIGINT PRIMARY KEY,
			name        TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return err
}

func appliedVersions(ctx context.Context, db execQuerier) (map[int64]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int64]bool)
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func applyOne(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if Commit succeeded

	if _, err := tx.ExecContext(ctx, m.upSQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.version, m.name); err != nil {
		return err
	}
	return tx.Commit()
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(files, migrationsDir)
	if err != nil {
		return nil, err
	}

	var out []migration
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		version, err := parseVersion(name)
		if err != nil {
			return nil, fmt.Errorf("migration file %q: %w", name, err)
		}
		content, err := files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: name, upSQL: string(content)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseVersion(filename string) (int64, error) {
	prefix, _, ok := strings.Cut(filename, "_")
	if !ok {
		return 0, fmt.Errorf("expected NNNN_description.up.sql, got %q", filename)
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected numeric version prefix, got %q: %w", prefix, err)
	}
	return v, nil
}
