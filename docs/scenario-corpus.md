# Scenario Corpus

Status: foundational. Each scenario below is a deterministic, named test
case. Together they form the executable specification of TaskForge's
reliability claims — every "Yes" entry in
[failure-model.md](failure-model.md) and every invariant in
[invariants.md](invariants.md) must be covered by at least one scenario
here (cross-referenced both ways via [testing-strategy.md](testing-strategy.md)'s
matrix).

Format per scenario: **Initial state**, **Actions**, **Fault** (if any),
**Expected durable state**, **Invariants proved**.

---

### SF-001 — Normal success

- **Initial state**: No job exists.
- **Actions**: `POST /jobs`. Worker claims it. Handler succeeds. Worker
  reports success.
- **Fault**: None.
- **Expected durable state**: `SUCCEEDED`, `attempt_count = 1`, one
  `job_attempts` row with `outcome = SUCCEEDED`.
- **Invariants proved**: TF-INV-001 (job persisted and observable
  throughout), TF-INV-007 (attempt history correct).

### SF-002 — Worker crashes before execution

- **Initial state**: Job `QUEUED`.
- **Actions**: Worker A claims job (`RUNNING`, generation 1). Worker A is
  killed before invoking the handler.
- **Fault**: Process kill.
- **Expected durable state**: Job remains `RUNNING` (generation 1) until
  `lease_expires_at` passes, then becomes claimable; Worker B claims it
  (generation 2) and succeeds. Final state `SUCCEEDED`, `attempt_count = 2`.
- **Invariants proved**: TF-INV-004, TF-INV-002.

### SF-003 — Worker crashes during execution

- **Initial state**: Job `RUNNING` under Worker A, generation 1, handler
  invoked but not yet returned.
- **Actions**: Worker A is killed mid-handler.
- **Fault**: Process kill.
- **Expected durable state**: Same reclaim behavior as SF-002.
- **Invariants proved**: TF-INV-004.

### SF-004 — Worker crashes after side effect, before acknowledgement

- **Initial state**: Job `RUNNING` under Worker A, generation 1. Handler's
  (simulated/test-double) side effect has been recorded as "performed" by
  the test harness.
- **Actions**: Worker A is killed after performing the side effect but
  before calling the completion endpoint.
- **Fault**: Process kill at the specific point between side effect and
  acknowledgement.
- **Expected durable state**: Job is reclaimed (generation 2) and retried;
  the test harness's side-effect double is invoked a second time,
  demonstrating the duplicate-side-effect risk documented in
  [retry-semantics.md](retry-semantics.md). This scenario's purpose is to
  **prove the documented limitation is real and observable**, not to prove
  it is prevented — it is a specification test for
  [ADR-0003](adr/0003-at-least-once-execution-not-exactly-once.md), and a
  companion test demonstrates that a handler using a `job_id`-keyed
  idempotency token ([idempotency.md](idempotency.md)) avoids the
  duplicate *logical* effect even though the handler code ran twice.
- **Invariants proved**: Documents the boundary of TF-INV-004 (job is not
  stranded) versus what TaskForge does not claim (side effect exactly-once).

### SF-005 — Duplicate submission, same idempotency key

- **Initial state**: No job exists.
- **Actions**: N concurrent `POST /jobs` requests with identical
  `job_type` and `Idempotency-Key`.
- **Fault**: None (concurrency itself is the stressor).
- **Expected durable state**: Exactly one `jobs` row for that
  `(job_type, idempotency_key)` pair; all N HTTP responses return the same
  `job_id`.
- **Invariants proved**: TF-INV-008, TF-INV-016.

### SF-006 — Two workers race for same job

- **Initial state**: Job `QUEUED`.
- **Actions**: Worker A and Worker B both run their claim query
  concurrently against a pool containing this job.
- **Fault**: None (concurrency itself is the stressor).
- **Expected durable state**: Exactly one worker's claim succeeds
  (`RUNNING`, generation 1, `lease_owner` = the winner); the other worker's
  claim query simply does not return this row (via `SKIP LOCKED`), not an
  error.
