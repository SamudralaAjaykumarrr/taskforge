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

## Phase 1 & Phase 2 Implementation Status

The following scenarios were executable as of Phase 1
([roadmap.md](roadmap.md)):

- **SF-001** (Normal success) — `TestRunOnce_SF001_NormalSuccess` in
  `internal/worker/worker_test.go`, exactly as specified: submit, claim,
  handler succeeds, `SUCCEEDED` with `attempt_count = 1`.
- **SF-014** (Database failure during transition) — a *simplified,
  single-worker* analogue only, per [roadmap.md](roadmap.md)'s explicit
  Phase 1 allowance ("fault-injection test for TF-INV-013 ... simplified
  to the single-worker case"):
  `TestFaultInjection_RollbackLeavesRowUnchanged` in
  `internal/store/store_test.go`. It proves the row is byte-for-byte
  unchanged after a forced mid-transaction failure; it does not exercise
  SF-014's exact "completion call, connection severed" shape. (Phase 2
  extends this coverage to the new multi-statement claim transaction —
  see below.)
- **SF-015** (Terminal state cannot reopen) — a partial version:
  `internal/jobstate`'s table tests exhaustively cover the full state
  machine (all three terminal states, every attempted outbound
  transition), and `TestTerminalStates_RejectFurtherTransitions` in
  `internal/store/store_test.go` proves it at the database level for the
  two terminal states Phase 1/2 actually reach (`SUCCEEDED`,
  `DEAD_LETTERED`) — `CANCELLED` is not reachable until Phase 6. Phase 2
  adds `TestClaim_TerminalJobNeverReclaimed`, proving the same property
  specifically against the reclaim query (a terminal row is never
  presented as a reclaim candidate, even with an artificially expired
  lease).
- **SF-018** (Process restart with outstanding jobs) — Phase 1 had a
  minimal version covering only a `RUNNING` job with a still-valid lease
  (`TestRestart_RunningJobSurvivesFreshStoreInstance` in
  `internal/store/store_test.go`, renamed from its Phase 1 description
  since its "no reclaim" assertion is now about lease validity, not a
  missing mechanism). Phase 2 adds the scenario's actual crash-recovery
  case — a `RUNNING` job whose lease *has* expired, reclaimed by a process
  that never held any in-memory reference to it:
  `TestReclaim_SurvivesFreshStoreInstance` in `internal/store/store_test.go`.
  Full-fleet (API + all workers) restart and a `QUEUED`/`RETRY_WAIT` mix
  remain unexercised — `RETRY_WAIT` does not exist yet (Phase 3).

The following scenarios are executable as of Phase 2, all in
`internal/store/lease_test.go` unless noted:

- **SF-002** (Worker crashes before execution) / **SF-003** (Worker
  crashes during execution) — proven at the worker-loop level (not just
  the store level) by `TestRunOnce_LeaseLostDuringExecution_SkipsCompletion`
  in `internal/worker/lease_test.go`: a worker's claim is force-expired
  and genuinely reclaimed by a second `Store.Claim` call while the first
  worker's handler is still (simulated-)running, and the first worker's
  `RunOnce` correctly detects the lost lease via heartbeat and skips
  completion.
- **SF-006** (Two workers race for same job) — `TestClaim_ConcurrentWorkersRaceForSameJob`:
  N real goroutines, real pooled PostgreSQL connections, racing for a
  shared pool of jobs; exactly one claim per job, no `lease_generation`
  reused.
- **SF-007** (Lease expires and job is reclaimed) — `TestClaim_ReclaimsExpiredLease`,
  using deterministic DB-time manipulation (`forceExpireLease`) rather
  than a sleep.
