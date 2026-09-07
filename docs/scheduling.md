# Scheduling

Status: foundational. Defines delayed/scheduled job semantics.

## Core Design: Scheduling Is a Column, Not a Service

There is no separate "scheduler" process, in-memory timer wheel, or cron-like
component in TaskForge. A "scheduled job" is simply a `QUEUED` job whose
`eligible_at` is in the future (see [data-model.md](data-model.md),
[execution-semantics.md](execution-semantics.md)). The exact same claim
query workers already run to find normal work (see
[worker-protocol.md](worker-protocol.md)) is the only mechanism that ever
makes a scheduled job eligible — there is no additional code path.

This is a deliberate simplification recorded in
[ADR-0006](adr/0006-database-backed-queue-first.md): it eliminates an entire
category of "scheduler restart" or "scheduler/worker disagree about what's
eligible" bugs by construction, because there is only one source of truth
for eligibility (the `eligible_at` column) and only one piece of code that
reads it for this purpose (the claim query).

## Fields Involved

- `scheduled_at` — the caller's originally requested execution time (or
  NULL for "run as soon as possible"). Immutable once set. Exists purely
  for audit/display; it is never compared against `now()` by any
  correctness-critical code path.
- `eligible_at` — the live, mutable gating timestamp. Set to `scheduled_at`
  (or `now()` if `scheduled_at` is NULL) at submission time. Advanced on
  each retry per [retry-semantics.md](retry-semantics.md). This is the
  **only** field the claim query consults.

Keeping these separate means a job's retry backoff never destroys the
record of when the caller originally wanted it to run — useful for
debugging "why did this scheduled job take so long" without conflating
"scheduled late" with "retried a few times."

## Clock Model

Per [failure-model.md](failure-model.md), all eligibility comparisons
(`eligible_at <= now()`) are evaluated by PostgreSQL against PostgreSQL's
own `now()`. Neither the API server's nor any worker's local clock ever
participates in the eligibility decision. This directly satisfies
TF-INV-011 and eliminates cross-machine clock skew as a source of early or
late execution.

## Behavior After Restart

Because scheduling has no in-memory component, there is nothing to lose on
restart:

- **API server restart**: does not affect any already-persisted
  `eligible_at` value. New submissions after restart behave identically to
  before.
- **Worker restart**: a restarted worker simply resumes polling with the
  same claim query; any job whose `eligible_at` has already passed is
  claimable immediately, whether or not any worker was running at the
  moment it became eligible. There is no "missed schedule" concept —
  eligibility is a durable predicate, not an event that must be observed at
  the instant it becomes true.
- **Total worker fleet outage** spanning a scheduled job's `eligible_at`:
  the job simply accumulates queue age (visible via observability metrics,
  see [observability.md](observability.md)) until a worker comes back
  online and claims it. Nothing is lost; execution is merely delayed beyond
  what was requested. This is a direct consequence of the "no
  scheduler-as-a-service" design and is called out as an accepted tradeoff
  in [ADR-0006](adr/0006-database-backed-queue-first.md): a job's actual
  execution time is "at or after `eligible_at`," never "exactly at
  `eligible_at`" — TaskForge does not offer real-time scheduling
  guarantees, only ordering-after-eligibility guarantees.

## Precision Expectations

TaskForge's scheduling precision is bounded by worker poll interval plus
queue contention, not by wall-clock-accurate timer firing. This is
appropriate for its target use cases (delayed jobs on the order of seconds
to days — e.g., "retry in 30 seconds," "send this reminder tomorrow") and
explicitly not appropriate for use cases requiring sub-second firing
precision (e.g., a real-time trading system), which is out of scope (see
[vision.md](vision.md) non-goals).

## Relationship to Retry Backoff

`RETRY_WAIT` is, mechanically, indistinguishable from a scheduled job to the
claim query — both are rows with `eligible_at` in the (possibly very near)
future. See [retry-semantics.md](retry-semantics.md). This is why
[execution-semantics.md](execution-semantics.md) treats `QUEUED` and
`RETRY_WAIT` as claimed by the identical predicate.

## Cross-References

- Invariant: TF-INV-011 in [invariants.md](invariants.md)
- Scenario: SF-013 "scheduled job survives restart" in
  [scenario-corpus.md](scenario-corpus.md)
- Claim query: [worker-protocol.md](worker-protocol.md)