- **Invariants proved**: TF-INV-002.

### SF-007 — Lease expires and job is reclaimed

- **Initial state**: Job `RUNNING` under Worker A, generation 1,
  `lease_expires_at` in the near future (short test TTL).
- **Actions**: Worker A stops heartbeating (simulated hang, not killed —
  distinguishing "unresponsive" from "crashed" at the process level, though
  TaskForge treats them identically). Time advances past
  `lease_expires_at`.
- **Fault**: Heartbeat loss / stall.
- **Expected durable state**: Worker B's claim query picks up the job
  (generation 2). If Worker A later attempts to heartbeat or complete with
  generation 1, it is rejected (see SF-008).
- **Invariants proved**: TF-INV-004, TF-INV-011 (eligibility timing).

### SF-008 — Stale worker attempts completion after losing its lease

- **Initial state**: Job `RUNNING` under Worker A, generation 1.
- **Actions**: Lease expires. Worker B claims (generation 2) and completes
  the job (`SUCCEEDED`). Worker A, unaware, later calls the completion
  endpoint with generation 1.
- **Fault**: Worker A's delayed/stale completion call.
- **Expected durable state**: Worker A's call affects zero rows and is
  reported as rejected/stale. Job remains `SUCCEEDED` from Worker B's
  completion, untouched by Worker A's call.
