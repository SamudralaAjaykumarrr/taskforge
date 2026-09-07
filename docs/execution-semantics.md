# Execution Semantics: The Job State Machine

Status: foundational — this is the authoritative state machine. Any
implementation must match this document exactly; if implementation reveals
this document is wrong, this document is updated first, deliberately, with
the reasoning recorded (not silently patched).

## Why Not the Naming Suggested Elsewhere

A naive design uses separate `PENDING`, `SCHEDULED`, and `FAILED` states.
TaskForge deliberately consolidates these:

- `PENDING` and `SCHEDULED` are **the same state** (`QUEUED`) distinguished
  only by the value of `eligible_at` (now-or-past vs. future). Modeling them
  as separate states would require a transition between them driven purely
  by the passage of time, which adds a state-transition edge with no
  behavioral difference on either side of it. See
  [scheduling.md](scheduling.md) and
  [ADR-0006](adr/0006-database-backed-queue-first.md).
- `FAILED` as a **job-level terminal state distinct from `DEAD_LETTERED`**
  is not used. A single attempt failing is not a job-level event — it is
  recorded in `job_attempts` (attempt-level outcome), and the *job* only
  changes state when the failure either (a) still has retries left →
  `RETRY_WAIT`, or (b) has exhausted retries or was classified permanent →
  `DEAD_LETTERED`. A separate job-level `FAILED` state would either be
  redundant with `DEAD_LETTERED` or ambiguous about whether it is terminal.
  See [ADR-0008](adr/0008-explicit-terminal-states.md).

## States

| State | Terminal? | Meaning |
|---|---|---|
| `QUEUED` | No | Durable, not owned by any worker. Eligible for claiming once `eligible_at <= now()`. Covers both "immediately runnable" and "scheduled for the future." |
| `RETRY_WAIT` | No | Durable, not owned. A previous attempt failed retryably; the job is waiting for `eligible_at` (the computed backoff time) before it can be reclaimed. Functionally identical to `QUEUED` for claiming purposes — the distinct name exists purely for observability and history clarity (a `RETRY_WAIT` job has `attempt_count > 0`). |
| `RUNNING` | No | Owned by exactly one worker holding a valid `(lease_owner, lease_generation)`. Claim and execution start are the same transition in v1's synchronous execution model (see [roadmap.md](roadmap.md) Phase 1). |
| `SUCCEEDED` | **Yes** | The job handler reported success while its lease was still valid. |
| `CANCELLED` | **Yes** | The job was cancelled, either before any worker claimed it or while running (worker acknowledged the cancellation, or the cancellation won the completion race — see below). |
| `DEAD_LETTERED` | **Yes** | Retries exhausted, or the worker classified the failure as permanent. Requires human inspection/resubmission; TaskForge never automatically resubmits a dead-lettered job. |

There are exactly six job-level states. `job_attempts` rows carry their own,
richer set of per-attempt outcomes (see [data-model.md](data-model.md)):
`SUCCEEDED`, `FAILED_RETRYABLE`, `FAILED_PERMANENT`, `TIMED_OUT`,
`LEASE_EXPIRED`, `CANCELLED`.

## Cancellation Is a Flag, Not a State, While Running

Cancellation of a `RUNNING` job is modeled as a request
(`cancel_requested = true`, `cancel_requested_at`) layered on top of the
`RUNNING` state, not as a separate top-level state. This is a deliberate
choice: a job that is `RUNNING` with `cancel_requested = true` is still
`RUNNING` (a worker still holds it, may still be executing) until one of two
things durably happens: the worker acknowledges the cancellation (→
`CANCELLED`) or the worker completes first (→ `SUCCEEDED`/`RETRY_WAIT`/
`DEAD_LETTERED`, and the pending cancellation request is superseded — see
TF-INV-010). Modeling cancellation as a separate top-level state would
require answering "what state is a cancel-requested-but-still-executing job
in?" with an answer that isn't really a new state — it's `RUNNING` with
extra metadata.

Cancellation of a `QUEUED`/`RETRY_WAIT` job (not yet claimed) is a direct,
uncontested transition straight to `CANCELLED` — there is no race to
resolve because no worker is involved.

## Transition Table

