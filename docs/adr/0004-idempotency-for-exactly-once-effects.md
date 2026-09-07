# ADR-0004: Idempotency as the Mechanism for Exactly-Once Logical Effects

Status: Accepted

## Context

Given [ADR-0003](0003-at-least-once-execution-not-exactly-once.md) commits
TaskForge to at-least-once *execution*, we need a documented, honest answer
to "how does a caller get exactly-once *effect* when they need it?" Two
separate problems exist and are frequently conflated: making job
*submission* safe to retry (a solvable problem within TaskForge's own
boundary), and making job *side effects* safe to repeat (a problem
TaskForge can enable but not solve on the handler's behalf, since it
doesn't control the downstream system). See
[idempotency.md](../idempotency.md).

## Decision

TaskForge solves submission idempotency completely and unconditionally, via
a database-enforced unique constraint on `(job_type, idempotency_key)`
(TF-INV-008, TF-INV-016). For execution/side-effect idempotency, TaskForge
does not attempt to solve the general problem; instead it provides the
stable identifiers (`job_id`, `attempt_number`) a handler needs to
implement its own idempotency against the specific downstream system it
calls, and documents the standard patterns (downstream idempotency keys,
database unique constraints, transactional outbox, dedup token tables) as
guidance rather than infrastructure.

## Alternatives Considered

- **Build a generic "exactly-once side-effect" framework (e.g., a built-in
  transactional outbox or generic dedup-token service) as a core TaskForge
  component.** Rejected for v1: this would only work for side effects that
  are themselves writes to a system TaskForge can integrate with (e.g.,
  another table in the same PostgreSQL instance) — it does not generalize
  to arbitrary third-party HTTP APIs, which are TaskForge's primary target
  use case. Building infrastructure that only covers a subset of real use
  cases while implying it covers all of them would reintroduce the
  overreach [ADR-0003](0003-at-least-once-execution-not-exactly-once.md)
  exists to avoid. This may be revisited as an optional, clearly-scoped
  add-on in a later phase (see [idempotency.md](../idempotency.md) open
  questions), not as a core guarantee.
- **Require handlers to be idempotent by contract, with no TaskForge-side
  identifiers provided.** Rejected: without a stable `job_id` surfaced to
  the handler, authors would have to invent their own idempotency keys
  independently, likely inconsistently, and possibly by hashing payloads
  (fragile — see [idempotency.md](../idempotency.md) open question about
  payload-equality semantics). Surfacing `job_id` is a small, cheap thing
  for TaskForge to do that meaningfully simplifies the handler author's
  job.
- **Solve submission idempotency with a check-then-act pattern (`SELECT`
  then `INSERT` if not found) instead of a database constraint.** Rejected
  — this reintroduces exactly the TOCTOU race TF-INV-008 exists to close
  under concurrent retries; see [ADR-0001](0001-postgresql-as-source-of-truth.md)
  and TF-INV-016's rationale for pushing correctness into the database
  layer rather than trusting application-level sequencing.

## Consequences

- Positive: Submission idempotency is unconditionally strong — a property
  provable by a single concurrency test (SF-005), not dependent on
  handler-author diligence.
- Positive: The `job_id`/`attempt_number` identifiers cost nothing extra to
  provide (they already exist for [invariants.md](../invariants.md)
  TF-INV-007's attempt history) and directly enable the documented
  patterns in [idempotency.md](../idempotency.md).
- Negative: Execution/side-effect idempotency quality varies by handler
  author discipline — TaskForge cannot guarantee it, only enable it. This
  is stated plainly rather than hidden, per
  [ADR-0003](0003-at-least-once-execution-not-exactly-once.md).

## Failure Implications

Without this decision, TaskForge would either (a) falsely imply
exactly-once side effects, damaging credibility on discovery, or (b)
provide no tooling at all for handler authors to protect themselves,
making the system less useful than it could honestly be. This decision is
the concrete resolution of that tension: solve what's solvable
(submission) completely; document and enable, but do not overclaim, what
isn't (side effects).