- **SF-008** (Stale worker attempts completion after losing its lease) —
  `TestFencing_StaleWorkerCompletionRejectedAfterReclaim` (the canonical
  sequence), `TestFencing_StaleCompletionRejectedBeforeNewOwnerCompletes`
  (the ordering where the stale call arrives before the new owner
  completes), and `TestFencing_ArbitrarilyLateArrivalAcrossManyGenerations`
  (the TF-INV-014 "3+ generations, out-of-order arrival" extension this
  scenario's description explicitly calls for).
- **SF-016** (Heartbeat delay) — `TestHeartbeat_ThenReclaim_ValidLeaseIsNotStolen`
  proves a heartbeat that arrives and extends the lease is never
  spuriously followed by a reclaim.
- **SF-017** (Long-running job renewal) — `TestRunOnce_LongRunningJob_HeartbeatKeepsLeaseAlive`
  in `internal/worker/lease_test.go`: a handler held open across several
  real heartbeat cycles (via `testdoubles.Gated`, not a fixed sleep on the
  assertion side) completes successfully under the same `lease_generation`
  throughout.

SF-004 and SF-005 became executable as of Phase 4 (idempotency keys) — see
below. SF-011 through SF-013 became executable as of Phase 6 (cancellation,
scheduling) — see the Phase 6 status paragraph further below.

The following scenarios are executable as of Phase 3
([roadmap.md](roadmap.md)):

- **SF-009** (Retryable failure eventually succeeds) — store-level:
  `TestCompleteRetryableFailure_SF009_EventuallySucceeds` in
  `internal/store/retry_test.go` (two `FAILED_RETRYABLE` attempts, then a
  third that succeeds, driven directly through `Store.CompleteRetryableFailure`/
  `Store.CompleteSuccess`). Worker-loop level (the full
  claim-execute-classify-retry cycle through a real `handler.Handler`, not
  just direct store calls): `TestRunOnce_SF009_RetryableFailureEventuallySucceeds`
  in `internal/worker/retry_test.go`, using `testdoubles.FlakyThenSucceed`
  and deterministic `eligible_at` fast-forwarding (never a real sleep for
  the backoff window) between cycles.
- **SF-010** (Retries exhausted, transitions to DLQ) — store-level:
  `TestCompleteRetryableFailure_SF010_ExhaustionTransitionsToDeadLettered`
  (asserts `DEAD_LETTERED` after the 3rd of 3 max attempts, `last_error`
  matching the final attempt, and all 3 `job_attempts` rows still
  queryable). Worker-loop level:
  `TestRunOnce_SF010_RetriesExhaustedDeadLetters`. Both also prove the
  scenario's negative half — a further claim attempt against the
  dead-lettered job never succeeds.

Phase 3 also adds test coverage beyond the two scenarios the roadmap names
explicitly (SF-009, SF-010), against the same adversarial-audit list this
document's format is meant to support — see
`internal/store/retry_test.go` and `internal/worker/retry_test.go` for the
full set: retry-eligibility timing (before/after `eligible_at`, concurrent
claim races once a `RETRY_WAIT` job becomes eligible, durability across a
simulated process restart), stale-generation fencing extended to the new
retry/dead-letter transition (mirroring SF-008's shape for
`CompleteRetryableFailure` and for a permanent `CompleteFailure` racing a
reclaim), a `TF-INV-013` fault-injection test for the new transition, and a
randomized property test asserting `attempt_count` never exceeds
`max_attempts` (`TestProperty_AttemptCountNeverExceedsMaxAttempts`).

The following scenarios are executable as of Phase 4 ("Idempotency",
[roadmap.md](roadmap.md)):

- **SF-005** (Duplicate submission, same idempotency key) — store level:
  `TestInsertIdempotent_ConcurrentDuplicateSubmissions_SF005` in
  `internal/store/idempotency_test.go` (60 real goroutines, real pooled
  PostgreSQL connections, identical `(job_type, idempotency_key)` —
  exactly one job row, every caller observes the same `job_id`, per this
  scenario's exact quality-gate wording, "50+ simultaneous duplicate-key
  submissions"). Full HTTP-boundary level:
  `TestCreateJob_IdempotencyKey_ConcurrentDuplicates_ExactlyOneJobCreated`
  in `internal/api/handlers_integration_test.go`. The sequential variant
  (submit, response lost, client retries) is
  `TestInsertIdempotent_SequentialDuplicate_ReturnsExistingJob` /
  `TestCreateJob_IdempotencyKey_SequentialDuplicateReturnsSameJob`.
- **SF-004** (Worker crashes after side effect, before acknowledgement) —
  now fully executable, both halves: the duplication-happens half is
  `TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice` in
  `internal/worker/idempotency_test.go` (a non-idempotent side-effect
  double is invoked twice across a forced reclaim, exactly this
  scenario's Initial-state/Actions/Fault sequence); the companion
  "handler using a `job_id`-keyed idempotency token avoids the duplicate
  logical effect" half is
  `TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect`
  in the same file (identical crash sequence, but the side effect goes
  through a `job_id`-keyed dedup table, ending with exactly one durable
  effect row despite two handler invocations).

Phase 4 also adds direct coverage of the scope note in SF-005's own
definition (`(job_type, idempotency_key)`) and of the execution-side
identity SF-004's companion half depends on:
`TestInsertIdempotent_DifferentJobTypeSameKey_CreatesSeparateJobs`,
`TestInsertIdempotent_ConflictingPayloadSameKey_FirstWriteWins` (the
documented [idempotency.md](idempotency.md) Open Questions decision),
`TestInsertIdempotent_DuplicateSubmissionAfterDeadLetter_ReturnsExistingJob`
(adversarial case #10), `TestInsertIdempotent_SurvivesFreshStoreInstance`
/ `TestCreateJob_IdempotencyKey_ProcessRestartThenDuplicateReturnsExistingJob`
(restart durability), `TestInsertIdempotent_RollbackLeavesNoPartialIdempotencyState`
(adversarial case #2 — moot by construction here since `idempotency_key`
is a column on the `jobs` row itself, never a separate mapping table, but
proved rather than only asserted), and
`TestIdempotencyIdentity_JobIDStableAcrossReclaim` /
`TestIdempotencyIdentity_JobIDStableAcrossRetry` (adversarial cases #7/#8
— `job_id` unchanged across a retry or a reclaim).

The following scenarios have extended/stress variants as of Phase 5
("Concurrency Hardening", [roadmap.md](roadmap.md)), which requires
"extended/stress variants of SF-006, SF-007, SF-008" specifically (not new
scenario IDs — Phase 5 adds no new named scenario, per its non-goal of
adding capability). All variants below are in
`internal/store/concurrency_stress_test.go` and
`internal/worker/concurrency_stress_test.go`:

- **SF-006 extended**: `TestStress_SF006_ManyWorkersManyJobs_NoDoubleClaimNoGenerationReuse`
  (25 workers/300 jobs), `TestStress_ManyWorkersRaceForOneJob` (20 workers/1
  job), `TestStress_ClaimContention_JobToWorkerRatios` (fewer/more/equal
  jobs-to-workers), and `TestStress_ConcurrentClaimPressureWithTerminalJobsPresent`
  (claim pressure with a large pool of already-terminal jobs mixed in).
- **SF-007 extended**: `TestStress_SF007_SustainedConcurrentReclaimOfManyExpiredLeases`
  (120 simultaneously expired leases, 20 concurrent reclaimers) and
  `TestStress_ConcurrentSweepOfManyExhaustedExpiredLeases_NoDoubleDeadLetter`
  (60 attempt-exhausted expired leases swept concurrently).
- **SF-008 extended**: `TestStress_SF008_FencingHoldsUnderSustainedConcurrentCompletionAttempts`
  (40 jobs × 4 generations, every generation's completion fired
  concurrently) and, at the worker-loop level,
  `TestStress_ManyConcurrentLeaseLossRaces_NoStaleAuthoritativeCompletions`
  (15 simultaneous lease-loss races between a first and second wave of
  real `*worker.Worker` instances).
- Additional Phase 5 tests not named as an extension of one specific
  scenario, but proving the same TF-INV-002/003/004/014 properties under
  load: `TestStress_ConcurrentWorkersRaceForManyEligibleRetryWaitJobs`,
  `TestStress_ClaimProgressesUnderConstrainedConnectionPool`,
  `TestStress_RandomizedCrashRetrySucceed_RepeatedSeeds_InvariantsHold`
  (repeated/overlapping randomized crash injection across 5 seeds, the
  specific case docs/roadmap.md's Phase 5 entry names: "repeated/
  overlapping crashes"), `TestStress_ManyWorkersProcessLargeMixedJobPool`,
  and `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`.

See [testing-strategy.md](testing-strategy.md)'s Phase 5 section for the
full list with descriptions, and [README.md](../README.md)'s "Phase 5:
What's Implemented" for how these map to invariants and what quality gate
they satisfy.

The following scenarios are executable as of Phase 6 ("Scheduling,
Cancellation, Timeouts", [roadmap.md](roadmap.md)):

- **SF-011** (Cancellation before claim) —
  `TestCancelQueuedOrRetryWait_Queued_TransitionsDirectlyToCancelled`
  (`internal/store/cancellation_test.go`) and the HTTP-boundary
  `TestCancelJob_Queued_TransitionsDirectlyToCancelled`
  (`internal/api/handlers_phase6_test.go`).
- **SF-012** (Cancellation races with completion) — both forced
  interleavings, deterministically: `TestSF012_CancelCommitsFirst_CompletionRejected`
  and `TestSF012_CompletionCommitsFirst_CancellationRejected`
  (`internal/store/cancellation_test.go`), extended to a retryable
  failure, a permanent failure, and an execution timeout racing
  cancellation: `TestSF012_CancelRacesRetryableFailure_FirstCommitWins`,
  `TestSF012_CancelRacesDeadLetter_FirstCommitWins`,
  `TestCompleteTimeout_RacesCancellation_FirstCommitWins`.
- **SF-013** (Scheduled job survives restart) —
  `TestInsertIdempotent_ScheduledJob_SurvivesFreshStoreInstance`
  (`internal/store/scheduling_test.go`): a fresh `*store.Store` sharing
  only the database still correctly refuses to claim before eligibility
  and claims correctly once eligible, with no in-memory scheduler state
  anywhere to lose or recover.

Phase 6 also adds coverage beyond the three scenarios the roadmap names
explicitly, against the same adversarial-audit list this document's format
supports — see `internal/store/scheduling_test.go`,
`internal/store/cancellation_test.go`, `internal/store/timeout_test.go`,
`internal/store/phase6_stress_test.go`, `internal/worker/cancellation_test.go`,
`internal/worker/timeout_test.go`, and `internal/api/handlers_phase6_test.go`
for the full set: scheduling-vs-retry-eligibility separation, many workers
racing a scheduled job's eligibility instant, cancellation of a
`RETRY_WAIT` job (never later executing merely because its backoff window
arrives), cancellation racing reclaim, cooperative and uncooperative
handler behavior under both cancellation and execution-timeout, execution
timeout firing independently of successful lease renewal, stale-generation
fencing extended to the two new completion paths (`CompleteCancelled`,
`CompleteTimeout`), fault-injection/rollback safety for both, and
idempotency-after-cancellation (the `CANCELLED` terminal state was
unreachable when Phase 4's own idempotency tests were written).

See [testing-strategy.md](testing-strategy.md)'s Phase 6 section for the
full list with descriptions, and [README.md](../README.md)'s "Phase 6:
What's Implemented" for how these map to invariants and what quality gate
they satisfy.

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
