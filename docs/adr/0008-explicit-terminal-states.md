# ADR-0008: Explicit, Minimal Terminal States

Status: Accepted

## Context

TF-INV-005 requires that terminal states never become non-terminal. This
property is only as strong as how clearly "terminal" is defined and how
few states carry that label ambiguously. We need to decide exactly which
states are terminal and commit to that set being closed (no code path ever
treats a terminal state as reopenable, including for operator convenience
features like manual retry).

## Decision

Exactly three states are terminal: `SUCCEEDED`, `CANCELLED`,
`DEAD_LETTERED`. Each is marked by setting `terminal_at` exactly once, and
no transition-writing query in [worker-protocol.md](../worker-protocol.md)
ever includes a terminal state on the *source* side of a state-changing
`WHERE` clause (every fenced write's guard is `state = 'RUNNING'` or, for
pre-claim cancellation, `state IN ('QUEUED','RETRY_WAIT')` — never a
terminal state). Manual resubmission of a `DEAD_LETTERED` job creates a
**new** job row (optionally linked via a `retried_from` field) rather than
mutating the old one back to a non-terminal state.

## Alternatives Considered

- **Allow `DEAD_LETTERED → RETRY_WAIT` for operator-triggered "retry this
  dead-lettered job" convenience.** Rejected: this directly violates
  TF-INV-005 for the sake of UX convenience. The same outcome (letting an
  operator get a failed job re-attempted) is achievable without breaking
  the invariant, by creating a new job that references the old one — a
  small API/UX difference that preserves a load-bearing correctness
  guarantee. This is called out explicitly because it is exactly the kind
  of "just this once, for convenience" exception that erodes an invariant
  in real systems if not decided deliberately up front.
- **Treat `CANCELLED` as non-terminal (allow a cancelled job to be
  "un-cancelled" and resumed) to support an operator changing their
  mind.** Rejected for the same reason — "resuming" a cancelled job is,
  semantically, submitting a new job, not reviving the old one, especially
  since a cancelled `RUNNING` job may have partially executed and its
  in-flight side effects are of unknown status (see
  [vision.md](../vision.md) core thesis about side-effect visibility).
- **Have a larger set of terminal states** (e.g., distinguishing
  `TIMED_OUT` as its own job-level terminal state rather than an
  attempt-level outcome that feeds into `RETRY_WAIT`/`DEAD_LETTERED`).
  Rejected per [ADR-0005](0005-durable-job-state-machine.md) — `TIMED_OUT`
  is meaningful at the attempt level (recorded in `job_attempts.outcome`)
  but does not need to be a distinct job-level terminal state, since a
  timed-out attempt is handled by the same retryable-failure decision path
  as any other retryable outcome.

## Consequences

- Positive: TF-INV-005 has a small, closed, exhaustively-testable surface
  — the state-machine table test in
  [testing-strategy.md](../testing-strategy.md) only needs to assert "no
  outbound edge exists" for exactly three states, not reason about a
  larger or fuzzier terminal set.
- Positive: "Retry a dead-lettered job" as a new-job-referencing-old-job
  operation is more auditable than reopening the old row would be — the
  full original attempt history stays intact and undisturbed under its
  original `job_id`, and the new attempt history starts clean under a new
  `job_id`, linked by `retried_from`.
- Negative: Operators/tooling that expect "just re-run this exact job" to
  mean "the same `job_id` continues" need to understand the
  new-job-with-a-reference model instead — a minor conceptual cost, worth
  paying to keep TF-INV-005 absolute rather than "absolute except for this
  one operator workflow."

## Failure Implications

This ADR is the direct implementation decision behind TF-INV-005's
"forbidden transitions" list in
[execution-semantics.md](../execution-semantics.md)
(`SUCCEEDED → RUNNING`, `CANCELLED → RUNNING`,
`DEAD_LETTERED → RETRY_WAIT`, all explicitly called out there). Any future
feature request that would require reopening a terminal state (however
well-intentioned) should be treated as a request to supersede this ADR
explicitly, with the consequences re-evaluated — not implemented as a
quiet special case.
