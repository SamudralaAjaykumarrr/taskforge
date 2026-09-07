# TaskForge

**Status: ARCHITECTURE FOUNDATION.** No application code exists yet. This
repository currently contains only design documentation: a precise state
machine, a numbered list of system invariants, a failure model, a full
scenario corpus, and architecture decision records. Nothing described below
has been implemented, benchmarked, or deployed. Treat every claim in this
README as a design target, not a demonstrated capability.

## The Problem

How do we execute background jobs reliably when workers crash, jobs are
retried, messages may be duplicated, processing times out, multiple workers
compete for the same work, and partial failures occur?

TaskForge is a durable, distributed job and workflow execution engine built
to answer that question precisely — with a documented state machine,
numbered invariants, and a test-scenario corpus that maps directly to
production failure modes, rather than an untested assertion of
reliability. It is not a to-do-list app. See [docs/vision.md](docs/vision.md)
for what TaskForge is and, just as importantly, what it explicitly is not.

## The Core Reliability Thesis

TaskForge is designed to guarantee **at-least-once execution** with fenced,
exclusive worker ownership and durable, auditable state transitions. It is
**not** designed to guarantee, and will never claim, exactly-once execution
of arbitrary external side effects — that is a fundamental limit of any
system that performs side effects outside its own transaction, not a gap to
be engineered away. What TaskForge *does* target is exactly-once **logical
effects**, achieved through application-level idempotency patterns the
design provides stable identifiers for. See
[docs/vision.md](docs/vision.md) and
[ADR-0003](docs/adr/0003-at-least-once-execution-not-exactly-once.md).

## Architecture Overview

The target architecture (not yet built) is deliberately small: an API
server, a worker pool, and PostgreSQL as the single durable source of
truth. There is no message broker, no Redis, no Kubernetes-specific
dependency, and no separate scheduler service in the core design —
scheduling and retry backoff are both expressed as a single durable
timestamp column checked by the same query workers already use to find
work. Full detail: [docs/architecture.md](docs/architecture.md).

```
   API Server(s)  --->  PostgreSQL (source of truth)  <---  Worker Pool
   (stateless)          jobs, job_attempts                  (fenced leases,
                                                              heartbeats)
```

## Planned Reliability Properties

Once implemented, TaskForge targets (each backed by a numbered invariant in
[docs/invariants.md](docs/invariants.md) and a named scenario in
[docs/scenario-corpus.md](docs/scenario-corpus.md)):

- Durable job persistence — an accepted job is never silently lost.
- Fenced worker leasing — at most one worker owns a job at a time, and a
  worker that loses its lease cannot retroactively complete the job, even
  arbitrarily late (TF-INV-002, TF-INV-003, TF-INV-014).
- Automatic crash recovery via lease expiration — no permanently stranded
  jobs (TF-INV-004).
- Durable, monotonic retry history with configurable backoff and dead-letter
  behavior (TF-INV-006, TF-INV-007, TF-INV-009).
- Database-enforced submission idempotency (TF-INV-008, TF-INV-016).
- Deterministic cancellation-vs-completion race semantics (TF-INV-010).
- Scheduling that survives full process/fleet restarts (TF-INV-011).
- (Later phase) dependency-gated workflow/DAG execution (TF-INV-012).

None of these are implemented yet. They are documented now, precisely,
so that implementation has an unambiguous contract to satisfy and external
reviewers have something falsifiable to check it against.

## Project Status

| Area | Status |
|---|---|
| Architecture & invariant documentation | **Done** (this repository, current state) |
| PostgreSQL schema | Not started |
| API server | Not started |
| Worker / claim / lease protocol | Not started |
| Retry / backoff / DLQ | Not started |
| Idempotency enforcement | Not started |
| Scheduling | Not started |
| Cancellation / timeouts | Not started |
| Workflow / DAG execution | Not started (staged for a later phase) |
| Observability | Not started |
| Test suite (unit/integration/concurrency/chaos) | Not started |

See [docs/roadmap.md](docs/roadmap.md) for the full phased plan, from
Phase 1 (single-node durable job engine) through Phase 10 (external-review
hardening and v1.0), including required invariants, required tests, and
completion criteria for each phase.

## Documentation Map

| Document | Contents |
|---|---|
| [docs/vision.md](docs/vision.md) | What TaskForge is/is not, primary users, core reliability thesis, precise lifecycle terminology |
| [docs/architecture.md](docs/architecture.md) | Component overview, why no broker/Redis/Kubernetes dependency |
| [docs/failure-model.md](docs/failure-model.md) | Every assumed failure mode, handled-in-v1 vs. out-of-scope |
| [docs/invariants.md](docs/invariants.md) | Numbered system invariants (TF-INV-001 through TF-INV-016), each with a violating example and enforcement mechanism |
| [docs/execution-semantics.md](docs/execution-semantics.md) | The authoritative job state machine and transition table |
| [docs/data-model.md](docs/data-model.md) | Conceptual PostgreSQL schema, constraints, indexing strategy |
| [docs/worker-protocol.md](docs/worker-protocol.md) | Claim/heartbeat/completion SQL, fencing mechanics, API contract |
| [docs/retry-semantics.md](docs/retry-semantics.md) | Attempt numbering, backoff policy, dead-letter transition, crash-timing analysis |
| [docs/idempotency.md](docs/idempotency.md) | Submission idempotency vs. execution/side-effect idempotency |
| [docs/scheduling.md](docs/scheduling.md) | Delayed/scheduled job semantics, clock model |
| [docs/workflows.md](docs/workflows.md) | DAG execution design (staged for a later phase) |
| [docs/observability.md](docs/observability.md) | Metrics, structured logs, trace boundaries |
| [docs/testing-strategy.md](docs/testing-strategy.md) | Test categories and the invariant-to-test matrix |
| [docs/scenario-corpus.md](docs/scenario-corpus.md) | 18 named, deterministic test scenarios (SF-001 through SF-018) |
| [docs/roadmap.md](docs/roadmap.md) | Phased implementation plan with entry/exit criteria per phase |
| [docs/adr/](docs/adr/README.md) | Architecture decision records — the real tradeoffs behind the design |

## Technology Direction

- **Go** as the implementation language (not yet written).
- **PostgreSQL** as the sole durable source of truth — no additional
  infrastructure (Kafka, Redis, Kubernetes-specific tooling) unless a
  documented limitation of this design genuinely requires it. See
  [ADR-0001](docs/adr/0001-postgresql-as-source-of-truth.md) and
  [ADR-0006](docs/adr/0006-database-backed-queue-first.md).

## Contributing / Reviewing

This project is built to withstand skeptical review from backend,
platform, infrastructure/SRE, and distributed-systems engineers. If you are
reviewing this repository: the invariants in
[docs/invariants.md](docs/invariants.md) and the scenarios in
[docs/scenario-corpus.md](docs/scenario-corpus.md) are the intended target
for scrutiny — if you can find a sequence of events that violates one of
the stated invariants under the documented design, that is exactly the kind
of finding this documentation set exists to surface before implementation
begins.
