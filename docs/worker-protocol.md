# Worker Protocol

Status: foundational. Defines the exact claim/heartbeat/complete protocol
between a worker process and PostgreSQL. This document is the authoritative
reference for the SQL patterns that enforce TF-INV-002, 003, 004, 014, 015.

## Overview

A worker runs a loop:

```
loop:
  jobs = claim(limit=N)
  if jobs is empty: sleep(poll_interval); continue
  for job in jobs (executed concurrently, one goroutine per job):
    run_attempt(job)
```

`run_attempt` executes the handler while periodically heartbeating (for
handlers that opt into long-running mode), then reports the outcome via a
fenced completion call.

## Claiming

### Why Not Hold a Transaction Open for the Whole Job

Holding a single database transaction open for the duration of external
side-effect execution is unacceptable: it would hold row locks (or at least
a long-lived connection and transaction ID) for however long an arbitrary
external HTTP call takes, starving connection pools and vacuum, and would
make crash recovery indistinguishable from "still working." **Leases exist
specifically so that ownership outlives the short claim transaction.**
Claiming is a single, fast, committed transaction. Execution happens
entirely outside any transaction. Completion is a second, separate, short
transaction.

### Lazy Dead-Letter Sweep (runs immediately before the claim query, same
connection, same or preceding short transaction)

Before claiming, a worker (or the optional background sweeper — see
below) first moves any expired-lease `RUNNING` job that has **already
exhausted its retry budget** straight to `DEAD_LETTERED`, so such rows are
never reclaimed back into `RUNNING`:

```sql
UPDATE jobs
SET state = 'DEAD_LETTERED',
    lease_owner = NULL,
    lease_expires_at = NULL,
    last_error = COALESCE(last_error, 'lease expired, retry budget exhausted'),
    last_error_class = 'LEASE_EXPIRED',
    terminal_at = now(),
    updated_at = now(),
    version = version + 1
WHERE state = 'RUNNING'
  AND lease_expires_at < now()
  AND attempt_count >= max_attempts
RETURNING id;
```

This is what makes TF-INV-006 hold on the reclaim path, not just on the
worker-reported-failure path: without it, the claim query below would
otherwise reclaim an attempt-exhausted job back into `RUNNING` and violate
both TF-INV-006 and the `CHECK (attempt_count <= max_attempts OR state =
'DEAD_LETTERED')` constraint in [data-model.md](data-model.md). Running
this statement is required for correctness; it is cheap (bounded by
`idx_jobs_expired_lease`) and is expected to affect zero rows in the
common case. A future background sweeper (Phase 3, see
[roadmap.md](roadmap.md)) may run this same statement on a timer purely to
shrink the window between exhaustion and DLQ visibility — it is an
optimization for promptness, not a substitute for running it here.

### Claim Query

```sql
WITH candidates AS (
  SELECT id
  FROM jobs
  WHERE (
          state IN ('QUEUED', 'RETRY_WAIT')
          AND eligible_at <= now()
        )
     OR (
          state = 'RUNNING'
          AND lease_expires_at < now()
          AND attempt_count < max_attempts
        )
  ORDER BY priority DESC, eligible_at ASC
  FOR UPDATE SKIP LOCKED
  LIMIT $1
)
UPDATE jobs
SET state = 'RUNNING',
    lease_owner = $2,
    lease_generation = jobs.lease_generation + 1,
    lease_expires_at = now() + make_interval(secs => jobs.execution_timeout_seconds),
    heartbeat_at = now(),
    attempt_count = jobs.attempt_count + 1,
    updated_at = now(),
    cancel_requested = false,
    cancel_requested_at = NULL,
    version = jobs.version + 1
FROM candidates
WHERE jobs.id = candidates.id
RETURNING jobs.*;
```

