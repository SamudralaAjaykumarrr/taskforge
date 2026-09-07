# Architecture Decision Records

This directory records the real engineering decisions behind TaskForge's
design — not filler documentation. Each ADR captures a decision that had a
genuine alternative, the reasoning for the choice made, and the
consequences (including negative ones) accepted as a result.

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-postgresql-as-source-of-truth.md) | PostgreSQL as the sole source of truth | Accepted |
| [0002](0002-lease-based-worker-ownership.md) | Lease-based worker ownership with fencing generations | Accepted |
| [0003](0003-at-least-once-execution-not-exactly-once.md) | At-least-once execution, not exactly-once | Accepted |
| [0004](0004-idempotency-for-exactly-once-effects.md) | Idempotency as the mechanism for exactly-once logical effects | Accepted |
| [0005](0005-durable-job-state-machine.md) | Durable job state machine with consolidated states | Accepted |
| [0006](0006-database-backed-queue-first.md) | Database-backed queue before any message broker | Accepted |
| [0007](0007-monotonic-attempt-history.md) | Monotonic, append-only attempt history | Accepted |
| [0008](0008-explicit-terminal-states.md) | Explicit, minimal terminal states | Accepted |

## Conventions

- ADRs are numbered sequentially and never renumbered or deleted, even if
  superseded — a superseding ADR references the one it replaces.
- Every ADR includes: Context, Decision, Alternatives Considered,
  Consequences, and Failure Implications.
- An ADR describes a decision that was actually weighed against a real
  alternative. If there was no genuine alternative, it belongs in
  [architecture.md](../architecture.md) as a description, not here as a
  decision.
