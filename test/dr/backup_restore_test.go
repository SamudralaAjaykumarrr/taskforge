// SF-071, SF-072, SF-075 (docs/phase-15-plan.md §8.1, §22;
// docs/scenario-corpus.md): the backup-then-restore drill the roadmap's
// own "Tests / evidence required" names as a literal deliverable -- "at
// least one full, timed backup-then-restore drill, invariants verified by
// the existing checker" -- built as permanent, CI-runnable test code, not
// a one-off manual exercise (docs/phase-15-postgres-evidence.md's own
// prerequisite-evidence pass was explicitly NOT a substitute for this).
package dr_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

// walSegmentName matches a real WAL segment filename (24 hex digits) --
// distinct from a .history file or a N.backup label file, both of which
// also live in the archive directory.
var walSegmentName = regexp.MustCompile(`^[0-9A-Fa-f]{24}$`)

// pgTimestamptzLayout renders an explicit, PostgreSQL-native timestamptz
// literal: space-separated date/time, a numeric UTC offset -- never the
// ISO-8601 "T" date/time separator or "Z" UTC designator time.RFC3339Nano
// produces. This is not a style preference: a real GitHub Actions
// PostgreSQL 16.9 run of this exact test rejected an RFC3339Nano value
// outright --
//
//	LOG:  invalid value for parameter "recovery_target_time": "2026-09-23T19:18:33.417116Z"
//	FATAL:  configuration file ".../postgresql.auto.conf" contains errors
//
// -- even though that same string parses without complaint as an ordinary
// SQL timestamptz literal. recovery_target_time (and PostgreSQL's other
// recovery-target GUCs) are validated very early during startup, before
// the backend's full datetime-parsing machinery is available, and that
// early path does not accept the ISO-8601 form. restore.sh's own usage
// comment and PostgreSQL's own recovery_target_time documentation both
// already show this exact space-separated, numeric-offset form (e.g.
// '2026-09-22 00:00:00+00') -- restore.sh itself was never the bug, only
// this package's own Go-side formatting of the value handed to it.
// ".999999" caps fractional seconds at microseconds, matching
// timestamptz's own storage resolution, so no precision beyond what
// PostgreSQL can actually represent is ever sent.
const pgTimestamptzLayout = "2006-01-02 15:04:05.999999-07:00"

// pgTimestamptzLiteral renders t (normalized to UTC first, so the offset
// is always the explicit, locale-independent "+00:00" -- never a
// local-zone abbreviation) as a PostgreSQL-native timestamptz literal.
// See pgTimestamptzLayout's doc comment for why this, and not
// time.RFC3339Nano, is required for recovery_target_time specifically.
func pgTimestamptzLiteral(t time.Time) string {
	return t.UTC().Format(pgTimestamptzLayout)
}

// recoveryTargetTimeLine matches a written "recovery_target_time = '...'"
// line in postgresql.auto.conf, capturing its literal value.
var recoveryTargetTimeLine = regexp.MustCompile(`(?m)^recovery_target_time = '([^']*)'$`)

// requireValidRecoveryTargetTimeConf reads restoreDataDir's own
// postgresql.auto.conf (written by restore.sh) directly off disk and
// asserts its recovery_target_time value is exactly the explicit
// PostgreSQL-timestamptz literal this package writes -- never inferring
// "the config was fine" merely from whether PostgreSQL later managed to
// start (see pgTimestamptzLiteral's doc comment for why that inference
// was wrong before).
func requireValidRecoveryTargetTimeConf(t *testing.T, restoreDataDir string, want time.Time) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(restoreDataDir, "postgresql.auto.conf"))
	require.NoError(t, err)
	m := recoveryTargetTimeLine.FindSubmatch(b)
	require.NotNil(t, m, "postgresql.auto.conf has no recovery_target_time line:\n%s", b)
	got := string(m[1])
	require.NotContains(t, got, "T", "recovery_target_time must not use the ISO-8601 'T' date/time separator -- PostgreSQL's recovery_target_time GUC rejects it: %q", got)
	require.NotContains(t, got, "Z", "recovery_target_time must not use the ISO-8601 'Z' UTC designator -- PostgreSQL's recovery_target_time GUC rejects it: %q", got)
	require.Equal(t, pgTimestamptzLiteral(want), got, "recovery_target_time in postgresql.auto.conf")
}