| From | To | Trigger | Guard (WHERE clause enforced) |
|---|---|---|---|
| *(none — insert)* | `QUEUED` | `POST /jobs` commits | New row; `eligible_at` set to `now()` (immediate) or `scheduled_at` (future) |
| `QUEUED` | `RUNNING` | Claim | `eligible_at <= now()`, row locked via `FOR UPDATE SKIP LOCKED`, sets `lease_owner`, increments `lease_generation`, sets `lease_expires_at`, `heartbeat_at`, increments `attempt_count` to 1 |
| `RETRY_WAIT` | `RUNNING` | Claim | Same as above; `attempt_count` incremented to its next value |
| `RUNNING` | `SUCCEEDED` | Worker reports success | `lease_owner = ? AND lease_generation = ? AND state = 'RUNNING'` |
| `RUNNING` | `RETRY_WAIT` | Worker reports retryable failure, `attempt_count < max_attempts` | Same fencing guard; sets new `eligible_at` per backoff policy ([retry-semantics.md](retry-semantics.md)) |
| `RUNNING` | `DEAD_LETTERED` | Worker reports permanent failure, OR retryable failure with `attempt_count >= max_attempts` | Same fencing guard; copies failure reason onto job row (TF-INV-009) |
| `RUNNING` | `DEAD_LETTERED` | Lease expiration detected with `attempt_count >= max_attempts` (no further claim attempt occurs) | `state = 'RUNNING' AND lease_expires_at < now() AND attempt_count >= max_attempts` (sweeper or lazy check at claim time — see [worker-protocol.md](worker-protocol.md)) |
| `RUNNING` | `RUNNING` (reclaim) | Lease expired, `attempt_count < max_attempts` | Same claim query as `QUEUED`→`RUNNING`, but source predicate is `state='RUNNING' AND lease_expires_at < now()`; this both increments `lease_generation` and increments `attempt_count` |
| `RUNNING` | `CANCELLED` | Worker acknowledges a pending cancellation request | `lease_owner = ? AND lease_generation = ? AND state = 'RUNNING' AND cancel_requested = true` |
| `QUEUED` | `CANCELLED` | Cancellation request, job not yet claimed | `state = 'QUEUED'` |
| `RETRY_WAIT` | `CANCELLED` | Cancellation request, job not yet claimed | `state = 'RETRY_WAIT'` |
| `SUCCEEDED` | *(none)* | — | Terminal. No outbound transition exists. |
| `CANCELLED` | *(none)* | — | Terminal. No outbound transition exists. |
| `DEAD_LETTERED` | *(none)* | — | Terminal. No outbound transition exists. (Manual resubmission creates a **new** job row referencing the old one, it does not reopen the old row — see [roadmap.md](roadmap.md) Phase 3/4.) |

## Explicitly Forbidden Transitions (non-exhaustive — anything not in the
table above is forbidden by default)

- `SUCCEEDED → RUNNING` — forbidden. A completed job never re-executes.
- `CANCELLED → RUNNING` — forbidden. Enforced because the claim query's
  candidate predicate only ever selects `QUEUED`/`RETRY_WAIT`/expired-lease
  `RUNNING` rows — a `CANCELLED` row can never match.
- `DEAD_LETTERED → RETRY_WAIT` — forbidden. Dead-lettering is a deliberate,
  terminal, human-actionable boundary; TaskForge does not auto-resurrect
  dead-lettered jobs (see [retry-semantics.md](retry-semantics.md)).
- `RUNNING → QUEUED` directly (skipping `RETRY_WAIT`) — forbidden. A failed
  attempt with retries remaining always passes through `RETRY_WAIT` so that
  attempt history and backoff timing are recorded consistently; there is no
  shortcut back to immediate eligibility.
- Any transition whose guard clause does not match (stale
  `lease_generation`, wrong `lease_owner`, or a `state` that has already
  moved on) — forbidden by construction: the `UPDATE` affects zero rows and
  the caller receives an explicit rejection, never a silent success.

## Persisted Fields Relevant to State (see [data-model.md](data-model.md) for full schema)

| Field | Relevant to |
|---|---|
| `state` | The state itself |
| `eligible_at` | Governs `QUEUED`/`RETRY_WAIT` → `RUNNING` transition timing |
| `lease_owner`, `lease_generation`, `lease_expires_at` | Governs ownership and fencing for all `RUNNING`-sourced transitions |
| `heartbeat_at` | Observability + long-running job liveness (not itself a guard condition; `lease_expires_at` is the actual guard, heartbeats extend it) |
| `attempt_count`, `max_attempts` | Governs `RETRY_WAIT` vs `DEAD_LETTERED` decision |
| `cancel_requested`, `cancel_requested_at` | Governs cancellation-vs-completion race resolution while `RUNNING` |
| `last_error`, `last_error_class` | Set on `DEAD_LETTERED` (TF-INV-009), also updated on each `RETRY_WAIT` transition for visibility |
| `terminal_at` | Set exactly once, on the transition into any terminal state; never updated again |

## Cancellation and Timeouts

### Cancellation Race Rule (TF-INV-010)

**First durable terminal write wins.** Concretely:

