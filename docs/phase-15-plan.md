# Phase 15 Implementation Plan — PostgreSQL HA / Backup / DR Proof

Status: **PLANNING ONLY. No code written, no migration authored, no ADR
committed, no runbook drafted.** This document is the pre-implementation
plan for Phase 15 — [docs/enterprise-roadmap.md](enterprise-roadmap.md)
"Phase 15 — PostgreSQL HA / Backup / DR Proof" — produced by inspecting
the repository's authoritative documents (roadmap, invariants, testing
strategy, data model, security model, compatibility policy, observability,
failure model, reference analysis, vision, every ADR) and the current
`main`-descended implementation at the commit this plan is written against
(`phase-15-planning`, tip `c55d9e5`, following merge PR #30
"fix-governance-rate-limit-refill-flake" — Phases 1–14 all merged, no
Phase 15 code exists anywhere in the tree). It does not redefine Phase
15's scope; that scope remains authoritative in
[enterprise-roadmap.md](enterprise-roadmap.md). Where this document and
that roadmap disagree, the roadmap wins.

This plan follows the discipline [phase-12-plan.md](phase-12-plan.md),
[phase-13-plan.md](phase-13-plan.md), and
[phase-14-plan.md](phase-14-plan.md) established: every design choice is
either **settled** (a concrete recommendation, ready to implement) or an
explicitly labeled **open decision (OD-N)**. Unlike Phase 13 (mandatory
blocking ADR) and like Phase 14 (no roadmap-mandated ADR, but one written
anyway because a real alternative was weighed), this plan recommends one
**non-blocking** ADR — see §17.

> **Post-planning update (prerequisite evidence pass)**: this plan's own
> §29 identified OD-1 and OD-2 as genuine implementation preconditions —
> questions no amount of reading code could close, requiring a real,
> empirical drill. That evidence pass has now run; both are **CLOSED**.
> See [docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)
> for the full method, exact commands/timestamps, and observed output.
> **OD-1: CLOSED — no `SetConnMaxLifetime`/connection-pool code change is
> required.** Two real drills (a real PostgreSQL 16 primary + streaming
> standby, promoted onto the primary's own vacated endpoint, with
> TaskForge's completely unmodified `internal/store`/`sql.Open("pgx", ...)`
> pool driving real traffic throughout — one at a single pooled
> connection, one at 20 concurrent pooled connections) showed the
> existing, unmodified pool recovers every connection automatically, with
> zero process restarts, in 514ms–857ms — tracking the independently
> measured 696–703ms infrastructure-side endpoint-replacement time
> essentially 1:1, with no stale connection blocking or materially
> delaying recovery. §15, §17, §25, §26, and §28 below are updated in
> place to reflect this closure rather than restating it as open.
> **OD-2: CLOSED — `embedded-postgres`'s `StartParameters` surface is
> sufficient; no raw `initdb`/`postgres` fallback is needed for
> `test/dr`.** Direct inspection plus a live drill proved
> `StartParameters` correctly enables `wal_level`/`archive_mode`/
> `archive_command` — confirmed not merely by `SHOW` round-trip but by a
> real, complete 16 MiB WAL segment actually being archived by
> PostgreSQL's own archiver process — and that `EmbeddedPostgres.Start()`
> reuses (does not reinitialize) a pre-populated data directory, which is
> the mechanism the restore drill (§8.1) needs. §8 and §26 below are
> updated in place. OD-3, OD-4, and OD-5 remain open and non-blocking, as
> originally assessed — this evidence pass did not touch them, per the
> task's own scope.

**A note on sourcing**, following prior plans' own convention: this plan
draws on (1) **roadmap-defined requirements** — quoted or closely
paraphrased from [enterprise-roadmap.md](enterprise-roadmap.md)'s Phase 15
section, never extended in substance; (2) **already-drafted findings** in
[reference-analysis.md](reference-analysis.md), [failure-model.md](failure-model.md),
and [ADR-0001](adr/0001-postgresql-as-source-of-truth.md), which named
this gap before the roadmap formalized it into a phase; and (3) **design
conclusions this plan itself introduces**, explicitly flagged `(this
plan)`.

---

## 1. Exact Phase 15 objective

