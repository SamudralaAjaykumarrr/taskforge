# Failure Model

Status: foundational. Every entry below is either **handled in v1** (with a
mechanism cited) or **out of scope for v1** (with the reason and the phase,
if any, that revisits it). Nothing is left ambiguous.

This document exists because a reliability system is only as credible as its
explicit list of assumed failures. A system that does not say what it
assumes cannot be verified.

## Clock Model

TaskForge assumes:

- Each process (API server, worker) has a monotonically non-decreasing local
  clock for measuring durations (used for lease TTL countdowns *within* a
  process where applicable).
- Wall-clock timestamps used for durable comparisons (`eligible_at`,
  `lease_expires_at`, `heartbeat_at`) are read from **PostgreSQL's clock**
  (`now()`), never from application-server or worker local clocks. This
  eliminates cross-machine clock skew from all correctness-critical
  comparisons, because every comparison is evaluated inside PostgreSQL
  against PostgreSQL's own `now()`.
- Bounded clock skew between the *machines* running API servers/workers and
  the PostgreSQL server is irrelevant to correctness (only to how promptly a
  human-facing timestamp like `scheduled_at` "feels" accurate), because all
  eligibility and lease-expiry decisions are evaluated by PostgreSQL itself,
  not compared against a worker's local clock.
- PostgreSQL's own clock is assumed monotonic non-decreasing during normal
  operation. Large backward clock jumps on the database host are out of
  scope (see below).

## Failure Catalog

