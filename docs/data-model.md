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
| `principal_id` | `uuid` NOT NULL REFERENCES `principals(id)` (migrations 0006–0009) | The API caller that submitted this job (Phase 12). Populated exactly once per submission, from the authenticated credential — never from a request body, header, or query parameter. `NOT NULL` is a steady state, not a transitional convenience, and it is reached across four migrations for transaction-boundary reasons (see "Phase 12 migration lock profile" below): `0006` adds the column nullable, `0007` backfills every pre-Phase-12 row to `principal.SystemPrincipalID`, `0008` adds the `NOT NULL` check constraint as `NOT VALID`, and `0009` validates it — and only then, in that same file, creates the principal-scoped idempotency index. So there is never a window in which a NULL could widen that index's uniqueness scope. Every principal-scoped read and cancellation matches on this column *inside the same statement* that does the work — see [phase-12-plan.md](phase-12-plan.md) §4a. |
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
| `max_attempts` | `int` NOT NULL DEFAULT 5 | Caller-configurable at submission. Governs TF-INV-006. `int` here is PostgreSQL's 32-bit signed `INTEGER` — as of Phase 11, `internal/job.ValidateSubmission` rejects a caller-supplied value outside that representable range (`job.MaxRepresentableMaxAttempts`) before any INSERT is attempted; see docs/compatibility-policy.md "API Evolution." |
| `execution_timeout_seconds` | `int` NOT NULL | Per-attempt wall-clock budget. See [execution-semantics.md](execution-semantics.md) Timeout Semantics. |
| `cancel_requested` | `boolean` NOT NULL DEFAULT `false` | Set by cancellation request while `RUNNING`. |
| `cancel_requested_at` | `timestamptz` NULL | Set when `cancel_requested` becomes true. |
| `idempotency_key` | `text` NULL | Caller-supplied submission dedup key. See [idempotency.md](idempotency.md). |
| `last_error` | `text` NULL | Set on `RETRY_WAIT` and `DEAD_LETTERED` transitions. |
| `last_error_class` | `text` NULL | e.g. `RETRYABLE`, `PERMANENT`, `TIMEOUT`, `LEASE_EXPIRED`. |
| `result_metadata` | `jsonb` NULL | Small, optional structured result set by the handler on success. Not intended for large payloads — TaskForge is not an artifact store. |
| `terminal_at` | `timestamptz` NULL | Set exactly once, on entry to any terminal state. Never updated again (immutability enforceable via trigger in implementation). **Not** used as a causal-ordering proof against other rows/transactions — see `terminal_attempt_count` below and the Clock Model in [failure-model.md](failure-model.md). |
| `terminal_attempt_count` | `int` NULL (migration 0004) | A durable snapshot of `attempt_count`, captured **once**, on the transition that FIRST makes this job terminal — every terminalization site writes it as `COALESCE(jobs.terminal_attempt_count, jobs.attempt_count)` in the same `UPDATE` that sets `terminal_at`, so no code path ever assigns it a second time. Exists because `terminal_at` itself is unconditionally overwritten by every terminal-write site, so it alone cannot prove a job was reopened and then reached a terminal state a second time (the overwrite erases the first terminalization's timestamp). Because `job_attempts` is append-only and gapless (TF-INV-007), any `job_attempts` row with `attempt_number > terminal_attempt_count` is proof, independent of wall-clock time, that a new attempt was opened after this job had already, durably, reached a terminal state at least once. See TF-INV-005 in [invariants.md](invariants.md) for the full mechanism, and this migration's own comment for why no `CHECK` constraint enforces its bounds. |
| `version` | `bigint` NOT NULL DEFAULT 0 | Optimistic-concurrency counter incremented on every row update, independent of `lease_generation`. Used for non-lease-guarded reads (e.g., `GET /jobs/{id}` consistency in tests), not a substitute for lease fencing. |

### Constraints and Indexes on `jobs`

- `PRIMARY KEY (id)`
- `CHECK (state IN ('QUEUED','RETRY_WAIT','RUNNING','SUCCEEDED','CANCELLED','DEAD_LETTERED'))`
- `UNIQUE (principal_id, job_type, idempotency_key) WHERE idempotency_key IS NOT NULL` (`idx_jobs_idempotency_scoped`, migration 0009) — enforces TF-INV-008/016, **scoped per principal as of Phase 12**. This replaced migration 0001's global `UNIQUE (job_type, idempotency_key)` (`idx_jobs_idempotency_key`, dropped by migration 0010) so two different tenants choosing the same `job_type` and `Idempotency-Key` are two independent submissions, not a false duplicate. `internal/store/idempotency.go`'s `idempotencyKeyIndexName` constant must always name this index exactly: the conflict-recovery path matches it against `pgErr.ConstraintName` by string, and a drift between the two would silently turn idempotency conflicts into generic insert failures.
- `CHECK (principal_id IS NOT NULL)` (`jobs_principal_id_not_null`) — added `NOT VALID` by migration 0008 and `VALIDATE CONSTRAINT`ed by migration 0009, rather than via `ALTER COLUMN ... SET NOT NULL`. The two steps are in **separate migration files on purpose**: `internal/migrate` wraps each file in one transaction and PostgreSQL releases locks only at commit, so keeping them together would hold `ADD CONSTRAINT`'s `ACCESS EXCLUSIVE` lock across `VALIDATE`'s full-table scan — exactly the blocking behaviour the two-step form exists to avoid. Split across files, the `VALIDATE CONSTRAINT` statement itself takes only `SHARE UPDATE EXCLUSIVE` and blocks neither reads nor writes. That is a statement about *that statement*, not about migration `0009` as a whole: `0009` goes on to build three indexes under `SHARE` in the same transaction, which **does** block every write to `jobs`, the worker claim query included. See "Phase 12 migration lock profile" below for the file-level behaviour.
- `CREATE INDEX idx_jobs_principal_id ON jobs (principal_id);` (migration 0009) — supports the ownership predicate every scoped read and cancellation carries.
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

## Table: `workflow_instances` (Phase 7)

One row per workflow execution. Carries the workflow-level state, a
**separate state space from `jobs.state`** — see
[workflows.md](workflows.md).

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `principal_id` | `uuid` NOT NULL REFERENCES `principals(id)` (migrations 0006–0009) | The API caller that submitted this workflow (Phase 12), with the same four-migration story as `jobs.principal_id` (`0006` adds, `0007` backfills, `0008` declares `NOT VALID`, `0009` validates). `CreateWorkflow` writes one principal across the instance row and every node's underlying `jobs` row in a single transaction, so a workflow and its nodes can never end up owned by different principals — which is what makes the workflow-level ownership check sufficient to protect the node jobs too. |
| `state` | `text` NOT NULL DEFAULT `'RUNNING'` | One of `RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED`. Enforced via `CHECK` constraint. |
| `cancel_requested` | `boolean` NOT NULL DEFAULT `false` | Set by `POST /workflows/{id}/cancel`. Distinguishes an explicit workflow-level cancellation from a workflow that reaches `FAILED` organically via node failure propagation — both drive every affected node through job-level `CANCELLED`/`DEAD_LETTERED`, but only the former's workflow-level terminal state is `CANCELLED` rather than `FAILED`. |
| `cancel_requested_at` | `timestamptz` NULL | Set when `cancel_requested` becomes true. |
| `created_at` | `timestamptz` NOT NULL DEFAULT `now()` | |
| `updated_at` | `timestamptz` NOT NULL DEFAULT `now()` | |
| `terminal_at` | `timestamptz` NULL | Set exactly once, on entry to any terminal workflow state (`SUCCEEDED`, `FAILED`, `CANCELLED`). Never updated again — every write to this table's `state` column is guarded by `WHERE state = 'RUNNING'`, so a terminal workflow state, once set, is never reopened. |

### Constraints and Indexes on `workflow_instances`

- `PRIMARY KEY (id)`
- `CHECK (state IN ('RUNNING','SUCCEEDED','FAILED','CANCELLED'))`
- `CHECK (principal_id IS NOT NULL)` (`workflow_instances_principal_id_not_null`, migrations 0008/0009)
- `CREATE INDEX idx_workflow_instances_state ON workflow_instances (state);`
- `CREATE INDEX idx_workflow_instances_principal_id ON workflow_instances (principal_id);` (migration 0009)

## Table: `workflow_nodes` (Phase 7)

One row per node in a workflow's DAG, referencing the `jobs` row it
wraps. A workflow node's underlying job uses the ordinary `jobs` state
machine completely unchanged — this table adds no new execution state of
its own, only structure (which job belongs to which workflow, and its
predecessor node IDs).

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `workflow_instance_id` | `uuid` NOT NULL REFERENCES `workflow_instances(id)` | |
| `node_key` | `text` NOT NULL | Caller-chosen identifier, unique within the workflow instance, used in `depends_on` submissions and API responses (the caller's own vocabulary, not raw internal UUIDs — see [workflows.md](workflows.md)). |
| `job_id` | `uuid` NOT NULL REFERENCES `jobs(id)` | The underlying job this node executes. |
| `depends_on` | `uuid[]` NOT NULL DEFAULT `'{}'` | Node IDs (this table's own `id` column, not `job_id`) that must satisfy their dependency condition (default: reach `SUCCEEDED`) before this node's job becomes eligible. See [workflows.md](workflows.md)'s "Why an Array Column Instead of an Edge Table" for why this is a `uuid[]` column rather than a join table. |
| `created_at` | `timestamptz` NOT NULL DEFAULT `now()` | |

### Constraints and Indexes on `workflow_nodes`

- `PRIMARY KEY (id)`
- `UNIQUE (workflow_instance_id, node_key)` — a `node_key` is only unique
  within its own workflow instance, not globally.
- `UNIQUE (job_id)` — a job row backs at most one workflow node; a job is
  never shared between two logical workflow positions.
- `CREATE INDEX idx_workflow_nodes_workflow_instance_id ON workflow_nodes (workflow_instance_id);`
- `CREATE INDEX idx_workflow_nodes_depends_on ON workflow_nodes USING GIN (depends_on);`
  — supports the reverse lookup "find every node that depends on node X"
  (`depends_on @> ARRAY[$1]`), which runs once per terminal node-state
  transition on any workflow-backed job.

### Dependency-Gating Mechanism (no new `jobs` column)

A workflow node's underlying `jobs` row needs **no schema change at all**
to support dependency gating: a node with one or more dependencies is
simply inserted with `jobs.eligible_at` set to a fixed, far-future
sentinel timestamp (`9999-12-31T23:59:59Z` — a concrete, portable value,
not PostgreSQL's special `infinity` timestamptz, which is not reliably
representable as a Go `time.Time` through the pgx driver this codebase
uses). The existing claim query (see
[worker-protocol.md](worker-protocol.md)) already treats any row with
`eligible_at > now()` as ineligible, so this requires zero changes to the
claim query itself. When every dependency is satisfied, the same
`eligible_at` column is advanced to `COALESCE(scheduled_at, now())` —
identical to how an ordinary job's `eligible_at` is set at submission
time. See [workflows.md](workflows.md)'s "Dependency Satisfaction
Semantics" for the full propagation algorithm.

## Table: `principals` (Phase 12)

One row per API-caller identity. This is the **application/API** trust
boundary only — see [security-model.md](security-model.md) and
[phase-12-plan.md](phase-12-plan.md) OD-3 for why worker identity is
deliberately *not* modeled here.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `kind` | `text` NOT NULL | `CHECK (kind IN ('caller', 'admin'))`. There is deliberately **no `'worker'` value**: a worker process's identity is its PostgreSQL role credential, verified by PostgreSQL itself at connection time — a different mechanism in a different layer, checked by a different system. Adding a worker kind "for symmetry" would create exactly the conflation that decision rejects. `admin` is the single documented exception to ownership scoping. |
| `display_name` | `text` NOT NULL | Operator-assigned. Never used for lookup, authentication, or authorization. |
| `created_at` | `timestamptz` NOT NULL DEFAULT `now()` | |
| `revoked_at` | `timestamptz` NULL | A revoked principal's keys are **all** treated as revoked regardless of their own `revoked_at`, so revoking a compromised caller is one write rather than one per credential. |

One row is seeded by migration `0005` itself: the **system principal**,
`id = '00000000-0000-0000-0000-000000000001'` (exported as
`principal.SystemPrincipalID`, so no code path ever needs a runtime lookup),
`kind = 'caller'`. It is never deleted, never revoked, and deliberately has
**no `api_keys` row** — nothing can authenticate *as* it. It exists purely as
the foreign-key target for migration `0007`'s backfill of pre-Phase-12 rows,
which means those legacy rows are not reachable by any credential a caller
could present. No application code path ever assigns it: a submission that
cannot name a principal is rejected, not defaulted.

### Constraints and Indexes on `principals`

- `PRIMARY KEY (id)`
- `CHECK (kind IN ('caller', 'admin'))`

## Table: `api_keys` (Phase 12)

Credentials belonging to a principal. Multiple live rows per principal are
ordinary — that *is* the rotation-with-overlap mechanism, with no separate
machinery.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PRIMARY KEY | |
| `principal_id` | `uuid` NOT NULL REFERENCES `principals(id)` | |
| `key_id` | `text` NOT NULL UNIQUE | The **non-secret** half of the `Authorization: Bearer <key_id>.<secret>` credential: 16 random bytes, hex-encoded. It exists so a key can be looked up, rotated, and revoked without ever comparing raw secrets, and so verification's database lookup is keyed on a non-secret value — a raw secret is never used as a lookup key. |
| `secret_hash` | `bytea` NOT NULL | `HMAC-SHA256(pepper, secret)`. **Never the raw secret, and never a bare digest of it.** The pepper lives only in the running process's environment (`TASKFORGE_API_KEY_PEPPER`), never in this database, so a stolen dump of this table alone is insufficient to forge or offline-verify a credential. |
| `scopes` | `text[]` NOT NULL DEFAULT `'{}'` | A flat per-key capability list: `jobs`, `metrics`, `admin`. Not a role, not a policy language. Deliberately has **no database `CHECK`** — the same precedent migration 0004 sets for `terminal_attempt_count`: where an invariant is fully owned by trusted application code, validation lives once, centrally, in `internal/principal.ValidateScopes`. |
| `created_at` | `timestamptz` NOT NULL DEFAULT `now()` | |
| `expires_at` | `timestamptz` NULL | Optional operator-set expiry. |
| `revoked_at` | `timestamptz` NULL | Takes effect on the key's very next verification — there is no cache and no TTL anywhere in the verification path, so revocation never requires a restart or redeploy. |
| `last_used_at` | `timestamptz` NULL | Best-effort telemetry, written off the request's critical path with errors ignored. **Never** read as a correctness signal by anything. |

### Constraints and Indexes on `api_keys`

- `PRIMARY KEY (id)`
- `UNIQUE (key_id)`
- `CREATE INDEX idx_api_keys_principal_id ON api_keys (principal_id);`

Credential lifecycle (create, rotate, revoke) is **operator tooling**
(`cmd/taskforge-admin`), deliberately not an HTTP API — Phase 12 adds no new
endpoint anywhere.

## Tables Deferred to Later Phases (documented now, not built yet)

### `job_events` (Phase 8, optional)

An append-only event log (`job_id`, `event_type`, `payload`, `occurred_at`)
for observability/audit beyond what `job_attempts` captures (e.g.,
heartbeat events, cancellation requests). Deferred because `job_attempts`
plus structured logs (see [observability.md](observability.md)) cover v1's
needs without a second history table.

## Transactional Enqueue Needed No Schema Change (Phase 11)

docs/enterprise-roadmap.md Phase 11 added a `pgx.Tx`-based enqueue entry
point (the `txenqueue` package, docs/transactional-enqueue.md). It required
**zero migrations**: the `jobs` table already supports an ordinary `INSERT`
from any transaction, including one a caller opened and controls — the
existing columns, constraints, and indexes above are used completely
unchanged by this entry point. This is called out explicitly because it
was a deliberate audit finding (docs/enterprise-roadmap.md's own
"Migration / Schema Discipline" instruction for that phase), not an
oversight.

## Why Not Over-Design v1

`job_attempts`, `workflow_instances`, and `workflow_nodes` are the only
history/structure tables beyond `jobs` itself. Workflow tables were
deferred until Phase 7 (now implemented) specifically because building
them before the single-job engine was proven (leasing, fencing, retries,
idempotency) would have meant designing DAG semantics on top of an
unvalidated foundation. See [roadmap.md](roadmap.md).

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

Phase 12's `idx_jobs_principal_id` is deliberately **not** folded into
`idx_jobs_claimable`: the claim query is worker-side and has no principal
predicate at all (a worker claims whatever is eligible, regardless of who
submitted it), so adding `principal_id` to the claim index would widen the
system's hottest index for no query that uses it. The ownership predicate
appears only on the caller-facing read/cancel statements, which are
point lookups by `id` — `idx_jobs_principal_id` supports the tenant-scoped
scans (and the composite idempotency index) rather than the claim path.

## Phase 12 migration lock profile

Phase 12 adds `principal_id` to two live tables and re-scopes an existing
unique index. `internal/migrate` runs each `.up.sql` inside a single
transaction (TF-INV-013) and PostgreSQL releases locks only at commit, so
**every lock a migration file takes is held until that whole file
finishes** — including locks taken by an earlier statement in the same file.

The six-file split exists because of that rule. It keeps each
`ACCESS EXCLUSIVE` statement in a file of its own, so no `ACCESS EXCLUSIVE`
lock is ever held across a full-table scan or a full-table write. It does
**not** make the sequence non-blocking, and this document does not claim it
does: `0009` still takes `SHARE`, which blocks every write to `jobs` — the
worker claim query included — for as long as that file runs.

Note when reading the table below: **the worker claim query is a write.**
`internal/store/claim.go`'s `claimQuery` is a `WITH candidate AS (SELECT …
FOR UPDATE SKIP LOCKED) UPDATE jobs …` — the CTE takes `ROW SHARE`, but the
statement as a whole takes `ROW EXCLUSIVE`. Anything that blocks writes to
`jobs` therefore blocks claiming, completing, and heartbeating, not just
submissions.

| Migration | Statements | Locks on `jobs` (held to commit) | Scans/writes the table? | Blocks reads? | Blocks writes, incl. the worker claim query? |
|---|---|---|---|---|---|
| `0005_create_principals_and_api_keys` | new tables + seed row | none on `jobs` | no | no | no |
| `0006_add_principal_id_columns` | `ADD COLUMN` ×2 | `ACCESS EXCLUSIVE` | **no** (catalog-only) | yes, while held | yes, while held |
| `0007_backfill_principal_id` | `UPDATE` ×2 | `ROW EXCLUSIVE` | **yes** (writes every row) | **no** | **no** (`ROW EXCLUSIVE` does not self-conflict) |
| `0008_require_principal_id` | `ADD CONSTRAINT … NOT VALID` ×2 | `ACCESS EXCLUSIVE` | **no** (catalog-only) | yes, while held | yes, while held |
| `0009_validate_principal_id_and_scope_idempotency` | `VALIDATE CONSTRAINT` ×2, `CREATE INDEX` ×3 | `SHARE UPDATE EXCLUSIVE`, then **also** `SHARE` | **yes** (scan + three builds) | **no** | **YES** — from the first `CREATE INDEX` until the file commits |
| `0010_drop_global_idempotency_index` | `DROP INDEX` | `ACCESS EXCLUSIVE` | **no** (catalog-only) | yes, while held | yes, while held |

**What the split does buy, stated narrowly**: no `ACCESS EXCLUSIVE` lock is
held across a full-table scan or a full-table write. `0006`, `0008` and
`0010` are catalog-only, and the two table-size-dependent files (`0007`,
`0009`) take no `ACCESS EXCLUSIVE` lock at all. That is the whole of the
guarantee. It is a statement about which lock is held during long work — not
a claim that the sequence is online, non-blocking, or safe to run under load
without a maintenance decision.

**What is NOT claimed**, stated because earlier revisions of this document
claimed more than PostgreSQL delivers and were measured to be wrong — twice:

- These migrations are **not** online, non-blocking, or zero-downtime, and
  no Phase 12 document should be read as saying otherwise.
- **`0009` blocks every write to `jobs`, including the worker claim query.**
  Its three `CREATE INDEX` statements each take `SHARE`, `SHARE` conflicts
  with the `ROW EXCLUSIVE` that every write — claim, complete, heartbeat,
  submit — requires, and `internal/migrate` holds that lock until the whole
  file commits. So the write stall is not "per build": it runs from the
  first `CREATE INDEX` to the end of the migration. Reads are unaffected
  (`ACCESS SHARE` is compatible with `SHARE`). Measured directly against
  this repository's real `0009` file: on a 3M-row / 426 MB `jobs` table the
  real claim query hit `lock_timeout` (SQLSTATE `55P03`) rather than
  claiming. `CREATE INDEX CONCURRENTLY` would avoid the write block but
  cannot run inside a transaction block, and every migration here runs
  inside one (TF-INV-013).
- The `VALIDATE CONSTRAINT` step, taken on its own, takes only
  `SHARE UPDATE EXCLUSIVE`, which conflicts with neither reads nor writes —
  that is why the `NOT VALID`/`VALIDATE` two-step is used and why `0008` is a
  separate file. That is a property of **that statement**, not of migration
  `0009`: `VALIDATE` shares a transaction with the three index builds that
  follow it, so the file's observable behaviour is the blocking one above.
- `0006`, `0008` and `0010` take `ACCESS EXCLUSIVE`. They are catalog-only
  and finish in O(1) once acquired (measured: `0006`'s
  `ADD COLUMN … REFERENCES` ~3 ms on an 800k-row / 52 MB table, against
  ~22 ms for a full scan of the same table, with the foreign key recorded
  `convalidated` without a scan; `0008`'s `ADD CONSTRAINT … NOT VALID`
  ~1 ms). But like all DDL they must *wait* for any conflicting lock already
  held, and while waiting PostgreSQL queues new lock requests — readers
  included — behind them.
- `0007` writes every row under `ROW EXCLUSIVE`, which does not conflict
  with reads or with other writes, so the claim query keeps running
  throughout. It does take ordinary row-level locks and will wait behind a
  worker currently holding one of those rows: bounded MVCC contention on a
  single row, which the claim query skips by design (`SKIP LOCKED`), not a
  table-wide stall.
- **Duration is unbounded by any of the above and is entirely
  data/table-size/environment dependent.** `0007` writes and `0009` reads
  the whole table; on a `jobs` table grown large by Phase 9 chaos/load runs
  either can run for a long time, and `0009`'s share of that time is time
  during which no worker can claim. No test can establish that duration for
  your data.

**Deployment obligations this section does not discharge** (they are the
operator's, and TaskForge cannot verify them from inside the process):

- **Benchmark `0007` and `0009` against a realistically-sized copy of your
  own `jobs` table before running them against production.** The numbers
  above are from synthetic tables and are illustrative only.
- Set `lock_timeout` and retry rather than letting a DDL statement queue
  readers behind it indefinitely.
- Schedule `0009` (and, where `jobs` is large or busy, the whole sequence)
  in a quiet window or maintenance window, sized by the benchmark above,
  and expect worker claiming to stop for `0009`'s duration.

`internal/migrate`'s `TestPhase12Migrations_BackfillDoesNotBlockReadsOrTheClaimPath`
and `TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery` measure both
behaviours through the real `migrate.Up`; the latter asserts the write block
rather than denying it.