- **Invariants proved**: TF-INV-003, TF-INV-014 (this is the canonical test
  for both — extended variant runs 3+ generations to specifically stress
  TF-INV-014's "arbitrarily late arrival" clause).

### SF-009 — Retryable failure eventually succeeds

- **Initial state**: Job `QUEUED`, `max_attempts = 5`.
- **Actions**: Attempts 1 and 2 report `FAILED_RETRYABLE`. Attempt 3
  reports `SUCCEEDED`.
- **Fault**: Simulated transient handler failures.
- **Expected durable state**: `SUCCEEDED`, `attempt_count = 3`, three
  `job_attempts` rows (`FAILED_RETRYABLE`, `FAILED_RETRYABLE`,
  `SUCCEEDED`), `eligible_at` for attempts 2 and 3 reflecting computed
  backoff from the prior failure.
- **Invariants proved**: TF-INV-006, TF-INV-007.

### SF-010 — Retries exhausted, transitions to DLQ

- **Initial state**: Job `QUEUED`, `max_attempts = 3`.
- **Actions**: All 3 attempts report `FAILED_RETRYABLE`.
- **Fault**: Sustained handler failure.
- **Expected durable state**: `DEAD_LETTERED` after attempt 3, `last_error`
  matches attempt 3's error, all 3 `job_attempts` rows present and
  queryable.
- **Invariants proved**: TF-INV-006, TF-INV-009.

### SF-011 — Cancellation before claim

- **Initial state**: Job `QUEUED`, not yet claimed.
- **Actions**: `POST /jobs/{id}/cancel`.
- **Fault**: None.
- **Expected durable state**: `CANCELLED` immediately, `terminal_at` set. A
  subsequent claim query never selects this row.
- **Invariants proved**: TF-INV-005 (no transition out of `CANCELLED`
  afterward, verified by a follow-up claim attempt).

### SF-012 — Cancellation races with completion

- **Initial state**: Job `RUNNING` under Worker A, generation 1.
- **Actions**: A cancellation request and Worker A's success report are
  issued concurrently, with test-controlled transaction ordering forcing
  both interleavings across repeated runs: (a) cancel commits first, (b)
  completion commits first.
- **Fault**: None (deliberate race construction is the stressor).
- **Expected durable state**: (a) `CANCELLED`, Worker A's later completion
  call rejected (zero rows affected). (b) `SUCCEEDED`, the cancellation
  request's later attempt finds `state != RUNNING` and is reported as a
  no-op against an already-terminal job.
- **Invariants proved**: TF-INV-010.

### SF-013 — Scheduled job survives restart

- **Initial state**: Job submitted with `scheduled_at` in the future;
  `eligible_at` set accordingly.
- **Actions**: API server and all workers are stopped entirely for a period
  spanning `eligible_at`, then restarted.
- **Fault**: Full process restart, not just a single component.
- **Expected durable state**: Once workers resume polling, the job is
  claimed and executed normally — no special "catch-up" logic is needed or
  invoked, because eligibility was never dependent on any running process.
- **Invariants proved**: TF-INV-011, TF-INV-001.

### SF-014 — Database failure during transition

- **Initial state**: Job `RUNNING` under Worker A.
- **Actions**: Worker A's completion call begins a transaction; the
  connection is forcibly severed (or a constraint violation is injected)
  before commit.
- **Fault**: Injected connection failure / forced rollback.
- **Expected durable state**: Job row is byte-for-byte unchanged from
  before the attempted transition — still `RUNNING`, generation and lease
  fields untouched. The job is later reclaimed normally once its lease
  expires.
- **Invariants proved**: TF-INV-013.

### SF-015 — Terminal state cannot reopen

- **Initial state**: Job `SUCCEEDED` (or `CANCELLED`, or `DEAD_LETTERED` —
  run as three sub-cases).
- **Actions**: Attempt every other transition against this row: claim,
  heartbeat, complete (success/retryable/permanent), cancel.
- **Fault**: None — this is an exhaustive negative test.
- **Expected durable state**: Every attempted transition affects zero rows
  and is rejected; the row's state and `terminal_at` never change.
- **Invariants proved**: TF-INV-005.

### SF-016 — Heartbeat delay

- **Initial state**: Job `RUNNING` under Worker A, heartbeating.
- **Actions**: One heartbeat is delayed (simulated network blip) but
  arrives before `lease_expires_at`.
- **Fault**: Transient delay, not loss.
- **Expected durable state**: `lease_expires_at` extends normally once the
  delayed heartbeat arrives; the job is never spuriously reclaimed despite
  the missed on-schedule heartbeat.
- **Invariants proved**: TF-INV-015 (extension-only, tolerance for a single
  missed cycle).

### SF-017 — Long-running job renewal

- **Initial state**: Job `RUNNING` under Worker A, a long-running handler
  (execution time exceeding a single lease TTL) heartbeating on schedule.
- **Actions**: Multiple heartbeat cycles renew the lease repeatedly across
  a duration longer than any single `lease_expires_at` window.
- **Fault**: None — this validates the intended long-running path.
- **Expected durable state**: Job remains `RUNNING` under the same
  generation throughout (no spurious reclaim), eventually completes
  successfully.
- **Invariants proved**: TF-INV-015, TF-INV-002 (generation stability under
  legitimate renewal).

### SF-018 — Process restart with outstanding jobs

- **Initial state**: A mix of jobs across `QUEUED`, `RETRY_WAIT`, and
  `RUNNING` (with valid, unexpired leases) states.
- **Actions**: All API server and worker processes are stopped and
  restarted.
- **Fault**: Full-fleet restart.
- **Expected durable state**: `QUEUED`/`RETRY_WAIT` jobs are claimed
  normally post-restart once eligible. `RUNNING` jobs whose leases were
  still valid at restart time remain `RUNNING` and are reclaimed normally
  once their (pre-restart) lease eventually expires (the restarted workers
  have no memory of them and correctly do not try to resume execution
  in-place — a new claim, new generation, and a fresh attempt is the only
  path back to progress). No job is lost or double-counted.
- **Invariants proved**: TF-INV-001, TF-INV-004.

## Scenario-to-Invariant Cross-Check

See [testing-strategy.md](testing-strategy.md) for the invariant-to-test
matrix, which lists which scenario(s) prove each `TF-INV-*`. Every
scenario above must appear in that matrix at least once; every invariant
must be covered by at least one scenario. This document and
[testing-strategy.md](testing-strategy.md) are kept in sync deliberately —
a new invariant added to [invariants.md](invariants.md) requires both a new
or extended scenario here and a new row in that matrix.
