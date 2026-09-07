# ADR-0005: Durable Job State Machine with Consolidated States

Status: Accepted

## Context

The task brief's candidate state list (`PENDING`, `SCHEDULED`, `RUNNING`,
`SUCCEEDED`, `FAILED`, `RETRY_WAIT`, `CANCELLED`, `DEAD_LETTERED`) is a
reasonable starting point but contains states that, on inspection, either
duplicate each other's behavior or conflate a job-level concept with an
attempt-level one. We need to settle on a final state set before writing
any transition-handling code, since every invariant in
[invariants.md](../invariants.md) and every query in
[worker-protocol.md](../worker-protocol.md) is defined in terms of the
state enum.

## Decision

The final job-level state enum is: `QUEUED`, `RETRY_WAIT`, `RUNNING`,
`SUCCEEDED`, `CANCELLED`, `DEAD_LETTERED` — six states. `PENDING` and
`SCHEDULED` are merged into `QUEUED`, distinguished only by the
`eligible_at` timestamp value. A job-level `FAILED` state is not used;
failure is recorded per-attempt in `job_attempts.outcome`, and the job
itself only moves on failure once the retry/dead-letter decision is made
(→ `RETRY_WAIT` or → `DEAD_LETTERED`). See
[execution-semantics.md](../execution-semantics.md) for the full rationale
and transition table.

## Alternatives Considered

- **Keep `PENDING` and `SCHEDULED` as separate states**, with a background
  process transitioning `SCHEDULED → PENDING` once `scheduled_at` passes.
  Rejected: this transition would carry zero behavioral difference on
  either side (both are "durable, unclaimed, waiting for eligibility") and
  would require exactly the kind of separate scheduler process
  [scheduling.md](../scheduling.md) and
  [ADR-0006](0006-database-backed-queue-first.md) argue against. It also
  adds a state-transition edge, and therefore an invariant surface, that
  proves nothing new.
- **Add a job-level `FAILED` state as a terminal state distinct from
  `DEAD_LETTERED`.** Rejected: it's ambiguous whether a single attempt
  failing should move the *job* to `FAILED` (implying terminal, but then
  what triggers a retry from a terminal state — a contradiction with
  TF-INV-005?) or whether `FAILED` should be non-terminal (in which case
  it's just a confusing rename of `RETRY_WAIT`). Every concrete meaning
  this state could have collapses into either `RETRY_WAIT` (non-terminal,
  retry pending) or `DEAD_LETTERED` (terminal, no more retries) — so it is
  omitted, per [ADR-0008](0008-explicit-terminal-states.md).
- **Represent cancellation as a top-level state machine node with its own
  transition edges from `RUNNING`, competing directly with completion
  transitions.** Considered and partially rejected: cancellation *of an
  unclaimed job* is a direct top-level transition
  (`QUEUED`/`RETRY_WAIT → CANCELLED`), but cancellation *of a running job*
  is modeled as a flag (`cancel_requested`) layered on `RUNNING`, not a
  separate state, because a "cancellation requested but not yet
  resolved" job is still meaningfully `RUNNING` (still owned, possibly
  still executing) — see [execution-semantics.md](../execution-semantics.md)
  "Cancellation Is a Flag, Not a State, While Running."

## Consequences

- Positive: A smaller state enum means a smaller, more exhaustively
  testable transition table (the state-machine table tests in
  [testing-strategy.md](../testing-strategy.md) enumerate all 6×6 pairs,
  not 8×8 or larger).
- Positive: `QUEUED`/`RETRY_WAIT` sharing identical claim-eligibility logic
  (see [worker-protocol.md](../worker-protocol.md)) means scheduling and
  retry backoff are implemented as the *same code path*, reducing the
  chance the two drift apart or are tested inconsistently.
- Negative: Observability queries that want to distinguish "jobs waiting
  for their first run" from "jobs waiting for a retry" must filter on
  `attempt_count = 0` vs `> 0` within `QUEUED`, rather than reading it
  directly off a `PENDING` vs `SCHEDULED` state value — a minor query
  ergonomics cost accepted in exchange for the simplification above.

## Failure Implications

A larger, less carefully consolidated state machine increases the surface
area for the exact bug class TF-INV-005 exists to prevent — an
under-specified state with an ambiguous terminal/non-terminal status is
exactly where an accidental "terminal → non-terminal" transition tends to
sneak in during implementation. Consolidating early, before code exists,
removes that ambiguity at the design stage rather than discovering it via
a production incident.
