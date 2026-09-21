// Phase 14 proofs for ADR-0010 / docs/phase-14-plan.md §8: the
// machine-checkable down-migration-status marker every .down.sql file must
// carry (SF-061), and the up->down->up-again reversibility cycle for the
// data-safe-reversible subset (SF-060). These are ordinary Go tests, run by
// `go test ./...` exactly like every other proof in this repository -- this
// IS the CI enforcement the roadmap requires, not a separate script: a
// migration file with a missing or unrecognized marker fails this suite,
// and a data-safe-reversible migration whose down-then-up-again cycle does
// not reproduce an identical schema fails it too.
package migrate_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestMigrations_SF061_EveryFileCarriesAValidDownMigrationStatusMarker is
// SF-061 (docs/phase-14-plan.md §16): migrate.Migrations() itself returns
// an error if any migration's .down.sql is missing the
// "taskforge:down-migration-status" marker or carries an unrecognized
// value (see parseDownStatus) -- so simply calling it successfully IS the
// proof that every file is labeled. This test additionally pins the exact,
// reviewed classification of all fourteen migrations that exist today, so
// a future migration's misclassification (or a silent change to an
// existing one) is caught by a diff to this list, not merely by the parser
// accepting whatever value happens to be present.
func TestMigrations_SF061_EveryFileCarriesAValidDownMigrationStatusMarker(t *testing.T) {
	infos, err := migrate.Migrations()
	require.NoError(t, err, "every migration file must carry a valid taskforge:down-migration-status marker")
	require.Len(t, infos, 14)

	want := map[int64]migrate.DownMigrationStatus{
		1:  migrate.StatusForwardFixOnly,     // create jobs table
		2:  migrate.StatusForwardFixOnly,     // create job_attempts table
		3:  migrate.StatusForwardFixOnly,     // create workflow tables
		4:  migrate.StatusDataSafeReversible, // additive terminal_attempt_count column
		5:  migrate.StatusForwardFixOnly,     // create principals/api_keys tables
		6:  migrate.StatusDataSafeReversible,
		7:  migrate.StatusDataSafeReversible,
		8:  migrate.StatusDataSafeReversible,
		9:  migrate.StatusDataSafeReversible,
		10: migrate.StatusDataSafeReversible, // honest, can legitimately fail -- still exercised
		11: migrate.StatusDataSafeReversible,
		12: migrate.StatusDataSafeReversible,
		13: migrate.StatusDataSafeReversible,
		14: migrate.StatusDataSafeReversible,
	}
	for _, info := range infos {
		wantStatus, ok := want[info.Version]
		require.True(t, ok, "migration version %d (%s) is not accounted for in this test's expected classification", info.Version, info.Name)
		require.Equal(t, wantStatus, info.DownStatus, "migration %s: unexpected down-migration-status classification", info.Name)
	}
}

// TestMigrations_SF060_DataSafeReversibleSubsetRoundTripsCleanly is SF-060
// (docs/phase-14-plan.md §16, ADR-0010): for every migration labeled
// data-safe-reversible, walking forward one migration at a time via UpTo,
// running Down then UpTo again for that migration must reproduce a byte-
// identical schema -- proving the down path genuinely round-trips rather
// than merely "not erroring." forward-fix-only migrations (1, 2, 3, 5) are
// deliberately never exercised this way: their whole point, per ADR-0010,
// is that no down-then-up-again promise is made for them.
//
// dropPhase12Schema (migrate_test.go) rewinds the already-fully-migrated
// database from testutil.DB to exactly the pre-migration-5 state (versions
// 1-4 applied, 5-14 unrecorded and reverted) -- precisely the "freshly
// migrated to that point" starting position this test needs, without
// duplicating that rewind logic.
func TestMigrations_SF060_DataSafeReversibleSubsetRoundTripsCleanly(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	dropPhase12Schema(t, db)

	infos, err := migrate.Migrations()
	require.NoError(t, err)
	sort.Slice(infos, func(i, j int) bool { return infos[i].Version < infos[j].Version })

	for _, m := range infos {
		require.NoError(t, migrate.UpTo(ctx, db, m.Version), "advancing to version %d (%s)", m.Version, m.Name)

		if m.DownStatus != migrate.StatusDataSafeReversible {
			continue
		}

		before := schemaSnapshot(t, db)
		require.NoError(t, migrate.Down(ctx, db, m.Version), "down migration %d (%s) must succeed against a database with no divergent data", m.Version, m.Name)
		require.NoError(t, migrate.UpTo(ctx, db, m.Version), "re-applying migration %d (%s) after Down must succeed", m.Version, m.Name)
		after := schemaSnapshot(t, db)

		require.Equal(t, before, after,
			"migration %s: down-then-up-again must reproduce an identical schema (ADR-0010's data-safe-reversible promise)", m.Name)
	}

	// Leave the shared test database fully migrated for whatever test in
	// this binary runs next -- the loop above already reaches version 14
	// as its last iteration, so this is a no-op confirmation, not
	// additional migration work.
	require.NoError(t, migrate.Up(ctx, db))
}

// schemaSnapshot captures every publicly visible schema object this
// project's migrations create -- columns, indexes, and constraints -- as a
// single deterministic string, so two snapshots can be compared with a
// plain string equality assertion. schema_migrations itself is excluded
// deliberately: Down/UpTo's bookkeeping rows are the mechanism under test,
// not part of the schema a migration's down path promises to reproduce.
func schemaSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder

	writeRows := func(label, query string, cols int) {
		rows, err := db.QueryContext(ctx, query)
		require.NoError(t, err, label)
		defer rows.Close()
		vals := make([]any, cols)
		ptrs := make([]any, cols)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		fmt.Fprintf(&b, "== %s ==\n", label)
		for rows.Next() {
			require.NoError(t, rows.Scan(ptrs...))
			fmt.Fprintln(&b, vals...)
		}
		require.NoError(t, rows.Err())
	}

	writeRows("columns", `
		SELECT table_name, column_name, data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		ORDER BY table_name, column_name`, 5)

	writeRows("indexes", `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = 'public'
		ORDER BY indexname`, 2)

	writeRows("constraints", `
		SELECT conname, contype, conrelid::regclass::text
		FROM pg_constraint
		WHERE connamespace = 'public'::regnamespace
		ORDER BY conname`, 3)

	writeRows("tables", `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		ORDER BY table_name`, 1)

	return b.String()
}
