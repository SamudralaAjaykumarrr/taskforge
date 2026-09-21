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

The following scenarios are new as of Phase 7 ("Workflow/DAG Execution",
[roadmap.md](roadmap.md)) — SF-019 through SF-030, defined in full below.
Per that phase's roadmap entry ("New DAG-specific scenarios ... to be
added to scenario-corpus.md at the start of this phase"), these did not
exist before Phase 7 began; they are now all executable, in
`internal/workflow/workflow_test.go` (pure DAG-validation scenarios, no
database) and `internal/store/workflow_test.go` /
`internal/api/workflow_handlers_test.go` (durable/HTTP-boundary
scenarios, real PostgreSQL):

- **SF-019** (Linear DAG executes in dependency order) —
  `TestCreateWorkflow_LinearChain_RootEligibleDependentsBlocked`
  (`internal/store/workflow_test.go`).
- **SF-020** (Fan-out: one predecessor, multiple independent dependents) —
  `TestFanOut_AllChildrenIndependentlyEligibleAfterParentSucceeds`.
- **SF-021** (Fan-in: one dependent, multiple required predecessors) —
  `TestFanIn_ChildBlockedUntilAllPredecessorsSucceed`.
- **SF-022** (Diamond dependency: A→B, A→C, B+C→D) — exercised throughout
  `internal/store/workflow_test.go` via `diamondSpec()`, most directly by
  `TestWorkflowState_SucceedsWhenEveryNodeSucceeds` (this task's named
  quality gate: "a diamond-dependency workflow ... executes nodes in
  correct order under concurrent workers, with a failed parent correctly
  cancelling dependents" — the failed-parent half is SF-024/SF-025 below,
  proved against the same diamond shape by
  `TestDeadLetteredPredecessor_CancelsDependentsTransitively`) and, at the
  HTTP boundary, `TestCreateWorkflow_DiamondDAG_AllNodesPersistedAtomically`
  (`internal/api/workflow_handlers_test.go`).
- **SF-023** (Retrying predecessor does not unblock a dependent) —
  `TestRetryingPredecessor_DoesNotUnblockDependent`.
- **SF-024** (Dead-lettered predecessor cancels dependents, transitively)
  — `TestDeadLetteredPredecessor_CancelsDependentsTransitively` (via
  `CompleteFailure`) and `TestDeadLetteredPredecessor_ExhaustionViaRetryPath_CancelsDependents`
  (via retry-budget exhaustion, i.e. `completeRetryableOutcome`'s
  dead-letter branch — the same cascade must fire regardless of which of
  the two dead-letter paths produced it).
- **SF-025** (Cancelled predecessor cancels dependents) —
  `TestCancelledPredecessor_CancelsDependents`.
- **SF-026** (Workflow-level cancellation across mixed node states) —
  `TestCancelWorkflow_QueuedAndBlockedNodesCancelledImmediately`,
  `TestCancelWorkflow_RunningNodeRequestedNotYetConfirmed`, and
  `TestCancelWorkflow_MixedNodeStates` (this task's exact required case
  list in one workflow: an unstarted/dependency-blocked node, a
  not-yet-eligible scheduled node, a `RETRY_WAIT` node, a `RUNNING` node,
  and an already-`SUCCEEDED` node), plus the HTTP-boundary
  `TestCancelWorkflow_QueuedNodesCancelledThroughAPI`
  (`internal/api/workflow_handlers_test.go`).
- **SF-027** (Concurrent dependency completion: two predecessors of the
  same fan-in node complete at the same instant) —
  `TestConcurrentFanIn_BothPredecessorsCompleteSimultaneously` (two real
  goroutines, real pooled PostgreSQL connections, released from a
  `sync.WaitGroup` start barrier so both `CompleteSuccess` calls race
  directly — never a sleep-based approximation of concurrency).
- **SF-028** (Stale generation cannot unblock or cancel dependents) —
  `TestStaleGeneration_CannotUnblockDependents` (the canonical "Worker A
  loses its lease, Worker B reclaims and succeeds, Worker A's stale
  success report arrives late" sequence, replayed against a workflow
  dependency edge) and its mirror,
  `TestStaleGeneration_LateFailureCannotCancelDependents` (a stale,
  late failure report must not cascade-cancel dependents either).
- **SF-029** (Workflow progress survives a full restart) —
  `TestRestart_WorkflowProgressSurvivesFreshStoreInstance` (a fresh
  `*store.Store` sharing only the database — standing in for a full
  process/fleet restart, per SF-018's shape — correctly sees a
  predecessor's earlier completion and claims its now-eligible
  dependents, with no in-memory workflow-coordinator state anywhere to
  lose).
- **SF-030** (Invalid graph is rejected atomically, before any durable
  state exists) — the DAG-validation scenarios in
  `internal/workflow/workflow_test.go` (empty graph, missing/duplicate
  node key, missing job type, self-dependency, unknown dependency,
  duplicate dependency, direct and transitive cycles, deterministic cycle
  reporting) plus `TestCreateWorkflow_AtomicCreation_InvalidGraphNeverPersisted`
  and `TestCreateWorkflow_RollbackLeavesNoPartialState`
  (`internal/store/workflow_test.go`, the latter a direct fault-injection
  test forcing a mid-transaction constraint violation and asserting zero
  rows survive in any of `workflow_instances`/`workflow_nodes`/`jobs`),
  and the HTTP-boundary rejection tests in
  `internal/api/workflow_handlers_test.go`.