Note the `AND attempt_count < max_attempts` clause on the expired-lease
branch: an expired-lease `RUNNING` row that has already exhausted its
retry budget must not be reclaimed into a new `RUNNING` attempt — it is
handled exclusively by the Lazy Dead-Letter Sweep above. Because the sweep
runs first (same worker cycle, before the claim query), such rows are
normally already `DEAD_LETTERED` by the time the claim query runs; this
clause is defense-in-depth against ordering gaps (e.g., a concurrent
worker's sweep not yet having run).

Notes:

- `FOR UPDATE SKIP LOCKED` in the candidate subquery ensures that if two
  workers run this query concurrently, each locks a disjoint set of
  candidate rows — neither blocks waiting for the other, and neither can
  select a row the other has already locked. This is what makes claiming
  cheap under high worker concurrency (TF-INV-002, F8, F20).
- The `UPDATE ... FROM candidates` pattern means the row is only mutated
  for IDs that survived the lock-and-filter step; this is a single
  statement, hence a single implicit transaction — no separate
  `BEGIN`/`COMMIT` needed for the claim step itself, minimizing lock
  duration.
- `lease_generation = jobs.lease_generation + 1` computed relative to the
  *current row value*, not a value read in a prior round trip — this is
  what guarantees strictly increasing generations even under concurrent
  claims of the same row (which cannot actually happen due to `SKIP
  LOCKED`, but the arithmetic is correct even if the isolation strategy
  ever changes).
- Combining the `QUEUED`/`RETRY_WAIT` branch and the expired-lease `RUNNING`
  branch in one query means there is no separate "reaper" process required
  for correctness (TF-INV-004) — reclaim happens automatically, opportunistically,
  the next time any worker polls. A background sweeper (Phase 3) is purely
  an optimization for prompt DLQ transitions when attempts are exhausted
  while no worker happens to poll.
- `cancel_requested` is reset to `false` on claim: a cancellation request
  logically applies to a specific attempt/generation. If a job is reclaimed
  after a lease expired (the previous attempt never got to honor the
  cancellation), the new attempt starts without an inherited cancellation
  flag — the caller must re-issue cancellation if still desired. This is a
  deliberate, documented choice (see Open Questions below) rather than
  attempting to carry cancellation intent across generations implicitly.

### Alternative Considered: Advisory Locks

PostgreSQL advisory locks (`pg_advisory_lock`) were considered as an
alternative to `FOR UPDATE SKIP LOCKED`. Rejected because advisory locks are
session-scoped and do not naturally expire — a crashed worker's advisory
lock requires the same session to disconnect for the lock to release,
which is a weaker and less directly observable liveness signal than an
explicit `lease_expires_at` column that any worker (or a human via `psql`)
can query directly. Row-level state plus explicit expiry timestamps are
strictly more transparent and testable.

## Heartbeating

For jobs whose handler opts into long-running mode (execution expected to
approach or exceed `execution_timeout_seconds`), the worker periodically
renews its lease:

```sql
UPDATE jobs
SET lease_expires_at = now() + make_interval(secs => $lease_extension_seconds),
    heartbeat_at = now(),
    version = version + 1
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'RUNNING'
RETURNING lease_expires_at;
```

If this returns zero rows, the worker has lost its lease (TF-INV-003/014/
015) and **must** abort execution immediately — any further work performed
after this point is unrecoverable from TaskForge's point of view (the job
may already be claimed by another worker). The worker-side handler contract
requires checking a cancellation/context signal that the heartbeat loop
sets on rejection (see [testing-strategy.md](testing-strategy.md) for how
this is tested without real external side effects).

Heartbeat interval should be a small fraction (e.g. 1/3) of
`lease_extension_seconds` so that a single missed heartbeat (F10) does not
cause spurious reclaim.

## Completion

### Success

```sql
UPDATE jobs
SET state = 'SUCCEEDED',
    lease_owner = NULL,
    lease_expires_at = NULL,
    result_metadata = $4,
    terminal_at = now(),
    updated_at = now(),
    version = version + 1
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'RUNNING'
RETURNING *;
```

The corresponding `job_attempts` row is updated in the **same transaction**:

```sql
UPDATE job_attempts
SET finished_at = now(),
    outcome = 'SUCCEEDED'
WHERE job_id = $1 AND attempt_number = $5;
```

Both statements commit together or not at all (TF-INV-013).

### Retryable Failure

```sql
UPDATE jobs
SET state = CASE WHEN attempt_count >= max_attempts THEN 'DEAD_LETTERED' ELSE 'RETRY_WAIT' END,
    lease_owner = NULL,
    lease_expires_at = NULL,
    eligible_at = CASE WHEN attempt_count >= max_attempts THEN eligible_at ELSE $4 END, -- computed backoff time
    last_error = $5,
    last_error_class = 'RETRYABLE',
    terminal_at = CASE WHEN attempt_count >= max_attempts THEN now() ELSE NULL END,
    updated_at = now(),
    version = version + 1
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'RUNNING'
RETURNING *;
```

See [retry-semantics.md](retry-semantics.md) for how `$4` (the next
`eligible_at`) is computed.

### Permanent Failure

Same shape as retryable failure but unconditionally sets `state =
'DEAD_LETTERED'`, `last_error_class = 'PERMANENT'`, and `terminal_at =
now()`, regardless of `attempt_count`.

### Cancellation Acknowledgement

