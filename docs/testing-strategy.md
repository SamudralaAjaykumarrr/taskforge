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

**Still not implemented**: property-based tests, fuzz tests, retry tests
(no `RETRY_WAIT`/backoff exists yet — Phase 3), idempotency tests (Phase
4), the cancellation race test SF-012 (Phase 6), the scheduling test
SF-013 (Phase 6), sustained load tests and chaos tests (Phase 5/9 — Phase
2's concurrency tests use small, fixed worker/job counts to prove
correctness deterministically, not sustained/randomized load).

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