Per [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 15 — PostgreSQL
HA / Backup / DR Proof": TaskForge does **not** build its own replication,
failover orchestration, or backup scheduling. It uses PostgreSQL-native
primitives (`pg_basebackup`, WAL archiving, streaming replication,
`recovery_target_time`) and third-party deployment tooling (Patroni,
repmgr, or a managed provider's own failover), and **proves TaskForge's
own behavior against them** — a documented, *drilled* runbook with a
stated RPO/RTO, and evidence (not argument) that TaskForge's fencing and
reconnect behavior survive a real failover.

**The one-sentence goal**: convert
[failure-model.md](failure-model.md)'s honest, undischarged boundary — "if
the database is down, TaskForge is down for writes; PostgreSQL
primary failure/failover is out of scope for v1" — from an unexamined
assumption into a measured, documented, at-least-once-drilled operational
fact, without writing a single line of failover/replication code inside
TaskForge itself.

Six concrete deliverables, each with its own roadmap-stated proof
obligation (see §2 for verbatim scope):

1. A documented `pg_basebackup` + continuous WAL-archiving backup
   procedure, with the `pg_backup_start`/`pg_backup_stop` misconception
   explicitly corrected.
2. A documented, **at-least-once-tested** PITR restore procedure using
   `recovery_target_time`, timed.
3. A stated RPO and RTO, with the drilled restore time compared against
   the RTO.
4. A documented HA topology recommendation (vanilla PostgreSQL streaming
   replication, sync vs. async tradeoff stated), explicitly naming
   automated failover orchestration as a deployment-time choice this
   project documents but does not build.
5. **At least one controlled standby-promotion/failover drill**, with
   TaskForge's own reconnect/recovery behavior and timing measured and
   recorded, and fencing (ADR-0002) observed — not assumed — to still
   hold across it.
6. If read replicas are evaluated for the two `GET` read paths, the
   read-after-write/staleness implications documented before
   recommending adoption.

## 2. Authoritative scope (verbatim from the roadmap)

### 2.1 Exact scope

- A documented backup procedure using `pg_basebackup` plus continuous WAL
  archiving. `pg_basebackup` manages `pg_backup_start`/`pg_backup_stop`
  internally; the runbook must not present manual bracketing as a
  required step.
- A documented, tested-at-least-once PITR restore procedure using
  `recovery_target_time` (or equivalent), timed.
- A stated RPO (data-loss window given the chosen archiving cadence) and
  RTO (expected restore duration at a reference data volume).
- A documented HA topology recommendation using vanilla PostgreSQL
  streaming replication (sync or async, tradeoff stated per PostgreSQL's
  own documentation), explicitly noting automated failover orchestration
  is a deployment-time choice TaskForge documents but does not build or
  bundle.
- At least one controlled standby-promotion/failover drill: promote a
  standby in a test environment, and record (a) TaskForge's own
  reconnect/recovery behavior and timing, and (b) whether in-flight
  claims/leases are affected and whether ADR-0002 fencing behaves
  correctly across it — observed, not assumed.
- If read replicas are evaluated for `GET /jobs/{id}`/`GET
  /workflows/{id}`, document the read-after-write/staleness implications
  before recommending adoption.

### 2.2 Explicit non-scope

- No TaskForge-built failover orchestration, consensus, or split-brain
  detection — that is deployment-layer tooling (Patroni, repmgr, or
  equivalent), operator-chosen, not built by this project.
- No multi-region/multi-datacenter durability (an explicit
  [vision.md](vision.md) non-goal, and outside the Enterprise Deployment
  Profile's single-region assumption).
- No automatic backup scheduling infrastructure built into TaskForge
  itself — the backup/restore procedure is documented and operator-run
  (cron, a managed provider's built-in feature, or equivalent).

### 2.3 Prerequisites

None — independent of Phases 11–14 and 16; can run in parallel if a
second work-stream is available. **This plan does not add a prerequisite
on any other phase.** §17 below discusses whether it needs an internal
prerequisite step of its own (an empirical drill before any code change
is even considered).

### 2.4 Invariants / proof obligations (roadmap text)

- A PITR restore to a stated point in time produces a database where all
  of TaskForge's existing invariants still hold — verified by running the
  existing invariant checker against the restored database.
- The measured restore time at a reference data volume is recorded and
  compared against the stated RTO.
- After a controlled standby-promotion drill, TaskForge resumes correct
  operation against the new primary within a measured, recorded duration,
  with fencing invariants intact throughout.

### 2.5 Failure scenarios to guard against (roadmap text)

- A restore attempted from a backup taken mid-write must fail cleanly or
  be prevented by the documented procedure, not silently produce a
  corrupt restore (a non-issue for `pg_basebackup` itself; applies to
  custom backup tooling that bypasses it).
- A WAL archive gap must be documented as "recovery fails at that point,
  with a clear error," not silently skipped forward.
- A standby promoted while TaskForge is still connected to the old
  primary — the reconnect behavior must be observed, and any window of
  failed writes bounded and stated.

### 2.6 Tests / evidence required (roadmap text)

- At least one full, timed backup-then-restore drill, invariants verified
  by the existing checker.
- At least one controlled standby-promotion/failover drill, reconnect/
  recovery time measured and recorded.
- A written runbook a *different* engineer can follow to reproduce both
  drills, reviewed for clarity.

### 2.7 Enterprise exit criteria (roadmap text, reproduced for §25)

- [ ] A documented `pg_basebackup`/WAL-archiving backup exists (with the
      `pg_backup_start`/`pg_backup_stop` misconception corrected), and a
      restore-from-backup drill has been executed and timed at least once.
- [ ] A stated RPO/RTO exists and the drilled restore time is compared
      against it.
- [ ] The restored database passes the existing invariant checker.
- [ ] An HA topology recommendation is documented, tradeoffs (sync vs.
      async) stated.
- [ ] At least one controlled standby-promotion/failover drill has been
      run, TaskForge's reconnect/recovery behavior measured and recorded.
- [ ] If read replicas are recommended for any read path, the
      consistency/read-after-write implications are documented before
      adoption.

## 3. Authoritative source for every major requirement

| Requirement | Authoritative source |
|---|---|
| Overall scope, invariant/test/exit-criteria checklist | [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 15" (§2 above) |
| "PostgreSQL down = TaskForge down for writes, no silent degradation" boundary this phase operationalizes but does not reverse | [ADR-0001](adr/0001-postgresql-as-source-of-truth.md) "Failure Implications"; [failure-model.md](failure-model.md) F6, "Out of Scope: PostgreSQL primary failure/failover" |
| Clock model (why `recovery_target_time` and lease timestamps are PostgreSQL-clock-authoritative) | [failure-model.md](failure-model.md) "Clock Model" |
| Why PostgreSQL-native primitives, not TaskForge code, and why sync/async tradeoff and HA-orchestration-tool choice are deployment-time, not TaskForge decisions | [reference-analysis.md](reference-analysis.md) PostgreSQL HA/backup rows (cited §6) |
| TF-INV-001 through TF-INV-019 (full current registry; the restore/failover proof obligation is against ALL of these, not a subset) | [invariants.md](invariants.md) |
| Durable-state invariant checker (`internal/invariant.Checker.CheckAll`) — the restore-verification mechanism | `internal/invariant/invariant.go` — direct inspection, §8 |
| Fault-injection primitives reusable for the failover drill (`TerminateBackend`, `BackendPID`) | `internal/chaos/chaos.go` — direct inspection, §11 |
| Real-OS-process / real-compiled-binary test precedent (this phase's drills need the same shape) | `test/procs/`, `test/compat/` (Phase 14) — direct inspection, §8 |
| Enterprise Deployment Profile's "PostgreSQL HA cluster" assumption, which Phase 15 is the phase that actually satisfies | [security-model.md](security-model.md) "Enterprise Deployment Profile" |
| Current DB connection setup (`sql.Open`, no explicit pool lifetime/retry tuning) | `cmd/api/main.go`, `cmd/worker/main.go`, `internal/store/store.go` — direct inspection, §6.4 |
| Retention's own TTL/data-volume shape (informs RTO reference data volume) | [phase-13-plan.md](phase-13-plan.md), `internal/retention/retention.go`, `.env.example` |
| Migration lock-safety precedent this phase's own docs (if any) must follow | [data-model.md](data-model.md) "Phase 12/13 migration lock profile" sections |
| Scenario-corpus numbering continuation | [scenario-corpus.md](scenario-corpus.md) (ends at SF-070) |
| ADR conventions | [docs/adr/README.md](adr/README.md) |
| No AGENTS/HANDOFF/CLAUDE instruction files exist in this repository | Direct repository search (none found) — this plan follows only the documents above |

## 4. In-scope work (this plan's organization of §2.1)

1. **A backup runbook** (`pg_basebackup` + `archive_mode`/`archive_command`
   WAL archiving), correcting the `pg_backup_start`/`pg_backup_stop`
   misconception explicitly.
2. **A PITR restore runbook** (`recovery_target_time`), drilled at least
   once, timed.
3. **A stated RPO/RTO**, with the drill's measured time compared against
   the RTO target.
4. **An HA topology document** (streaming replication, sync/async
   tradeoff, tool-agnostic failover-orchestration statement).
5. **A standby-promotion/failover drill**, with TaskForge's reconnect
   behavior/timing measured, and fencing observed intact.
6. **A read-replica consistency note** (conditional — only "if evaluated,"
   per the roadmap's own hedge; this plan does not mandate adopting read
   replicas, only documenting the implication if a future deployment
   considers them).
7. **Test/drill infrastructure** to actually produce items 2, 3, and 5's
   evidence reproducibly (not a one-off manual exercise) — the roadmap's
   own "Tests / evidence required" (§2.6) demands this be reproducible by
   a different engineer, which this plan reads as requiring committed,
   re-runnable test code, not only prose.

## 5. Explicit non-goals / deferred work

Reproduced from [enterprise-roadmap.md](enterprise-roadmap.md) §2.2, plus
this plan's own scoping calls flagged `(this plan)`:

- No TaskForge-built failover orchestration, consensus, or split-brain
  detection.
- No multi-region/multi-datacenter durability.
- No automatic backup scheduling infrastructure inside TaskForge.
- **`(this plan)`** No mandatory adoption of read replicas — §2.1's last
  bullet is conditional ("if... are evaluated"); this plan documents the
  consistency implication as a reusable reference for a future deployment
  decision, but does not itself recommend adopting read replicas for the
  two existing `GET` endpoints, absent a demonstrated scaling need. Doing
  so would be inventing scope the roadmap deliberately left conditional.
- **`(this plan)`** No naming of one specific HA-orchestration vendor
  (Patroni vs. repmgr vs. a managed provider) as *the* TaskForge
  recommendation. The roadmap's own Phase 15 sequencing note explicitly
  leaves this "named or left tool-agnostic per the team's later
  judgment" (per [reference-analysis.md](reference-analysis.md) and
  [ADR-0010](adr/0010-expand-migrate-contract.md)'s own citation of that
  framing). This plan documents the *category* of tooling and the
  properties a deployment needs from whichever one is chosen, without
  picking a winner.
- **`(this plan)`** No schema/migration change. Direct inspection (§6)
  found nothing in Phase 15's scope that requires a new column, table, or
  index — this is the first enterprise-roadmap phase since Phase 10 with
  no migration at all, and that is a finding worth stating plainly rather
  than inventing a migration to justify this being a "real" phase.
- **`(this plan)`** No new HTTP endpoint, no new CLI subcommand shipped as
  part of *TaskForge's own binaries*. Any scripts this phase produces
  (backup invocation wrapper, restore wrapper, replica-bootstrap script)
  are **operator tooling that ships as shell scripts under `deploy/`**,
  analogous to `deploy/postgres-roles.sql` — not Go code compiled into
  `cmd/api`/`cmd/worker`/`cmd/taskforge-admin`. This mirrors the roadmap's
  own "no automatic backup scheduling infrastructure built into TaskForge
  itself" non-goal, read literally: a documented, operator-invoked
  script is not "infrastructure built into TaskForge," but a Go binary
  living under `cmd/` that TaskForge ships and operates would blur that
  line.
- **`(this plan)`** No change to `internal/store`'s public API, the claim
  query, or any invariant-enforcing SQL statement. §17's connection-pool
  question (below) is the one place this plan flags a *possible* small
  code change, and it is explicitly conditioned on the drill's own
  evidence, not decided here.

## 6. Current-state and gap analysis

### 6.1 What exists today: nothing Phase-15-specific

Direct repository search confirms:

- No file under `deploy/`, `docs/`, or anywhere else documents a backup
  procedure, WAL archiving, PITR restore, or an HA topology.
  `deploy/postgres-roles.sql` (Phase 12) is the only file in `deploy/`,
  and it is unrelated (least-privilege role grants, not availability).
- `docker-compose.yml` starts a single, unreplicated `postgres:16`
  container with no `archive_mode`, no `wal_level` override, and no
  volume-mounted WAL archive directory — a plain default-configuration
  instance, matching [ADR-0001](adr/0001-postgresql-as-source-of-truth.md)'s
  "single durable PostgreSQL endpoint" assumption exactly, with no HA or
  backup machinery layered on top anywhere in this repository.
- CI (`.github/workflows/ci.yml`) runs one ephemeral `postgres:16` service
  container per job — also unreplicated, also with no WAL archiving
  configured. There is no CI job today that starts two coordinated
  PostgreSQL instances.
- `internal/testutil` (the shared test-database helper every existing
  integration test uses) supports exactly one mode of PostgreSQL: either
  `TASKFORGE_TEST_DATABASE_URL` (a single instance, typically the CI
  service container) or a single `embedded-postgres` instance started
  per test binary. **Neither mode currently starts a second, replicating
  instance.** This is the load-bearing gap this plan's architecture (§8)
  must close, because Phase 15's two drills (restore, failover) are
  fundamentally about *more than one PostgreSQL data directory existing
  at once* — something no existing TaskForge test infrastructure has ever
  needed before this phase.

### 6.2 What TaskForge's own code assumes today about database availability

- `cmd/api/main.go` and `cmd/worker/main.go` both call `sql.Open("pgx",
  cfg.DatabaseURL)` — Go's ordinary lazy connection pool
  (`database/sql`). Direct inspection found **no** `SetConnMaxLifetime`,
  `SetConnMaxIdleTime`, or `SetMaxOpenConns` call anywhere in either
  `main.go` or `internal/store`. This matters directly for the roadmap's
  own proof obligation ("TaskForge's own reconnect/recovery behavior"):
  `database/sql` does not proactively retry a failed query on a fresh
  connection — a query issued against a pooled connection that now points
  at a demoted/unreachable former primary fails and returns that error to
  the caller (`internal/worker`'s claim loop, or an HTTP handler); the
  pool only dials a *new* connection lazily, on the *next* attempt. There
  is no `SetConnMaxLifetime` forcing already-pooled connections to be
  proactively recycled, so a connection opened before a promotion could
  sit in the pool, apparently idle-and-healthy, until the next query
  against it fails.
- `internal/worker.Worker.Run`'s poll loop (per [failure-model.md](failure-model.md)
  F6) already treats a claim-query failure as "back off and retry," not a
  crash — this is the existing mechanism this plan expects to carry a
  worker through a failover window, *if* the pool's own reconnect
  behavior cooperates (§6.4's open question).
- `cmd/api`'s handlers have no explicit retry of their own; an in-flight
  request during a failover window fails with whatever error `pgx`
  surfaces (typically a connection-refused/reset class error), which
  today's handlers map to an ordinary `500`. Per [failure-model.md](failure-model.md)
  F6, this is the documented, accepted v1 behavior ("degrades to
  unavailability, not corruption") — Phase 15 does not change this
  contract; it measures how long that window actually lasts under a real
  promotion.
- **Whether this is "good enough" or needs a small, targeted fix
  (`SetConnMaxLifetime`, or an explicit bounded retry wrapper around the
  claim/submit path) is not decided by reading the code — it is an
  empirical question this phase's own drill answers.** See §17 for why
  this plan treats the drill as a genuine prerequisite-evidence step, not
  only a final proof obligation.

### 6.3 What PostgreSQL/reference-analysis already concluded (not re-litigated here)

[reference-analysis.md](reference-analysis.md) already recommends, and
this plan does not re-open:

- Document a deployment-time HA topology using vanilla PostgreSQL
  replication; do not build TaskForge-specific failover code.
- Write and drill a runbook using existing PostgreSQL primitives
  (`pg_basebackup`, WAL archiving, `recovery_target_time`); the gap is
  the runbook and the drill, not new capability — PostgreSQL already
  provides everything needed.

### 6.4 The Enterprise Deployment Profile already assumes Phase 15 is done

[security-model.md](security-model.md)'s "Enterprise Deployment Profile"
(which Phase 12 was scoped against) already states: "**PostgreSQL HA
cluster**: the database is deployed with replication/failover per
[enterprise-roadmap.md](enterprise-roadmap.md) Phase 15, not a single
unreplicated instance." This means Phase 15 is not merely an optional
operational nicety — it is a load-bearing assumption an already-shipped
phase (Phase 12) is written against. This plan does not need to change
anything in Phase 12 as a result (Phase 12's security controls do not
depend on Phase 15's *mechanism*, only on the *fact* of an HA-deployed
database existing in the target deployment profile), but it is worth
recording that this dependency runs backward (Phase 12 assumes Phase 15
conceptually) even though the roadmap's own sequencing lists no hard
prerequisite.

## 7. Architecture

```
   +------------------------------------------------------------------+
   | docs/disaster-recovery.md (NEW) -- the authoritative runbook       |
   | - Backup procedure (pg_basebackup + WAL archiving)                 |
   | - PITR restore procedure (recovery_target_time)                    |
   | - Stated RPO / RTO                                                  |
   | - HA topology recommendation (streaming replication, sync/async)   |
   | - Failover drill procedure + its own recorded results               |
   | - Read-replica consistency note (conditional)                       |
   | Written so a DIFFERENT engineer can follow it end to end            |
   | (roadmap's own "Tests / evidence required" wording)                 |
   +------------------------------------------------------------------+
                |                                    |
                v                                    v
   +--------------------------------+   +--------------------------------+
   | deploy/pg-dr/ (NEW)              |   | test/dr/ (NEW) -- mirrors       |
   | operator-invoked shell scripts,  |   | test/compat, test/procs'        |
   | never compiled into TaskForge's  |   | real-OS-process precedent:      |
   | own binaries:                    |   |                                  |
   |  - backup.sh (pg_basebackup +    |   |  - backup_restore_test.go:      |
   |    archive_command wrapper)      |   |    drives real workload via a   |
   |  - restore.sh (PITR restore      |   |    real cmd/api+cmd/worker      |
   |    driver, recovery_target_time) |   |    pair, runs backup.sh/        |
   |  - setup-standby.sh (pg_basebackup|   |    restore.sh against a real   |
   |    -R style streaming-replica    |   |    PostgreSQL data directory,   |
   |    bootstrap)                    |   |    times the restore, runs      |
   |                                   |   |    internal/invariant.Checker   |
   | These scripts ARE what the       |   |    against the restored DB      |
   | runbook tells an operator to run;|   |                                  |
   | the test harness (right) invokes |   |  - failover_drill_test.go:      |
   | the SAME scripts, not a          |   |    boots primary + standby via  |
   | reimplementation, so the runbook |   |    the same setup-standby.sh,   |
   | and the tested behavior can      |   |    runs real cmd/api+cmd/worker |
   | never silently diverge.          |   |    traffic against the primary, |
   +--------------------------------+   |    promotes the standby (real   |
                                          |    `pg_ctl promote`), measures  |
                                          |    TaskForge's reconnect time,  |
                                          |    re-runs the invariant        |
                                          |    checker throughout           |
                                          +--------------------------------+
                                                          |
                                                          v
                                          +--------------------------------+
                                          | Reused, not reinvented:          |
                                          |  - internal/invariant.Checker    |
                                          |    (CheckAll against a *sql.DB)  |
                                          |  - internal/chaos.TerminateBackend|
                                          |    /BackendPID (simulating the   |
                                          |    primary vanishing mid-write,  |
                                          |    the F6-adjacent case)         |
                                          |  - the Phase 14 real-binary      |
                                          |    os/exec harness pattern       |
                                          |    (test/procs, test/compat)     |
                                          +--------------------------------+
```

**No new network-facing component, no new TaskForge process, no new
schema.** Every new artifact is documentation, operator-run shell
scripts, or test infrastructure — consistent with
[architecture.md](architecture.md)'s "smallest architecture" discipline
and the roadmap's own explicit non-goals (§2.2).

## 8. Test/drill infrastructure — the load-bearing new piece

§6.1 already identified the real gap: no existing TaskForge test harness
starts a second, coordinated PostgreSQL instance. This section is
therefore this plan's most consequential design decision.

**Settled recommendation**: build the second instance the same way
Phase 14 built its second *binary* — using tooling this repository
already depends on, invoked via `os/exec`, rather than introducing a new
dependency (Docker Compose, `testcontainers-go`, or similar) this project
has not needed until now.

- `internal/testutil` already depends on
  `github.com/fergusstrange/embedded-postgres`, which vendors real
  PostgreSQL server binaries (not a mock, not a different implementation
  — literal `postgres`/`pg_basebackup`/`pg_ctl` binaries) and exposes
  their installation directory. This plan's `test/dr` package uses that
  same binary distribution to start **two** independent data directories
  on two different local TCP ports — one primary, one standby — via
  direct `os/exec` calls to `pg_basebackup -R` (seeding the standby and
  writing `standby.signal` + `primary_conninfo` automatically, the
  PostgreSQL 12+ mechanism — no hand-written `recovery.conf`, which no
  longer exists as of PostgreSQL 12) and `pg_ctl start`/`pg_ctl promote`.
  This requires **no Docker dependency** anywhere in this phase's own
  test suite, preserving the exact portability property
  `internal/testutil`'s own package doc comment states as its reason for
  existing ("Docker is not guaranteed to be available in every
  environment TaskForge is developed or reviewed in").
- **OD-2, CLOSED — verified, not merely assumed**: this requires the
  primary's `postgresql.conf` to be started with `wal_level = replica`
  (or higher) and `archive_mode = on` plus a concrete `archive_command`,
  which is **not** `embedded-postgres`'s default configuration.
  `embedded-postgres`'s `Config.StartParameters(map[string]string{...})`
  (confirmed directly against that library's source, `config.go`/
  `embedded_postgres.go` — not merely its README) **does** enable exactly
  this: values are passed through as `pg_ctl start -o "-c key=\"value\""`,
  and a live drill proved not just that the GUCs take effect (`SHOW
  wal_level` etc.) but that PostgreSQL's own archiver process actually
  archives a real, complete WAL segment under a `StartParameters`-supplied
  `archive_command` — see
  [docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md) §3
  for the exact commands and observed output. Direct source inspection
  additionally confirmed `EmbeddedPostgres.Start()` reuses (does not
  reinitialize) a pre-populated `DataPath`, which is the mechanism §8.1's
  restore drill needs. **The raw `initdb`/`postgres` fallback this
  section originally proposed as a contingency is not needed.** One
  concrete, small implementation-time action item this evidence pass does
  surface: pin `.Version(embeddedpostgres.V16)` explicitly when `test/dr`
  is built, for fidelity with the project's actual deployment-target
  PostgreSQL version (the cached binary this evidence pass exercised was
  18.3, incidental to this session's environment, not a project pin) —
  see the evidence document's §3.4 for the full reasoning.
- **CI environment requirement**: `.github/workflows/ci.yml`'s existing
  `postgres:16` service container remains used by every *other* test
  package unchanged. `test/dr`'s own tests do not use that service
  container at all — they bring up their own two `embedded-postgres`-style
  instances on ports the CI runner itself allocates, exactly as
  `internal/testutil`'s own embedded-postgres fallback path already does
  for local development without Docker. This means `test/dr` can run
  in CI without any new service-container configuration — a real
  simplification relative to what a Docker-Compose-based two-node design
  would have required.

### 8.1 Backup/restore drill (`test/dr/backup_restore_test.go`)

1. Start one `embedded-postgres`-backed primary with `wal_level =
   replica`, `archive_mode = on`, `archive_command` pointed at a local,
   test-scoped directory.
2. Run `migrate.Up` against it, then drive a realistic workload through
   real `cmd/api`/`cmd/worker` binaries (or, for a faster/cheaper variant,
   directly through `internal/store` — see OD-3, §17, for which this plan
   recommends) at a **reference data volume**: this plan recommends
   reusing Phase 9's own documented soak-run scale
   (`cmd/chaos -mode=soak`, e.g. 150 jobs/15 workflows/20 workers per the
   README's own recorded manual-soak invocation) as the reference volume,
   per the roadmap's own suggestion ("Phase 9's soak-run scale, or larger
   if available") — this is the smallest choice that reuses an existing,
   already-documented number rather than inventing a new one.
3. Invoke `deploy/pg-dr/backup.sh` (a thin, documented wrapper around
   `pg_basebackup -D <dir> -Ft -z -Xs` or equivalent, plus the already-
   running `archive_command`) to take a base backup partway through the
   workload.
4. Continue driving workload after the backup (so the WAL archive has
   segments the backup alone does not cover — this is what makes it a
   genuine PITR test, not merely "restore the backup").
5. Record a `recovery_target_time` timestamp partway through this
   post-backup activity (read from PostgreSQL's own clock, per the Clock
   Model — never a test-host clock).
6. Stop generating workload; invoke `deploy/pg-dr/restore.sh` against a
   **fresh** data directory (base backup + WAL archive replay up to the
   recorded `recovery_target_time`), timing the full restore
   (`pg_basebackup` restore + WAL replay wall-clock, not just the
   `pg_ctl start` call).
7. Once the restored instance reports `recovery_target_time` reached and
   promotes itself to a normal read/write instance, run
   `internal/invariant.Checker.CheckAll(ctx)` against it directly — the
   exact mechanism the roadmap's own proof obligation names ("verified by
   running the existing invariant checker against the restored
   database").
8. Assert: zero violations, and the restore's measured wall-clock time is
   recorded (compared against the stated RTO in the runbook — §12/§13).

### 8.2 Failover drill (`test/dr/failover_drill_test.go`)

1. Start a primary as in §8.1 step 1.
2. Invoke `deploy/pg-dr/setup-standby.sh` (`pg_basebackup -R` against the
   running primary, on a second local port) to bring up a genuine
   streaming replica, and confirm it has caught up (`pg_stat_replication`
   on the primary, or `pg_last_wal_replay_lsn()` on the standby, reaching
   the primary's current LSN).
3. Start real `cmd/api` and `cmd/worker` binaries pointed at the
   primary's connection string, and drive live traffic (ordinary HTTP
   submissions plus real worker claims) — reusing the Phase 14 real-binary
   `os/exec` harness pattern (`test/procs`, `test/compat`) rather than
   inventing a new one.
4. At a recorded instant, forcibly terminate the primary (this plan
   recommends `internal/chaos.TerminateBackend`/`BackendPID`-style forced
   connection severance is **not sufficient by itself** here — that
   simulates a lost connection, not a lost primary; §8.2 needs the
   primary *process* itself stopped, via `pg_ctl stop -m immediate`
   against the primary's own data directory, which is the closer analogue
   to an actual primary-host failure) and, immediately after, promote the
   standby (`pg_ctl promote`, the real PostgreSQL 12+ promotion command,
   or `SELECT pg_promote()`).
5. **Do not change `TASKFORGE_DATABASE_URL` on the already-running
   `cmd/api`/`cmd/worker` processes.** This is deliberate and load-bearing
   for what this drill actually measures: in a real deployment, an
   operator's HA topology (Patroni's virtual IP, a DNS record, HAProxy/
   PgBouncer, or a cloud provider's floating endpoint) is what makes "the
   same connection string now points at the new primary" true without
   TaskForge's own configuration changing — TaskForge is not expected to
   discover a new hostname on its own (that would be exactly the
   "TaskForge-built failover orchestration" the roadmap's non-scope
   rules out). **This plan's drill therefore promotes the standby to
   listen on the SAME port the primary was using** (both are local
   `embedded-postgres`-style instances on the test host; the primary is
   fully stopped in step 4 before the standby is promoted onto that same
   port) — a deliberate, documented simplification that reproduces "the
   connection string still resolves to a live, correct PostgreSQL
   server" without building or depending on real DNS/VIP/proxy
   infrastructure inside the test. **This is flagged explicitly, not
   silently assumed adequate — see OD-4, §17**: it proves TaskForge's own
   reconnect behavior *given* a working failover-orchestration layer; it
   does not itself test any specific orchestration tool's own failover
   speed or correctness, which is out of this phase's scope by the
   roadmap's own non-goal.
6. Measure, from the moment the primary process stops (step 4) to the
   moment TaskForge (either process) successfully resumes writes against
   the promoted standby: (a) how long until the next successful job
   submission via `cmd/api`, and (b) how long until the next successful
   claim via `cmd/worker`. Record both — they may differ, since
   `internal/worker`'s poll-and-backoff loop and `cmd/api`'s per-request
   handling have different retry shapes today (§6.2).
7. Confirm no in-flight claim/lease from *before* the promotion produced
   a fenced-out, stale write that nonetheless succeeded — i.e., confirm
   `internal/invariant.Checker` finds zero violations across the whole
   drill, and confirm directly (by reading the job row) that any job
   claimed under the pre-promotion primary's now-abandoned connection
   either (a) completed normally before the primary was stopped, or (b)
   is left `RUNNING` with its lease intact, to be reclaimed by ordinary
   TF-INV-004 lease expiry once workers resume — never silently
   duplicated or silently lost.
8. Record every measured duration in the runbook itself (§12), not only
   in test output — the roadmap's own proof obligation is a "measured,
   recorded duration," and a number that lives only in a CI log is not
   "recorded" in the sense an enterprise reviewer or a different engineer
   reproducing the drill (§2.6) can find it.

## 9. PostgreSQL availability/failure model (this phase's contribution)

This section is new content this phase adds to the existing
[failure-model.md](failure-model.md), not a replacement for it.
[failure-model.md](failure-model.md)'s existing entry ("PostgreSQL primary
failure/failover... out of scope for v1... TaskForge is down for writes;
it does not silently continue with stale or divergent state") is **not
reversed by Phase 15** — Phase 15 does not make TaskForge itself HA-aware
or failover-orchestrating. What changes is that the *duration* and
*correctness* of that "down for writes" window, once a deployment layers
PostgreSQL-native HA underneath TaskForge, becomes a measured fact instead
of an unstated one.

**Recommended addition to `failure-model.md`'s Failure Catalog** (F21,
next available number — implementation-time addition, not this plan's own
table):

| # | Failure | Handled in v1 (Phase 15)? | Mechanism / Reason |
|---|---|---|---|
| F21 | PostgreSQL primary failure with a deployment-layer HA topology (streaming replication + third-party failover orchestration) underneath TaskForge | **Documented and drilled, not TaskForge-built.** TaskForge itself does no failover detection or orchestration (unchanged from F6/ADR-0001). Given a deployment-layer promotion and a connection endpoint that repoints to the new primary (an operator obligation TaskForge cannot verify — analogous to the existing TLS-proxy and `sslmode=verify-full` deployment obligations in [security-model.md](security-model.md)), TaskForge resumes writes within the window measured by this phase's drill (§8.2, §12), with fencing (TF-INV-002/003/014) intact throughout, proven by the same drill. |

**What this section explicitly does not claim**: that TaskForge detects a
dead primary, that it fails over automatically without a deployment-layer
mechanism doing so, or that the measured drill duration is a guaranteed
bound for every deployment's own connection-repointing latency (which
depends entirely on the operator's chosen HA tooling, not on TaskForge).

## 10. Backup model

**Settled recommendation, per the roadmap's own explicit correction**:

- **Mechanism**: `pg_basebackup` (physical base backup) plus continuous
  WAL archiving (`archive_mode = on`, `archive_command` shipping segments
  to durable storage — object storage, a separate host, or any target the
  operator's `archive_command` script writes to; TaskForge is
  storage-target-agnostic here, consistent with staying tool-agnostic per
  §5).
- **Explicit correction, carried verbatim into the runbook**:
  `pg_basebackup` manages the low-level backup-mode API
  (`pg_backup_start`/`pg_backup_stop`) internally. An operator running
  ordinary `pg_basebackup` must **not** wrap it in manual
  `pg_backup_start`/`pg_backup_stop` calls — that low-level API exists for
  custom backup tooling that copies the data directory by other means
  (e.g., a filesystem/LVM snapshot tool), not for `pg_basebackup` itself.
  The runbook's `deploy/pg-dr/backup.sh` never calls those functions
  directly; it invokes `pg_basebackup` only.
- **Cadence**: an operator-scheduled concern (cron, a managed provider's
  own backup feature), not a TaskForge feature — per §2.2's explicit
  non-goal. The runbook documents a **recommended** cadence (e.g., daily
  full base backup, continuous WAL archiving between them) and states the
  RPO that cadence implies (§13), but does not build a scheduler.
- **What backup mode does NOT protect against**: storage-layer corruption
  below PostgreSQL's own durability guarantees remains explicitly out of
  scope, per [failure-model.md](failure-model.md)'s existing "Storage-layer
  corruption" entry — unchanged by this phase.

## 11. Restore model

**Settled recommendation**:

- **Mechanism**: restore the most recent base backup into a fresh data
  directory, write a `recovery.signal` file (PostgreSQL 12+ mechanism —
  not the pre-12 `recovery.conf`, which no longer exists) with
  `recovery_target_time` set to the desired point in time, and start
  PostgreSQL — it replays archived WAL segments up to that target and
  then either pauses, promotes, or shuts down, per
  `recovery_target_action` (this plan recommends `promote`, so the
  restored instance becomes an ordinary read/write server automatically
  once the target is reached, matching what `internal/invariant.Checker`
  needs to connect to it as an ordinary `*sql.DB`).
- **Documented failure mode for a WAL archive gap** (per §2.5): if a
  requested `recovery_target_time` requires a WAL segment that is
  missing from the archive, PostgreSQL's recovery process **fails with an
  explicit error at that point** (it does not silently skip forward to
  the next available segment, and does not silently stop earlier than
  requested without saying so) — this is PostgreSQL's own documented
  behavior, not a TaskForge mechanism, and the runbook states it plainly
  as the expected, correct failure mode, together with the operational
  remediation (restore to the latest point *before* the gap, and accept
  the resulting narrower RPO for that specific incident).
- **Documented non-issue for `pg_basebackup`-taken backups mid-write**
  (per §2.5): because `pg_basebackup` brackets its own backup-mode
  internally, a backup "taken mid-write" is not a corruption risk for
  `pg_basebackup` itself — the restored base backup is always internally
  consistent once WAL replay (which the restore procedure always performs
  up to at least the backup's own end-of-backup WAL position) completes.
  The runbook states this explicitly as the reason manual
  `pg_backup_start`/`pg_backup_stop` bracketing is unnecessary (§10), and
  notes the scenario applies only to custom backup tooling that bypasses
  `pg_basebackup`.
- **Verification step, mandatory per this phase's own proof obligation**:
  every restore this phase drills (and every restore the runbook
  instructs an operator to perform, as a stated best practice) ends with
  `internal/invariant.Checker.CheckAll` run against the restored database
  before it is trusted — this is the roadmap's own literal proof
  obligation (§2.4), and the runbook states it as a required step, not
  merely something this phase's own test happens to do.

## 12. Disaster-recovery model

The runbook (`docs/disaster-recovery.md`) documents, as one coherent
narrative rather than scattered across sections, the actual operator
decision tree for "the primary is gone":

1. **Deployment layer detects/confirms primary loss** (Patroni, repmgr, a
   managed provider's own health check — not TaskForge; TaskForge has no
   visibility into this step at all).
2. **Deployment layer promotes a standby** (or, absent a standby, the
   operator restores from the most recent backup + WAL archive — this is
   the DR path proper, distinct from the HA/failover path, and the
   runbook states which of the two applies to which incident class:
   *failover* when a synchronized standby exists and is promoted;
   *disaster recovery* when no live standby exists and restore-from-backup
   is the only path, which is necessarily slower and has a real RPO gap
   since the last archived WAL segment).
3. **TaskForge resumes writes** once its configured connection endpoint
   resolves to the new primary — the window this phase's drill measures
   (§8.2, §12.1) for the failover case, and a materially longer window
   (backup restore time, §8.1/§13) for the disaster-recovery case.
4. **Operator verifies** via `internal/invariant.Checker` (a documented,
   operator-runnable command — this plan recommends the runbook show the
   exact `go run`/binary invocation, reusing the checker package Phase 9
   already ships, not proposing a new tool) before declaring the incident
   resolved.

**Explicit statement carried into the runbook, mirroring
[ADR-0001](adr/0001-postgresql-as-source-of-truth.md)'s own "Failure
Implications"**: TaskForge does not silently continue with stale or
divergent state during any part of this sequence — if PostgreSQL is
unreachable, TaskForge is unreachable for writes, by construction (no
in-memory fallback exists anywhere in the codebase), for the entire
duration between primary loss and the deployment layer's own resolution
of it.

## 13. RPO / RTO contracts

The roadmap requires these be **stated**, not merely aspirational
industry-standard numbers. This plan recommends stating them as a
function of the drill's own measured evidence (§8), not inventing a
number the drill has not yet produced:

- **RPO (Recovery Point Objective)**: bounded by the WAL-archiving
  cadence, not the base-backup cadence — an operator using continuous WAL
  archiving (this phase's recommended default, §10) has an RPO bounded by
  the **archive lag** (how far behind `archive_command` can fall before a
  segment is lost if the primary host itself is destroyed, e.g. disk
  loss) — typically seconds to low minutes for a healthy
  `archive_command` shipping to durable storage promptly, but this plan
  does not assert a specific number without the drill measuring
  `archive_command`'s actual observed lag under this phase's reference
  workload (§8.1). **This plan's recommended contract**: "RPO ≈ the
  measured `archive_command` lag under the reference workload, stated as
  an upper bound with the measured value cited, not a marketing-style
  'near-zero' claim."
- **RTO (Recovery Time Objective)**:
  - **Failover path** (standby already exists, promotion only): the
    §8.2 drill's measured reconnect/recovery duration, **plus** whatever
    the deployment's own orchestration tool takes to detect failure and
    promote (which TaskForge's drill does not measure, since it is
    deployment-tool-specific and explicitly out of this phase's scope —
    the runbook states this boundary explicitly, so the two numbers are
    never silently combined into one claimed total).
  - **Disaster-recovery path** (restore from backup, no live standby):
    the §8.1 drill's measured restore duration, **at the reference data
    volume this plan specifies** (§8.1: Phase 9's documented soak-run
    scale). The runbook states plainly that RTO scales with data volume —
    a production `jobs`/`job_attempts` table grown far beyond the
    reference volume (exactly the scenario [data-model.md](data-model.md)'s
    "Phase 12/13 migration lock profile" sections already document
    scaling concerns for) will restore more slowly, and the runbook
    recommends operators re-benchmark against their own real data volume
    before trusting the stated number for their deployment, mirroring
    the same "benchmark before running in production" discipline those
    migration-lock-profile sections already establish for schema changes.
- **This plan does not pre-commit to specific numeric RPO/RTO targets in
  this planning document** — the roadmap's own proof obligation is that a
  stated RPO/RTO exists and the *drilled* time is compared against it;
  inventing a number now, before the drill in §8 has actually run, would
  be exactly the kind of unsupported claim [invariants.md](invariants.md)'s
  and [vision.md](vision.md)'s own discipline exists to prevent. The
  implementation PR that runs the drills is what populates this section's
  actual numbers in the runbook.

## 14. Schema/migration implications

**None.** Direct inspection (§6.1) found no Phase-15-scoped requirement
that touches `jobs`, `job_attempts`, `workflow_instances`,
`workflow_nodes`, or any Phase 12/13 table. No new migration file is
proposed by this plan. This is stated explicitly because every other
enterprise-roadmap phase to date (10 excepted) has shipped at least one
migration, and a reader should not have to infer "no migration" from
silence.

## 15. API / CLI / config implications

**No new HTTP endpoint, no new request/response field, no new
`cmd/taskforge-admin` subcommand.** Per §5's non-goal, this phase's
operator-facing surface is:

- `deploy/pg-dr/backup.sh`, `deploy/pg-dr/restore.sh`,
  `deploy/pg-dr/setup-standby.sh` — plain shell scripts, not Go binaries,
  invoked directly by an operator or by cron/a scheduler the operator
  already runs, mirroring `deploy/postgres-roles.sql`'s existing
  "operator applies this directly against PostgreSQL, TaskForge does not
  run it" precedent.
- **No new `TASKFORGE_*` environment variable for `cmd/api`/`cmd/worker`.**
  §6.2 originally flagged the absence of `SetConnMaxLifetime` as a
  possible gap conditional on drill evidence (OD-1). **OD-1 is now
  CLOSED, negatively**: two independent drills
  ([docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)
  §2) showed the existing, completely unmodified `sql.Open("pgx", ...)`
  pool recovers every pooled connection automatically, with zero process
  restarts, within a window that tracks the raw infrastructure-side
  endpoint-replacement time essentially 1:1 (no stale connection
  materially delayed recovery). No `SetConnMaxLifetime` call, and no new
  `TASKFORGE_DB_CONN_MAX_LIFETIME` config variable, is added by this
  phase.

## 16. Security / trust-boundary implications

- **No new trust boundary is introduced.** The standby/replica PostgreSQL
  instance this phase documents sits inside the same "PostgreSQL, trusted
  by construction" boundary [security-model.md](security-model.md)
  already draws (§"Trust Domains") — it is not a new principal kind, not
  a new credential type, and does not change `internal/principal`'s model
  in any way.
- **Replication credential**: streaming replication requires its own
  PostgreSQL role (conventionally `replicator`, with the `REPLICATION`
  attribute) distinct from `taskforge_api`/`taskforge_worker`/
  `taskforge_retention` (`deploy/postgres-roles.sql`, Phase 12). This
  plan recommends `deploy/postgres-roles.sql` gain a documented
  (not necessarily code-enforced) convention for this role, following the
  same least-privilege discipline Phase 12 already established — a
  replication role should have no access to `jobs`/`job_attempts`/
  `principals`/`api_keys` table contents beyond what physical replication
  inherently requires (which, for physical/streaming replication, is WAL
  access, not row-level `SELECT` — a materially narrower exposure than a
  logical-replication role would need). **This is documentation-only
  scope**, consistent with §5's non-goal of no new production code.
- **Backup artifact confidentiality**: a `pg_basebackup` output (or WAL
  archive) contains the **entire** database, including `payload`/
  `result_metadata` (opaque, potentially sensitive per
  [security-model.md](security-model.md) §3), `api_keys.secret_hash`
  (an HMAC digest, not a raw secret — recoverable only with the
  out-of-database pepper, per [data-model.md](data-model.md)'s `api_keys`
  table), and `principals`/audit-relevant data. The runbook must state
  this explicitly: a backup artifact requires **at least the same**
  access-control and encryption-at-rest discipline as the live database
  itself — this is a new, explicit statement this phase adds to
  [security-model.md](security-model.md) §3's existing "TaskForge has no
  data-classification or retention policy for what operators put in
  [`payload`]" finding, extended to say the same opacity/no-TaskForge-
  level-protection statement applies identically to any backup or replica
  of that data. This is documentation, not a new control — TaskForge
  still does not encrypt or classify `payload` at rest, and this phase
  does not change that.
- **No change to TLS/`sslmode` requirements.** The existing
  `sslmode=verify-full` enterprise-reference-deployment requirement
  (Phase 12, [security-model.md](security-model.md) §4) applies
  identically to a connection against either the primary or a promoted
  standby — this phase adds no new connection-security surface.

## 17. Secrets / credential handling

- **No new secret type.** The replication role's own credential (§16) is
  an ordinary PostgreSQL role password/credential, handled with the exact
  same discipline `.env.example` and [security-model.md](security-model.md)
  §3 already document for `TASKFORGE_DATABASE_URL` and
  `TASKFORGE_API_KEY_PEPPER` — never committed, never logged, rotated
  like any other database credential. This plan does not introduce a
  secrets-manager integration (unchanged non-goal, per
  [security-model.md](security-model.md) §3's existing "no secrets-
  manager integration... a gap only relative to enterprise
  secret-rotation expectations" framing, which Phase 15 does not close).
- **Backup-artifact encryption at rest**: whether `archive_command`
  encrypts WAL segments/base backups in transit to and at rest in their
  storage target is an **operator/deployment decision**, dependent on the
  chosen storage target (object storage with server-side encryption, a
  self-managed encrypted volume, etc.) — this plan documents that the
  decision must be made (§16), but does not mandate or implement a
  specific encryption mechanism, consistent with staying tool-agnostic
  (§5).

## 18. Observability and operator signals

**Settled recommendation, minimal and reuse-first**:

- **No new Prometheus metric is proposed.** This phase's evidence is
  operational/drill evidence (a runbook, a timed measurement, a passing
  invariant check), not steady-state production telemetry that would
  warrant a new `taskforge_*` series. [observability.md](observability.md)'s
  existing `taskforge_stale_completion_rejections_total` and
  `taskforge_lease_expirations_total` are already the correct, existing
  signals an operator would watch *during* a real failover — a spike in
  either, immediately following a promotion, is exactly what fencing
  working correctly under a real failover would look like (stale writes
  from a pre-promotion connection being correctly rejected), and this
  plan recommends the runbook cite these two existing metrics explicitly
  as "what to watch during a live failover," rather than inventing new
  ones that would duplicate signal already emitted.
- **The one new operator signal this phase does add**: the runbook itself
  documents, as an operational checklist item (not a metric), how an
  operator confirms a restore or promotion actually succeeded —
  `internal/invariant.Checker.CheckAll` run against the recovered
  database, per §11's verification step. This plan recommends exposing
  this as a small, documented `go run` invocation (reusing the existing
  `internal/invariant` package directly — it already has no CLI wrapper
  today, only test-internal callers) rather than building a new
  `cmd/taskforge-invariant-check` binary; if implementation finds the
  `go run ./internal/invariant/cmd/...`-style invocation too awkward for
  an operator to run against a production restore, a minimal CLI wrapper
  is a small, justified addition — flagged as OD-5 (§17 below), not
  decided in this plan.

## 19. Compatibility / upgrade implications

- **No interaction with expand/migrate/contract** ([ADR-0010](adr/0010-expand-migrate-contract.md)):
  since this phase ships no migration (§14), there is no new schema state
  for a restored-from-backup database to be incompatible with. A backup
  taken under an older schema version and restored today must still pass
  `internal/migrate.Up` cleanly before `internal/invariant.Checker` is
  run against it — the runbook states this ordering explicitly (restore
  → `migrate.Up` if the backup predates the current schema → invariant
  check), reusing the existing migration tooling rather than inventing a
  restore-specific migration path.
- **Interaction with Phase 14's two-binary-version proof**: orthogonal,
  not overlapping — Phase 14 proves two *code* versions coexist safely
  against one database across a schema change; Phase 15 proves one code
  version behaves correctly across a *database instance* change (restore,
  promotion). This plan does not combine the two into a single harness;
  §8's `test/dr` package is deliberately separate from `test/compat`,
  though both reuse the same "real binaries via `os/exec`" pattern.
- **Interaction with Phase 13 retention**: a large `jobs`/`job_attempts`
  table (grown because retention is disabled, or configured with a long
  TTL) directly increases restore time (§13's RTO discussion) and backup
  size — this plan recommends the runbook cross-reference
  [phase-13-plan.md](phase-13-plan.md)'s retention configuration as a
  lever an operator can use to bound both, rather than treating table
  growth as an unbounded, unaddressable RTO risk.

## 20. Failure modes (this phase's own, beyond §9's F21)

| Failure mode | Documented behavior |
|---|---|
| Restore attempted from a backup taken mid-write, via `pg_basebackup` | Not a corruption risk — `pg_basebackup` brackets its own backup-mode internally (§10, §11). |
| Restore attempted from a backup taken mid-write, via **custom** backup tooling bypassing `pg_basebackup` | Explicitly out of this phase's proof scope — the runbook states `pg_basebackup` is the only backup mechanism this phase documents/drills, and warns that any operator-substituted mechanism must independently ensure backup-mode correctness (manual `pg_backup_start`/`pg_backup_stop`), which this project does not test. |
| WAL archive gap during restore | Recovery fails with an explicit PostgreSQL error at the missing segment, not a silent skip-forward (§11). |
| Standby promoted while TaskForge is still connected to the old (now-demoted) primary | The old primary rejects further writes once demoted (PostgreSQL's own behavior); TaskForge's pooled connections to it fail on next use and the process falls back to its existing retry/backoff (worker) or returns an error (API), until the connection endpoint resolves to the new primary — the exact window is what §8.2's drill measures. |
| A job's lease was acquired under the pre-promotion primary and the worker is mid-execution when the primary is lost | Fencing (TF-INV-002/003/014) is unaffected by which physical PostgreSQL instance enforced it — lease state is durable, replicated data, so the promoted standby enforces the identical fencing rule against the identical row. The open question this phase's drill answers empirically is only *timing* (how long until the worker can report completion or be safely reclaimed), never *correctness* (§8.2 step 7). |
| Two workers, one still talking to the old (now-unreachable) primary and one talking to the newly-promoted standby, momentarily coexist | Cannot produce a split-brain completion: the old primary is unreachable for writes (it is either stopped or refuses writes once demoted by the promotion), so a worker still trying to reach it simply fails/retries — it can never durably record a conflicting completion against a *different* database, because there is only ever one instance accepting writes at a time under a correctly-configured PostgreSQL replication topology (this is PostgreSQL's own replication guarantee, not a TaskForge mechanism, and the runbook states this boundary explicitly, including the caveat that a *mis*-configured topology allowing two simultaneous writable primaries — split-brain at the PostgreSQL level — is exactly the failure class §2.2's non-goal ("no split-brain detection... deployment-layer tooling's job") places outside TaskForge's and this phase's own scope). |

## 21. Invariant mapping

Per [invariants.md](invariants.md)'s own "Cross-Phase Governance
Additions" method (already applied once, cleanly, to Phase 14 — see
"Phase 14 reviewed: no new invariant added"): a new `TF-INV-*` ID is
allocated only for a durable, state-machine-level safety property of the
kind `internal/invariant.Checker` can check against row-level state — not
for a deployment/operational proof obligation, however important.

**Phase 15 reviewed against that method: adds no new invariant.**

| Candidate | Disposition | Why |
|---|---|---|
| "A restore reproduces a database where every existing `TF-INV-*` holds" | **Not a new invariant.** Proof obligation against the existing set. | This is a breadth requirement — "every existing invariant, proven against a new *database instance* rather than a new adversarial code path" — exactly the same category [invariants.md](invariants.md) already used to close out Phase 14 ("a breadth requirement across a new adversarial condition, not a new state-machine property"). |
| "Fencing (TF-INV-002/003/014) holds across a standby promotion" | **Not a new invariant.** Proof obligation against TF-INV-002/003/014 specifically. | Fencing is a property of the *data* (`lease_owner`/`lease_generation` columns and the conditional-`UPDATE` mechanism that reads/writes them), which a promoted standby serves identically — this phase's drill is evidence the existing mechanism is unaffected by which physical instance is primary, not a new mechanism. |
| "TaskForge resumes writes within a measured, bounded window after promotion" | **Not an invariant.** Operational/SLO-shaped expectation, timing-only. | No durable row-level state property is being asserted — this is a stopwatch measurement, the same category [invariants.md](invariants.md) already excluded Phase 13's backpressure-liveness expectation from. |
| "A WAL archive gap fails recovery loudly rather than silently skipping" | **Not an invariant.** PostgreSQL's own documented behavior, not a TaskForge-enforced property. | TaskForge builds no code path here at all (§5, §14) — there is nothing for `internal/invariant.Checker` (or any TaskForge mechanism) to check, because PostgreSQL itself is what refuses to proceed. |

**Proof obligation, restated plainly**: every existing `TF-INV-001`
through `TF-INV-019` must hold, checked via
`internal/invariant.Checker.CheckAll`, against (a) a database restored
via §8.1's drill, and (b) a database that has just undergone the §8.2
promotion drill while live traffic was in flight. Both are proof
obligations against the existing registry, not additions to it.

## 22. Test/proof matrix

New scenario IDs, continuing from the corpus's current end (SF-070, per
[scenario-corpus.md](scenario-corpus.md)/[testing-strategy.md](testing-strategy.md);
next available is **SF-071**). Per this project's convention, these are
proposed here (mirroring how [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md)
pre-allocated SF-051–059 before Phase 13's own implementation, and how
Phase 14's plan pre-allocated SF-063–070 before implementation) — folding
them into `scenario-corpus.md`/`testing-strategy.md` proper is
implementation-PR scope, not this planning pass's.

| Scenario | Initial state | Actions | Fault | Expected outcome | Test |
|---|---|---|---|---|---|
| **SF-071** — Backup-then-restore drill, timed | Primary running reference-volume workload (§8.1), WAL archiving live | `pg_basebackup` taken mid-workload; workload continues; `recovery_target_time` recorded; workload stops; restore to a fresh directory targeting that time | None (happy-path drill) | Restored DB reaches the target time, promotes to read/write, `internal/invariant.Checker.CheckAll` reports zero violations; restore wall-clock time recorded against RTO | `test/dr/backup_restore_test.go` |
| **SF-072** — WAL archive gap fails recovery loudly | Same as SF-071, but a WAL segment between the backup and the target time is deliberately deleted from the archive before restore | Restore attempted with the deliberate gap | WAL archive gap | Recovery fails at the missing segment with an explicit PostgreSQL error; the restored instance does not silently promote past the gap or skip forward | `test/dr/backup_restore_test.go` |
| **SF-073** — Controlled standby-promotion/failover drill, live traffic | Primary + streaming standby, real `cmd/api`/`cmd/worker` binaries driving live traffic against the primary | Primary forcibly stopped (`pg_ctl stop -m immediate`); standby promoted onto the same endpoint | Primary loss | TaskForge resumes writes within a measured, recorded window (both API-submission and worker-claim timing recorded separately); `internal/invariant.Checker` reports zero violations across the whole drill; no stale pre-promotion write succeeds against the new primary | `test/dr/failover_drill_test.go` |
| **SF-074** — In-flight lease survives a promotion mid-execution | As SF-073, but a job is claimed and actively `RUNNING` (heartbeating) at the moment the primary is stopped | Primary stopped mid-heartbeat-interval; standby promoted; the worker's next heartbeat/completion attempt is against the (now-promoted) same endpoint | Primary loss during an open lease | Either the heartbeat/completion succeeds against the promoted standby (lease/fencing state was replicated) and the job proceeds normally, or it fails/times out and the job is later reclaimed via the ordinary, unmodified TF-INV-004 path once `lease_expires_at` passes — never a duplicated or silently lost completion | `test/dr/failover_drill_test.go` |
| **SF-075** — Backup/restore preserves fencing history, not just current state | Restore a database (SF-071's mechanism) taken after at least one lease-generation advance (a reclaim already occurred pre-backup) | Restore to a target time after the reclaim | None | The restored database's `lease_generation` sequence for the affected job is intact and monotonic (checked by `internal/invariant.Checker`'s existing `checkLeaseGenerationMonotonicUnique`), proving a restore does not itself introduce a fencing regression | `test/dr/backup_restore_test.go` |

**Invariant-to-test matrix addition** (for
[testing-strategy.md](testing-strategy.md), implementation-PR scope): all
five scenarios above map to the **full existing `TF-INV-001`–`TF-INV-019`
set**, checked via `internal/invariant.Checker.CheckAll` — per §21, this
is breadth proof, not new-invariant proof, exactly mirroring how Phase
14's SF-066/067 are listed against "the full set" in
[invariants.md](invariants.md)'s "Phase 14 reviewed" note rather than
against one specific invariant ID.

## 23. Real-process/integration proof requirements

All five scenarios above require **real PostgreSQL server processes**
(not embedded-in-the-test-binary mocks, not a single shared instance) and,
for SF-073/SF-074, **real, separately-running `cmd/api`/`cmd/worker`
binaries** driven via `os/exec` — mirroring Phase 14's
`test/procs`/`test/compat` precedent exactly (§8, §3's own citation of
that precedent). This is a **new test category** for this project's own
`testing-strategy.md` table, alongside Phase 14's "Mixed-binary-version /
OS-process tests" entry:

**Recommended new row for [testing-strategy.md](testing-strategy.md)'s
Test Categories table** (implementation-PR scope): "**Multi-instance
PostgreSQL tests (Phase 15)** | Two or more real, independently-running
PostgreSQL server processes (a primary and a standby/restore target),
coordinated via real `pg_basebackup`/streaming-replication/promotion
commands run through `os/exec` — never a single shared instance
standing in for both roles, and never a simulated promotion. This is a
new category because every earlier PostgreSQL-touching test in this
project's history (Phase 1 through 14) uses exactly one PostgreSQL
instance. | `test/dr` (SF-071 through SF-075)."

## 24. Failure-injection requirements

- **Primary termination**: `pg_ctl stop -m immediate` against the
  primary's own data directory (a real process stop, the closest
  available analogue in a single-test-host harness to an actual primary
  host failure) — not `internal/chaos.TerminateBackend`, which severs one
  *connection*, not the server process itself (§8.2 step 4 already states
  this distinction explicitly).
- **WAL archive gap** (SF-072): deliberate deletion of one archived
  segment file from the test-scoped archive directory between backup and
  restore — a filesystem-level fault injection, not a PostgreSQL-level
  one, and the simplest faithful reproduction of "an operator's
  `archive_command`/storage target lost a segment."
- **Reused, not reinvented**, where applicable: `internal/chaos`'s
  existing `ForceExpireLease`/`ForceSetEligibleAt`-style DB-time
  manipulation helpers remain available to `test/dr` for constructing
  SF-074's "job actively `RUNNING`, mid-lease" precondition deterministically
  (rather than a sleep-based race), consistent with
  [testing-strategy.md](testing-strategy.md)'s existing determinism
  requirement.

## 25. Rollout / rollback strategy

- **This phase ships no product code to roll out.** OD-1 is now CLOSED,
  negatively (§17, [docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)):
  no `SetConnMaxLifetime` or other connection-pool change is made, so
  there is no `cmd/api`/`cmd/worker` behavior change to roll back at all.
  The runbook, scripts, and `test/dr` package are purely additive
  documentation/tooling.
- **Operational rollout**: this phase's actual "rollout" is an operator
  adopting the documented HA/backup topology in their own deployment —
  entirely outside TaskForge's own release process, exactly as Phase 12's
  `sslmode=verify-full` and least-privilege-role recommendations already
  are (documented deployment obligations, not something a TaskForge
  release enables or disables).
- **Runbook review requirement** (per the roadmap's own §2.6): the
  written runbook must be reviewed for clarity by an engineer who did not
  write it, and that review is itself part of this phase's exit
  criteria's evidence, not a separate optional step.

## 26. Implementation order

Following the same "cheapest, least-coupled, least-novel first" logic
prior plans use, deferring the largest, most novel infrastructure (the
failover drill, which depends on the backup/restore harness's own
two-instance test scaffolding) to last. **OD-1 and OD-2, both originally
listed as blocking checks at the head of this sequence, are now CLOSED**
(prerequisite evidence pass, [docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)) —
the order below reflects that: item 2 no longer carries an open
verification risk, and the former item 6 ("OD-1's resolution") is removed
outright, since its answer (no code change) is already known and requires
no further implementation step.

1. **`deploy/pg-dr/backup.sh` + `deploy/pg-dr/restore.sh`**, hand-tested
   once manually against a local PostgreSQL instance (not yet via
   automated `test/dr`), to validate the scripts themselves before
   building test infrastructure around them. The manual commands this
   evidence pass already ran against a real PostgreSQL 16 instance
   (`docs/phase-15-postgres-evidence.md` §2.1 steps 1-2: `initdb`,
   `postgresql.conf` WAL-archiving settings, `pg_basebackup -R`) are a
   direct, reusable starting point for these scripts' own content, not a
   from-scratch design task.
2. **`test/dr` package scaffolding**: the `embedded-postgres`-with-custom-
   `StartParameters` primary (§8). **OD-2 is CLOSED — no verification
   risk remains here.** Implementation should pin
   `.Version(embeddedpostgres.V16)` explicitly (the one concrete action
   item the evidence pass surfaced, §8/OD-2) and can otherwise build
   directly on the already-proven `StartParameters` configuration
   (`docs/phase-15-postgres-evidence.md` §3.2's harness source is a
   direct starting point).
3. **SF-071/SF-072 (backup/restore drill)** — depends only on item 2; no
   dependency on a second PostgreSQL instance.
4. **`deploy/pg-dr/setup-standby.sh` + the second (standby) instance in
   `test/dr`** — depends on item 2's primary-with-WAL-archiving already
   working, since a streaming standby is seeded from it. The exact
   `pg_basebackup -R`/promote/repoint sequence this evidence pass already
   ran twice, successfully, against real PostgreSQL 16
   (`docs/phase-15-postgres-evidence.md` §2.3) is the direct blueprint for
   this script and for `failover_drill_test.go`'s own driving logic.
5. **SF-073/SF-074/SF-075 (failover drill)** — depends on item 4 and on
   the real-binary `os/exec` harness pattern (reused, not rebuilt, from
   Phase 14's `test/compat`/`test/procs`). SF-073's own measurement
   methodology is already validated end-to-end by this evidence pass's
   OD-1 trials (§2.4) — the implementation task is to fold that
   methodology into the permanent, CI-runnable `test/dr` package (driving
   real `cmd/api`/`cmd/worker` binaries rather than this pass's
   `internal/store`-direct harness, per §27 OD-3), not to re-derive it.
6. **`docs/disaster-recovery.md`** (the runbook itself) — written after
   items 3 and 5 land as permanent, passing tests, so every number and
   procedure it states is backed by a repeatable, CI-checked drill rather
   than only this evidence pass's one-off manual trial. This evidence
   pass's own measured numbers (§2.4, §3.3) are usable as the runbook's
   *first* RPO/RTO reference points, refined once the permanent `test/dr`
   harness reproduces them on real CI hardware.
7. **ADR-0011** (§17, if adopted) — written alongside or immediately
   after the runbook, citing its now-real evidence.
8. **`failure-model.md`/`security-model.md`/`data-model.md` cross-reference
   updates** (§9's F21 addition, §16's replication-role note) — small,
   final documentation passes once everything above has landed.

## 27. Open architectural decisions

### OD-1 — CLOSED: `cmd/api`/`cmd/worker` do NOT need `SetConnMaxLifetime`
(or any other connection-pool code change)

**Status: CLOSED, by empirical prerequisite-evidence drill, per §29.**
§6.2 found no `SetConnMaxLifetime`/`SetMaxOpenConns` tuning anywhere in
the current codebase, and this plan originally left whether that mattered
as an open, drill-decided question — deliberately not answerable by
reading code alone. **Resolution**: two real drills (a real PostgreSQL 16
primary + streaming standby, promoted onto the primary's own vacated
endpoint, TaskForge's completely unmodified `internal/store`/
`sql.Open("pgx", ...)` pool driving real traffic throughout — one at a
single pooled connection, one at 20 concurrent pooled connections) showed
the existing, unmodified pool recovers every connection automatically,
with zero process restarts, in 514ms–857ms — tracking the independently
measured 696–703ms infrastructure-side endpoint-replacement time
essentially 1:1. No stale pooled connection blocked or materially delayed
recovery in either trial. **Decision: no code change.** Full method,
exact commands/timestamps, and raw output:
[docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md) §2.

### OD-2 — CLOSED: `embedded-postgres`'s `StartParameters` surface DOES
support `wal_level`/`archive_mode`/`archive_command` injection; no fallback
needed

**Status: CLOSED, by empirical prerequisite-evidence drill, per §29.**
§8 originally flagged this as a load-bearing assumption not yet
independently verified against that library's source. **Resolution**:
direct source inspection (`config.go`/`embedded_postgres.go`) confirmed
the mechanism (`StartParameters` values passed through as `pg_ctl start
-o "-c key=\"value\""`), and a live drill proved it works completely —
not merely that `SHOW wal_level`/`SHOW archive_mode` round-trip correctly,
but that PostgreSQL's own archiver process actually archives a real,
complete 16 MiB WAL segment under a `StartParameters`-supplied
`archive_command`. Direct source inspection additionally confirmed
`EmbeddedPostgres.Start()` reuses (does not reinitialize) a pre-populated
data directory — the mechanism §8.1's restore drill needs to start a
recovering/restored instance through the same library. **Decision: the
raw `initdb`/`postgres` fallback is not needed.** One small,
non-blocking implementation-time action item this evidence surfaced: pin
`.Version(embeddedpostgres.V16)` explicitly when `test/dr` is built, for
fidelity with the project's actual deployment-target PostgreSQL version
(this evidence pass's cached binary was incidentally 18.3, which does not
affect the validity of the `StartParameters` finding itself). Full
method, exact commands, and raw output:
[docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md) §3.

### OD-3 — Does the backup/restore drill's workload generator drive real
`cmd/api`/`cmd/worker` binaries, or call `internal/store` directly?

**This plan's recommendation, stated as settled but flagged as an open
decision an implementer could reasonably override**: for §8.1
specifically (backup/restore), this plan recommends driving workload
**directly through `internal/store`** (faster, simpler, no `os/exec`
binary-build overhead) rather than real compiled binaries — because the
property under test is "does a *database* restore correctly," which does
not depend on which client process wrote the data. For §8.2
(failover), real binaries **are** required (settled, not open) because
the property under test is explicitly "TaskForge's own process-level
reconnect behavior," which only real `cmd/api`/`cmd/worker` processes can
demonstrate. This split (direct-store for §8.1, real-binaries for §8.2)
is this plan's recommendation; an implementer preferring real binaries
for both, for harness-consistency reasons, is not blocked by anything
else in this plan.

### OD-4 — Is "promote the standby onto the primary's own now-vacated
port" an adequate proxy for a real deployment's connection-repointing
mechanism?

**Status: open, explicitly flagged in §8.2 step 5 as a deliberate
simplification, not a claim of equivalence.** This plan's answer: it is
adequate for proving *TaskForge's own* reconnect behavior (the thing this
phase's roadmap text actually asks to be measured), but it is **not**
adequate as a substitute for testing any specific real HA-orchestration
tool's own promotion/DNS/VIP-repointing latency, which is explicitly out
of scope (§2.2, "no TaskForge-built failover orchestration" — and, by
extension, no TaskForge-built *test* of a third-party orchestrator's own
behavior either). The runbook must state this boundary explicitly so a
reader does not mistake the drill's measured number for "how fast Patroni
fails over," which it does not measure.

### OD-5 — Does `internal/invariant.Checker` need a minimal operator-facing
CLI wrapper?

**Status: open, non-blocking.** §18 recommends documenting a `go
run`-style invocation first and only building a dedicated CLI if that
proves too awkward during runbook-writing (§26 item 7). This does not
block any other item in this plan.

## 28. Genuinely unresolved architectural decisions (summary, per the
task's own required framing)

Restating §27 as a flat list. **OD-1 and OD-2 are now CLOSED** (see the
"Post-planning update" note at the top of this document and
[docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)) —
retained here, marked closed, for a complete record rather than deleted,
since the task instructions ask this section to summarize the full OD
set:

1. **CLOSED.** Whether `cmd/api`/`cmd/worker`'s connection pool needs
   `SetConnMaxLifetime` (or equivalent) added — decided empirically:
   **no**. Two real drills showed the unmodified pool recovers every
   connection automatically, in a window tracking raw infrastructure
   endpoint-replacement time 1:1, with zero restarts needed (OD-1,
   [evidence](phase-15-postgres-evidence.md) §2).
2. **CLOSED.** Whether `embedded-postgres`'s parameter-injection surface
   is sufficient for WAL-archiving configuration, or whether a raw
   `initdb`/`postgres` fallback is required — decided by direct
   verification and a live drill: **sufficient; no fallback needed**
   (OD-2, [evidence](phase-15-postgres-evidence.md) §3).
3. Whether the backup/restore drill's workload generator should be
   direct-`internal/store` or real-binary-based — **this plan recommends
   direct-store, flagged as overridable** (OD-3).
4. Whether "same-port promotion" is an adequate proxy for a deployment's
   real connection-repointing mechanism — **this plan states it is
   adequate for TaskForge's own reconnect-behavior proof only, explicitly
   not for testing any specific orchestration tool** (OD-4).
5. Whether a dedicated invariant-checker CLI is needed — **non-blocking,
   decided during runbook-writing** (OD-5).
6. Whether to name one specific HA-orchestration tool (Patroni/repmgr/a
   managed provider) as *the* recommendation, or stay tool-agnostic —
   **this plan recommends staying tool-agnostic** (§5), consistent with
   the roadmap's own framing, but this is a genuine judgment call a
   future reviewer could reasonably want revisited once an actual
   deployment target is known.
7. Whether the runbook should state a specific numeric RPO/RTO target in
   this planning document before any drill has run — **this plan
   deliberately declines to invent one** (§13); the number is populated
   by implementation once the drill produces real evidence.

## 29. Prerequisite ADR / evidence determination

**PREREQUISITE ADR/EVIDENCE REQUIRED: YES — and, for the two
implementation-precondition questions this required (OD-1, OD-2), now
SATISFIED.** See
[docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md) for
the full record.

This was never the roadmap's own "Prerequisites" field (§2.3 correctly
states **no cross-phase prerequisite** — Phase 15 does not need Phase 11–
14 or 16 to land first). It was a narrower, phase-internal prerequisite
this plan itself identified: **a real failover drill and a real
`embedded-postgres` capability check had to run, and their evidence had
to be examined, before any decision could be made about whether
TaskForge's own connection-pool code needs to change (OD-1) or whether
`test/dr` needs a raw-binary fallback (OD-2).** Skipping straight to "add
`SetConnMaxLifetime` because it seems prudent," or straight to "build the
raw-binary fallback because embedded-postgres might not support it,"
would have been exactly the kind of invented-without-evidence behavior
the task's own instructions warn against. That prerequisite pass has now
run (§2, §3 of the evidence document) and both ODs are closed, negatively
for OD-1 (no code change) and positively for OD-2 (no fallback needed).

**This prerequisite pass is deliberately narrower than, and is not a
substitute for, Phase 15's own full roadmap-stated proof obligations.**
The evidence document's own §7 "Limitations" states this explicitly: no
`test/dr` package exists yet, SF-071 through SF-075 are not yet
implemented as permanent, CI-runnable tests, no `docs/disaster-recovery.md`
runbook exists, and the roadmap's own "at least one full, timed
backup-then-restore drill" and "at least one controlled
standby-promotion/failover drill" exit criteria (§2.6/§30) are not yet
satisfied by a committed, reproducible artifact — only by this pass's
one-off manual trials, which exist to unblock *design* decisions (OD-1,
OD-2), not to themselves be the drills the roadmap's exit criteria cite.
Implementation (§26) still owes the permanent, repeatable versions of
these drills.

**Prerequisite ADR**: **recommended, non-blocking** (unlike Phase 13's
roadmap-mandated OD-1 blocking ADR). This plan recommends **ADR-0011**,
written after the drills produce evidence (§26 item 8), documenting the
decision this phase actually makes with a real alternative genuinely
weighed: *TaskForge documents and drills PostgreSQL-native HA/backup/DR
primitives, and deliberately builds no failover-orchestration or
backup-scheduling code of its own* — with the rejected alternatives
being (a) building TaskForge-native replication/failover machinery
(rejected: duplicates what PostgreSQL and mature third-party tooling
already solve, and ADR-0001 already made this call at the storage-layer
level) and (b) picking and hard-coding assumptions about one specific
third-party orchestration tool (rejected per §5/OD-4/OD-6, staying
tool-agnostic). This mirrors exactly the shape
[ADR-0010](adr/0010-expand-migrate-contract.md) took for Phase 14: not
roadmap-mandated, but written because [docs/adr/README.md](adr/README.md)'s
own test ("a decision that was actually weighed against a real
alternative") is met.

**Prerequisite empirical evidence**: **required and central to this
phase, not incidental to it.** Per §2.6, the roadmap's own tests/evidence
requirement is not satisfiable by documentation alone — "at least one
full, timed backup-then-restore drill," "at least one controlled
standby-promotion/failover drill," both with measured, recorded
durations, are the literal deliverable. This plan's own §26 implementation
order reflects that: the runbook (item 7) is sequenced *after* the drills
(items 3 and 5) specifically so it describes evidence that exists, not an
aspiration that has not yet been tested.

---

## 30. Exit criteria

Reproduced from [enterprise-roadmap.md](enterprise-roadmap.md) §2.7,
annotated with this plan's own mapping to the sections above (not yet
checked — no implementation has occurred):

- [ ] A documented `pg_basebackup`/WAL-archiving-based backup exists (with
      the `pg_backup_start`/`pg_backup_stop` misconception corrected in
      the runbook) and a restore-from-backup drill has been executed and
      timed at least once. → §10, §11, §8.1, SF-071.
- [ ] A stated RPO/RTO exists and the drilled restore time is compared
      against it. → §13.
- [ ] The restored database passes the existing invariant checker. → §11
      "Verification step," §21, SF-071/SF-075.
- [ ] An HA topology recommendation is documented, with its tradeoffs
      (sync vs. async replication) stated per PostgreSQL's own guidance.
      → §12, §5 (tool-agnostic framing).
- [ ] At least one controlled standby-promotion/failover drill has been
      run, with TaskForge's reconnect/recovery behavior measured and
      recorded. → §8.2, SF-073/SF-074.
- [ ] If read replicas are recommended for any read path, the
      consistency/read-after-write implications are documented before
      adoption. → §5 non-goal (this plan does not recommend adoption; the
      implication is documented conditionally, in the runbook, for a
      future deployment's own decision).

## 31. Cross-references

- **Prerequisite evidence (OD-1, OD-2 closure)**:
  [docs/phase-15-postgres-evidence.md](phase-15-postgres-evidence.md)
- Invariants: [invariants.md](invariants.md)
- Scenarios: [scenario-corpus.md](scenario-corpus.md) (SF-071 through
  SF-075 proposed here, folded in at implementation time)
- Failure model: [failure-model.md](failure-model.md) (F21 proposed here)
- Security model: [security-model.md](security-model.md) "Enterprise
  Deployment Profile," §3/§4 (backup-artifact confidentiality, TLS)
- Data model: [data-model.md](data-model.md) ("Phase 12/13 migration lock
  profile" sections, cited for the "benchmark before production" pattern
  this plan's RTO guidance reuses)
- Compatibility policy: [compatibility-policy.md](compatibility-policy.md)
  (expand/migrate/contract, cited §19, unaffected by this phase)
- Reference analysis: [reference-analysis.md](reference-analysis.md)
  (PostgreSQL HA/backup rows, the origin of this phase's scope)
- ADR-0001: [PostgreSQL as sole source of truth](adr/0001-postgresql-as-source-of-truth.md)
  (the decision this phase operationalizes, does not reverse)
- ADR-0002: [Lease-based worker ownership with fencing](adr/0002-lease-based-worker-ownership.md)
  (the mechanism §8.2/§21's fencing proof obligation is against)
- ADR-0010: [Expand/migrate/contract](adr/0010-expand-migrate-contract.md)
  (structural precedent for this plan's recommended ADR-0011, §29)
- Prior phase plans this plan mirrors in structure:
  [phase-13-plan.md](phase-13-plan.md), [phase-14-plan.md](phase-14-plan.md)
- Roadmap: [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 15 —
  PostgreSQL HA / Backup / DR Proof"
