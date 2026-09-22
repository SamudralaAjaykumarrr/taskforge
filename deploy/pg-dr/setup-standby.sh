#!/usr/bin/env bash
#
# TaskForge Phase 15 (docs/phase-15-plan.md §8.2/§12, docs/disaster-recovery.md):
# bootstrap a streaming-replication standby off a running primary, using
# pg_basebackup -R (the PostgreSQL 12+ mechanism that writes
# standby.signal and primary_conninfo automatically -- no hand-written
# recovery.conf, which no longer exists).
#
# This is operator-invoked tooling (docs/phase-15-plan.md §5/§15), and
# the same script test/dr/failover_drill_test.go invokes to bring up the
# standby its promotion drill promotes -- runbook and tested behavior can
# never silently diverge.
#
# The connecting role must have the REPLICATION privilege (or be a
# superuser) -- see deploy/postgres-roles.sql's "Replication role"
# section for the least-privilege convention this project documents for
# production use.
#
# Usage:
#   [PGUSER=replicator] [PG_BIN=/path/to/postgres/bin] \
#   [START=1] [PGPORT=<standby-port>] \
#     ./setup-standby.sh <primary-host> <primary-port> <standby-data-dir>
#
# <standby-data-dir> must not already exist. Set START=0 to only seed the
# standby's data directory without starting it.

set -euo pipefail

PRIMARY_HOST="${1:?usage: setup-standby.sh <primary-host> <primary-port> <standby-data-dir>}"
PRIMARY_PORT="${2:?usage: setup-standby.sh <primary-host> <primary-port> <standby-data-dir>}"
STANDBY_DATA_DIR="${3:?usage: setup-standby.sh <primary-host> <primary-port> <standby-data-dir>}"
PGUSER="${PGUSER:-postgres}"
# See backup.sh's identical comment: PG_BASEBACKUP_BIN, defaulting to
# PG_BIN, lets the pg_basebackup CLIENT come from a different binary
# directory than PG_CTL/the server it starts.
PG_BASEBACKUP="${PG_BASEBACKUP_BIN:-${PG_BIN:-}}"
PG_BASEBACKUP="${PG_BASEBACKUP:+${PG_BASEBACKUP}/}pg_basebackup"
PG_CTL="${PG_BIN:+${PG_BIN}/}pg_ctl"
START="${START:-1}"

if [ -e "$STANDBY_DATA_DIR" ]; then
	echo "setup-standby.sh: $STANDBY_DATA_DIR already exists -- refusing to overwrite" >&2
	exit 1
fi
mkdir -p "$(dirname "$STANDBY_DATA_DIR")"

echo "setup-standby.sh: seeding standby from $PRIMARY_HOST:$PRIMARY_PORT -> $STANDBY_DATA_DIR"

# -R: writes standby.signal and primary_conninfo into the seeded data
# directory automatically -- the standby starts in streaming-replication
# mode with no further hand-editing.
# -X stream: the standby's own base backup is self-contained the same
# way an ordinary backup.sh backup is (see that script's comment).
"$PG_BASEBACKUP" \
	-h "$PRIMARY_HOST" -p "$PRIMARY_PORT" -U "$PGUSER" \
	-D "$STANDBY_DATA_DIR" \
	-Fp \
	-X stream \
	-R \
	--checkpoint=fast \
	-P -v

echo "setup-standby.sh: standby seeded at $STANDBY_DATA_DIR"

if [ "$START" = "1" ]; then
	: "${PGPORT:?PGPORT must be set to start the standby (or set START=0 to skip starting)}"
	echo "setup-standby.sh: starting standby on port $PGPORT"
	# -l <logfile>: see restore.sh's identical comment -- required so a
	# caller capturing this script's stdout/stderr (including a Go
	# os/exec.Cmd) does not block forever on the daemonized postgres
	# process holding that pipe open.
	"$PG_CTL" start -w -D "$STANDBY_DATA_DIR" -l "${STANDBY_DATA_DIR%/}.log" -o "-p $PGPORT"
	echo "setup-standby.sh: standby started on port $PGPORT -- streaming from primary"
fi
