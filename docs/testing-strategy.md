# Testing Strategy

Status: foundational. This is the verification plan for every invariant in
[invariants.md](invariants.md). No invariant is considered credible without
a corresponding entry here mapping to an executable test.

## Phase 1 & Phase 2 Implementation Status

As of Phase 1 ([roadmap.md](roadmap.md)), the following test categories
below have real, passing, executable tests — everything else in this
document remains the verification *plan* for later phases, not yet built:

- **Unit tests** (request validation): `internal/api/handlers_validation_test.go`.
- **State-machine table tests**: `internal/jobstate/state_test.go` — the
  full 6×6 matrix, not just Phase 1's subset.
- **PostgreSQL integration tests**: `internal/store/store_test.go`,
  `internal/worker/worker_test.go`, `internal/api/handlers_integration_test.go`,
  `internal/migrate/migrate_test.go` — all against a real PostgreSQL
  instance per `internal/testutil` (embedded-postgres locally, or a real
  service container in CI via `TASKFORGE_TEST_DATABASE_URL`), never a mock.
- **Fault-injection tests** (TF-INV-013 only):
  `TestFaultInjection_RollbackLeavesRowUnchanged` in
  `internal/store/store_test.go`.
- **Process restart tests** (an early, minimal version of SF-018 only —
  the exact scope [roadmap.md](roadmap.md) requires for Phase 1):
  `TestRestart_RunningJobSurvivesFreshStoreInstance` in
  `internal/store/store_test.go`.

As of Phase 2, the following additional categories now have real, passing,
executable tests, all in `internal/store/lease_test.go` and
`internal/worker/lease_test.go` unless noted:

- **Concurrency tests with real concurrent workers** (TF-INV-002, SF-006):
  `TestClaim_ConcurrentWorkersRaceForSameJob` (N goroutines, real pooled
  PostgreSQL connections, racing for a shared job pool) and
  `TestClaim_ConcurrentReclaimRace` (the reclaim-branch analogue).
- **Lease-expiration tests** (TF-INV-004, SF-007): `TestClaim_ReclaimsExpiredLease`,
  `TestClaim_UnexpiredLeaseIsNeverReclaimed`, `TestClaim_TerminalJobNeverReclaimed`,
  `TestClaim_SweepDeadLettersAttemptExhaustedExpiredLease` — all use
  deterministic DB-time manipulation (`forceExpireLease` in
  `internal/store/testhelpers_test.go`), never a sleep-based wait.
- **Stale-worker fencing under genuine concurrency** (TF-INV-003,
  TF-INV-014, SF-008): `TestFencing_StaleWorkerCompletionRejectedAfterReclaim`
  (the canonical two-generation sequence), `TestFencing_ArbitrarilyLateArrivalAcrossManyGenerations`
  (4+ generations), `TestFencing_StaleCompletionRejectedBeforeNewOwnerCompletes`.
  This closes the gap Phase 1 explicitly left open (only the single-worker
  stale-credential *mechanism* was tested then).
- **Heartbeat-specific fencing tests** (TF-INV-015, SF-016/SF-017):
  `TestHeartbeat_ExtendsValidLease`, `TestHeartbeat_MonotonicAcrossRapidRenewals`,
  `TestHeartbeat_RejectsStaleGeneration`, `TestHeartbeat_RejectsWrongOwner`,
  `TestHeartbeat_NeverResurrectsOrReopensTerminalJob`,
  `TestHeartbeat_ThenReclaim_ValidLeaseIsNotStolen`,
  `TestReclaim_ThenHeartbeat_StaleHeartbeatRejected`; the long-running-job
  path (SF-017) at the worker-loop level:
  `TestRunOnce_LongRunningJob_HeartbeatKeepsLeaseAlive` in
  `internal/worker/lease_test.go`.
- **Worker-crash tests at the worker-loop level** (TF-INV-004, SF-002/003):
  `TestRunOnce_LeaseLostDuringExecution_SkipsCompletion` in
  `internal/worker/lease_test.go` — the canonical Worker A/Worker B
  scenario reproduced through the actual claim→heartbeat→execute loop, not
  just direct store calls.
- **Fault-injection tests, extended to the new multi-statement claim
  transaction** (TF-INV-013): `TestClaim_RollbackOnAttemptConflictLeavesJobRowUnchanged`.
- **Migration upgrade tests**: `TestUp_UpgradesPhase1SchemaToPhase2` in
  `internal/migrate/migrate_test.go` — a database with only migration
  `0001` applied is upgraded to Phase 2's schema without data loss.