```sql
UPDATE jobs
SET state = 'CANCELLED',
    lease_owner = NULL,
    lease_expires_at = NULL,
    terminal_at = now(),
    updated_at = now(),
    version = version + 1
WHERE id = $1
  AND lease_owner = $2
  AND lease_generation = $3
  AND state = 'RUNNING'
  AND cancel_requested = true
RETURNING *;
```

## The Fencing Guarantee, Stated Precisely

Every write above shares the shape:

```sql
UPDATE jobs SET ... WHERE id = ? AND lease_owner = ? AND lease_generation = ? AND state = 'RUNNING' ...
```

If the affected row count is 0, the caller's request is **rejected as
stale**, and the worker library surfaces this as a distinct error type
(not conflated with, e.g., a network error) so the worker can log it and
abandon the attempt cleanly rather than retrying the same completion call.
This is the concrete mechanism behind TF-INV-002, TF-INV-003, TF-INV-014,
and TF-INV-015, and the canonical scenario it defends against:

1. Worker A claims job, receives `lease_generation = 5`.
2. Worker A pauses (GC pause, VM suspend, network partition) for longer
   than the lease TTL.
3. `lease_expires_at` passes; the job is now eligible for reclaim.
4. Worker B's claim query picks it up, sets `lease_generation = 6`.
5. Worker B completes the job successfully.
6. Worker A resumes and issues its completion call with `lease_generation =
   5`.
7. The `WHERE lease_generation = 5` clause no longer matches (current value
   is 6, and `state` is now `SUCCEEDED` besides) — **0 rows affected**.
   Worker A's completion is rejected. The job's actual state (from Worker
   B) is untouched.

This exact sequence is [scenario-corpus.md](scenario-corpus.md) SF-008 and
must be a deterministic, repeatable integration test.

## API Contract (Job Submission and Query)

These are documented as **semantics**, not implementation, per the
project's current phase.

### `POST /jobs`

Request: `job_type`, `payload`, optional `priority`, `scheduled_at`,
`max_attempts`, `execution_timeout_seconds`, and an optional
`Idempotency-Key` header.

Semantics:
- The response is sent **only after** the INSERT transaction commits
  (TF-INV-001). A 2xx response is a durable guarantee, not a
  best-effort acknowledgement.
- If `Idempotency-Key` is supplied and a job with that
  `(job_type, idempotency_key)` already exists, the **existing** job's
  current representation is returned (not a new job), with the same 2xx
  status the original submission would have produced — see
  [idempotency.md](idempotency.md), including that document's
  "Implementation Notes (Phase 4)" for the header's exact validation rules
  (empty-header handling, max length).
- Response body includes the job's `id`, `state`, and `created_at`, i.e.
  enough for the caller to poll `GET /jobs/{id}` afterward.

### `GET /jobs/{id}`

Returns the current durable row: `state`, `attempt_count`, `last_error`
(if any), `result_metadata` (if terminal-succeeded), timestamps. This is a
plain read — no side effects, no locking.

### `POST /jobs/{id}/cancel`

Semantics:
- If the job is `QUEUED`/`RETRY_WAIT`: transitions directly to `CANCELLED`.
- If the job is `RUNNING`: sets `cancel_requested = true`; response
  indicates "cancellation requested, not yet confirmed" — the caller must
  poll `GET /jobs/{id}` to observe the eventual outcome (which may be
  `CANCELLED` or may be a completion that won the race — see TF-INV-010).
- If the job is already terminal: no-op, response indicates the job's
  actual terminal state (idempotent — calling cancel on an already-
  `SUCCEEDED` job is not an error, it simply reports reality).

### Deferred Endpoints (documented, not built in v1)

- `POST /jobs/{id}/retry` — manually resubmit a `DEAD_LETTERED` job. Per
  [execution-semantics.md](execution-semantics.md), this creates a **new**
  job row (referencing the old one via a `retried_from` field), it does not
  reopen the terminal row (TF-INV-005 must never be compromised for
  operator convenience).
- Workflow submission endpoints — see [workflows.md](workflows.md), Phase 7.
- `GET /jobs/{id}/history` — returns `job_attempts` rows for a job.

## Open Questions (deliberately unresolved, tracked here rather than
silently decided)

- Should `cancel_requested` survive across a reclaim (lease expiry →
  new generation)? Current design: **no**, it resets on claim (see Claim
  Query notes above). This trades "a cancellation might need to be
  re-issued after a crash-and-reclaim" against "cancellation semantics per
  generation are simple and don't require carrying intent across an
  attempt boundary." Revisit if real usage shows this surprises callers.
- Exact backoff formula constants (base delay, max delay, jitter strategy)
  are proposed in [retry-semantics.md](retry-semantics.md) as v1 defaults,
  not yet validated against real workloads.
