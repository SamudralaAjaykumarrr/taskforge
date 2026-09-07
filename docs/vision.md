# Vision

Status: foundational — defines scope before any implementation exists.

## What TaskForge Is

TaskForge is a durable, distributed job and workflow execution engine. Its
purpose is to answer one question precisely and honestly:

> How do we execute background work reliably when workers crash, jobs are
> retried, messages may be duplicated, processing times out, multiple workers
> compete for the same work, and partial failures occur?

TaskForge is a demonstration of the engineering discipline required to answer
that question in a way that survives skeptical review: precise state
machines, documented invariants, fencing against split-brain execution,
durable retry history, and a test suite that proves the invariants rather
than merely exercising the happy path.

The system is built around a single durable source of truth (PostgreSQL) and
a small number of well-specified protocols: job submission, worker leasing,
heartbeats, completion, retry, and (later) workflow dependency resolution.

## What TaskForge Is NOT

- **Not a generic task-manager / to-do CRUD app.** There is no concept of a
  human-facing task list, project board, or collaboration UI.
- **Not a message broker.** TaskForge does not aim to replace Kafka, SQS, or
  RabbitMQ as a high-throughput pub/sub transport. It is a job *execution*
  system with strong durability and ownership semantics, not a streaming
  system.
- **Not a general workflow orchestration platform** (e.g. not a Temporal or
  Airflow competitor) in v1. Workflow/DAG execution is a later, explicitly
  staged phase ([roadmap.md](roadmap.md)), built on top of the same job
  primitives rather than as a separate engine.
- **Not horizontally infinite.** v1 targets correctness and a single
  PostgreSQL instance as the durability boundary, not planet-scale
  throughput. Scaling the durable store is an explicit non-goal for v1.
- **Not a guarantee of exactly-once execution of arbitrary external side
  effects.** See "The Core Reliability Thesis" below. This is the single
  most important thing this document states.

## Primary Users

- **Backend / platform engineers** who need to run background jobs
  (webhooks, emails, billing operations, data pipeline steps) with
  durability guarantees stronger than "fire and hope."
- **Infrastructure / SRE engineers** evaluating or operating a job engine who
  need to reason precisely about failure modes, recovery time, and
  observability.
- **Distributed systems engineers / reviewers** evaluating the credibility of
  the reliability claims made by the project — this includes the target
  audience for external review of the codebase itself.

TaskForge is also, deliberately, a teaching artifact: every non-obvious
decision is recorded as an ADR ([docs/adr/](adr/README.md)) so a reader can
audit *why*, not just *what*.

## Primary Use Cases