Phase 7 also adds coverage beyond these twelve named scenarios, against
this task's own adversarial-audit list:
`TestSchedulingInteraction_ActivationRespectsFutureSchedule` /
`TestSchedulingInteraction_PastScheduleActivatesImmediately` (dependency
satisfaction never bypasses a node's own `scheduled_at`, and vice versa),
`TestMultiWorker_FanOutClaimedSafelyUnderContention` (20 real workers
racing 8 simultaneously-eligible fan-out children — no child claimed
twice), `TestWorkflowState_TerminalCannotReopen` (cancelling an
already-`SUCCEEDED` workflow is a no-op; its `terminal_at` never changes),
and `TestGetWorkflow_DependsOnResolvedToNodeKeys` /
`TestGetWorkflow_NotFound` / `TestCancelWorkflow_NotFound`.

See [testing-strategy.md](testing-strategy.md)'s Phase 7 section for the
full list with descriptions, and [README.md](../README.md)'s "Phase 7:
What's Implemented" for how these map to invariants and what quality gate
they satisfy.

Phase 8 ("Observability", [roadmap.md](roadmap.md)) introduces no new
scenario IDs, per that phase's own explicit scope ("Required tests:
Metric-assertion tests attached to existing scenarios rather than a new
scenario set"). SF-001, SF-005, SF-007, SF-008, SF-009, SF-010, SF-011,
and SF-012 each gained a companion metric-assertion test proving the
[observability.md](observability.md) metrics/logs their own documented
event sequence must produce — see
[testing-strategy.md](testing-strategy.md)'s Phase 8 section for the full
list.

Phase 9 ("Chaos, Load, and Failure Testing", [roadmap.md](roadmap.md))
likewise introduces no new named scenario IDs: its "Required tests" entry
calls for "a chaos-test harness driving randomized combinations of every
fault in failure-model.md's 'handled in v1' list," i.e. combined,
seeded execution of the scenarios already defined above (and the
invariants they prove), not a new corpus. Every scenario SF-001 through
SF-030 is exercised, in combination rather than isolation, by
`internal/chaos`'s seeded campaigns and `cmd/chaos`'s manual stress/soak
harness — see [testing-strategy.md](testing-strategy.md)'s Phase 9
section for the full test list and README.md's "Phase 9: What's
Implemented" for results actually obtained. No defect found during this
phase's implementation required a new regression scenario here — every
one found was in the chaos test harness's own assumptions, not in
product behavior; see README.md's "Phase 9: Defects found" for the full,
honest account.

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

### SF-019 — Linear DAG executes in dependency order

- **Initial state**: A workflow `A -> B -> C` is submitted; no node has
  been claimed.
- **Actions**: A worker claims and succeeds A, then B, then C, in order.
- **Fault**: None.
- **Expected durable state**: A is claimable immediately; B and C are
  durably `QUEUED` but not claimable (`eligible_at` far in the future)
  until their sole predecessor succeeds, at which point each becomes
  claimable exactly once. Final workflow state `SUCCEEDED`.
- **Invariants proved**: TF-INV-012.

### SF-020 — Fan-out: one predecessor, multiple independent dependents

- **Initial state**: A workflow `A -> B`, `A -> C` is submitted.
- **Actions**: A worker succeeds A.
- **Fault**: None.
- **Expected durable state**: B and C both become independently claimable
  the instant A succeeds — no ordering dependency between them, and
  neither's job row is duplicated (exactly one `jobs` row per node, per
  `workflow_nodes_job_unique`).
- **Invariants proved**: TF-INV-012.

### SF-021 — Fan-in: one dependent, multiple required predecessors

- **Initial state**: A workflow `A -> C`, `B -> C` is submitted; A and B
  are independent roots.
- **Actions**: A worker succeeds A only.
- **Fault**: None.
- **Expected durable state**: C remains durably not claimable (AND
  fan-in semantics: `SUCCEEDED` from only one of two required
  predecessors does not satisfy the dependency condition). Once B also
  succeeds, C becomes claimable.
- **Invariants proved**: TF-INV-012.

### SF-022 — Diamond dependency (A→B, A→C, B+C→D)

- **Initial state**: A workflow `A -> B`, `A -> C`, `B -> D`, `C -> D` is
  submitted (this task's named quality-gate shape).
- **Actions**: A worker drives all four nodes to `SUCCEEDED` in dependency
  order (A; then B and C, in either order or concurrently; then D).
- **Fault**: None.
- **Expected durable state**: D never becomes claimable until both B and
  C have succeeded; the workflow reaches `SUCCEEDED` once all four nodes
  have.
- **Invariants proved**: TF-INV-012.

### SF-023 — Retrying predecessor does not unblock a dependent

- **Initial state**: A workflow `A -> B` is submitted, `A.max_attempts >
  1`.
- **Actions**: A's first attempt reports a retryable failure (→
  `RETRY_WAIT`). A's second attempt succeeds.
- **Fault**: Simulated transient handler failure on A's first attempt.
- **Expected durable state**: B remains durably not claimable while A is
  `RETRY_WAIT` — a retrying predecessor is explicitly not treated as
  failed (docs/workflows.md's Failure Propagation table) and does not
  satisfy or violate the dependency condition. B becomes claimable only
  once A's retry actually reaches `SUCCEEDED`.
- **Invariants proved**: TF-INV-012, TF-INV-006 (A's own retry budget is
  unaffected by being part of a workflow).

### SF-024 — Dead-lettered predecessor cancels dependents, transitively

- **Initial state**: A workflow `A -> B -> C` is submitted, `A.max_attempts
  = 1`.
- **Actions**: A's only attempt reports a permanent failure (or exhausts
  its retry budget — both paths must produce this outcome identically).
- **Fault**: Simulated permanent handler failure, or retry-budget
  exhaustion.
