# Architecture

Status: foundational. Describes the target architecture that later phases
implement incrementally (see [roadmap.md](roadmap.md)). No component
described here exists in code yet.

## Design Philosophy

**Smallest architecture capable of proving the reliability properties.**
TaskForge deliberately avoids Kafka, Redis, Kubernetes, or any additional
infrastructure dependency unless the invariants genuinely cannot be proven
without it. PostgreSQL, used correctly, is sufficient to provide durable
persistence, transactional claiming, and a queue — see
[ADR-0006](adr/0006-database-backed-queue-first.md). Every additional moving
part is a new failure mode and a new thing to prove correct; each is added
only when a documented limitation of the current design demands it.

## Components

```
                    +-------------------+
   HTTP clients --->|     API Server    |----+
                    +-------------------+    |
                                              v
                                     +------------------+
                                     |   PostgreSQL      |
                                     |  (source of truth)|
                                     |  jobs, attempts,   |
                                     |  workflow tables   |
                                     +------------------+
                                              ^
                    +-------------------+     |
                    |   Worker Pool     |-----+
                    |  (N processes,    |
                    |   M goroutines    |
                    |   each)           |
                    +-------------------+
                              |
                              v
                    (external side effects:
                     HTTP calls, emails,
                     other systems — outside
                     TaskForge's durability
                     boundary)
```

- **API Server**: stateless HTTP process. Accepts job submissions,
  cancellation requests, and status queries. Holds no in-memory job state —
  every request is served by reading from or writing to PostgreSQL. Multiple
  API server instances can run concurrently with no coordination between
  them, because PostgreSQL is the only shared state.
- **PostgreSQL**: the sole durable source of truth for job state, attempt
  history, leases, and (later) workflow structure. There is no second
  database, cache-as-source-of-truth, or in-memory queue. See
  [data-model.md](data-model.md).
- **Worker Pool**: one or more worker processes, each running a claim loop
  that polls PostgreSQL for eligible jobs, acquires a fenced lease, executes
  the job handler, and reports the outcome. Workers are stateless between
  jobs — a crashed worker's in-flight jobs are recoverable purely from
  PostgreSQL state (lease expiration), never from worker-local memory. See
  [worker-protocol.md](worker-protocol.md).
- **Scheduler** (logical, not a separate process in v1): "scheduling" is not
  a distinct service — it is the `eligible_at` column on `jobs`, checked by
  the same claim query workers already run. There is no in-memory timer
  wheel. See [scheduling.md](scheduling.md) and
  [ADR-0006](adr/0006-database-backed-queue-first.md).
- **Reclaim path** (logical): jobs whose lease has expired are picked up by
  the *same* claim query that finds newly eligible jobs (see
  [worker-protocol.md](worker-protocol.md) claim SQL). A background sweeper
  process is introduced in Phase 3 purely to move attempt-exhausted,
  lease-expired jobs to `DEAD_LETTERED` promptly rather than waiting for a
  claimer to notice — it is an optimization, not a correctness dependency.

## Why No Message Broker

A broker (Kafka, SQS, RabbitMQ) would add a second system that must agree
with PostgreSQL about job state, or would need to become the source of
truth itself (at the cost of transactional claiming semantics and ad hoc
querying of job state). Since PostgreSQL already provides durable storage,
row-level locking (`SELECT ... FOR UPDATE SKIP LOCKED`), and transactional
`UPDATE ... RETURNING`, it can serve as a correctness-first queue at the
throughput TaskForge targets in v1. This tradeoff is revisited explicitly in
[ADR-0006](adr/0006-database-backed-queue-first.md); if throughput or fan-out
requirements outgrow it, a broker can be introduced later as a *delivery*
layer in front of the same durable jobs table, not as a replacement for it.

## Why No Redis

Redis is commonly used for locks, leases, or caching. TaskForge does not use
it because:

- Leases are load-bearing for correctness (TF-INV-002/003/014), and adding a
  second system that can independently fail, partition, or lose data
  (depending on persistence configuration) reintroduces exactly the
  split-brain risk the lease/fencing design exists to prevent.
- PostgreSQL row locks and conditional updates already provide the
  transactional guarantees leases need, in the same system that holds the
  job record itself, avoiding cross-system consistency problems entirely.

## Why No Kubernetes (as an architectural dependency)

TaskForge's correctness does not depend on any particular deployment
substrate. Workers are plain OS processes that can be run under systemd, in
plain containers, under a process supervisor, or under Kubernetes — the
architecture makes no assumption beyond "processes can run and reach
PostgreSQL over the network." Kubernetes-specific tooling (operators, CRDs)
is out of scope for v1 (see [vision.md](vision.md) non-goals).

## Trust and Durability Boundary

Everything inside the dashed box below is covered by TaskForge's durability
guarantees. Everything outside it is not, and cannot be, by construction.

```
 +-------------------------------------------+
 |  API Server  --  PostgreSQL  --  Workers   |   <- durable, fenced,
 |  (job state, lease state, attempt history) |      invariant-checked
 +-------------------------------------------+
                     |
                     | job handler code runs here, calls out
                     v
        [ external systems: HTTP APIs, email,     <- NOT covered;
          payment processors, other databases ]      idempotency is the
                                                       job author's
                                                       responsibility,
                                                       enabled by
                                                       TaskForge's stable
                                                       identifiers
```

See [idempotency.md](idempotency.md) for how a job handler uses
`job_id`/`attempt_number` to make its own side effects idempotent.

## Data Flow: Submission to Terminal State

1. Client calls `POST /jobs` (optionally with `Idempotency-Key`).
2. API server validates the request and performs a single INSERT (or
   idempotent no-op if the key already exists) into `jobs`. The HTTP
   response is only sent after this transaction commits — see
   [ADR-0006](adr/0006-database-backed-queue-first.md) and
   [execution-semantics.md](execution-semantics.md).
3. Job sits in `QUEUED` (or, if `scheduled_at` is in the future, is
   immediately durable but not yet eligible) until `eligible_at <= now()`.
4. A worker's claim loop runs a claim query (see
   [worker-protocol.md](worker-protocol.md)) that atomically selects an
   eligible job, assigns a new `lease_generation`, sets `lease_owner`,
   `lease_expires_at`, and transitions the job to `RUNNING`.
5. The worker executes the job handler, sending periodic heartbeats to renew
   the lease if the handler runs long.
6. The worker reports the outcome via a fenced completion call (conditional
   `UPDATE ... WHERE lease_owner = ? AND lease_generation = ?`).
7. Depending on outcome: transition to `SUCCEEDED` (terminal), `RETRY_WAIT`
   (if retryable and attempts remain), or `DEAD_LETTERED` (terminal, retries
   exhausted or permanent failure).
8. If the worker crashes anywhere between step 4 and step 6, the lease
   expires and the job becomes claimable again by the same claim query,
   with a new `lease_generation`, once `lease_expires_at < now()`.

## Cross-References

- State machine detail: [execution-semantics.md](execution-semantics.md)
- Schema detail: [data-model.md](data-model.md)
- Lease/claim protocol detail: [worker-protocol.md](worker-protocol.md)
- Failure assumptions: [failure-model.md](failure-model.md)
- Invariants enforced by this architecture: [invariants.md](invariants.md)
