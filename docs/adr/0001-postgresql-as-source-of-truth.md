# ADR-0001: PostgreSQL as the Sole Source of Truth

Status: Accepted

## Context

TaskForge needs a durable store for job state, lease ownership, and attempt
history that survives process crashes and supports transactional,
concurrency-safe claiming (see [failure-model.md](../failure-model.md),
[worker-protocol.md](../worker-protocol.md)). We must choose the storage
substrate this entire project is built on before any other design decision
can be made concrete.

## Decision

PostgreSQL is the single, sole durable source of truth for all
correctness-relevant state: job records, lease fields, attempt history, and
(later) workflow structure. No other datastore holds correctness-relevant
state. Caches, if introduced later, are strictly read-through/best-effort
and never authoritative.

## Alternatives Considered

- **A dedicated message broker (Kafka, SQS, RabbitMQ) as the primary
  queue**, with PostgreSQL (or nothing) for state. Rejected for v1: brokers
  excel at throughput and fan-out but do not natively provide the
  conditional, fenced compare-and-swap semantics TaskForge's lease model
  needs (TF-INV-002/003/014) without building a second layer of
  bookkeeping on top — at which point PostgreSQL would still be needed as
  the state layer, and the broker would be redundant complexity for v1's
  targeted scale. See [ADR-0006](0006-database-backed-queue-first.md) for
  the queue-specific version of this argument.
- **A dedicated distributed lock service (etcd, ZooKeeper, Consul) for
  leases, with PostgreSQL for job data.** Rejected: this splits
  correctness-relevant state across two systems that must agree with each
  other, reintroducing a distributed consistency problem TaskForge exists
  to demonstrate solving *without* extra infrastructure. A lease is just a
  few columns and a conditional UPDATE — PostgreSQL already does this
  natively and transactionally with the data it's guarding.
- **An embedded/local datastore (SQLite, BoltDB) for single-node
  simplicity.** Rejected: TaskForge's entire premise is multiple workers
  (potentially on multiple machines) competing for shared work; an embedded
  single-process store cannot serve as shared state across a worker fleet.
- **NoSQL document/key-value store (MongoDB, DynamoDB, etc.).** Rejected:
  TaskForge's core mechanism (`SELECT ... FOR UPDATE SKIP LOCKED` combined
  with a conditional `UPDATE ... RETURNING` in one transaction) depends on
  relational, ACID, row-level-locking semantics that are either unavailable
  or substantially more awkward to express correctly in most NoSQL stores.

## Consequences

- Positive: One system to operate, back up, and reason about
  transactionally. Every invariant in [invariants.md](../invariants.md) can
  be expressed as a single-database transaction or constraint, which is
  what makes them provable by direct integration tests rather than
  distributed-protocol reasoning.
- Positive: PostgreSQL's maturity (WAL durability, well-understood
  isolation levels, `SKIP LOCKED` support since 9.5) means TaskForge is not
  inventing new durability primitives, only composing well-understood ones.
- Negative: PostgreSQL becomes a single point of scaling and (absent
  external HA setup) availability for the whole system — see
  [failure-model.md](../failure-model.md) "Out of Scope: PostgreSQL primary
  failure/failover." This is an accepted v1 tradeoff, not an oversight.
- Negative: Throughput is bounded by what a single PostgreSQL primary can
  sustain for the claim query's write pattern. This is acceptable for v1's
  target scale (see [vision.md](../vision.md) non-goals) and is explicitly
  named as a future scaling question, not solved here.

## Failure Implications

If PostgreSQL is unavailable, TaskForge is unavailable for writes (job
submission, claiming, completion) — it does not silently degrade to an
inconsistent or partially-functioning mode (see
[failure-model.md](../failure-model.md) F6). This is a deliberate choice:
correctness under partial availability of a second system would be strictly
harder to reason about than "the one system we depend on is down, so we
correctly refuse writes."