As of Phase 3 ("Retries, Backoff, DLQ"), the following additional
categories now have real, passing, executable tests, in
`internal/store/retry_test.go`, `internal/worker/retry_test.go`, and
`internal/retry/backoff_test.go` unless noted:

- **Unit tests** (backoff calculation, no database): `internal/retry/backoff_test.go`
  — the exact `raw_delay = min(max_backoff, base_delay * 2^(attempt-1))`
  formula against concrete attempts, the equal-jitter formula's exact
  boundaries via a deterministic stub `RandSource`, `Config.Validate`
  rejecting non-positive/inverted configuration, and overflow safety at
  extreme attempt counts (`TestRawDelay_OverflowSafeAtExtremeAttempts`,
  adversarial case #12).
- **Retry tests** (SF-009, SF-010): see
  [scenario-corpus.md](scenario-corpus.md)'s Phase 3 section for the full
  test list at both the store level (direct `Store.CompleteRetryableFailure`
  calls) and the worker-loop level (a real `handler.Handler` driven through
  `Worker.RunOnce`).
- **Property-based test** (the roadmap's Phase 3 quality gate: "property
  test confirming `attempt_count` never exceeds `max_attempts` across
  randomized failure sequences"): `TestProperty_AttemptCountNeverExceedsMaxAttempts`
  in `internal/store/retry_test.go` — a fixed-seed sweep of randomized
  `max_attempts` values and randomized retryable-failure-before-success/
  exhaustion sequences, asserting `TF-INV-006` holds in every generated
  scenario, not just hand-picked examples.
- **Stale-worker fencing tests, extended to the retry/dead-letter
  transition**: `TestCompleteRetryableFailure_RejectsStaleGeneration`,
  `TestCompleteRetryableFailure_RejectsWrongOwner`,
  `TestCompleteRetryableFailure_RejectedAfterReclaim` (the SF-008 shape
  replayed against `CompleteRetryableFailure`), and
  `TestCompleteFailure_PermanentRejectedAfterReclaim` (the same for a
  permanent failure racing a reclaim — adversarial cases #3/#4/#15). The
  documented "late but not yet superseded" boundary
  (`TestCompleteSuccess_AcceptedAfterExpiryButBeforeReclaim`, Phase 2) has a
  retry-path analogue: `TestCompleteRetryableFailure_AcceptedAfterExpiryButBeforeReclaim`
  (adversarial case #14).
- **Fault-injection test, extended to the retry transition** (TF-INV-013):
  `TestRetryTransition_RollbackLeavesJobAndAttemptConsistent` (adversarial
  cases #6/#7) — forces the transaction to fail after the job-row UPDATE
  but before the `job_attempts` finalize UPDATE commits, and asserts both
  rows are left byte-for-byte as they were before the transaction began.
- **Retry-eligibility / concurrency tests**: no claim before `eligible_at`
  (`TestClaim_RetryWaitNotClaimableBeforeEligibility`), claim after
  (`TestClaim_RetryWaitClaimableAfterEligibility`), N workers racing a
  single `RETRY_WAIT` job the instant it becomes eligible
  (`TestClaim_ConcurrentWorkersRaceForEligibleRetryWaitJob`, adversarial
  case #1), and multiple independently-eligible `RETRY_WAIT` jobs claimed
  by a worker pool with no double-claim
  (`TestClaim_MultipleWorkersOnlyOneWinsOnceEligible`) — all using DB-time
  manipulation (`forceSetEligibleAt`, mirroring Phase 2's
  `forceExpireLease`), never a sleep.
- **Durability-across-restart test** (adversarial case #9):
  `TestRetryWait_SurvivesFreshStoreInstance` — a fresh `*store.Store`
  standing in for a full process restart correctly still refuses to claim
  before `eligible_at` and correctly claims after, with no in-memory retry
  timer involved at any point.
- **Reclaim interaction test** (adversarial case #8):
  `TestClaim_ReclaimStillWorksAfterPriorRetryCycle` — Phase 2's
  lease-expiry reclaim mechanism (a worker crash that never even calls
  `CompleteRetryableFailure`) still functions correctly on the second (and
  later) attempt of a job that has already been through one `RETRY_WAIT`
  cycle.

As of Phase 4 ("Idempotency"), the following additional categories now
have real, passing, executable tests, in `internal/store/idempotency_test.go`,
`internal/api/handlers_integration_test.go`, and
`internal/worker/idempotency_test.go`:

- **Idempotency tests** (SF-005, TF-INV-008, TF-INV-016): sequential
  duplicate, 60-goroutine concurrent duplicate (store level) and
  25-goroutine concurrent duplicate (full HTTP boundary), scope
  (`job_type` isolation), the documented first-write-wins conflicting-
  payload decision, duplicate submission after the mapped job reaches a
  terminal state, and durability across a simulated process/API restart
  (a fresh `*store.Store`/`*api.Handlers` sharing only the database) — see
  [scenario-corpus.md](scenario-corpus.md)'s Phase 4 section for the full
  test list.
- **Fault-injection test, extended to submission idempotency**
  (TF-INV-013): `TestInsertIdempotent_RollbackLeavesNoPartialIdempotencyState`
  — proves a rolled-back submission leaves neither a job row nor an
  idempotency mapping (the two cannot diverge here: `idempotency_key` is a
  column on the `jobs` row itself, written in the same single `INSERT`
  statement as the row it maps to, not a separate table).
- **Execution-side idempotency identity tests** (SF-004, ADR-0004):
  `TestIdempotencyIdentity_JobIDStableAcrossReclaim` and
  `TestIdempotencyIdentity_JobIDStableAcrossRetry` prove `job_id` is
  unchanged across a reclaim/retry even though `lease_generation`/
  `attempt_count` advance. `TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice`
  and its companion `TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect`
  are the exactly-once-logical-effect demonstration docs/roadmap.md's
  Phase 4 scope requires: the same forced-reclaim crash sequence, once
  with a non-idempotent side-effect double (duplication occurs, proving
  TaskForge still only offers at-least-once execution) and once with a
  `job_id`-keyed dedup table (exactly one durable effect row despite two
  handler invocations).

**Still not implemented**: fuzz tests, the cancellation race test SF-012
(Phase 6), the scheduling test SF-013 (Phase 6), sustained load tests and
chaos tests (Phase 5/9 — Phase 2's concurrency tests use small, fixed
worker/job counts to prove correctness deterministically, not
sustained/randomized load). Phase 3's optional background sweeper
(docs/architecture.md: "an optimization, not a correctness dependency")
was not built — the Lazy Dead-Letter Sweep already running inside every
`Claim` call (Phase 2) remains the sole, sufficient mechanism for
`TF-INV-006` on the reclaim path; a periodic out-of-band sweeper would
only shrink the (already bounded) window before an attempt-exhausted
expired lease is visible as `DEAD_LETTERED`, which no required invariant
or scenario depends on.

The invariant-to-test matrix below is the full, multi-phase plan and is
**not** rewritten per phase — see [docs/roadmap.md](roadmap.md)'s Phase 1
and Phase 2 sections for exactly which invariants each phase is
responsible for proving, and README.md's "Phase 1 guarantees" / "Phase 2
guarantees" sections for which of this matrix's scenarios have a passing
test today.

## Test Categories

| Category | Purpose | Example |
|---|---|---|
| **Unit tests** | Pure logic with no database — backoff calculation, state-transition table lookups, request validation. | `backoff(attempt=3) == expected_delay ± jitter bound` |
| **State-machine table tests** | Enumerate every `(from_state, to_state)` pair and assert the transition function accepts exactly the allowed set and rejects every other pair, including all combinations involving terminal states. | Table-driven test iterating all 6×6 state pairs. |
| **PostgreSQL integration tests** | Real Postgres (via a throwaway test database/container), exercising actual SQL from [worker-protocol.md](worker-protocol.md) against real transactions and constraints. | Claim query against a seeded `jobs` table, assert `RETURNING` row shape and side effects. |
| **Concurrency tests** | Multiple real goroutines/processes racing against the same rows, asserting exclusivity properties. | N workers claiming from a pool of M jobs; assert each job claimed exactly once. |
| **Property-based tests** | Randomized inputs (attempt counts, max_attempts, failure sequences) checked against invariants rather than fixed examples. | For random `max_attempts` and random failure/success sequences, assert `attempt_count` never exceeds `max_attempts` before a terminal state. |
| **Fuzz tests** | Randomized malformed/edge-case inputs at the API boundary (payload shapes, header values) to find panics/crashes, not correctness-of-business-logic. | Fuzzing `POST /jobs` payload parsing. |
| **Fault-injection tests** | Deliberately induced failures (killed connections, forced rollbacks, injected errors mid-transaction) to verify TF-INV-013 and related atomicity properties. | Kill the DB connection mid-transaction and assert the row is unchanged afterward. |
| **Process restart tests** | Kill and restart API server / worker processes mid-operation, assert durable state is the sole source of truth. | Kill API server between commit and response write (TF-INV-001); kill worker mid-execution and assert reclaim (TF-INV-004). |
| **Worker crash tests** | Simulate a worker crash at each point in [retry-semantics.md](retry-semantics.md)'s crash-timing table. | SF-002/003/004. |
| **Lease-expiration tests** | Advance time (or use short TTLs) to force expiry and assert reclaim behavior and generation increment. | SF-007. |
| **Stale-worker fencing tests** | Construct the exact generation-race sequence from [worker-protocol.md](worker-protocol.md) and assert rejection. | SF-008. |
| **Retry tests** | Assert backoff timing, attempt numbering, and DLQ transition boundary. | SF-009, SF-010. |
| **Idempotency tests** | Concurrent duplicate submissions, assert single job row. | SF-005. |
| **Race tests** | Deterministic interleavings of cancel-vs-complete, forced via held-open transactions or synchronization points in test code. | SF-012. |
| **Load tests** | Sustained throughput against realistic job volume, verifying the claim index strategy holds up and metrics remain accurate under load. | Phase 9. |
| **Chaos tests** | Combinations of the above injected randomly and continuously against a running system, assert invariants hold cumulatively over a long run (not just for a single crafted scenario). | Phase 9. |

## Invariant-to-Test Matrix

Every invariant maps to at least one test category and at least one named
scenario from [scenario-corpus.md](scenario-corpus.md).

| Invariant | Test category | Scenario(s) |
|---|---|---|
| TF-INV-001 | Process restart tests | SF-018 |
| TF-INV-002 | Concurrency tests | SF-006 |
| TF-INV-003 | Stale-worker fencing tests | SF-008 |
| TF-INV-004 | Worker crash tests, lease-expiration tests | SF-002, SF-003, SF-007 |
| TF-INV-005 | State-machine table tests | SF-015 |
| TF-INV-006 | Property tests | SF-010 |
| TF-INV-007 | Integration tests (append-only assertions) | SF-009, SF-010 |
| TF-INV-008 | Idempotency tests (concurrency) | SF-005 |
| TF-INV-009 | Integration tests | SF-010 |
| TF-INV-010 | Race tests | SF-012 |
| TF-INV-011 | Integration tests (clock-controlled) | SF-013 |
| TF-INV-012 | DAG scenario tests (Phase 7) | (Phase 7 scenarios, TBD) |
| TF-INV-013 | Fault-injection tests | SF-014 |
| TF-INV-014 | Stale-worker fencing tests (multi-generation) | SF-008 |
| TF-INV-015 | Fencing tests (heartbeat-specific) | SF-016, SF-017 |
| TF-INV-016 | Schema tests + idempotency tests | SF-005 |

Every row in this table must remain populated as the project moves into
implementation; a code change that would leave any invariant without a
passing test is a regression regardless of what other tests pass.

## What "Proving an Invariant" Means Here

A test proves an invariant when it constructs the *specific adversarial
sequence* the invariant's "violating example" describes (see
[invariants.md](invariants.md)) and asserts the correct outcome, not merely
when it exercises the happy path and happens not to fail. Coverage
percentage is not the metric that matters — scenario coverage against the
corpus in [scenario-corpus.md](scenario-corpus.md) is.

## Test Environment Expectations

- PostgreSQL integration tests run against a real PostgreSQL instance (a
  disposable per-test-run database/container), not an in-memory
  substitute or a mocked driver — the correctness properties depend on
  actual PostgreSQL semantics (`FOR UPDATE SKIP LOCKED`, transaction
  isolation, constraint enforcement), which cannot be faithfully mocked.
- Time-dependent tests (lease expiration, scheduling, backoff) control time
  explicitly — either via short, test-configured TTLs plus real sleeps, or
  via a controllable clock source — rather than relying on wall-clock waits
  long enough to be flaky under CI load. The exact mechanism is an
  implementation detail decided in Phase 1/2, but the requirement (no
  flaky wall-clock-duration-dependent assertions) is binding now.
- Concurrency and race tests must be deterministic, not merely
  probabilistic — where true nondeterminism is unavoidable (e.g., OS
  thread scheduling), tests use explicit synchronization points
  (channels, barriers, or transaction-hold techniques) to force the
  interleaving under test, rather than relying on repeated runs to
  "usually" hit the race.

## Cross-References

- Invariants: [invariants.md](invariants.md)
- Scenarios: [scenario-corpus.md](scenario-corpus.md)
- Roadmap placement of test categories: [roadmap.md](roadmap.md) (each
  phase names its required tests)