- Executing a webhook delivery with retries and backoff.
- Running a scheduled/delayed job (e.g., "send this reminder at 09:00 UTC
  tomorrow").
- Executing a multi-step pipeline with dependencies (e.g., "extract, then
  transform, then load, then notify") — Phase 7+.
- Providing an idempotent submission API so that a caller can safely retry a
  job submission over an unreliable network without creating duplicate work.
- Giving operators a way to observe queue depth, retry rates, dead-letter
  volume, and lease health without instrumenting each job type by hand.

## Explicit Non-Goals for v1

- Multi-region / multi-datacenter durability.
- Pluggable storage backends (v1 is PostgreSQL-only; see
  [ADR-0001](adr/0001-postgresql-as-source-of-truth.md)).
- A general-purpose workflow DSL or visual editor.
- Exactly-once delivery of arbitrary external side effects (see below).
- High-throughput streaming ingestion (millions of jobs/sec). TaskForge
  targets correctness under moderate throughput first; performance work is
  staged in [roadmap.md](roadmap.md) Phase 9.
- Multi-tenant isolation / auth / quota enforcement.
- Kubernetes-native operators, Helm charts, or any deployment automation
  beyond what is needed to run and test the system locally.

## The Core Reliability Thesis

**TaskForge guarantees at-least-once execution of jobs, with durable,
auditable state transitions and fenced worker ownership. It does not, and
cannot, guarantee exactly-once execution of arbitrary external side
effects.**

This is not a limitation specific to TaskForge — it is a fundamental property
of distributed systems that perform side effects outside their own
transactional boundary (see [ADR-0003](adr/0003-at-least-once-execution-not-exactly-once.md)).
No job engine can atomically couple "I ran your handler" with "your handler's
external side effect (an HTTP call, an email send, a charge) took effect
exactly once," because the side effect and the durable record of having
performed it are not part of the same transaction.

What TaskForge *can* guarantee, and does guarantee, is:

1. **At-least-once execution**: every accepted job will be attempted at least
   once, and will keep being retried (subject to retry policy) until it
   reaches a terminal state, even across worker crashes and process
   restarts.
2. **Exactly-once logical effects, where the job handler cooperates**: if a
   job's side effect is made idempotent (via an idempotency key passed
   through to the downstream system, or a database unique constraint, or a
   transactional outbox), then the *logical* effect happens exactly once
   even though the *handler code* may run more than once. TaskForge supplies
   the stable identifiers (`job_id`, `attempt_number`, `lease_generation`)
   needed to build that idempotency; see [idempotency.md](idempotency.md).
3. **Fenced ownership**: at most one worker holds authoritative ownership of
   a job at any instant, and a worker that loses its lease cannot
   retroactively "win" after another worker has taken over (see
   [invariants.md](invariants.md) TF-INV-002/003/014).

## The Job Lifecycle: Distinguishing Related but Different Concepts

These terms are used precisely and consistently across every document in
this repository. Conflating them is the single most common source of bugs in
systems like this, so they are called out explicitly:

| Term | Meaning |
|---|---|
| **Job acceptance** | The API has validated a submission request and is about to persist it. Acceptance is not yet durable. |
| **Job persistence** | The job's durable row exists and has committed to PostgreSQL. Only after this point does the job "exist" from the system's point of view. Acceptance without persistence must never be acknowledged to the caller (see [ADR](adr/0006-database-backed-queue-first.md) and the API contract in [architecture.md](architecture.md)). |
| **Job eligibility** | The job's `eligible_at` timestamp has passed, so it is a candidate for claiming. A persisted job is not necessarily eligible yet (e.g. scheduled jobs, retry backoff). |
| **Worker ownership** | A specific worker holds a valid, unexpired lease (a `(lease_owner, lease_generation)` pair) on the job. Ownership is exclusive and fenced — see [invariants.md](invariants.md) TF-INV-002. |
| **Execution** | The worker is running the job handler. Execution happens *inside* a window of ownership but is not itself tracked transactionally by TaskForge — TaskForge only sees the start (claim) and the end (completion report or lease expiry). |
| **Side effects** | Whatever the job handler does to the outside world (HTTP calls, writes to other systems, emails). TaskForge has no visibility into side effects and cannot roll them back. |
| **Acknowledgement** | The worker's durable report of an attempt's outcome (success, retryable failure, permanent failure), submitted while its lease is still valid. An acknowledgement is only authoritative if it passes the fencing check (TF-INV-003). |
| **Retry** | A new attempt at the same logical job, after a durable transition through `RETRY_WAIT`, governed by the retry policy in [retry-semantics.md](retry-semantics.md). |
| **Terminal completion** | The job has reached a state from which no further transition is possible (`SUCCEEDED`, `CANCELLED`, or `DEAD_LETTERED`). See [execution-semantics.md](execution-semantics.md). |

The gap between **execution** and **acknowledgement** is exactly where
duplicate side effects can occur (a worker performs the side effect, then
crashes before acknowledging) — this is why exactly-once *execution* is
unattainable but exactly-once *logical effect* is attainable through
idempotency. See [failure-model.md](failure-model.md) scenario "worker
crashes after external side effect but before acknowledgement."

## Non-Negotiable Properties (carried through every phase)

- A job, once durably accepted, is never silently lost (TF-INV-001).
- A terminal state is permanent (TF-INV-005).
- Ownership is exclusive and fenced (TF-INV-002, TF-INV-003, TF-INV-014).
- Retry history is durable, monotonic, and auditable (TF-INV-007).

See [invariants.md](invariants.md) for the full, numbered list and their
enforcement mechanisms.