// TestDR_SF071_SF075_BackupRestoreDrill_PITR is SF-071 and SF-075: a real
// pg_basebackup taken mid-workload, continued workload generating WAL the
// backup itself does not cover, a recovery_target_time recorded from
// PostgreSQL's own clock, further post-target activity that must NOT
// survive the restore, and a real PITR restore via deploy/pg-dr/restore.sh
// -- timed, then verified with internal/invariant.Checker.CheckAll
// against the restored database directly (the roadmap's own literal
// proof obligation, docs/phase-15-plan.md §2.4/§11).
//
// SF-075 rides on the same mechanism (docs/phase-15-plan.md §22's own
// framing: "Restore a database (SF-071's mechanism) taken after at least
// one lease-generation advance"): driveReferenceWorkload deliberately
// forces one lease-expiry reclaim before the backup is taken, so the
// restored database's own job_attempts.lease_generation sequence for
// that job must be monotonic and intact -- proving a restore does not
// itself introduce a fencing regression.
func TestDR_SF071_SF075_BackupRestoreDrill_PITR(t *testing.T) {
	ctx := context.Background()
	archiveDir := t.TempDir()

	primary := startPrimary(t, archiveDir)
	db := primary.db(t)
	require.NoError(t, migrate.Up(ctx, db))
	st := store.New(db)

	// Phase 1: workload before the backup (docs/phase-15-plan.md §8.1
	// step 2's reference volume is Phase 9's own documented soak-run
	// scale, 150 jobs/20 workers -- split across phases 1-3 below so the
	// backup genuinely lands mid-workload, not at a clean boundary).
	phase1IDs := driveReferenceWorkload(t, ctx, db, st, 90, 20)

	sysBaseBackupBin := findSystemPGBaseBackup(t)

	backupDir := t.TempDir() + "/backup"
	out, err := runScript(t, "backup.sh", []string{
		"PGPORT=" + fmt.Sprint(primary.port),
		"PGHOST=127.0.0.1",
		"PGUSER=postgres",
		"PGPASSWORD=postgres",
		"PG_BIN=" + primary.binDir,
		"PG_BASEBACKUP_BIN=" + sysBaseBackupBin,
	}, backupDir)
	require.NoError(t, err, "backup.sh output:\n%s", out)
	t.Logf("SF-071: base backup taken mid-workload (%d jobs already inserted); backup.sh output:\n%s", len(phase1IDs), out)

	baselineArchiveCount := archiveEntryCount(t, archiveDir)

	// Phase 2: workload after the backup -- this is what makes it a
	// genuine PITR test, not merely "restore the backup" (docs/phase-15-plan.md
	// §8.1 step 4). Includes the SF-075 reclaim precondition (inside
	// driveReferenceWorkload).
	phase2IDs := driveReferenceWorkload(t, ctx, db, st, 50, 20)

	// recovery_target_time is read from PostgreSQL's own clock (Clock
	// Model), never the test host's -- docs/phase-15-plan.md §8.1 step 5.
	targetTime := pgNow(t, db)
	time.Sleep(50 * time.Millisecond) // ensure phase 3 strictly postdates targetTime

	// Phase 3: activity strictly AFTER the recorded target -- must NOT
	// appear in the restored database, proving this is a real
	// point-in-time restore and not "replay everything in the archive."
	phase3IDs := driveReferenceWorkload(t, ctx, db, st, 10, 5)

	// Force a WAL switch and wait for the segment(s) covering the target
	// to actually land in the archive (archive_command execution is
	// asynchronous -- docs/phase-15-postgres-evidence.md §3.3's own
	// finding).
	var switchLSN string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_switch_wal()::text`).Scan(&switchLSN))
	waitForArchiveSegment(t, archiveDir, baselineArchiveCount, 15*time.Second)

	restoreDir := t.TempDir() + "/restored"
	restoreCommand := fmt.Sprintf("cp %s/%%f %%p", archiveDir)

	restoreStart := time.Now()
	out, err = runScript(t, "restore.sh", []string{
		"RECOVERY_TARGET_TIME=" + pgTimestamptzLiteral(targetTime),
		"RESTORE_COMMAND=" + restoreCommand,
		"START=0", // this test starts the restored instance itself, via the same embedded-postgres library the primary used (docs/phase-15-postgres-evidence.md §3.1 item 5)
	}, backupDir, restoreDir)
	require.NoError(t, err, "restore.sh (prepare-only) output:\n%s", out)
	requireValidRecoveryTargetTimeConf(t, restoreDir, targetTime)

	restoredPort := freePort(t)
	restored := startFromDataDir(t, restoreDir, restoredPort)
	require.NotNil(t, restored, "restored instance failed to start")
	restoredDB := restored.db(t)

	waitUntil(t, 30*time.Second, 100*time.Millisecond,
		"restored instance never reached recovery_target_time and promoted to read/write", func() bool {
			var inRecovery bool
			if err := restoredDB.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
				return false
			}
			return !inRecovery
		})
	restoreElapsed := time.Since(restoreStart)

	// The roadmap's own literal proof obligation (docs/phase-15-plan.md
	// §2.4/§11): every existing TF-INV-* invariant, checked against the
	// RESTORED database directly.
	violations, err := invariant.New(restoredDB).CheckAll(ctx)
	require.NoError(t, err, "invariant checker itself failed against restored database")
	require.Empty(t, violations, "invariant violations found against restored database: %v", violations)

	// Every job that existed at or before the recorded target time must
	// still exist; every job inserted strictly after it must not.
	for _, id := range phase1IDs {
		requireJobExists(t, ctx, restoredDB, id, true)
	}
	for _, id := range phase2IDs {
		requireJobExists(t, ctx, restoredDB, id, true)
	}
	for _, id := range phase3IDs {
		requireJobExists(t, ctx, restoredDB, id, false)
	}

	// SF-075: prove the reclaim precondition actually survived the
	// restore as real, monotonic fencing history -- not merely that the
	// checker trivially passed because no job had more than one attempt.
	var maxLeaseGeneration int64
	require.NoError(t, restoredDB.QueryRowContext(ctx, `SELECT COALESCE(max(lease_generation), 0) FROM job_attempts`).Scan(&maxLeaseGeneration))
	require.GreaterOrEqual(t, maxLeaseGeneration, int64(2),
		"SF-075 precondition not represented in the restored database: no job_attempts row shows a lease_generation advance from a pre-backup reclaim")

	t.Logf("SF-071 MEASURED RESTORE TIME (backup+WAL replay, restore.sh invocation to promoted read/write): %s", restoreElapsed)
	t.Logf("SF-071 evidence: %d pre-backup jobs, %d post-backup/pre-target jobs, %d post-target jobs correctly excluded, switch LSN %s", len(phase1IDs), len(phase2IDs), len(phase3IDs), switchLSN)
}

// TestDR_SF072_WALArchiveGap_RecoveryFailsLoudly is SF-072: a WAL segment
// the restore genuinely needs (produced after the base backup, holding
// the transaction nearest the recovery target) is deliberately deleted
// from the archive before restore.sh is invoked. Per docs/phase-15-plan.md
// §2.5/§11, PostgreSQL's own recovery process must fail loudly and
// explicitly at that point -- never silently skip forward past the gap,
// never silently stop early without saying so.
func TestDR_SF072_WALArchiveGap_RecoveryFailsLoudly(t *testing.T) {
	ctx := context.Background()
	archiveDir := t.TempDir()

	primary := startPrimary(t, archiveDir)
	db := primary.db(t)
	require.NoError(t, migrate.Up(ctx, db))
	st := store.New(db)

	driveReferenceWorkload(t, ctx, db, st, 30, 10)

	sysBaseBackupBin := findSystemPGBaseBackup(t)

	backupDir := t.TempDir() + "/backup"
	out, err := runScript(t, "backup.sh", []string{
		"PGPORT=" + fmt.Sprint(primary.port),
		"PGHOST=127.0.0.1",
		"PGUSER=postgres",
		"PGPASSWORD=postgres",
		"PG_BIN=" + primary.binDir,
		"PG_BASEBACKUP_BIN=" + sysBaseBackupBin,
	}, backupDir)
	require.NoError(t, err, "backup.sh output:\n%s", out)

	// backupBoundarySegment is PostgreSQL's OWN authoritative WAL segment
	// name (pg_walfile_name(pg_current_wal_lsn()), queried immediately
	// after backup.sh returns, before any further workload runs) for
	// "everything at or before this point is already covered by the base
	// backup's own streamed WAL" (pg_basebackup -X stream, see backup.sh's
	// comment) -- deleting an archived copy of a segment at or before this
	// boundary would not reproduce a genuine gap, since recovery would
	// simply use the copy already inside the base backup's own pg_wal
	// directory and never touch the archive for it at all.
	//
	// This intentionally replaces an earlier, racy design that instead
	// snapshotted len(archivedSegments(archiveDir)) right after backup.sh
	// returned and later picked "whichever segment now sits at that same
	// index." archive_command runs asynchronously (docs/phase-15-postgres-evidence.md
	// §3.3): a segment already fully covered by the backup can still be
	// mid-flight to archiveDir at the exact instant of that count
	// snapshot, and land moments later -- during the very polling window
	// below -- miscounted as "new." Under go test -race's much slower
	// scheduling this window widens enough to intermittently select that
	// already-covered segment instead of a genuinely-required one,
	// deleting it produces no gap at all, and restore.sh spuriously
	// succeeds. Comparing against PostgreSQL's own authoritative WAL
	// segment name, rather than two directory-listing snapshots taken at
	// different times, is immune to that archiver lag by construction.
	backupBoundarySegment := currentWALSegment(t, ctx, db)

	driveReferenceWorkload(t, ctx, db, st, 20, 10)

	// Force a fresh WAL segment strictly after everything above, commit
	// one more, minimal, definite write into that NEW segment BEFORE
	// recording recovery_target_time, then force a SECOND switch to
	// complete that exact segment (the one holding the probe write) so it
	// becomes archivable. Two switches are both required, for two
	// separate reasons found by direct, isolated reproduction against a
	// real PostgreSQL 16 server (not merely reasoned about):
	//
	// (1) ORDERING, why the target must be recorded AFTER the probe, not
	// before it: an earlier version of this test called pg_switch_wal()
	// AFTER targetTime was already recorded, then picked "whichever
	// segment that switch produced" as the gap segment. That does not
	// guarantee recovery ever needs it -- if nothing with a commit time
	// at-or-before the target ever lands in the switched-to segment (the
	// common case, since driveReferenceWorkload's own last commit -- the
	// record nearest the target -- already landed in the PRIOR segment
	// before the switch), PostgreSQL's recovery determines it has reached
	// recovery_target_time using only that prior segment's own records
	// and never opens the switched-to segment via restore_command at
	// all. Switching first, writing the probe, and only then recording
	// the target guarantees a real commit record dated at-or-before the
	// target lives inside the segment about to be deleted.
	//
	// (2) WHY A SECOND SWITCH: pg_switch_wal() completes and archives
	// whichever segment was ACTIVE BEFORE the call, not the new one it
	// creates. A single switch (called before the probe) starts a fresh
	// segment for the probe to land in, but that segment itself is never
	// forced to complete -- archive_command only ever runs against it
	// once it either fills to a full 16 MiB (never, for one small probe
	// row) or is itself closed by a LATER switch. Without that second
	// switch, gapSegment sits open indefinitely, archive_command is never
	// invoked for it, and the polling wait below times out. The FIRST
	// version of this fix only switched once and relied on incidental
	// background WAL activity (checkpointer/autovacuum) to coincidentally
	// nudge the probe's segment to completion -- which is exactly why it
	// still failed intermittently.
	var switchLSN1 string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_switch_wal()::text`).Scan(&switchLSN1))

	probeJob, err := st.Insert(ctx, job.NewParams{
		PrincipalID:             principal.SystemPrincipalID,
		JobType:                 "dr.probe",
		Payload:                 []byte(`{}`),
		MaxAttempts:             3,
		ExecutionTimeoutSeconds: 30,
		QueueName:               "default",
	})
	require.NoError(t, err)

	// gapSegment is read AFTER the probe commit, deliberately, not
	// immediately after the first switch. Also confirmed by direct,
	// isolated reproduction: pg_walfile_name() applied to an LSN sitting
	// exactly on a fresh segment boundary -- which is exactly what
	// pg_current_wal_lsn() can report immediately after pg_switch_wal(),
	// before any further bytes are written -- resolves to the segment
	// that just ENDED, not the one starting at that boundary (PostgreSQL's
	// own XLByteToPrevSeg convention: nothing has actually been inserted
	// past the boundary yet, so it isn't attributed to the new segment).
	// Reading the current segment only after the probe's own insert has
	// actually written real content past that boundary is what makes this
	// unambiguous.
	gapSegment := currentWALSegment(t, ctx, db)
	require.Greater(t, gapSegment, backupBoundarySegment,
		"the segment containing the probe's own write must be strictly after the base backup's own boundary -- "+
			"otherwise it would already be covered by the backup's own included WAL and deleting it would not reproduce a genuine gap")
	t.Logf("SF-072: probe job %s inserted into the post-switch segment %s, guaranteeing recovery must open it to reach the target", probeJob.ID, gapSegment)

	targetTime := pgNow(t, db)

	// The second switch: forces gapSegment itself (still the current,
	// active segment at this point -- nothing since the probe has written
	// enough to roll over on its own) to complete, making it eligible for
	// archive_command.
	var switchLSN2 string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_switch_wal()::text`).Scan(&switchLSN2))
	time.Sleep(50 * time.Millisecond)

	// archive_command still runs asynchronously (docs/phase-15-postgres-evidence.md
	// §3.3) -- poll until this SPECIFIC, already-known-required segment
	// (not "whichever one shows up next") actually appears in the
	// archive before deleting it.
	waitUntil(t, 15*time.Second, 200*time.Millisecond,
		"the WAL segment recovery actually needs ("+gapSegment+") never appeared in the archive", func() bool {
			for _, s := range archivedSegments(t, archiveDir) {
				if s == gapSegment {
					return true
				}
			}
			return false
		})

	// Reproduces "an operator's archive_command/storage target lost a
	// segment" (docs/phase-15-plan.md §24).
	require.NoError(t, os.Remove(archiveDir+"/"+gapSegment))
	t.Logf("SF-072: deliberately deleted archived WAL segment %s (the segment recovery must open to reach recovery_target_time) before restore (base backup's own boundary segment: %s)",
		gapSegment, backupBoundarySegment)

	restoreDir := t.TempDir() + "/restored"
	restoreCommand := fmt.Sprintf("cp %s/%%f %%p", archiveDir)
	restorePort := freePort(t)

	out, err = runScript(t, "restore.sh", []string{
		"RECOVERY_TARGET_TIME=" + pgTimestamptzLiteral(targetTime),
		"RESTORE_COMMAND=" + restoreCommand,
		"START=1",
		"PGPORT=" + fmt.Sprint(restorePort),
		"PG_BIN=" + primary.binDir,
	}, backupDir, restoreDir)
	// Verify the config restore.sh actually wrote BEFORE trusting that any
	// startup failure below is the intended missing-WAL recovery failure
	// (SF-072's whole point) rather than a config-parse failure that never
	// even reached recovery -- see requireValidRecoveryTargetTimeConf's
	// doc comment.
	requireValidRecoveryTargetTimeConf(t, restoreDir, targetTime)

	// restore.sh's own exit code (via pg_ctl start -w) is NOT used as the
	// primary pass/fail signal here -- it is logged only as a diagnostic.
	// Direct, isolated reproduction against a real PostgreSQL 16 server
	// found that pg_ctl start -w can report SUCCESS even when recovery is
	// already doomed: this project's own primary config sets
	// hot_standby = on (startPrimary), which the base backup copies
	// verbatim into the restored data directory's own postgresql.conf.
	// Under hot_standby, the restored instance accepts read-only
	// connections as soon as it reaches "consistent recovery state" from
	// the base backup's OWN bundled WAL alone -- which happens BEFORE
	// recovery has even attempted to fetch the archived segment this
	// scenario deleted. pg_ctl start -w's "wait for ready" condition is
	// satisfied by that early signal, so restore.sh can return exit 0
	// while the separate startup process is still moments away from its
	// own FATAL. In every one of several isolated, direct reproductions
	// of this exact race, the startup process's FATAL followed within
	// 1-2 seconds, without exception -- PostgreSQL's own recovery
	// enforcement is correct and immediate; only the exit-code-based
	// observation of it was stale. Polling the server's own log content
	// for the expected FATAL -- real PostgreSQL behavior, not a mock --
	// is therefore the authoritative signal, not restore.sh's own exit
	// code.
	t.Logf("SF-072: restore.sh returned error=%v (informational only -- see the log-polling assertion below for the authoritative outcome); output:\n%s", err, out)

	logPath := restoreDir + ".log"
	var logContent string
	waitUntil(t, 10*time.Second, 200*time.Millisecond,
		"the restored instance's own log never reported the expected loud recovery failure", func() bool {
			b, readErr := os.ReadFile(logPath)
			if readErr != nil {
				return false
			}
			logContent = string(b)
			return strings.Contains(logContent, "recovery ended before configured recovery target was reached")
		})
	require.Contains(t, logContent, "No such file or directory",
		"the log must name the missing WAL segment restore_command could not find, for an operator to diagnose")

	// Confirm the instance is not left durably promoted and reachable --
	// it must have shut itself down as part of the FATAL (per the log
	// lines this project's own reproduction always observed:
	// "startup process ... exited with exit code 1" /
	// "database system is shut down"), not merely logged the FATAL while
	// somehow continuing to serve.
	waitUntil(t, 10*time.Second, 200*time.Millisecond,
		"the restored instance never actually shut down after its own FATAL", func() bool {
			db, dialErr := sql.Open("pgx", dsnFor(restorePort))
			if dialErr != nil {
				return true
			}
			defer db.Close()
			return db.PingContext(ctx) != nil
		})

	t.Logf("SF-072 evidence: postgres log confirms explicit, loud recovery failure at the missing segment (not a silent skip-forward), and the restored instance shut itself down rather than remaining reachable")
}

// currentWALSegment returns the WAL segment filename containing
// PostgreSQL's own current WAL insert position (pg_walfile_name(pg_current_wal_lsn()))
// -- the same authoritative segment-naming PostgreSQL's own archiver and
// restore_command already use, so a caller comparing against it (SF-072)
// is never subject to any directory-listing timing race against the
// asynchronous archiver process.
func currentWALSegment(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var name string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_walfile_name(pg_current_wal_lsn())`).Scan(&name))
	return name
}

// archiveEntryCount returns how many files currently exist in dir -- used
// as a "before" baseline so waitForArchiveSegment can detect genuinely
// NEW archive activity rather than files an earlier phase already left
// behind.
func archiveEntryCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return len(entries)
}

// archivedSegments returns every real WAL segment filename (24 hex digits
// -- excluding .history and .backup label files) currently in dir,
// sorted. WAL segment filenames sort lexicographically in LSN order, so
// this is also chronological archive order.
func archivedSegments(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	var segments []string
	for _, e := range entries {
		if walSegmentName.MatchString(e.Name()) {
			segments = append(segments, e.Name())
		}
	}
	sort.Strings(segments)
	return segments
}

// requireJobExists asserts a job's presence (or deliberate absence) in db
// by direct SQL.
func requireJobExists(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID, wantExists bool) {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&count))
	if wantExists {
		require.Equal(t, 1, count, "job %s should exist in the restored database (inserted at or before recovery_target_time)", id)
	} else {
		require.Equal(t, 0, count, "job %s should NOT exist in the restored database (inserted strictly after recovery_target_time)", id)
	}
}
