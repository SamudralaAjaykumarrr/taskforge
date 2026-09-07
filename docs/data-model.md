# Data Model

Status: foundational — conceptual PostgreSQL schema. This is a design
contract, not a migration file; exact column types may be refined during
Phase 1 implementation, but the fields, constraints, and indexing strategy
below are binding.

## Design Principle

The schema is the state machine. Every field exists because a transition,
guard, or invariant in [execution-semantics.md](execution-semantics.md) or
[invariants.md](invariants.md) needs it — no speculative columns.

## Table: `jobs`

The single durable record of a logical unit of work.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | Generated at submission time (client-supplied or server-generated — see [worker-protocol.md](worker-protocol.md) API contract). |
| `job_type` | `text` NOT NULL | Identifies which handler executes this job. Indexed as part of the idempotency constraint. |
| `payload` | `jsonb` NOT NULL | Opaque to TaskForge; interpreted by the job handler. |
| `state` | `text` NOT NULL | One of `QUEUED`, `RETRY_WAIT`, `RUNNING`, `SUCCEEDED`, `CANCELLED`, `DEAD_LETTERED`. See [execution-semantics.md](execution-semantics.md). Enforced via `CHECK` constraint, not a separate enum table (simplicity — see [ADR-0005](adr/0005-durable-job-state-machine.md)). |
| `priority` | `smallint` NOT NULL DEFAULT 0 | Higher claims first. Tie-broken by `eligible_at`. |
| `created_at` | `timestamptz` NOT NULL DEFAULT `now()` | Submission time. Immutable. |
| `updated_at` | `timestamptz` NOT NULL DEFAULT `now()` | Set on every transition. |
| `eligible_at` | `timestamptz` NOT NULL DEFAULT `now()` | The single field governing both "scheduled for later" and "retry backoff until." See [scheduling.md](scheduling.md), [retry-semantics.md](retry-semantics.md). |
| `scheduled_at` | `timestamptz` NULL | The caller's originally requested execution time, if any. Distinct from `eligible_at`: `scheduled_at` is immutable caller intent; `eligible_at` is the live, mutable gating timestamp (it moves forward on each retry). Kept separate so retry backoff never destroys the original scheduling intent for audit purposes. |
| `lease_owner` | `text` NULL | Opaque worker identifier (e.g. hostname+pid+random). NULL when not `RUNNING`. |
| `lease_generation` | `bigint` NOT NULL DEFAULT 0 | Monotonically increasing per job. Incremented on every successful claim. See [worker-protocol.md](worker-protocol.md), TF-INV-002/003/014. |
| `lease_expires_at` | `timestamptz` NULL | NULL when not `RUNNING`. Governs reclaim eligibility. |
| `heartbeat_at` | `timestamptz` NULL | Last heartbeat received. Observability only — `lease_expires_at` is the actual correctness guard (TF-INV-015). |
| `attempt_count` | `int` NOT NULL DEFAULT 0 | Incremented on each claim. 1-based numbering (first attempt is `attempt_count = 1`) — see [retry-semantics.md](retry-semantics.md). |
| `max_attempts` | `int` NOT NULL DEFAULT 5 | Caller-configurable at submission. Governs TF-INV-006. |
| `execution_timeout_seconds` | `int` NOT NULL | Per-attempt wall-clock budget. See [execution-semantics.md](execution-semantics.md) Timeout Semantics. |
| `cancel_requested` | `boolean` NOT NULL DEFAULT `false` | Set by cancellation request while `RUNNING`. |
| `cancel_requested_at` | `timestamptz` NULL | Set when `cancel_requested` becomes true. |
| `idempotency_key` | `text` NULL | Caller-supplied submission dedup key. See [idempotency.md](idempotency.md). |
| `last_error` | `text` NULL | Set on `RETRY_WAIT` and `DEAD_LETTERED` transitions. |
| `last_error_class` | `text` NULL | e.g. `RETRYABLE`, `PERMANENT`, `TIMEOUT`, `LEASE_EXPIRED`. |
| `result_metadata` | `jsonb` NULL | Small, optional structured result set by the handler on success. Not intended for large payloads — TaskForge is not an artifact store. |
| `terminal_at` | `timestamptz` NULL | Set exactly once, on entry to any terminal state. Never updated again (immutability enforceable via trigger in implementation). |
| `version` | `bigint` NOT NULL DEFAULT 0 | Optimistic-concurrency counter incremented on every row update, independent of `lease_generation`. Used for non-lease-guarded reads (e.g., `GET /jobs/{id}` consistency in tests), not a substitute for lease fencing. |

### Constraints and Indexes on `jobs`

