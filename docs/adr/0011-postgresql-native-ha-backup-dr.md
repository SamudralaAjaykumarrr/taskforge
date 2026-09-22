# ADR-0011: PostgreSQL-Native HA/Backup/DR Primitives, Not TaskForge-Built Machinery

Status: Accepted

## Context

[enterprise-roadmap.md](../enterprise-roadmap.md) "Phase 15 — PostgreSQL
HA / Backup / DR Proof" names a real, undischarged gap:
[failure-model.md](../failure-model.md)'s own Failure Catalog states
"PostgreSQL primary failure/failover... out of scope for v1" as an
honest, unexamined boundary, and [ADR-0001](0001-postgresql-as-source-of-truth.md)'s
"Failure Implications" already accepted that TaskForge is down for writes
whenever PostgreSQL is unreachable, without ever having drilled what that
actually means in practice — how long, how correctly fencing survives it,
whether a restore actually works.

[phase-15-plan.md](../phase-15-plan.md) planned this phase in detail
before any code was written, including a prerequisite empirical-evidence
pass ([phase-15-postgres-evidence.md](../phase-15-postgres-evidence.md))
that closed two implementation-precondition questions (OD-1: no
connection-pool code change needed; OD-2: `embedded-postgres`'s
`StartParameters` surface is sufficient for WAL-archiving test
infrastructure, no raw `initdb`/`postgres` fallback needed) before
implementation began. That plan's own §29 recommended this ADR,
non-blocking, written after the real drills produce evidence — this is
that ADR, and the evidence now exists:
[docs/disaster-recovery.md](../disaster-recovery.md) documents the
drilled backup/restore/failover procedures and their measured RPO/RTO;
`test/dr/` is the permanent, CI-runnable test code that produced those
numbers, not a one-off manual exercise.

## Decision

TaskForge documents and drills **PostgreSQL-native HA/backup/DR
primitives** (`pg_basebackup`, continuous WAL archiving, streaming
replication, `recovery_target_time`/`recovery_target_action=promote`),
plus **tool-agnostic deployment-layer integration** for automated
failover orchestration (Patroni, repmgr, or a managed provider's own
failover — named as a category, not a specific product). TaskForge
deliberately builds **no** failover orchestration, consensus, split-brain
detection, or backup-scheduling code of its own.

TaskForge's own, in-scope responsibility is narrower and concrete:

1. **Operator tooling** that invokes those PostgreSQL-native primitives
   consistently and correctly (`deploy/pg-dr/backup.sh`,
   `deploy/pg-dr/restore.sh`, `deploy/pg-dr/setup-standby.sh`) — plain
   shell scripts, never compiled into `cmd/api`/`cmd/worker`.
2. **Proof that TaskForge's own client behavior** — its connection pool,
   its worker claim/heartbeat/completion path, its fencing mechanism
   (ADR-0002) — survives a real restore and a real failover, drilled as
   permanent test code (`test/dr/`), not asserted.
3. **A minimal, operator-facing invariant-verification tool**
   (`cmd/taskforge-invariant-check`) so the mandatory post-restore/
   post-promotion verification step is a single runnable command, not
   throwaway code an operator has to write.
4. **This runbook** ([docs/disaster-recovery.md](../disaster-recovery.md)),
   stating a measured RPO/RTO and the HA topology's sync/async tradeoff,
   written so a different engineer can reproduce both drills.

No new schema migration, no new HTTP endpoint, and no change to
`internal/store`'s public API or any invariant-enforcing SQL statement
(confirmed by direct inspection during planning, [phase-15-plan.md](../phase-15-plan.md)
§14, and unchanged by implementation).

## Alternatives Considered