| # | Failure | Handled in v1? | Mechanism / Reason |
|---|---|---|---|
| F1 | Worker crashes before starting (after claim, before any execution) | Yes | Lease expires (`lease_expires_at < now()`), job becomes reclaimable by the same claim query with a new `lease_generation`. See [worker-protocol.md](worker-protocol.md). Enforces TF-INV-004. |
| F2 | Worker crashes during execution | Yes | Same as F1 — the lease is the only thing that matters; TaskForge has no visibility into "during execution" beyond lease liveness (heartbeats). Enforces TF-INV-004. |
| F3 | Worker crashes after external side effect but before acknowledgement | Partially — detection yes, dedup no | The lease expires and the job is retried, which **may re-run the side effect**. TaskForge cannot prevent this by itself (see [vision.md](vision.md) core thesis); it provides idempotency primitives ([idempotency.md](idempotency.md)) so the *job handler* can make the side effect idempotent. This is not a bug to be fixed later — it is a fundamental limit, documented as [ADR-0003](adr/0003-at-least-once-execution-not-exactly-once.md). |
| F4 | API process crash | Yes | API servers are stateless; a crash mid-request either commits (client may not see the response — see idempotent retry via `Idempotency-Key`) or does not commit (no job created). No in-memory state is lost because none is load-bearing. |
| F5 | Database transaction rollback | Yes | All durable state transitions are single-transaction; a rollback leaves the row exactly as it was before the transaction started — there is no intermediate durable state (TF-INV-013). |
| F6 | Temporary database unavailability | Yes (degrades to unavailability, not corruption) | API servers return 503/5xx and reject writes; workers' claim loops back off and retry. No component fabricates state during an outage. Full HA/failover of PostgreSQL itself (e.g. automatic primary promotion) is out of scope for v1 — see "Out of Scope" below. |
| F7 | Duplicate submission (same logical request sent twice) | Yes | `Idempotency-Key` + unique constraint on `(job_type, idempotency_key)`. See [idempotency.md](idempotency.md), TF-INV-008. |
| F8 | Duplicate worker delivery/claim attempt (two workers try to claim the same job) | Yes | Claim query uses `SELECT ... FOR UPDATE SKIP LOCKED` plus a conditional `UPDATE`; at most one worker's UPDATE affects the row. See [worker-protocol.md](worker-protocol.md), TF-INV-002. |
| F9 | Long-running / stuck jobs | Yes | Bounded by lease expiration for a job that never heartbeats (reclaimed once `lease_expires_at` passes) and by `execution_timeout_seconds`, checked against `now() - started_at`, for a job that heartbeats indefinitely without completing — see [execution-semantics.md](execution-semantics.md) "Timeout Semantics." |
| F10 | Heartbeat loss (network blip, GC pause) | Yes | Bounded by `lease_expires_at`; a single missed heartbeat is tolerated as long as a subsequent one arrives before expiry. Sustained loss is treated identically to a crash (F1/F2). |
| F11 | Lease expiration | Yes | Core mechanism — see [worker-protocol.md](worker-protocol.md) and TF-INV-002/004. |
| F12 | Stale worker continues after lease loss | Yes | Fencing via `lease_generation`; a stale worker's completion call affects zero rows once a new generation has been issued. TF-INV-003, TF-INV-014. This is the canonical "Worker A / Worker B" scenario — see [worker-protocol.md](worker-protocol.md) and [scenario-corpus.md](scenario-corpus.md) SF-008. |
| F13 | Timeout (job runs longer than allowed) | Yes | `execution_timeout_seconds` bounds a single attempt; for a non-heartbeating job this is enforced as the lease TTL itself (exceeding it is a lease expiration). For a heartbeating job, the same `execution_timeout_seconds` value is instead checked as an outer bound against `now() - started_at`, independent of lease renewal. See [execution-semantics.md](execution-semantics.md) "Timeout Semantics." |
| F14 | Cancellation racing with execution | Yes | Deterministic race rule: **first durable terminal write wins**, enforced by the same fencing check used for completion. See [Cancellation and Timeouts](execution-semantics.md#cancellation-and-timeouts), TF-INV-010. |
| F15 | Retry exhaustion | Yes | `attempt_count >= max_attempts` transitions to `DEAD_LETTERED` instead of `RETRY_WAIT`. TF-INV-006, TF-INV-009. |
| F16 | Process restart (API server or worker) | Yes | No correctness-relevant state lives outside PostgreSQL; a restarted process resumes by reading durable state. Scheduled jobs, in-flight leases, and retry timers all survive restart because none of them ever existed only in memory. |
| F17 | Clock skew (between machines) | Yes, by design elimination | All correctness-critical time comparisons are evaluated by PostgreSQL against its own clock (see Clock Model above); application-host clock skew cannot affect correctness, only the perceived accuracy of human-facing timestamps in logs. |
| F18 | Partial network failure (e.g., worker can reach DB but not the external side-effect target, or vice versa) | Partially | If the worker can reach PostgreSQL, it can report failure/timeout normally and the retry mechanism applies. If the worker *cannot* reach PostgreSQL at all, this degrades to F2 (crash-equivalent — lease expires). The specific case of "side effect succeeded but the worker cannot reach PostgreSQL to report it" degrades to F3 and carries the same limitation. |
| F19 | Scheduler restart | Yes | There is no separate scheduler process/state in v1 to restart — see [scheduling.md](scheduling.md). Eligibility is a durable column checked by every claim query, so "scheduler restart" is not a distinct failure mode. |
| F20 | Concurrent workers racing to claim the same job | Yes | Same mechanism as F8. Proven by concurrency tests — see [testing-strategy.md](testing-strategy.md). |

## Explicitly Out of Scope for v1

These are real failure modes in production distributed systems. They are
named here rather than ignored, so the project never implicitly claims to
handle them:

- **PostgreSQL primary failure / failover.** TaskForge assumes a single
  durable PostgreSQL endpoint (which may itself be made HA by
  infrastructure outside TaskForge's scope, e.g. a managed Postgres service
  with its own failover). TaskForge does not implement multi-database
  replication, split-brain detection across database replicas, or
  consensus. If the database is down, TaskForge is down for writes; it does
  not silently continue with stale or divergent state.
- **Large backward clock jumps on the database server.** TaskForge assumes
  PostgreSQL's `now()` does not jump backwards during operation. NTP-slew
  style bounded correction is fine; an operator manually setting the clock
  back is not modeled.
- **Byzantine failures.** Workers and API servers are assumed to fail by
  crashing, stalling, or being partitioned — never by returning corrupted
  data while pretending it is correct, or by maliciously interfering with
  other workers' data.
- **Multi-datacenter partition tolerance.** v1 is a single-database-region
  design; network partitions between datacenters are not modeled beyond
  "the worker or API server temporarily cannot reach the database"
  (F6/F18).
- **Storage-layer corruption** (disk bit rot, filesystem corruption) below
  PostgreSQL's own durability guarantees. TaskForge trusts PostgreSQL's
  WAL/durability guarantees once a transaction commits.
- **Malicious workers.** A worker that intentionally forges its
  `lease_owner`/`lease_generation` to bypass fencing is not defended
  against in v1; workers are assumed to be trusted internal processes, not
  arbitrary untrusted clients. (This may be revisited if TaskForge is ever
  exposed to third-party worker fleets.)

## How This Maps to Design Decisions

- The lease/fencing design (F8, F11, F12, F20) directly drives
  [worker-protocol.md](worker-protocol.md) and
  [ADR-0002](adr/0002-lease-based-worker-ownership.md).
- The impossibility of preventing duplicate side effects (F3) directly
  drives [ADR-0003](adr/0003-at-least-once-execution-not-exactly-once.md)
  and [ADR-0004](adr/0004-idempotency-for-exactly-once-effects.md).
- The elimination of clock skew as a correctness concern (F17) is why every
  timestamp comparison in [data-model.md](data-model.md) is documented as
  "evaluated by PostgreSQL," not by application code.
- The absence of a separate scheduler process (F19) is why
  [scheduling.md](scheduling.md) treats scheduling as a query predicate, not
  a service.

Every "Yes" entry above must have a corresponding scenario in
[scenario-corpus.md](scenario-corpus.md) and a corresponding row in the
invariant-to-test matrix in [testing-strategy.md](testing-strategy.md).