- `PRIMARY KEY (id)`
- `CHECK (state IN ('QUEUED','RETRY_WAIT','RUNNING','SUCCEEDED','CANCELLED','DEAD_LETTERED'))`
- `UNIQUE (job_type, idempotency_key) WHERE idempotency_key IS NOT NULL` — enforces TF-INV-008/016.
- `CHECK (attempt_count <= max_attempts OR state IN ('DEAD_LETTERED'))` — a defense-in-depth check (application logic is the primary enforcement of TF-INV-006; this constraint catches regressions).
- Partial index for the claim query (the most performance-critical query in
  the system):
  ```sql
  CREATE INDEX idx_jobs_claimable
    ON jobs (priority DESC, eligible_at ASC)
    WHERE state IN ('QUEUED', 'RETRY_WAIT');
  ```
  This index does **not** cover expired-lease `RUNNING` rows because those
  are expected to be rare relative to the steady-state queue (crash
  recovery is the exception, not the common case); a separate, smaller
  index handles that path:
  ```sql
  CREATE INDEX idx_jobs_expired_lease
    ON jobs (lease_expires_at)
    WHERE state = 'RUNNING';
  ```
- Index for status queries: `CREATE INDEX idx_jobs_state ON jobs (state);`
  (supports observability queries like "count jobs by state" — see
  [observability.md](observability.md)).

## Table: `job_attempts`

Append-only durable history of every claim → outcome cycle. See TF-INV-007.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `job_id` | `uuid` NOT NULL REFERENCES `jobs(id)` | |
| `attempt_number` | `int` NOT NULL | Matches `jobs.attempt_count` at the time of this claim. 1-based. |
| `lease_generation` | `bigint` NOT NULL | The generation this attempt was claimed under. |
| `worker_id` | `text` NOT NULL | Same value as `jobs.lease_owner` at claim time. |
| `started_at` | `timestamptz` NOT NULL DEFAULT `now()` | Set at claim time. |
| `finished_at` | `timestamptz` NULL | NULL while the attempt is in flight. Set on outcome report or on lazy detection of lease expiry. |
| `outcome` | `text` NULL | One of `SUCCEEDED`, `FAILED_RETRYABLE`, `FAILED_PERMANENT`, `TIMED_OUT`, `LEASE_EXPIRED`, `CANCELLED`. NULL while in flight. |
| `error_class` | `text` NULL | Structured error category, handler-supplied for failures. |
| `error_message` | `text` NULL | Free-text detail. |

### Constraints and Indexes on `job_attempts`

- `UNIQUE (job_id, attempt_number)` — direct enforcement of half of
  TF-INV-007 (no duplicate attempt numbers). Gaplessness is enforced by
  application logic (attempt numbers are always derived from
  `jobs.attempt_count`, never client-supplied) plus a table test.
- `CREATE INDEX idx_job_attempts_job_id ON job_attempts (job_id, attempt_number);`
  — supports "get full history for job X."
- No `UPDATE` ever targets `started_at`, `attempt_number`, `lease_generation`,
  or `worker_id` after insert — only `finished_at`/`outcome`/`error_*` are
  filled in once, by the same attempt's completion call (fenced by
  `lease_generation`, per TF-INV-003).

## Tables Deferred to Later Phases (documented now, not built yet)

These are named here so the v1 schema's foreign keys and column choices
don't foreclose them, but they are **not created in Phase 1**:

### `workflow_instances` (Phase 7)

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `state` | `text` | `RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED` at the workflow level — see [workflows.md](workflows.md). |
| `created_at` / `terminal_at` | `timestamptz` | |

### `workflow_nodes` (Phase 7)

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `workflow_instance_id` | `uuid` REFERENCES `workflow_instances(id)` | |
| `job_id` | `uuid` REFERENCES `jobs(id)` | The underlying job this node executes. |
| `depends_on` | `uuid[]` | Node IDs that must satisfy their dependency condition first. See [workflows.md](workflows.md) for why an edge table is deferred in favor of an array column for v1's DAG size expectations. |

### `job_events` (Phase 8, optional)

An append-only event log (`job_id`, `event_type`, `payload`, `occurred_at`)
for observability/audit beyond what `job_attempts` captures (e.g.,
heartbeat events, cancellation requests). Deferred because `job_attempts`
plus structured logs (see [observability.md](observability.md)) cover v1's
needs without a second history table.

## Why Not Over-Design v1

`job_attempts` is the only history table in v1 beyond `jobs` itself.
Workflow tables are deferred until Phase 7 because building them before the
single-job engine is proven (leasing, fencing, retries, idempotency) would
mean designing DAG semantics on top of an unvalidated foundation. See
[roadmap.md](roadmap.md).

## Indexing Strategy Summary for Worker Polling

The claim query (see [worker-protocol.md](worker-protocol.md)) needs to
answer, cheaply and under concurrent load: "give me up to N claimable jobs,
highest priority first, oldest-eligible first, skipping rows already locked
by another transaction." `idx_jobs_claimable` is a partial index scoped to
exactly the two non-terminal, unowned states, ordered to match the claim
query's `ORDER BY`, so the planner can satisfy the query without a sort or a
full-table scan even as the `jobs` table accumulates millions of terminal
rows over time. `idx_jobs_expired_lease` keeps the (rare) reclaim path
similarly cheap without polluting the primary claim index.
