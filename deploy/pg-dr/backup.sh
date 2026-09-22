#!/usr/bin/env bash
#
# TaskForge Phase 15 (docs/phase-15-plan.md §10, docs/disaster-recovery.md):
# take a physical base backup with pg_basebackup.
#
# This script calls pg_basebackup ONLY. It never calls pg_backup_start /
# pg_backup_stop (the low-level backup-mode API) directly: pg_basebackup
# already brackets the backup-mode API internally, and wrapping it in a
# second, manual bracketing is both unnecessary and, for a plain
# pg_basebackup invocation, actively wrong (that low-level API exists for
# custom backup tooling that copies the data directory by other means --
# a filesystem/LVM snapshot tool -- not for pg_basebackup itself). See
# docs/disaster-recovery.md's "Backup Procedure" section for the full
# explanation this script's own comments intentionally keep short.
#
# This is operator-invoked tooling, not something TaskForge's own binaries
# run (docs/phase-15-plan.md §5/§15): no automatic backup scheduling is
# built here -- an operator (or their cron/managed-provider backup
# feature) invokes this directly. It is also exactly what
# test/dr/backup_restore_test.go invokes (via PG_BIN pointed at a test
# instance's own binaries) -- the runbook and the tested behavior can
# never silently diverge, because they are the same script.
#
# Requires: continuous WAL archiving (archive_mode=on, a real
# archive_command) already configured and running on the source
# PostgreSQL instance. A base backup alone, without WAL archiving, cannot
# be used for point-in-time recovery -- see restore.sh and
# docs/disaster-recovery.md.
#
# Usage:
#   PGPORT=5432 [PGHOST=localhost] [PGUSER=postgres] [PGPASSWORD=...] \
#     [PG_BIN=/path/to/postgres/bin] \
#     ./backup.sh <backup-dir>
#
# <backup-dir> must not already exist (this script creates it via
# pg_basebackup's own -D, which requires an empty or non-existent target).

set -euo pipefail

BACKUP_DIR="${1:?usage: backup.sh <backup-dir>}"
: "${PGPORT:?PGPORT must be set to the source PostgreSQL instance port}"
PGHOST="${PGHOST:-localhost}"
PGUSER="${PGUSER:-postgres}"
# PG_BASEBACKUP_BIN, defaulting to PG_BIN, lets a caller point the
# one-shot pg_basebackup CLIENT at a different binary directory than
# PG_BIN's pg_ctl/postgres -- needed because some PostgreSQL server
# distributions (e.g. embedded-postgres's vendored binaries, used by
# test/dr) bundle only initdb/pg_ctl/postgres, not the client tools an
# operator's OS package (postgresql-client) normally provides alongside
# them. Client/server version skew within the same major version is
# ordinary and supported by PostgreSQL itself.
PG_BASEBACKUP="${PG_BASEBACKUP_BIN:-${PG_BIN:-}}"
PG_BASEBACKUP="${PG_BASEBACKUP:+${PG_BASEBACKUP}/}pg_basebackup"

if [ -e "$BACKUP_DIR" ]; then
	echo "backup.sh: $BACKUP_DIR already exists -- refusing to overwrite" >&2
	exit 1
fi
mkdir -p "$(dirname "$BACKUP_DIR")"

echo "backup.sh: starting pg_basebackup -> $BACKUP_DIR (host=$PGHOST port=$PGPORT)"

# -Fp (plain format): the backup directory is a directly-startable
# PostgreSQL data directory once restore.sh adds recovery.signal --
# no separate untar/gunzip step. For storage-efficient archival, an
# operator may instead use -Ft -z (tar+gzip); restore.sh would then need
# an extraction step first, which this project's runbook documents as the
# storage-efficient alternative without building tooling for it (out of
# this phase's scope -- see docs/disaster-recovery.md).
#
# -X stream: WAL generated during the backup itself is streamed
# alongside it, so the backup directory is self-contained up to its own
# end-of-backup LSN even before any archived WAL is replayed -- this is
# part of what makes a pg_basebackup-taken backup safe to restore
# regardless of what was mid-write when the backup started (docs/
# phase-15-plan.md §11's "non-issue for pg_basebackup" note).
#
# --checkpoint=fast: starts the backup's required checkpoint immediately
# rather than waiting for the next scheduled one -- appropriate for an
# operator-invoked backup (not a background job squeezed into idle I/O
# budget).
"$PG_BASEBACKUP" \
	-h "$PGHOST" -p "$PGPORT" -U "$PGUSER" \
	-D "$BACKUP_DIR" \
	-Fp \
	-X stream \
	--checkpoint=fast \
	-P -v

echo "backup.sh: complete: $BACKUP_DIR"
