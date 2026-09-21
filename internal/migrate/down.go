package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DownMigrationStatus classifies whether a migration's .down.sql file is
// safe to exercise in CI's up->down->up reversibility proof, per ADR-0010
// (docs/adr/0010-expand-migrate-contract.md) and docs/phase-14-plan.md §8.
// This formally replaces any universal "every migration needs a tested
// down migration" rule: only the data-safe-reversible subset is required
// to actually work, and CI enforces that every migration file carries one
// of these two labels -- never neither.
type DownMigrationStatus string

const (
	// StatusDataSafeReversible means reversing the migration cannot
	// destroy information already written under the new shape. CI runs
	// this migration's down-then-up-again cycle. This also covers
	// migration 0010's "honest, not blanket-safe" case: its down SQL may
	// legitimately fail at runtime if divergent tenant data now exists
	// (docs/adr/0010-expand-migrate-contract.md), but CI still exercises
	// it -- the label governs whether CI *runs* the down path, not
	// whether that down path is guaranteed to succeed against arbitrary
	// production data.
	StatusDataSafeReversible DownMigrationStatus = "data-safe-reversible"
	// StatusForwardFixOnly means the migration is not genuinely safe to
	// reverse (e.g. it drops a table or column that real data may depend
	// on). Its down file, where one exists at all, is exempted from CI's
	// up->down->up cycle; remediation is a new, later forward-fix
	// migration, never a down migration pretending to honestly undo it.
	StatusForwardFixOnly DownMigrationStatus = "forward-fix-only"
)

// downMarkerPrefix is the machine-checkable marker every .down.sql file
// must carry as its first line (docs/phase-14-plan.md §8). Existing prose
// reversibility comments stay, unchanged, below it.
const downMarkerPrefix = "-- taskforge:down-migration-status: "

// parseDownStatus extracts and validates the marker from a .down.sql
// file's contents. A file with no marker line at all, or an unrecognized
// value, is an error -- this is what makes "every migration is explicitly
// labeled" (the roadmap's own exit criterion) enforced rather than merely
// documented: there is no silent default.
func parseDownStatus(downSQL string) (DownMigrationStatus, error) {
	firstLine, _, _ := strings.Cut(downSQL, "\n")
	firstLine = strings.TrimSpace(firstLine)
	if !strings.HasPrefix(firstLine, downMarkerPrefix) {
		return "", fmt.Errorf("missing %q marker as the first line", strings.TrimSpace(downMarkerPrefix))
	}
	value := DownMigrationStatus(strings.TrimSpace(strings.TrimPrefix(firstLine, downMarkerPrefix)))
	switch value {
	case StatusDataSafeReversible, StatusForwardFixOnly:
		return value, nil
	default:
		return "", fmt.Errorf("unrecognized down-migration-status %q (must be %q or %q)",
			value, StatusDataSafeReversible, StatusForwardFixOnly)
	}
}

// MigrationInfo describes one migration for tooling that needs more than
// Up/UpTo/Down's own entry points -- the CI migration-reversibility job
// and its equivalent test coverage (docs/phase-14-plan.md §8, SF-060/061).
type MigrationInfo struct {
	Version    int64
	Name       string
	DownName   string
	DownStatus DownMigrationStatus
}

// Migrations returns every migration in ascending version order, each
// classified by its .down.sql marker. It fails if any migration's down
// file is missing the marker entirely or carries an unrecognized value --
// see parseDownStatus.
func Migrations() ([]MigrationInfo, error) {
	all, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	out := make([]MigrationInfo, len(all))
	for i, m := range all {
		out[i] = MigrationInfo{
			Version:    m.version,
			Name:       m.name,
			DownName:   m.downName,
			DownStatus: m.downStatus,
		}
	}
	return out, nil
}

// Down runs exactly one migration's .down.sql file inside a single
// transaction and removes that version's row from schema_migrations on
// success -- the symmetric counterpart to applyOne's handling of .up.sql
// files. It is new, additive surface (docs/phase-14-plan.md §8): neither
// cmd/api nor cmd/worker calls it, so its mere existence cannot cause a
// running server to roll back its own schema. It exists for the CI
// migration-reversibility job and its test coverage, and for a deliberate,
// manual operator rollback of a specific data-safe-reversible migration --
// never as an automated production entry point.
//
// Down does not check the migration's DownMigrationStatus label itself --
// callers that must respect the data-safe-reversible/forward-fix-only
// split (the CI job, in particular) check Migrations()'s classification
// before calling Down, exactly as ADR-0010 requires. Down running a
// forward-fix-only migration's down SQL on request is not itself unsafe;
// it is the caller's job to decide whether that is the right thing to do.
func Down(ctx context.Context, db *sql.DB, version int64) error {
	all, err := loadMigrations()
	if err != nil {
		return fmt.Errorf("migrate: load migrations: %w", err)
	}

	var target *migration
	for i := range all {
		if all[i].version == version {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("migrate: no migration with version %d", version)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: reserve connection: %w", err)
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: begin down transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if Commit succeeded

	if _, err := tx.ExecContext(ctx, target.downSQL); err != nil {
		return fmt.Errorf("migrate: apply down %s: %w", target.downName, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE version = $1`, target.version); err != nil {
		return fmt.Errorf("migrate: remove schema_migrations row for version %d: %w", target.version, err)
	}
	return tx.Commit()
}