- **Expected durable state**: A reaches `DEAD_LETTERED`. B, whose only
  predecessor just dead-lettered, is cancelled (`CANCELLED`) as a direct
  consequence, without ever being claimed. C, whose only predecessor (B)
  was just cancelled, is transitively cancelled in the same cascade,
  also without ever being claimed. The workflow reaches `FAILED`.
- **Invariants proved**: TF-INV-012, TF-INV-006, TF-INV-009 (A's own
  dead-letter history is preserved and unaffected by cascading).

### SF-025 — Cancelled predecessor cancels dependents

- **Initial state**: A workflow `A -> B` is submitted; A has not been
  claimed.
- **Actions**: A is cancelled directly (`POST /jobs/{id}/cancel` against
  A's own underlying job, or an equivalent direct store call) — not via
  workflow-level cancellation.
- **Fault**: None.
- **Expected durable state**: B is cancelled as a direct consequence,
  identically to the dead-letter case (docs/workflows.md's Failure
  Propagation table treats `CANCELLED` and `DEAD_LETTERED` predecessor
  outcomes the same way), regardless of which API path produced A's
  cancellation.
- **Invariants proved**: TF-INV-012, TF-INV-010 (A's own cancellation race
  semantics are unaffected by being part of a workflow).

### SF-026 — Workflow-level cancellation across mixed node states

- **Initial state**: A workflow with five independent nodes, one each in:
  dependency-blocked (unstarted), scheduled for the future (not yet
  eligible), `RETRY_WAIT`, `RUNNING`, and already `SUCCEEDED`.
- **Actions**: `POST /workflows/{id}/cancel`.
- **Fault**: None.
- **Expected durable state**: The blocked, scheduled, and `RETRY_WAIT`
  nodes transition directly to `CANCELLED` (no worker involved for any of
  them). The `RUNNING` node's cancellation is requested
  (`cancel_requested = true`) but not yet confirmed — the workflow itself
  remains `RUNNING` until that node's worker acknowledges. The already
  `SUCCEEDED` node is completely untouched.
- **Invariants proved**: TF-INV-010 (applied independently per node),
  TF-INV-005 (the succeeded node's terminal state is never disturbed).

### SF-027 — Concurrent dependency completion

- **Initial state**: A workflow `A -> C`, `B -> C` is submitted; A has
  already succeeded, and B and C's other predecessor conditions are ready
  to be evaluated.
- **Actions**: Two predecessors of the same fan-in node (B and C, both
  required by C — reusing the diamond shape's naming, D) commit their
  `SUCCEEDED` transitions at, as close as test synchronization allows,
  the same instant, released from a shared start barrier across two real,
  concurrently executing goroutines with real pooled PostgreSQL
  connections.
- **Fault**: None (concurrency itself is the stressor).
- **Expected durable state**: The shared dependent becomes eligible
  exactly once — not zero times (a lost-update bug where both
  transactions conclude "not all predecessors have succeeded yet" and
  neither activates it) and not claimed twice.
- **Invariants proved**: TF-INV-012, TF-INV-002 (exactly one claim, no
  matter how the activation race resolved).

### SF-028 — Stale generation cannot unblock or cancel dependents

- **Initial state**: A workflow `X -> Y` is submitted. Worker A claims X
  (generation 1).
- **Actions**: Worker A's lease expires. Worker B reclaims X (generation
  2) and reports success. Worker A, unaware, later reports (a) success or
  (b) permanent failure for X under generation 1.
- **Fault**: Worker A's delayed, stale completion call, in both outcome
  variants.
- **Expected durable state**: Worker A's stale call is rejected outright
  (zero rows affected, `ErrStaleTransition`) in both variants — it never
  reaches the point of attempting to propagate anything to Y. Y's
  eligibility reflects only Worker B's genuine, committed completion: it
  becomes eligible if and only if B's generation-2 completion was
  `SUCCEEDED`, and is never spuriously cancelled by A's rejected stale
  failure report.
- **Invariants proved**: TF-INV-003, TF-INV-014, TF-INV-012.

### SF-029 — Workflow progress survives restart

- **Initial state**: A workflow with a still-blocked dependent node exists;
  its predecessor has just succeeded (dependent now eligible) durably.
- **Actions**: All API server and worker processes are stopped and
  restarted (or, at the store level, a fresh `*store.Store` sharing only
  the database replaces the pre-restart instance).
- **Fault**: Full-fleet restart.
- **Expected durable state**: The restarted fleet correctly claims and
  executes the now-eligible dependent — no "catch-up" logic is needed or
  invoked, because dependency-satisfaction state (`eligible_at` on the
  dependent's own job row) was never held only in memory.
- **Invariants proved**: TF-INV-012, TF-INV-001, TF-INV-004.

### SF-030 — Invalid graph is rejected atomically

- **Initial state**: No workflow exists.
- **Actions**: `POST /workflows` with a structurally invalid graph (a
  cycle, a self-dependency, an unknown dependency, a duplicate node key,
  or an empty node list).
- **Fault**: None — the invalid input itself is the stressor.
- **Expected durable state**: The submission is rejected with `400 Bad
  Request` before any SQL is issued. No `workflow_instances` row, no
  `jobs` row, and no `workflow_nodes` row is created for the rejected
  submission — an invalid workflow is never partially persisted and never
  acknowledged as created.
- **Invariants proved**: TF-INV-012 (a malformed dependency graph can
  never reach a state where TF-INV-012 would even need to be evaluated),
  TF-INV-013 (analogously — no half-created workflow, proved directly by
  a fault-injection test forcing a mid-transaction failure).

### SF-031 — Transactional enqueue commits with caller's business write

- **Initial state**: No business row, no job.
- **Actions**: Within one caller-owned `pgx.Tx`: insert a business row,
  call `txenqueue.EnqueueTx`, commit.
- **Fault**: None.
- **Expected durable state**: Both the business row and the job exist
  after commit; the job is `QUEUED` and claimable via the ordinary
  `internal/store.Claim` query — the same claim path any other job uses.
- **Invariants proved**: TF-INV-001 (accepted jobs cannot disappear,
  extended to the transactional entry point), TF-INV-013 (no
  half-transitioned/half-created state).
- **Test**: `txenqueue/txenqueue_test.go`'s
  `TestEnqueueTx_Commit_BusinessDataAndJobBothDurable_ClaimableThroughNormalEngine`.

### SF-032 — Transactional enqueue rolls back with caller's business write

- **Initial state**: No business row, no job.
- **Actions**: Within one caller-owned `pgx.Tx`: insert a business row,
  call `txenqueue.EnqueueTx`, roll back. A companion ordering (enqueue
  first, business write second, roll back) is also exercised.
- **Fault**: The caller's own decision to roll back (standing in for any
  later business-logic failure).
- **Expected durable state**: Neither the business row nor the job exists
  after rollback, regardless of statement order.
- **Invariants proved**: TF-INV-013 (rollback never leaves a
  half-transitioned/half-created job), extended to the transactional entry
  point — no orphan job from a successful-but-later-rolled-back enqueue.
- **Test**: `TestEnqueueTx_Rollback_NoBusinessDataNoJob`,
  `TestEnqueueTx_SuccessfulEnqueueThenLaterCallerRollback_NoOrphanJob`.

### SF-033 — Transactional enqueue itself fails inside the caller's transaction

- **Initial state**: No job.
- **Actions**: Call `txenqueue.EnqueueTx` against a `pgx.Tx` pgx has
  already closed (standing in for any failure that ends the transaction
  before the enqueue statement can run).
- **Fault**: The already-closed transaction.
- **Expected durable state**: `EnqueueTx` returns an error (never a false
  success); no job row exists for the attempted `job_type`; transaction
  ownership remains entirely with the caller.
- **Invariants proved**: TF-INV-001 (no success is ever reported for a
  write that did not durably happen), TF-INV-013.
- **Test**: `TestEnqueueTx_FailsOnClosedTransaction_NoFalseSuccessNoOrphanJob`.

### SF-034 — Transactional-idempotency inside and across transactions

- **Initial state**: No job for the idempotency key under test.
- **Actions**: (a) two `EnqueueTx` calls with the same
  `(job_type, idempotency_key)` inside one transaction; (b) the same key
  reused by a second, separate transaction after the first rolled back;
  (c) N separate transactions concurrently calling `EnqueueTx` with the
  same key, each racing to commit.
- **Fault**: Concurrent transactions racing on the same database unique
  constraint (case c).
- **Expected durable state**: (a) exactly one job row, the second call's
  `created=false`; (b) a genuine new row after reuse post-rollback,
  `created=true`; (c) exactly one job row across all N transactions,
  exactly one `created=true`, every transaction observing the same
  `job_id`.
- **Invariants proved**: TF-INV-008 (an idempotency key never creates two
  logical jobs, extended to the transactional entry point and to
  concurrent transactions rather than only concurrent pool callers),
  TF-INV-016 (enforced by the database constraint, not application logic,
  proved here via the SAVEPOINT-based conflict recovery in
  `internal/store.InsertTx`).
- **Test**: `TestEnqueueTx_IdempotencyKey_DuplicateWithinSameTransaction`,
  `TestEnqueueTx_IdempotencyKey_RollbackThenReuseSameKey_Succeeds`,
  `TestEnqueueTx_IdempotencyKey_ConcurrentTransactions_ExactlyOneCreates`
  (renamed from `...ExactlyOneCommits` — every successful transaction
  commits; exactly one's own `INSERT` creates the row).

### SF-035 — Many concurrent transactions independently commit or roll back

- **Initial state**: No jobs for the `job_type` under test.
- **Actions**: 20 concurrent goroutines, each opening its own transaction,
  calling `EnqueueTx`, and independently choosing to commit (half) or roll
  back (half).
- **Fault**: True concurrency across separate connections/transactions.
- **Expected durable state**: Exactly the committed half's jobs exist and
  are independently queryable by ID; every rolled-back half's job does
  not exist. No cross-transaction leakage or corruption.
- **Invariants proved**: TF-INV-001, TF-INV-013, under concurrency rather
  than only sequentially.
- **Test**: `TestEnqueueTx_ConcurrentCommitRollbackRace_OnlyCommittedJobsExist`.

### SF-036 — Idempotency-key conflict outside a REPEATABLE READ/SERIALIZABLE snapshot (Phase 11 audit-fix addition)

- **Initial state**: No job for the idempotency key under test.
- **Actions**: Transaction B opens at REPEATABLE READ (respectively
  SERIALIZABLE) and fixes its snapshot with an initial statement.
  Transaction A, separately, then inserts and commits a job under the same
  `(job_type, idempotency_key)`. B then calls `EnqueueTx` with that same
  key.
- **Fault**: B's snapshot was fixed strictly before A's commit — B's
  `INSERT` still loses the uniqueness race (PostgreSQL's unique-index
  enforcement checks the latest committed data, not a transaction's
  snapshot), but B's fallback re-read, constrained to B's own snapshot,
  cannot see A's now-committed row.
- **Expected durable state**: Exactly one row (A's). `EnqueueTx` returns
  `txenqueue.ErrMustRetryTransaction` — never `store.ErrNotFound`'s "job
  doesn't exist" meaning, never a false success — with no raw
  internal/store/PostgreSQL detail in the error text. B's transaction
  ownership (rollback/retry) remains entirely with the caller.
- **Invariants proved**: TF-INV-008/TF-INV-016 (an idempotency key never
  creates two logical jobs, including when a conflict cannot be resolved
  within the losing transaction's own snapshot), TF-INV-013 (no
  half-transitioned/half-created state), and Phase 11's public-API
  error-leakage requirement.
- **Test**: `TestEnqueueTx_RepeatableRead_IdempotencyConflictOutsideSnapshot_MapsToMustRetry`,
  `TestEnqueueTx_Serializable_IdempotencyConflictOutsideSnapshot_MapsToMustRetry`
  (`txenqueue/errors_test.go`).

## Phase 13 — Workload Governance and Retention (SF-051 through SF-059)

Folded back from [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md)
("Required implementation proof obligations"), where these were first
defined and implemented, per [phase-14-plan.md](phase-14-plan.md) §16's
own recommendation that Phase 14 — the phase auditing cross-document
consistency anyway — close this reconciliation gap rather than leave
`scenario-corpus.md` ending at SF-036 while SF-051–059 existed only in the
ADR and in-code comments.

### SF-051 — Slot-table concurrency-limit exactness under concurrent claim attempts, mixed fresh/reclaim load

- **Initial state**: A capacity-limited queue with `N` free slots.
- **Actions**: Many concurrent claim attempts, including a mix of fresh
  claims and lease-expiry reclaims, racing for the same limited capacity.
- **Fault**: True concurrency across separate connections.
- **Expected durable state**: Never more than `N` jobs concurrently
  `RUNNING` for that queue, under fresh-only load and under mixed
  fresh/reclaim load alike.
- **Invariants proved**: TF-INV-019 (exact concurrency-limit enforcement).
- **Test**: `TestSlotTable_SF051_ExactConcurrencyLimitUnderConcurrentClaims`,
  `TestSlotTable_SF051_MixedFreshAndReclaimLoad_NeverExceedsLimit`
  (`internal/store/phase13_slot_concurrency_test.go`).

### SF-052 — Reclaim never writes `queue_slots`

- **Initial state**: A `RUNNING` job whose lease has expired, already
  holding a `queue_slots` row from its original claim.
- **Actions**: A worker reclaims the expired-lease job.
- **Fault**: None — a direct assertion on the reclaim code path, not
  merely an absence of violations under load.
- **Expected durable state**: The reclaim does not acquire a second slot
  or otherwise write `queue_slots` — the existing held slot carries over
  to the new lease generation unchanged.
- **Invariants proved**: TF-INV-019 (slot/lease-generation independence).
- **Test**: `TestSlotTable_SF052_ReclaimNeverWritesQueueSlots`,
  `TestSlotTable_ReclaimDoesNotAcquireASecondSlot`
  (`internal/store/phase13_slot_concurrency_test.go`).

### SF-053 — Per-row slot/job consistency (strengthened)

- **Initial state**: A capacity-limited queue under sustained, mixed
  claim/complete/reclaim load.
- **Actions**: Concurrent claims, completions, and reclaims over a
  sustained run.
- **Fault**: True concurrency; the aggregate check
  `count(RUNNING) == count(held slots)` alone cannot distinguish a
  correct state from two offsetting errors (one job wrongly holding two
  slots, another wrongly holding none).
- **Expected durable state**: Checked **per row**: every `RUNNING` job's
  id appears in exactly one held `queue_slots` row, and every held
  `queue_slots` row's `held_by_job_id` names exactly one currently-`RUNNING`
  job for that queue; a terminal completion releases exactly its own slot.
- **Invariants proved**: TF-INV-019, strengthened past the original
  aggregate-only phrasing.
- **Test**: `TestSlotTable_SF053_TerminalCompletionReleasesExactlyItsSlot`,
  `TestSlotTable_SF053_PerRowConsistencyUnderSustainedMixedLoad`
  (`internal/store/phase13_slot_concurrency_test.go`).

### SF-054 — Sparse queue not starved by a hot queue

- **Initial state**: A flooded ("hot") queue and a sparse,
  continuously-pending, capacity-eligible queue, both competing for
  service.
- **Actions**: Sustained claim traffic across both queues.
- **Fault**: The hot queue's volume could starve the sparse queue absent
  a fairness bound.
- **Expected durable state**: The sparse queue is never starved beyond
  `E − 1` other queues' service opportunities (TF-INV-019's exact bound),
  traced directly from served-queue order, not inferred from wall-clock
  wait alone.
- **Invariants proved**: TF-INV-019.
- **Test**: `TestFairness_SF054_SparseQueueNotStarvedByHotQueue`
  (`internal/store/phase13_fairness_test.go`).

### SF-055 — Temporary capacity ineligibility preserves fairness position

- **Initial state**: A queue at its capacity limit (temporarily
  ineligible), with a `last_claimed_at` fairness position already
  established.
- **Actions**: The queue regains capacity eligibility after a window
  during which a competing queue was served.
- **Fault**: A naive fairness implementation might disturb the
  temporarily-ineligible queue's position, or double-count the competing
  queue's service against it.
- **Expected durable state**: The queue's `last_claimed_at` position is
  not disturbed by the ineligibility window, and the competing queue's
  service during that window counts at most once against any other
  queue's bound.
- **Invariants proved**: TF-INV-019's exact accounting rule.
- **Test**: `TestFairness_SF055_TemporaryCapacityIneligibility_PositionPreserved`
  (`internal/store/phase13_fairness_test.go`).

### SF-056 — Newly-eligible queue served first despite existing backlog

- **Initial state**: An existing backlog of queues with established
  `last_claimed_at` positions; a newly-configured or never-before-served
  queue becomes eligible.
- **Actions**: The new queue's first eligible job is claimed.
- **Fault**: A `last_claimed_at`-ordered fairness scheme could otherwise
  make a brand-new queue wait behind every existing queue's backlog.
- **Expected durable state**: The new queue is served on its first
  opportunity, not gated behind existing backlog (the `-infinity` default
  for a never-before-served queue).
- **Invariants proved**: TF-INV-019.
- **Test**: `TestFairness_SF056_NewlyEligibleQueue_ServedFirstDespiteExistingBacklog`
  (`internal/store/phase13_fairness_test.go`).

### SF-057 — Reclaim participates in fairness accounting identically to a fresh claim

- **Initial state**: A queue with an expired-lease job eligible for
  reclaim.
- **Actions**: A worker reclaims the expired-lease job.
- **Fault**: A fairness implementation that only updates
  `last_claimed_at` on the fresh-claim code path (not reclaim) would let
  a queue's reclaim traffic escape the fairness bound entirely.
- **Expected durable state**: The reclaim advances `last_claimed_at`
  identically to a fresh claim — one uniform "service opportunity"
  definition, both branches of the claim query.
- **Invariants proved**: TF-INV-019.
- **Test**: `TestFairness_SF057_ReclaimAdvancesLastClaimedAtIdenticallyToFreshClaim`
  (`internal/store/phase13_fairness_test.go`).

### SF-058 — Sweep releases capacity durably and idempotently on attempt-budget exhaustion

- **Initial state**: A job whose lease has expired and whose
  `attempt_count` has reached `max_attempts` while still `RUNNING`,
  holding a `queue_slots` row.
- **Actions**: The Lazy Dead-Letter Sweep transitions it to
  `DEAD_LETTERED`; a second, later sweep pass runs against the
  already-`DEAD_LETTERED` job.
- **Fault**: Neither SF-052 (reclaim never writes slots) nor SF-053
  (per-row consistency) alone exercises this path — an independent review
  found a real benchmark database holding 22 jobs permanently stuck in
  exactly this unaddressed state before this scenario was added.
- **Expected durable state**: The slot is released in the same
  transaction as the terminal transition (durable — a crash immediately
  after commit must not leave the slot ambiguous); the second sweep pass
  is a no-op that does not re-release or otherwise disturb a slot that
  may since have been legitimately reclaimed by a different job
  (idempotent); a subsequent claim attempt for that queue can
  successfully use the freed slot.
- **Invariants proved**: TF-INV-005 (no attempt after terminal), TF-INV-019
  (slot release).
- **Test**: `TestSweep_SF058_ReleasesSlotDurablyOnAttemptBudgetExhaustion`,
  `TestSweep_SF058_SecondSweepPassIsIdempotent_DoesNotDisturbANewHolder`
  (`internal/store/phase13_sweep_test.go`).

### SF-059 — Fault injection: terminal transition and slot release are atomic

- **Initial state**: A `RUNNING` job holding a `queue_slots` row, about to
  reach a terminal state (success, failure, cancellation, or dead-letter).
- **Actions**: A crash/rollback is forced between the job's terminal
  state transition and its slot-release write.
- **Fault**: Injected transaction abort at that exact boundary.
- **Expected durable state**: PostgreSQL's own transaction atomicity means
  the row is left exactly as it was before the transition began — never
  terminal with its slot still held, and never slot-released with the job
  row still non-terminal.
- **Invariants proved**: TF-INV-013 (no half-transitioned state), extended
  to the slot-table mechanism — mirrors SF-014's own precedent ("Rollback
  never leaves a half-transitioned job").
- **Test**: `TestSweep_SF059_FaultInjection_TerminalTransitionAndSlotReleaseAreAtomic`
  (`internal/store/phase13_sweep_test.go`).

## Phase 14 — Upgrade & Compatibility Proof (SF-060 through SF-070)

Continuing the numbering from SF-059 (Phase 13's concurrency/fairness
scenarios, folded back into this file immediately above, per
[phase-14-plan.md](phase-14-plan.md) §16's own recommendation that Phase
14 — the phase auditing cross-document consistency anyway — close that
reconciliation gap rather than leave it for a future pass).

### SF-060 — Data-safe-reversible migration round trip

- **Initial state**: A database freshly migrated to exactly one migration
  version earlier than the migration under test.
- **Actions**: `internal/migrate.UpTo` to the version under test, then
  `internal/migrate.Down` for that version, then `UpTo` again for the
  same version.
- **Fault**: None — this proves the down path works, not merely that it
  fails safely.
- **Expected durable state**: The resulting schema (columns, indexes,
  constraints) is byte-identical to what a straight-through `Up` to that
  version produces, for every migration labeled `data-safe-reversible`.
  `forward-fix-only` migrations (`0001`, `0002`, `0003`, `0005`) are
  deliberately never exercised this way.
- **Invariants proved**: [ADR-0010](adr/0010-expand-migrate-contract.md)'s
  data-safe-reversible promise; no `TF-INV-*` directly (a schema-tooling
  property, not a state-machine one).
- **Test**: `TestMigrations_SF060_DataSafeReversibleSubsetRoundTripsCleanly`
  (`internal/migrate/reversibility_test.go`).

### SF-061 — Every migration file is explicitly labeled

- **Initial state**: The full, current `migrations/` directory.
- **Actions**: `internal/migrate.Migrations()`.
- **Fault**: A migration file with no `taskforge:down-migration-status`
  marker, or an unrecognized value, would be the fault condition under
  test; today's real files all pass.
- **Expected durable state**: N/A (a static/schema-file check, not a
  runtime state proof) — `Migrations()` returns an error for any
  unlabeled or mislabeled file rather than silently defaulting.
- **Invariants proved**: The roadmap's own exit criterion, "every
  forward-fix-only migration is explicitly labeled."
- **Test**: `TestMigrations_SF061_EveryFileCarriesAValidDownMigrationStatusMarker`
  (`internal/migrate/reversibility_test.go`).

### SF-062 — Workflow node with no registered handler

- **Initial state**: A single-node workflow submitted with a `job_type`
  no handler is registered for.
- **Actions**: A worker claims and attempts the node's underlying job.
- **Fault**: `handler.Registry.Lookup` finds nothing.
- **Expected durable state**: The node's job dead-letters via the same
  `ErrNoHandler`/`CompleteFailure` path a plain job uses; the workflow
  itself reaches `FAILED` (TF-INV-012's cascade).
- **Invariants proved**: TF-INV-012 (cascade correctness) extended to the
  unregistered-handler failure mode; documents the previously-untested
  half of docs/compatibility-policy.md's "PROPOSED: Workflows Surviving
  Deployments."
- **Test**: `TestRunOnce_SF062_WorkflowNodeWithNoRegisteredHandler`
  (`internal/worker/worker_test.go`).

### SF-063 — `cmd/api` ordinary graceful shutdown (real OS process)

- **Initial state**: A real, separately-compiled `cmd/api` binary running
  and accepting connections.
- **Actions**: A real SIGTERM is sent while an ordinary request is
  in flight; a new connection is attempted shortly afterward.
- **Fault**: None — proves the ordinary, within-budget path.
- **Expected durable state/behavior**: The in-flight request completes
  successfully (it finishes well within `TASKFORGE_API_SHUTDOWN_TIMEOUT`);
  a new connection attempted after SIGTERM is refused; the process exits
  cleanly (code 0).
- **Invariants proved**: The graceful-drain contract's ordinary case,
  docs/compatibility-policy.md's "PROPOSED: Rolling Server Upgrades."
- **Test**: `TestProc_SF063_APIGracefulShutdown_OrdinaryRequestCompletes_NewConnectionsRefused`
  (`test/procs/api_shutdown_test.go`).

### SF-064 — `cmd/worker` opt-in drain: in-flight job finishes before the deadline (real OS process)

- **Initial state**: A real, separately-compiled `cmd/worker` binary
  (configured with `TASKFORGE_WORKER_DRAIN_TIMEOUT`) executing a job
  whose handler will finish well within that window.
- **Actions**: A real SIGTERM is sent mid-execution.
- **Fault**: None — the ordinary, within-drain-budget case.
- **Expected durable state**: The job reaches `SUCCEEDED` under its
  original `lease_generation`; the process attempts no new claim after
  SIGTERM and exits cleanly.
- **Invariants proved**: §6.4's opt-in drain redesign's core positive
  claim.
- **Test**: `TestProc_SF064_WorkerDrain_InFlightJobFinishesBeforeDeadline`
  (`test/procs/worker_drain_test.go`).

### SF-064a — Default (un-opted-in) `Worker` cancels immediately (in-process)

- **Initial state**: A `*Worker` constructed via `worker.New`,
  `SetDrainTimeout` never called, executing a job.
- **Actions**: The context passed to `Run`/`RunOnce` is cancelled.
- **Fault**: None — proves the zero-value default is unchanged.
- **Expected durable state/behavior**: The handler's context is observed
  cancelled within a sub-second window (no grace period at all); the job
  remains `RUNNING`, untouched.
- **Invariants proved**: The default `Worker.Run` contract is
  byte-for-byte identical to pre-Phase-14 behavior for every caller that
  does not explicitly opt in.
- **Test**: `TestRunOnce_SF064a_DefaultWorkerCancelsExecutionImmediately`
  (`internal/worker/drain_test.go`).

### SF-064b — Phase 5 graceful-shutdown stress test, re-run unmodified

- **Initial state/Actions/Fault**: Identical to Phase 5's own
  `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
  — not a new scenario, a mandatory regression re-run against the changed
  worker code.
- **Expected durable state**: Unchanged from Phase 5: no goroutine leak,
  no orphaned authority, same 5-second hard timeout, same assertions, the
  test file itself not edited.
- **Invariants proved**: The specific regression an earlier design
  iteration of the drain redesign would have broken; confirms the
  opt-in-only correction actually holds.
- **Test**: `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
  (`internal/worker/concurrency_stress_test.go`, unmodified).

### SF-065 — Worker SIGKILL mid-execution (real OS process)

- **Initial state**: A real `cmd/worker` binary executing a job.
- **Actions**: A real SIGKILL (not SIGTERM) is sent mid-execution.
- **Fault**: The process is terminated instantly, bypassing Go signal
  handling and the drain contract entirely.
- **Expected durable state**: The job remains `RUNNING` under its
  original lease; once `lease_expires_at` passes, an ordinary fresh
  worker reclaims and completes it via the existing, unmodified
  TF-INV-004 path.
- **Invariants proved**: TF-INV-004 (lease-expiry reclaim), and that the
  drain redesign makes no claim about, and did not accidentally change,
  SIGKILL behavior.
- **Test**: `TestProc_SF065_WorkerSIGKILL_LeaseExpiryReclaimUnaffected`
  (`test/procs/worker_drain_test.go`).

### SF-066 — Two-binary-version compatibility across the full expand/migrate/contract window (real OS processes)

- **Initial state**: A database rewound to exactly the pre-Phase-13
  schema (migrations `0001`-`0010`).
- **Actions**: A real `cmd/worker` binary pinned to
  `794abbb57a7e2a965d556700468930a1ebed23e4` (built via a detached `git
  worktree`; no knowledge of `queue_name` at the Go type level at all) and
  one pinned to `c17f89c592c803a8f7d2fbd61cde566467ebe062` (the full Phase
  13 delta) run concurrently while migrations `0011`→`0014` are applied
  one at a time via `internal/migrate.UpTo`; a job is submitted and driven
  to completion at every intermediate stage.
- **Fault**: The schema changing underneath two binaries compiled against
  different versions of it, concurrently.
- **Expected durable state**: Every submitted job reaches `SUCCEEDED`;
  `internal/invariant.Checker.CheckAll` finds zero violations at every
  stage, including the intermediate ones, not just before `0011` and
  after `0014`.
- **Invariants proved**: Every `TF-INV-*` holding throughout a
  mixed-binary-version window (Phase 14's stated breadth requirement, not
  a new invariant).
- **Test**: `TestCompat_SF066_SF067_TwoBinaryVersionAcrossExpandMigrateContractWindow`
  (`test/compat/two_binary_test.go`).

### SF-067 — Old worker binary continues functioning against a post-Phase-13 schema

- **Initial state/Actions**: The same run as SF-066.
- **Fault**: The OLD binary's claim query was compiled before `queue_name`
  existed at all.
- **Expected durable state**: Jobs claimed and completed throughout the
  window carry a schema-defaulted `queue_name = 'default'`, regardless of
  which of the two binaries actually claimed them — the old binary's
  completely queue_name-unaware query neither errors nor stalls against
  the evolved schema.
- **Invariants proved**: The roadmap's explicit "old worker binary
  continues to function correctly against a post-Phase-13 schema"
  requirement — a real-binary strengthening of the existing SF-046
  in-process proof.
- **Test**: `TestCompat_SF066_SF067_TwoBinaryVersionAcrossExpandMigrateContractWindow`
  (`test/compat/two_binary_test.go`, same run as SF-066).

### SF-068 — Repurposed `job_type` across an incompatible payload shape

- **Initial state**: A job queued under a `job_type` with an old payload
  shape.
- **Actions**: A handler now registered under that same `job_type`
  expects a field the old shape never had.
- **Fault**: The handler's own validation/unmarshal logic fails against
  the old-shaped payload.
- **Expected durable state**: The job dead-letters via the ordinary
  classified-failure path (permanent, by `internal/handler.Classify`'s
  documented default) — not a panic, not a silent no-op, not an infinite
  retry loop.
- **Invariants proved**: Documents, rather than prevents, the named gap
  in docs/compatibility-policy.md's "PROPOSED: Job Payload/Schema
  Evolution" — the roadmap's own explicitly flagged requirement that this
  failure mode be known even though it is not guarded against.
- **Test**: `TestCompat_SF068_RepurposedJobTypeAcrossIncompatiblePayloadShape`
  (`test/compat/two_binary_test.go`).

### SF-069 — `cmd/worker` drain timeout elapses: no completion written, lease reclaimable (real OS process)

- **Initial state**: A real `cmd/worker` binary (drain timeout
  configured) executing a job whose handler will run longer than the
  configured drain window.
- **Actions**: A real SIGTERM is sent; the configured drain timeout then
  elapses before the handler returns.
- **Fault**: The in-flight job does not finish within the drain budget.
- **Expected durable state**: `dispositionDraining` fires; no `Complete*`
  call is made; the job's row is left exactly as it was (`RUNNING`, under
  the draining worker's own lease) — directly verified by querying the
  row immediately after the process exits; once `lease_expires_at`
  passes, a second, freshly started worker reclaims and completes it
  through the ordinary, unmodified TF-INV-004 path.
- **Invariants proved**: §19 OD-3's core safety claim — a drain-timeout
  expiry is indistinguishable, from the job's and the store's perspective,
  from an ordinary worker crash at that instant; no `job_attempts` row
  records a false failure/timeout.
- **Test**: `TestProc_SF069_WorkerDrainTimeout_NoCompletionWritten_LeaseReclaimable`
  (`test/procs/worker_drain_test.go`); see also the in-process analogue
  `TestRunOnce_SF069InProcess_DrainTimeoutExpiryReportsNoCompletion_LeaseReclaimable`
  (`internal/worker/drain_test.go`).

### SF-070 — `cmd/api` shutdown deadline: a still-running handler is forcibly ended, not abandoned (real OS process)

- **Initial state**: A real `cmd/api` binary handling a deliberately slow
  request (a real client streaming its body far slower than
  `TASKFORGE_API_SHUTDOWN_TIMEOUT`).
- **Actions**: A real SIGTERM is sent; the shutdown deadline passes while
  the request is still in flight.
- **Fault**: `srv.Shutdown`'s own deadline elapsing does not, by itself,
  interrupt a handler blocked on real socket I/O.
- **Expected durable state/behavior**: The still-open connection is
  forcibly closed (`srv.Close`, after `BaseContext` cancellation) once the
  deadline passes — the client observes its request fail, rather than
  hanging forever or succeeding long after the deadline; the process
  itself still exits promptly afterward, with a structured
  `api_shutdown_completed` log recording `outcome=deadline_exceeded`.
- **Invariants proved**: §19 OD-4's redesign — `cmd/api`'s
  shutdown-deadline behavior is now deterministic and testable, not an
  unexamined stdlib default that silently abandons a goroutine.
- **Test**: `TestProc_SF070_APIShutdown_SlowHandlerObservesCancellationAtDeadline`
  (`test/procs/api_shutdown_test.go`).

## Scenario-to-Invariant Cross-Check

See [testing-strategy.md](testing-strategy.md) for the invariant-to-test
matrix, which lists which scenario(s) prove each `TF-INV-*`. Every
scenario above must appear in that matrix at least once; every invariant
must be covered by at least one scenario. This document and
[testing-strategy.md](testing-strategy.md) are kept in sync deliberately —
a new invariant added to [invariants.md](invariants.md) requires both a new
or extended scenario here and a new row in that matrix.
