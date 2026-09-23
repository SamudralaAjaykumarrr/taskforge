# PostgreSQL Backup, Restore, and Disaster-Recovery Runbook

Status: **Implemented and drilled.** This is Phase 15's
([enterprise-roadmap.md](enterprise-roadmap.md) "Phase 15 — PostgreSQL HA
/ Backup / DR Proof") authoritative operational runbook, written after
every procedure below was executed as permanent, CI-runnable test code
(`test/dr/`) against real PostgreSQL 16 instances, not merely described.
Every number in this document is a measured result from those drills, not
an invented target — see each section for the exact evidence.

This document assumes the design already settled in
[phase-15-plan.md](phase-15-plan.md) (§10–§13) and does not re-derive it;
it states the procedure and the drilled evidence. Where this runbook and
that plan disagree, this runbook — written after real drills ran — is
authoritative for the numbers; the plan remains authoritative for the
architecture.

**TaskForge builds none of the mechanisms below.** Backup, WAL archiving,
streaming replication, and promotion are PostgreSQL-native primitives
(`pg_basebackup`, `archive_command`, streaming replication,
`recovery_target_time`). TaskForge's own contribution is the operator
tooling that invokes them consistently (`deploy/pg-dr/*.sh`), the proof
that TaskForge's own client behavior survives a real restore/failover
(`test/dr/`), and this runbook. See [ADR-0011](adr/0011-postgresql-native-ha-backup-dr.md)
for the decision record.

## 1. Scope and non-scope

- **In scope**: backup, PITR restore, HA topology recommendation, a
  controlled failover drill, and TaskForge's own measured behavior across
  both.
- **Out of scope, by design** ([phase-15-plan.md](phase-15-plan.md) §2.2):
  TaskForge-built failover orchestration, consensus, or split-brain
  detection; multi-region durability; automatic backup scheduling inside
  TaskForge itself. An operator's chosen HA-orchestration tool (Patroni,
  repmgr, a managed provider's own failover) is a deployment-time choice
  this runbook documents the required properties of, without naming or
  building one.

## 2. Backup Procedure

**Mechanism**: `pg_basebackup` (a physical base backup) plus continuous
WAL archiving (`archive_mode = on`, a real `archive_command`).

**The `pg_backup_start`/`pg_backup_stop` misconception, corrected
explicitly**: `pg_basebackup` manages the low-level backup-mode API
(`pg_backup_start`/`pg_backup_stop`) **internally**. Do **not** wrap an
ordinary `pg_basebackup` invocation in manual calls to that API — it
exists for custom backup tooling that copies the data directory by other
means (a filesystem/LVM snapshot tool), not for `pg_basebackup` itself.
[deploy/pg-dr/backup.sh](../deploy/pg-dr/backup.sh) calls `pg_basebackup`
only, never `pg_backup_start`/`pg_backup_stop` directly.

**A backup taken mid-write is not a corruption risk for `pg_basebackup`
itself**: because it brackets its own backup-mode internally, and streams
the WAL generated during the backup alongside it (`-X stream`), the
backup directory is self-contained and internally consistent up to its
own end-of-backup LSN, regardless of what was mid-write when it started.
This is a non-issue for `pg_basebackup`; it applies only to custom backup
tooling that bypasses it.

### 2.1 Prerequisites

The source PostgreSQL instance must already be running with:

```
wal_level = replica          # or logical
archive_mode = on
archive_command = '<a real command that ships %p to durable storage, e.g. cp %p /archive/%f>'
```

### 2.2 Procedure

```
PGPORT=5432 PGHOST=<primary-host> PGUSER=<backup-role> PGPASSWORD=<...> \
  ./deploy/pg-dr/backup.sh /path/to/backup-dir
```

This is the exact script `test/dr/backup_restore_test.go` invokes — the
runbook and the tested behavior can never silently diverge.

### 2.3 Cadence

An operator-scheduled concern (cron, a managed provider's own backup
feature) — TaskForge does not build a scheduler, per
[phase-15-plan.md](phase-15-plan.md) §2.2's explicit non-goal. A
recommended starting point: one daily full base backup, with WAL
archiving running continuously between them. §5 below states the RPO this
implies.

### 2.4 What backup mode does not protect against

Storage-layer corruption below PostgreSQL's own durability guarantees
remains out of scope, unchanged from [failure-model.md](failure-model.md)'s
existing "Storage-layer corruption" entry.

## 3. Restore Procedure (Point-in-Time Recovery)

**Mechanism**: restore the base backup into a fresh data directory, write
`recovery.signal` (PostgreSQL 12+; `recovery.conf` no longer exists) plus
`restore_command`/`recovery_target_time`/`recovery_target_action` into
`postgresql.auto.conf`, then start PostgreSQL. It replays archived WAL up
to the target and, with `recovery_target_action = promote` (this
runbook's default), becomes an ordinary read/write server automatically.

### 3.1 Procedure

```
RECOVERY_TARGET_TIME='2026-09-22 08:00:00-05' \
RESTORE_COMMAND='cp /path/to/wal-archive/%f %p' \
PGPORT=5433 \
  ./deploy/pg-dr/restore.sh /path/to/backup-dir /path/to/restore-dir
```

Recording a valid `RECOVERY_TARGET_TIME`: read it from PostgreSQL's own
clock (`SELECT now()`), never from an operator's local machine clock — see
[failure-model.md](failure-model.md)'s Clock Model. All correctness-critical
timestamps in this project are PostgreSQL-clock-authoritative.

If the backup predates the currently-deployed schema, run `migrate.Up`
against the restored instance **before** the verification step below —
this project ships no schema migration in Phase 15 itself
([phase-15-plan.md](phase-15-plan.md) §14), but a backup taken before a
later, unrelated phase's migration still needs the ordinary migration
path, never a restore-specific one.

### 3.2 Mandatory verification step

Every restore ends with the existing durable invariant checker run
against the restored database, **before it is trusted**:

```
go run ./cmd/taskforge-invariant-check -db-url "$RESTORED_DATABASE_URL"
```

See §8 for what this command is and why it exists.

Zero violations is the acceptance bar. This is the roadmap's own literal
proof obligation, not an optional extra step.

### 3.3 Documented failure mode: a WAL archive gap

If `recovery_target_time` requires a WAL segment missing from the
archive, PostgreSQL's recovery process **fails with an explicit,
loud error at that point** — it does not silently skip forward, and does
not silently stop early without saying so.

**Drilled, not assumed** (`test/dr/backup_restore_test.go`,
`TestDR_SF072_WALArchiveGap_RecoveryFailsLoudly`): a required, post-backup
WAL segment was deliberately deleted from the archive before restore.
Observed, verbatim, from the real PostgreSQL server log:

```
FATAL:  recovery ended before configured recovery target was reached
```

`pg_ctl start -w` (and therefore `restore.sh`) exits nonzero, reporting
"could not start server" — the whole postmaster shuts down cleanly rather
than silently proceeding to an incomplete, wrong restore. The log also
names the exact missing segment (`cp: cannot stat '<path>': No such file
or directory`) — the diagnostic an operator needs.

**Operational remediation**: restore to the latest point *before* the
gap, and accept the resulting narrower RPO for that specific incident.

## 4. HA Topology Recommendation

**Mechanism**: vanilla PostgreSQL streaming replication
(`standby.signal` + `primary_conninfo`, written automatically by
`pg_basebackup -R` — see [deploy/pg-dr/setup-standby.sh](../deploy/pg-dr/setup-standby.sh)).

**Sync vs. async, tradeoff stated** (per PostgreSQL's own documentation,
not a TaskForge-invented tradeoff):

- **Asynchronous** (PostgreSQL's default): the primary commits and
  acknowledges a write without waiting for the standby to receive it.
  Zero write-latency cost from replication; a promotion after primary
  loss can lose the most recent transactions that had not yet reached the
  standby (an RPO gap bounded by replication lag, not by WAL-archiving
  cadence).
- **Synchronous** (`synchronous_standby_names`): the primary waits for the
  standby to confirm receipt (or, with `remote_apply`, replay) before
  acknowledging a commit. Zero data loss on a clean promotion, at the cost
  of added write latency and, if the standby becomes unreachable, either
  a further-configurable degradation or the primary blocking writes
  entirely (`synchronous_commit` interacts with this — see PostgreSQL's
  own documentation for the full tradeoff space).

This runbook does not pick one for every deployment: it is an
operator/deployment decision traded against the specific RPO the
deployment needs versus the write-latency budget it can afford.

**Automated failover orchestration is explicitly a deployment-time
choice this project documents, not builds or bundles** — Patroni,
repmgr, or a managed provider's own failover mechanism are all valid,
tool-agnostic choices. Whichever is chosen must own: failure detection,
standby promotion, and repointing the client-facing connection endpoint
(a VIP, DNS record, or connection-pooler failover) onto the new primary.
TaskForge's own reconnect behavior against that repointed endpoint is
what §6 below measures — this runbook does not test any specific
orchestration tool's own detection/promotion speed, which is not
TaskForge's to test (see §6.3, OD-4).

## 5. RPO / RTO

Stated as a function of this phase's own measured drill evidence
(`test/dr/`), not an invented target — per
[phase-15-plan.md](phase-15-plan.md) §13's own discipline.

### 5.1 RPO (Recovery Point Objective)

Bounded by the WAL-archiving cadence — specifically, by how far
`archive_command` can fall behind before a segment would be lost if the
primary host itself were destroyed.

**Measured** (a dedicated timing harness, ten trials, a real embedded
PostgreSQL 16 instance, a local-filesystem `cp %p <dir>/%f`
`archive_command`, each trial inserting 1,000 rows then calling
`pg_switch_wal()` and timing until the segment appeared in the archive
directory):

```
n=10  min=13.6ms  max=29.8ms  mean=21.1ms
```

**RPO contract**: for a local-filesystem (or comparably fast)
`archive_command`, RPO is on the order of **tens of milliseconds** under
this reference workload. This is an upper bound for *this* archive
target, not a universal claim — an `archive_command` shipping to remote
object storage will have materially higher, network-bound latency, and
must be re-measured against the operator's own actual target before this
number is trusted for that deployment. State the re-measured number in
this section when it changes.

### 5.2 RTO (Recovery Time Objective)

**Disaster-recovery path** (restore from backup, no live standby) —
**measured, `test/dr/backup_restore_test.go`**,
`TestDR_SF071_SF075_BackupRestoreDrill_PITR`, timed from `restore.sh`
invocation to the restored instance reporting itself out of recovery
(promoted, read/write), at a reference volume of 150 jobs (Phase 9's own
documented soak-run scale, per [phase-15-plan.md](phase-15-plan.md) §8.1):

```
Run 1: 8.02s
Run 2: 4.06s
```

**RTO contract**: restore time at the reference data volume is a few
seconds, dominated by `pg_basebackup`/directory-copy time plus WAL replay,
not by anything TaskForge adds. **RTO scales with data volume** — a
production `jobs`/`job_attempts` table grown far beyond this reference
size (see [data-model.md](data-model.md)'s "Phase 12/13 migration lock
profile" for the same scaling discussion applied to migrations) will
restore more slowly. Re-benchmark against the deployment's own real data
volume before trusting this number for it — the same "benchmark before
production" discipline those sections already establish. Retention
([phase-13-plan.md](phase-13-plan.md)) is the lever an operator has to
bound both backup size and restore time.

**Failover path** (a standby already exists, promotion only) —
**measured, `test/dr/failover_drill_test.go`**,
`TestDR_SF073_SF074_FailoverDrill_LiveTraffic`, from the instant the
primary process was stopped (`pg_ctl stop -m immediate`):

```
Infrastructure-side promotion complete (standby promoted + repointed):  488ms – 571ms
TaskForge cmd/api: next successful submission:                          721ms – 809ms
TaskForge cmd/worker: next successful claim:                            6.07s – 6.10s
```

These numbers are TaskForge's own reconnect/recovery time **given** a
working connection-endpoint repointing mechanism — they do **not**
include, and must never be silently combined with, whatever a real
deployment's chosen HA-orchestration tool itself takes to detect failure
and complete promotion (see §6.3, OD-4). A production failover's total
observed downtime is this measured window **plus** that tool's own
detection/promotion time.

`cmd/api` and `cmd/worker` recover on materially different timescales
because they have different retry shapes: `cmd/api`'s per-request
handling fails and returns promptly, so the very next request after the
endpoint is live again succeeds; `internal/worker`'s poll-and-backoff
claim loop does not retry as aggressively (see
[phase-15-plan.md](phase-15-plan.md) §6.2's "backoff, not a crash"
observation), so the observed worker-side window is materially longer
even though **no code changed** in Phase 15 (OD-1 closed negatively — see
§7). An operator whose workload is worker-claim-latency-sensitive during
a failover should factor this asymmetry into their own SLOs; TaskForge's
worker retry/backoff timing is unchanged, ordinary, pre-existing behavior
(docs/retry-semantics.md), not a Phase 15 regression.

## 6. Failover Drill Procedure

### 6.1 Procedure

1. Bring up a primary with WAL archiving configured (§2.1).
2. Seed and start a streaming standby:
   ```
   PGUSER=<replication-role> PGPASSWORD=<...> PGPORT=<standby-port> \
     ./deploy/pg-dr/setup-standby.sh <primary-host> <primary-port> /path/to/standby-dir
   ```
3. Confirm the standby has caught up: `SELECT state FROM pg_stat_replication`
   on the primary must read `streaming`.
4. Drive ordinary traffic through real `cmd/api`/`cmd/worker` processes
   pointed at the primary.
5. At a recorded instant, stop the primary (`pg_ctl stop -m immediate` —
   the closest local analogue to an actual primary-host failure) and
   promote the standby (`pg_ctl promote`).
6. **Do not change `TASKFORGE_DATABASE_URL` on the already-running
   `cmd/api`/`cmd/worker` processes.** In a real deployment, the operator's
   HA-orchestration layer (a VIP, DNS, a connection pooler) is what makes
   "the same connection string now points at the new primary" true without
   TaskForge's own configuration changing — TaskForge is not expected to,
   and does not, discover a new hostname on its own.
7. Measure time-to-recovery for both the API-submission path and the
   worker-claim path independently (they differ — §5.2).
8. Run the invariant checker (§3.2) throughout and after — zero
   violations is the acceptance bar.

### 6.2 Fencing across a promotion — drilled, not assumed

`test/dr/failover_drill_test.go` deliberately puts one job in-flight
(claimed, actively executing, mid-lease) at the exact instant the primary
is stopped — the scenario a purely "before/after" drill would miss.
**Observed result: the job's completion succeeded against the promoted
standby** (`SUCCEEDED`), because lease/fencing state (`lease_owner`,
`lease_generation`) is durable, replicated data — the promoted standby
enforces the identical fencing rule (ADR-0002, TF-INV-002/003/014)
against the identical row. The invariant checker found zero violations
across the whole drill.

The plan's own §8.2 step 7 states this as an "either/or, never silently
wrong" outcome: either the in-flight completion succeeds against the
promoted standby (observed here), or it fails/times out and the job is
later reclaimed via the ordinary, unmodified TF-INV-004 lease-expiry
path — both are correct; only silent loss or silent duplication would not
be. An incidental, corroborating signal from
[phase-15-postgres-evidence.md](phase-15-postgres-evidence.md) §2.4's own
prerequisite drill observed the reclaim branch of this same either/or
directly (jobs whose lease expired during that drill's outage were
correctly swept to `DEAD_LETTERED` by the existing, unmodified Lazy
Dead-Letter Sweep) — both branches of the documented outcome have now
been observed in real drills, across the two evidence-gathering passes.

### 6.3 What this drill does and does not prove (OD-4)

This drill promotes the standby onto the primary's own now-vacated port —
a deliberate, documented simplification, not a claim of equivalence to
any specific production topology. It proves **TaskForge's own** reconnect
behavior given a working connection-endpoint-repointing mechanism. It
does **not** test, and its measured numbers must never be read as, any
specific HA-orchestration tool's own failover detection/promotion speed
(a Patroni failover, a DNS TTL, a load balancer health-check interval) —
that is explicitly out of this project's scope
([phase-15-plan.md](phase-15-plan.md) §2.2). A production deployment's
total observed downtime during a real failover is this section's §5.2
numbers **plus** whatever that deployment's own chosen orchestration tool
takes.

## 7. Connection-Pool Behavior (OD-1 — no TaskForge code change)

**TaskForge's `sql.Open("pgx", ...)` connection pool (`cmd/api`,
`cmd/worker`) required no code change for Phase 15.** Two independent
drills — [phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)
§2 (the prerequisite-evidence pass, native PostgreSQL 16, 1 and 20
concurrent pooled connections) and this runbook's own §6/§5.2 drill (the
permanent `test/dr` harness, real `cmd/api`/`cmd/worker` binaries) — both
show the existing, completely unmodified pool recovers every connection
automatically, with zero process restarts, once the endpoint is live
again. No `SetConnMaxLifetime`, `SetMaxOpenConns`, or retry wrapper was
added, and none is needed.

## 8. Operator Verification Tooling (OD-5)

**Decision: a minimal CLI wrapper was needed, and was built.**
[phase-15-plan.md](phase-15-plan.md) §18/§27 OD-5 left open whether
`internal/invariant.Checker` needed a dedicated CLI, deferring the
decision to implementation/runbook-writing time. Writing §3.2 above
surfaced the concrete need directly: `internal/invariant` is a library
package with no `main` package anywhere in it, so there is no way to
`go run` it at all without a wrapper — the plan's own tentative
`go run ./internal/invariant/cmd/...`-style phrasing already anticipated
this. `cmd/taskforge-invariant-check`
([cmd/taskforge-invariant-check/main.go](../cmd/taskforge-invariant-check/main.go))
is that wrapper: it takes `-db-url` (or `TASKFORGE_DATABASE_URL`), runs
`invariant.Checker.CheckAll`, and exits 0 (clean), 1 (violations found,
printed), or 2 (could not connect / the checker itself failed to run —
deliberately distinct from "ran cleanly and found nothing," per this
project's own "a query failure is not evidence of no violations"
discipline, mirroring `internal/invariant.Checker.CheckAll`'s own
doc comment). It performs no writes and is not started by `cmd/api` or
`cmd/worker` — it is operator-invoked, exactly like the `deploy/pg-dr/*.sh`
scripts, just as a small Go binary rather than a shell script, since it
needs to import `internal/invariant` directly rather than reimplement its
SQL.

## 9. Read-Replica Consistency Note (conditional)

Per [phase-15-plan.md](phase-15-plan.md) §2.1's own hedge ("if read
replicas are evaluated..."), this section documents the implication for a
**future** deployment decision — it does not itself recommend adopting
read replicas for `GET /jobs/{id}`/`GET /workflows/{id}`, absent a
demonstrated scaling need.

A streaming replica lags its primary by an amount bounded by replication
lag (§4's sync/async tradeoff). Reading `GET /jobs/{id}` from a replica
immediately after a `POST /jobs` submission (or a worker's completion
write) risks a stale or not-yet-visible read — a real
read-after-write-consistency cost, not a hypothetical one. Any future
deployment routing these read paths to a replica must accept that
tradeoff explicitly (e.g., by only routing reads that can tolerate
eventual consistency, or by reading from the primary for any
request immediately following a write from the same caller) — this
runbook does not build that routing logic, consistent with §1's
non-scope.

## 10. Security / Credential Handling

- **No new secret type.** A replication role's credential
  (`deploy/postgres-roles.sql`'s documented `taskforge_replicator`
  convention) is handled with the exact same discipline as
  `TASKFORGE_DATABASE_URL`/`TASKFORGE_API_KEY_PEPPER`: never committed,
  never logged, rotated like any other database credential.
- **Backup artifact confidentiality**: a `pg_basebackup` output (or WAL
  archive) contains the **entire** database — including `payload`/
  `result_metadata` (opaque, potentially sensitive per
  [security-model.md](security-model.md) §3) and `api_keys.secret_hash`
  (an HMAC digest, not a raw secret). A backup artifact requires **at
  least the same** access-control and encryption-at-rest discipline as
  the live database itself. TaskForge does not encrypt or classify
  `payload` at rest, and Phase 15 does not change that — this is a
  restatement of an existing gap, extended explicitly to backups/replicas
  of the same data, not a new one.
- **Backup-artifact encryption at rest** is an operator/deployment
  decision, dependent on the chosen storage target (object storage with
  server-side encryption, an encrypted volume, etc.) — this runbook
  documents that the decision must be made, without mandating a specific
  mechanism (staying tool-agnostic, per §4).
- **No new TLS/`sslmode` requirement.** The existing
  `sslmode=verify-full` enterprise-reference-deployment requirement
  ([security-model.md](security-model.md) §4) applies identically whether
  the connection resolves to the primary or a promoted standby.

## 11. Observability

No new Prometheus metric is added for Phase 15 — this phase's evidence is
drill/operational evidence, not steady-state production telemetry. The
existing `taskforge_stale_completion_rejections_total` and
`taskforge_lease_expirations_total` ([observability.md](observability.md))
are the correct, existing signals to watch **during** a live failover: a
spike in either, immediately following a promotion, is exactly what
correctly-functioning fencing under a real failover looks like (stale
writes from a pre-promotion connection being correctly rejected). The one
operational checklist item this phase adds is §3.2/§8's invariant-checker
verification step itself.

## 12. Reproducing These Drills

Every number in §5/§6 comes from permanent, committed test code, not a
one-off manual exercise — a different engineer reproduces them by
running:

```
go test ./test/dr/... -v
```

This requires no Docker and no new CI service container — `test/dr`
brings up its own PostgreSQL 16 instances (via `embedded-postgres`,
pinned to `V16`) on locally-allocated ports, exactly as
`internal/testutil`'s existing embedded-postgres fallback path already
does for every other package. **One additional, genuinely new
environment dependency**: `deploy/pg-dr/backup.sh` and
`setup-standby.sh` invoke `pg_basebackup`, which
`embedded-postgres`'s own vendored binary distribution does **not**
include (it bundles only `initdb`/`pg_ctl`/`postgres` — a finding this
implementation pass made, beyond what
[phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)'s own OD-2
evidence pass tested). `test/dr` locates a real `pg_basebackup` from the
test host (a `postgresql-client` OS package, the ordinary way an operator
has one too) via `PG_BASEBACKUP_BIN`, the common Debian/Ubuntu
versioned-package path, or `PATH`, and **skips** (does not fail) if none
is found. Install `postgresql-client` (or point `PG_BASEBACKUP_BIN` at an
existing installation) to run these tests; every environment this
runbook's own drills were executed in already had PostgreSQL 16 client
tools installed.

## 13. Cross-References

- Plan: [phase-15-plan.md](phase-15-plan.md)
- Prerequisite evidence (OD-1/OD-2 closure): [phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)
- ADR: [ADR-0011](adr/0011-postgresql-native-ha-backup-dr.md)
- Scripts: [deploy/pg-dr/backup.sh](../deploy/pg-dr/backup.sh),
  [deploy/pg-dr/restore.sh](../deploy/pg-dr/restore.sh),
  [deploy/pg-dr/setup-standby.sh](../deploy/pg-dr/setup-standby.sh)
- Tests: `test/dr/backup_restore_test.go` (SF-071, SF-072, SF-075),
  `test/dr/failover_drill_test.go` (SF-073, SF-074) —
  [scenario-corpus.md](scenario-corpus.md)
- Invariants: [invariants.md](invariants.md) ("Phase 15 reviewed: no new
  invariant added")
- Failure model: [failure-model.md](failure-model.md) (F21)
- Security model: [security-model.md](security-model.md) "Enterprise
  Deployment Profile"
- Replication role convention: [deploy/postgres-roles.sql](../deploy/postgres-roles.sql)
