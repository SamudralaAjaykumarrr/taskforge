# Retry Semantics

Status: foundational. Governs the `RUNNING → RETRY_WAIT → RUNNING` cycle and
the `→ DEAD_LETTERED` boundary.

## Attempt Numbering

**Attempts are 1-based.** The first execution of a job is `attempt_number =
1` (equivalently, `attempt_count = 1` on the job row after the first
claim). This is chosen because "attempt 1 of 5" reads naturally to an
operator inspecting `job_attempts`, and because `attempt_count` doubling as
"how many attempts have happened so far" (a natural cardinal count) is more
useful for the `attempt_count >= max_attempts` comparison than a 0-based
index would be. This is stated explicitly because the task requires
resolving the ambiguity, not leaving it implicit.

## Retryable vs. Permanent Failures

TaskForge does not itself classify errors — the job handler does, by
reporting one of two outcomes when an attempt fails:

- **`FAILED_RETRYABLE`**: the handler believes the failure may succeed on a
  future attempt (e.g., a downstream 503, a transient network error).
- **`FAILED_PERMANENT`**: the handler believes retrying will not help (e.g.,
  a 400 Bad Request from a downstream API indicating the payload itself is
  invalid).

TaskForge trusts this classification in v1 — it has no visibility into the
semantics of the handler's side effect and cannot infer retryability
itself. A handler that misclassifies a failure (e.g., always reports
`FAILED_RETRYABLE`) will exhaust `max_attempts` and dead-letter normally;
this is a handler-authoring concern, not a gap in TaskForge, and is called
out in [testing-strategy.md](testing-strategy.md) as something a job
author's own tests should cover.

A `TIMED_OUT` or `LEASE_EXPIRED` attempt outcome (recorded when TaskForge
itself detects the failure, not the handler) is treated as **retryable** by
default — a timeout does not necessarily mean the work is unsafe to retry,
only that this attempt did not confirm success in time.

## Max Attempts and Dead-Lettering

`max_attempts` is set at submission time (default: 5, caller-overridable).
The transition decision after any failed attempt is:

```
if outcome == FAILED_PERMANENT:
    -> DEAD_LETTERED (regardless of attempt_count)
elif attempt_count >= max_attempts:
    -> DEAD_LETTERED
else:
    -> RETRY_WAIT (eligible_at = now() + backoff(attempt_count))
```

This is TF-INV-006 (ceiling respected) and TF-INV-009 (failure history
preserved on dead-letter) made concrete. See
[worker-protocol.md](worker-protocol.md) for the exact SQL.

`DEAD_LETTERED` is permanent (TF-INV-005). TaskForge never automatically
resubmits a dead-lettered job. An operator (or automation built on top of
the API) may manually create a **new** job that references the dead-lettered
one via a `retried_from` field — this is a fresh job with `attempt_count`
reset to 0, not a reopening of the old row.

## Backoff Policy (v1 default)

**Exponential backoff with equal jitter**, capped at a maximum delay:

```
base_delay   = 1s      (configurable per job type, later phase)
max_backoff  = 300s    (5 minutes)
attempt      = attempt_count (the attempt that just failed)

raw_delay = min(max_backoff, base_delay * 2^(attempt - 1))
delay     = (raw_delay / 2) + random_uniform(0, raw_delay / 2)

eligible_at = now() + delay
```

Equal jitter (half fixed, half random) is chosen over full jitter to avoid
the pathological case of a job getting `delay ≈ 0` by chance immediately
after a burst of failures (which is more likely under full jitter and can
cause thundering-herd retries), while still avoiding the synchronized-retry
problem of no jitter at all. This is a v1 default, not a claim that it is
optimal for every workload — per-job-type backoff configuration is a later
enhancement (see [roadmap.md](roadmap.md) Phase 3), tracked as an open
question below.

`eligible_at` is the same column used for scheduled-job delay (see
[scheduling.md](scheduling.md), [data-model.md](data-model.md)) —
deliberately: the claim query does not need to know or care whether a job
is waiting because it's scheduled for the future or because it's backing
off from a failure. This is why `RETRY_WAIT` and `QUEUED` are claimed by the
identical predicate in [worker-protocol.md](worker-protocol.md).

## Crash Timing and Duplicate Side Effects

The task requires explicitly discussing what happens if the worker crashes
at each point in the attempt lifecycle. Restated precisely against the
state machine:

| Crash point | What TaskForge observes | Outcome |
|---|---|---|
| **Before executing** (crash immediately after claim, handler never invoked) | Lease expires, no completion call ever arrives | Job reclaimed once `lease_expires_at < now()`; no side effect occurred, so retry is safe with no duplication risk. |
| **During executing** (handler is mid-flight, side effect not yet performed) | Same as above | Same as above — safe retry. |
| **After side effect, before acknowledgement** (the dangerous case) | Lease expires, no completion call ever arrives (the process died before it could call the completion endpoint) | Job is reclaimed and **retried**, which **may re-invoke the handler and re-perform the side effect**. TaskForge has no way to distinguish this from the "during executing" case — it only observes "no acknowledgement arrived." This is the fundamental reason exactly-once execution is unattainable (see [vision.md](vision.md), [ADR-0003](adr/0003-at-least-once-execution-not-exactly-once.md)); it is why [idempotency.md](idempotency.md) exists as a first-class concern rather than an afterthought. |
| **Before recording success** (handler returned success internally, but the completion HTTP/RPC call to record it durably failed or the process died before making it) | Same as above — indistinguishable from the previous row from TaskForge's point of view | Same as above. |

The takeaway, stated plainly: **any attempt outcome that is not durably
acknowledged before the lease expires will be retried, and if the side
effect already happened, it may happen again.** TaskForge's contribution is
making this window as small and as observable as possible (short leases for
non-heartbeating jobs, prompt heartbeat-based liveness for long jobs) and
providing the identifiers needed for the handler to make its own side
effects idempotent — not eliminating the window, which cannot be done by a
system that does not participate in the side effect's own transaction.

## Retry-Specific Invariants

- TF-INV-006 (limits respected)
- TF-INV-007 (history durable and monotonic)
- TF-INV-009 (dead-letter preserves failure reason)

See [invariants.md](invariants.md) for full detail.

## Open Questions

- Per-job-type backoff configuration (different base delay/max/jitter per
  `job_type`) is deferred past v1; the schema's `jobs` table would need
  either per-row override columns or a `job_type` config table. Not
  designed yet — tracked for Phase 3 revisit.
- Whether a handler should be able to request a *specific* retry delay
  (rather than accepting the computed backoff) is deferred; v1 gives the
  handler only a binary retryable/permanent signal.
