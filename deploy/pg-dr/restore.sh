#!/usr/bin/env bash
#
# TaskForge Phase 15 (docs/phase-15-plan.md §11, docs/disaster-recovery.md):
# prepare (and, by default, start) a point-in-time restore from a
# pg_basebackup-taken base backup plus an archived WAL stream.
#
# Mechanism: copy the base backup into a fresh data directory, write
# recovery.signal (the PostgreSQL 12+ mechanism -- recovery.conf no
# longer exists as of PostgreSQL 12) plus restore_command/
# recovery_target_time/recovery_target_action into postgresql.auto.conf,
# then start PostgreSQL. It replays archived WAL up to the target time
# and, per recovery_target_action=promote (this script's default),
# becomes an ordinary read/write server automatically once the target is
# reached -- no separate manual promotion step.
#
# Documented failure mode (docs/phase-15-plan.md §2.5/§11): if
# recovery_target_time requires a WAL segment missing from the archive,
# PostgreSQL's recovery process fails with an explicit error at that
# point. It does not silently skip forward, and does not silently stop
# earlier than requested without saying so. That is PostgreSQL's own
# correct, documented behavior -- this script does not, and must not,
# work around it.
#
# This is operator-invoked tooling (docs/phase-15-plan.md §5/§15), and
# the same script test/dr/backup_restore_test.go invokes (with START=0,
# since the test harness starts the restored instance itself through the
# same embedded-postgres library the primary was started through, per
# docs/phase-15-postgres-evidence.md §3.1 item 5's verified
# pre-populated-DataPath reuse) -- runbook and tested behavior can never
# silently diverge.
#
# Usage:
#   RECOVERY_TARGET_TIME='2026-09-22 00:00:00+00' \
#   RESTORE_COMMAND='cp /path/to/wal-archive/%f %p' \
#   [RECOVERY_TARGET_ACTION=promote] \
#   [START=1] [PGPORT=5433] [PG_BIN=/path/to/postgres/bin] \
#     ./restore.sh <base-backup-dir> <restore-data-dir>
#
# <restore-data-dir> must not already exist. Set START=0 to only prepare
# the directory (recovery.signal + postgresql.auto.conf written) without
# starting PostgreSQL -- the caller then starts it however it needs to
# (e.g. test/dr's own embedded-postgres.Start() call).

set -euo pipefail

BASE_BACKUP_DIR="${1:?usage: restore.sh <base-backup-dir> <restore-data-dir>}"
RESTORE_DATA_DIR="${2:?usage: restore.sh <base-backup-dir> <restore-data-dir>}"
: "${RECOVERY_TARGET_TIME:?RECOVERY_TARGET_TIME must be set (a PostgreSQL timestamp, e.g. from SELECT now())}"
: "${RESTORE_COMMAND:?RESTORE_COMMAND must be set to a shell command template with %f/%p, e.g. cp /archive/%f %p}"
RECOVERY_TARGET_ACTION="${RECOVERY_TARGET_ACTION:-promote}"
START="${START:-1}"
PG_CTL="${PG_BIN:+${PG_BIN}/}pg_ctl"

if [ ! -d "$BASE_BACKUP_DIR" ]; then
	echo "restore.sh: base backup dir $BASE_BACKUP_DIR does not exist" >&2
	exit 1
fi
if [ -e "$RESTORE_DATA_DIR" ]; then
	echo "restore.sh: $RESTORE_DATA_DIR already exists -- refusing to overwrite" >&2
	exit 1
fi
mkdir -p "$(dirname "$RESTORE_DATA_DIR")"

echo "restore.sh: copying base backup $BASE_BACKUP_DIR -> $RESTORE_DATA_DIR"
cp -a "$BASE_BACKUP_DIR" "$RESTORE_DATA_DIR"

# A base backup taken while the source was running can carry a stale
# postmaster.pid; PostgreSQL refuses to start with one present (correctly
# -- it exists to prevent two postmasters sharing a data directory) so it
# must be removed here, on the COPY, never on the live source.
rm -f "$RESTORE_DATA_DIR/postmaster.pid"

touch "$RESTORE_DATA_DIR/recovery.signal"
{
	echo "restore_command = '${RESTORE_COMMAND}'"
	echo "recovery_target_time = '${RECOVERY_TARGET_TIME}'"
	echo "recovery_target_action = '${RECOVERY_TARGET_ACTION}'"
} >>"$RESTORE_DATA_DIR/postgresql.auto.conf"

echo "restore.sh: prepared $RESTORE_DATA_DIR (recovery_target_time='$RECOVERY_TARGET_TIME', recovery_target_action=$RECOVERY_TARGET_ACTION)"

if [ "$START" = "1" ]; then
	: "${PGPORT:?PGPORT must be set to start the restored instance (or set START=0 to skip starting)}"
	echo "restore.sh: starting restored instance on port $PGPORT"
	# -l <logfile> is required, not cosmetic: without it, pg_ctl's spawned
	# postgres daemon inherits this script's own stdout/stderr file
	# descriptors, and a caller piping this script's output (or a Go
	# os/exec.Cmd capturing it) then blocks forever waiting for that pipe
	# to close -- it never does, because the daemonized postgres process
	# keeps the write end open long after pg_ctl itself returns.
	"$PG_CTL" start -w -D "$RESTORE_DATA_DIR" -l "${RESTORE_DATA_DIR%/}.log" -o "-p $PGPORT"
	echo "restore.sh: restored instance started on port $PGPORT -- replaying WAL to target, then promoting"
fi
