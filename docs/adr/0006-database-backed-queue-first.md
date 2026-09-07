# ADR-0006: Database-Backed Queue Before Any Message Broker

Status: Accepted

## Context

Job engines conventionally use a message broker (Kafka, SQS, RabbitMQ) as
the queue layer. TaskForge's stated design philosophy is "smallest
architecture capable of proving the reliability properties" (see
[architecture.md](../architecture.md)), and needs a decision, made early,
about whether a broker belongs in the core v1 architecture at all.

## Decision

TaskForge's queue is PostgreSQL itself: eligible work is a query predicate
(`state IN ('QUEUED','RETRY_WAIT') AND eligible_at <= now()`, plus the
expired-lease reclaim branch), and claiming is a
`SELECT ... FOR UPDATE SKIP LOCKED` combined with a conditional `UPDATE ...
RETURNING` (see [worker-protocol.md](../worker-protocol.md)). No message
broker is part of the core v1 architecture. Scheduling
([scheduling.md](../scheduling.md)) and retry backoff
([retry-semantics.md](../retry-semantics.md)) are both implemented as the
same mechanism — a durable `eligible_at` timestamp — rather than as
separate broker-delay features (e.g., SQS delay queues, Kafka scheduled
delivery via a separate component).

## Alternatives Considered

- **Kafka as the primary queue**, with PostgreSQL only for state/history.
  Rejected for v1: Kafka's consumer-group model does not natively express
  "exactly one consumer claims exactly this message, with a
  fencable, per-message lease and per-message retry backoff" — building
  that on top of Kafka requires essentially reimplementing the lease/state
  table in a side database anyway (at which point PostgreSQL alone, used
  directly, is simpler and has one fewer moving part to keep consistent).
  Kafka's strengths (very high throughput, log compaction, multi-consumer
  fan-out) are not what v1 needs to prove; see
  [vision.md](../vision.md) non-goals.
- **SQS (or a similar managed queue) for delivery, PostgreSQL for state.**
  Rejected for the same reason: SQS visibility timeouts are a coarser,
  less introspectable version of the lease mechanism TaskForge already
  implements natively and testably in PostgreSQL, and SQS is a
  vendor-specific dependency the project explicitly wants to avoid
  requiring for a demonstration of underlying distributed-systems
  technique (see [architecture.md](../architecture.md) design philosophy).
- **A dedicated in-memory scheduler process for delayed/scheduled jobs**,
  separate from the worker claim loop. Rejected: this reintroduces a
  "scheduler restart" failure mode (F19 in
  [failure-model.md](../failure-model.md)) that the eligible_at-as-column
  design eliminates entirely by construction — there is no separate
  process holding scheduling state in memory to lose on restart.

## Consequences

- Positive: Every reliability property (claim exclusivity, lease fencing,
  retry backoff, scheduling) is provable via direct PostgreSQL transactions
  and constraints, testable with ordinary integration tests against a real
  database — no need to reason about cross-system consistency between a
  broker and a database.
- Positive: Fewer infrastructure dependencies to run locally, in CI, and in
  the eventual production deployment story — directly serves the "smallest
  architecture" philosophy and keeps the project approachable for external
  review and contribution.
- Negative: Throughput ceiling is bounded by what a single PostgreSQL
  primary can sustain for the claim-query write pattern (`UPDATE` under
  row lock contention at high polling frequency across many workers). This
  is an accepted v1 tradeoff (see [ADR-0001](0001-postgresql-as-source-of-truth.md))
  and is explicitly named, not hidden, as a scaling question for a future
  phase if TaskForge's throughput needs ever exceed what this design
  supports — at that point, a broker could be introduced as a *delivery
  layer in front of* the same durable jobs table (workers still ultimately
  claim from PostgreSQL, but notified/batched via the broker to reduce
  polling overhead), not as a wholesale replacement of the durable model.
- Negative: No native multi-consumer fan-out (the same job delivered to
  many independent consumers) — TaskForge's model is strictly "one job, one
  successful execution, at most one owner at a time," which is correct for
  its target use case (job execution) but is a different problem from
  pub/sub fan-out, which remains explicitly out of scope
  ([vision.md](../vision.md)).

## Failure Implications

This decision directly addresses F19 (scheduler restart) by eliminating
the separate component entirely, and underlies the uniform treatment of
`QUEUED` and `RETRY_WAIT` claiming in
[worker-protocol.md](../worker-protocol.md). If TaskForge later needs to
introduce a broker for throughput reasons, this ADR should be revisited
and superseded explicitly — it should never be quietly bypassed by adding
broker-based delivery as a side channel that could disagree with
PostgreSQL's state about what has been claimed.