- **Build TaskForge-native replication/failover machinery** (a
  TaskForge-level leader-election, log-shipping, or quorum mechanism
  layered above PostgreSQL). Rejected: this duplicates what PostgreSQL
  itself, and mature third-party orchestration tooling, already solve
  correctly and at far greater operational maturity than a first
  implementation of the same problem inside this project could achieve.
  [ADR-0001](0001-postgresql-as-source-of-truth.md) already made the
  storage-layer version of this call ("PostgreSQL as the sole source of
  truth," not a TaskForge-built durability layer); this would have
  reversed that decision's own logic one layer up, for no offsetting
  benefit — [failure-model.md](../failure-model.md)'s "down for writes,
  never silently divergent" contract is exactly what a bespoke
  half-built failover layer would put at risk.
- **Pick and hard-code one specific HA-orchestration tool** (Patroni,
  repmgr, or a specific managed provider) as *the* TaskForge
  recommendation, with deployment tooling or documentation assuming it.
  Rejected, per [phase-15-plan.md](../phase-15-plan.md) §5/§27 OD-4/OD-6's
  own framing: TaskForge's actual proof obligation is that *its own*
  client behavior (pool recovery, fencing) survives a promotion **given**
  a working connection-endpoint-repointing mechanism — not that any one
  orchestration tool's own detection/promotion speed is good. Naming one
  tool would have invited exactly the confusion
  [docs/disaster-recovery.md](../disaster-recovery.md) §6.3 now states
  explicitly: a reader mistaking this project's measured reconnect-time
  evidence for a claim about a specific third-party tool's own failover
  speed, which it does not measure and is not this project's to test.
- **Skip the drill; document the procedure from PostgreSQL's own manual
  without running it against TaskForge.** Rejected: this is precisely the
  "argument, not evidence" gap [phase-15-plan.md](../phase-15-plan.md) §1
  set out to close, and the roadmap's own "Tests / evidence required"
  (§2.6) is explicit that a timed, drilled restore and a drilled failover
  are the literal deliverable, not a description of one. The drills
  themselves surfaced a real, previously-unknown gap that pure
  documentation-from-the-manual would have missed entirely:
  `embedded-postgres`'s vendored binary distribution does not include
  `pg_basebackup` at all (only `initdb`/`pg_ctl`/`postgres`) — a fact no
  amount of reading PostgreSQL's own documentation would surface, only
  running the real tooling against the real test infrastructure did.

## Consequences

- Positive: TaskForge's own reconnect/fencing behavior across a real
  restore and a real failover is now a measured, documented,
  drilled-at-least-once operational fact
  ([docs/disaster-recovery.md](../disaster-recovery.md) §5), not an
  unexamined assumption — directly closing the gap
  [failure-model.md](../failure-model.md) and
  [ADR-0001](0001-postgresql-as-source-of-truth.md) both left honestly
  open.
- Positive: the runbook and the tested behavior can never silently
  diverge — `test/dr/` invokes the exact same `deploy/pg-dr/*.sh` scripts
  the runbook instructs an operator to run, not a reimplementation.
- Positive: [security-model.md](../security-model.md)'s "Enterprise
  Deployment Profile" assumption ("the database is deployed with
  replication/failover per Phase 15") is now backed by a real,
  drilled procedure rather than a forward-reference to a phase that had
  not yet run.
- Negative / accepted limitation: the measured RTO/RPO numbers
  ([docs/disaster-recovery.md](../disaster-recovery.md) §5) are
  reference-volume, reference-environment numbers (Phase 9's documented
  soak-run scale, a local-filesystem `archive_command`), not a
  production SLA — the runbook states this explicitly and directs an
  operator to re-benchmark against their own real data volume and
  storage target before trusting them.
- Negative / accepted limitation: this project depends on a real
  `pg_basebackup` client binary being present wherever `deploy/pg-dr`'s
  scripts (or `test/dr`'s own tests) run — an ordinary `postgresql-client`
  OS package for an operator, but a genuinely new environment dependency
  for this repository's own CI/test story, absent from
  `embedded-postgres`'s vendored binaries. `test/dr` skips (does not
  fail) when none is found, rather than either silently passing or
  breaking CI outright.
- Negative / accepted limitation: `cmd/api`'s and `cmd/worker`'s measured
  post-failover recovery windows differ materially (§5.2's ~800ms vs.
  ~6s) because of pre-existing, unrelated retry-shape differences between
  the two processes, not anything this phase changed — an operator with
  worker-claim-latency-sensitive SLOs should read this asymmetry
  carefully rather than assume a single "TaskForge failover time" number.

## Failure Implications

Unchanged from [ADR-0001](0001-postgresql-as-source-of-truth.md) and
[failure-model.md](../failure-model.md)'s F6/F21: if PostgreSQL is
unreachable, TaskForge is unreachable for writes, by construction, for
the entire duration between primary loss and the deployment layer's own
resolution of it — this ADR does not reverse that; it measures and
documents the duration and correctness of that window once a deployment
layers PostgreSQL-native HA underneath TaskForge, and proves (not
assumes) that fencing (ADR-0002, TF-INV-002/003/014) survives it.