| Scenario | Outcome |
|---|---|
| Cancel arrives before any claim (`QUEUED`/`RETRY_WAIT`) | Direct transition to `CANCELLED`. No race — no worker involved. |
| Cancel arrives while `RUNNING`, worker has not yet completed | `cancel_requested = true` is set. Worker observes this flag cooperatively (checked at heartbeat time and/or via a context cancellation signal — see [worker-protocol.md](worker-protocol.md)) and, if it can, stops and acknowledges cancellation → `CANCELLED`. |
| Cancel arrives while `RUNNING`, worker completes (success or failure report) **before** observing the flag or before the cancel transaction commits | The completion transition wins because it commits first and its guard (`state='RUNNING'`) still matched at that instant. The job reaches `SUCCEEDED`/`RETRY_WAIT`/`DEAD_LETTERED`. A cancellation request that arrives after this point finds `state != 'RUNNING'` and is rejected as a no-op, with the response indicating the job already reached a terminal/non-cancellable state. |
| Worker's completion call arrives **after** the cancellation transaction has already committed `CANCELLED` | The completion call's guard (`state = 'RUNNING'`) no longer matches; it affects zero rows and is rejected (same fencing mechanism as TF-INV-003/014). The job stays `CANCELLED`. The side effect, if the handler already performed it, is not undone — this is documented, not hidden (see [vision.md](vision.md) core thesis: TaskForge cannot roll back external side effects). |

This rule is deliberately simple: **whichever transaction's `UPDATE`
commits first wins**, because PostgreSQL serializes writes to the same row
and the loser's guard clause will not match afterward. No distributed
consensus or explicit "cancel token" negotiation is needed.

### Timeout Semantics

Two distinct timeout concepts, not to be confused:

1. **`execution_timeout`** (per job, configured at submission): the maximum
   wall-clock duration a *single attempt* is allowed to run before it is
   considered stuck. For a job that does not heartbeat, this equals the
   initial `lease_expires_at - claimed_at` window. For a job that does
   heartbeat (declares itself long-running), `execution_timeout` is instead
   enforced as `now() - started_at > execution_timeout`, checked
   independently of lease renewal — see [worker-protocol.md](worker-protocol.md).
2. **Lease TTL** (`lease_expires_at`): the mechanism that actually causes
   reclaim. For short jobs these are the same number. For long-running
   heartbeating jobs, the lease TTL is short (e.g. 30s) and renewed
   repeatedly, while `execution_timeout` is the outer bound across all
   those renewals.

A job that exceeds `execution_timeout` while still heartbeating is treated
as a `TIMED_OUT` attempt outcome (the worker's own client-side deadline
should fire and report `TIMED_OUT` cooperatively; if the worker fails to do
so, the job simply keeps heartbeating until the operator or a future
supervisory check intervenes — this gap is documented as an open question
in [roadmap.md](roadmap.md) Phase 6 rather than papered over).

A job whose worker stops heartbeating entirely is reclaimed purely via
lease expiration (TF-INV-004), regardless of which timeout concept applies.

### Cancel/Timeout Interaction Cases (from the task's required case list)

| Case | Resolution |
|---|---|
| Cancel before worker claim | Direct `→ CANCELLED`, no race. |
| Cancel while running | Flag set; resolved by TF-INV-010 race rule above. |
| Worker succeeds at the same instant cancellation arrives | Whichever `UPDATE` commits first wins (TF-INV-010); the other is rejected as a no-op. |
| Timeout while worker is still alive (heartbeating past `execution_timeout`) | Attempt is expected to self-report `TIMED_OUT`; if it does not, the job remains `RUNNING` until lease expiry or operator intervention (documented gap, not silently solved — see [roadmap.md](roadmap.md)). |
| Worker reports completion after timeout/lease expiry | If a new generation has already been issued (job reclaimed), the stale worker's completion is fenced and rejected (TF-INV-003/014). If no reclaim has happened yet (lease merely expired but no other worker has claimed), the completion call's guard (`lease_generation` match, `state='RUNNING'`) still succeeds — a late-but-not-yet-superseded completion is accepted. This is intentional: fencing prevents *incorrect* completions (from a superseded generation), not merely *late* ones from the still-current generation. |

## Cross-References

- Invariants this state machine must satisfy: [invariants.md](invariants.md)
- Claim/lease mechanics: [worker-protocol.md](worker-protocol.md)
- Retry/backoff decision logic: [retry-semantics.md](retry-semantics.md)
- Schema backing every field mentioned here: [data-model.md](data-model.md)
- ADR for consolidating states: [ADR-0005](adr/0005-durable-job-state-machine.md), [ADR-0008](adr/0008-explicit-terminal-states.md)
