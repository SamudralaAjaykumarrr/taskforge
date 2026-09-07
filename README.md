# TaskForge

**Status: Experimental (Phase 1 — Single-Node Durable Job Engine, Phase 2 —
Worker Leases and Heartbeats, Phase 3 — Retries, Backoff, DLQ, Phase 4 —
Idempotency, Phase 5 — Concurrency Hardening, and Phase 6 — Scheduling,
Cancellation, Timeouts — all complete; see "Proposed maturity label" under
"Phase 6: What's Implemented" below for why this README does not yet
promote to Stable despite [docs/roadmap.md](docs/roadmap.md) naming
Hardening → Stable as Phase 6's on-completion label).**
Phases 1 through 6 of [docs/roadmap.md](docs/roadmap.md) are implemented: a
durable PostgreSQL-backed job engine with HTTP submission (including
database-enforced `Idempotency-Key` deduplication and optional
`scheduled_at` future-execution requests), multiple concurrent worker
processes, fenced lease-based ownership with heartbeat renewal, automatic
crash recovery via lease expiration/reclaim, and the full
`QUEUED -> RUNNING -> SUCCEEDED` / `RUNNING -> RETRY_WAIT -> RUNNING` /
`RUNNING -> DEAD_LETTERED` / `RUNNING -> CANCELLED` / `QUEUED|RETRY_WAIT ->
CANCELLED` documented state machine (all six job-level states are now
reachable), with real durable exponential backoff, retryable-vs-permanent
failure classification, durable scheduled-execution eligibility, a
`POST /jobs/{id}/cancel` endpoint with deterministic cancel-vs-completion
race resolution, and a real execution-timeout ceiling distinct from lease
duration — see "Phase 6: What's Implemented" below. It is **not**
distributed beyond a single PostgreSQL instance, and does not implement
workflow/DAG execution yet — see "Phase 1: What's Implemented" through
"Phase 6: What's Implemented" below for the exact boundary. Everything else
described in this README past those sections remains a design target for
later phases, not a demonstrated capability. TaskForge guarantees
**at-least-once** execution, never exactly-once for arbitrary external side
effects — see "Phase 2 guarantees" below for the specific, tested
duplicate-side-effect window that phase closes and the one it deliberately
leaves open (retries, including Phase 3's new backed-off `RETRY_WAIT`
retries, are exactly this same at-least-once mechanism, not a new
exposure); see "Phase 4 guarantees" below for how a handler achieves
exactly-once **logical effects** on top of that at-least-once model,
without TaskForge ever claiming exactly-once execution itself. Phase 5 does
not change any of these guarantees — it adds no new capability, per its
explicit non-goal — it only proves the existing ones continue to hold under
tens of concurrent workers and sustained, repeated, randomized-crash
contention rather than just isolated scenarios. Phase 6 adds cooperative
cancellation and execution-timeout enforcement on top of this
foundation — TaskForge can **request** cooperative cancellation of a
running job and can **detect** that an attempt exceeded its configured
execution timeout, but it cannot physically terminate arbitrary handler
code that ignores `ctx.Done()`; see "Phase 6: What's Implemented" below for
exactly what is and is not guaranteed.

## The Problem

How do we execute background jobs reliably when workers crash, jobs are
retried, messages may be duplicated, processing times out, multiple workers
compete for the same work, and partial failures occur?

TaskForge is a durable, distributed job and workflow execution engine built
to answer that question precisely — with a documented state machine,
numbered invariants, and a test-scenario corpus that maps directly to
production failure modes, rather than an untested assertion of
reliability. It is not a to-do-list app. See [docs/vision.md](docs/vision.md)
for what TaskForge is and, just as importantly, what it explicitly is not.

## The Core Reliability Thesis

TaskForge is designed to guarantee **at-least-once execution** with fenced,
exclusive worker ownership and durable, auditable state transitions. It is
**not** designed to guarantee, and will never claim, exactly-once execution
of arbitrary external side effects — that is a fundamental limit of any
system that performs side effects outside its own transaction, not a gap to
be engineered away. What TaskForge *does* target is exactly-once **logical
effects**, achieved through application-level idempotency patterns the
design provides stable identifiers for. See
[docs/vision.md](docs/vision.md) and
[ADR-0003](docs/adr/0003-at-least-once-execution-not-exactly-once.md).

## Architecture Overview

The target architecture (not yet built) is deliberately small: an API
server, a worker pool, and PostgreSQL as the single durable source of
truth. There is no message broker, no Redis, no Kubernetes-specific
dependency, and no separate scheduler service in the core design —
scheduling and retry backoff are both expressed as a single durable
timestamp column checked by the same query workers already use to find
work. Full detail: [docs/architecture.md](docs/architecture.md).

```
   API Server(s)  --->  PostgreSQL (source of truth)  <---  Worker Pool
   (stateless)          jobs, job_attempts                  (fenced leases,
                                                              heartbeats)
```

## Planned Reliability Properties

Once implemented, TaskForge targets (each backed by a numbered invariant in
[docs/invariants.md](docs/invariants.md) and a named scenario in
[docs/scenario-corpus.md](docs/scenario-corpus.md)):

- Durable job persistence — an accepted job is never silently lost.
  **Proven (Phase 1).**
- Fenced worker leasing — at most one worker owns a job at a time, and a
  worker that loses its lease cannot retroactively complete the job, even
  arbitrarily late (TF-INV-002, TF-INV-003, TF-INV-014). **Proven (Phase
  2) — including under real concurrent workers, not just the stale-credential
  mechanism; re-proven under sustained tens-of-workers/hundreds-of-jobs
  contention and repeated randomized crash injection (Phase 5).**
- Automatic crash recovery via lease expiration — no permanently stranded
  jobs (TF-INV-004). **Proven (Phase 2); re-proven under sustained
  concurrent reclaim pressure (Phase 5).**
- Heartbeat renewal without transferring or shortening a lease
  (TF-INV-015). **Proven (Phase 2).**
- Durable, monotonic retry history with configurable backoff and dead-letter
  behavior (TF-INV-006, TF-INV-007, TF-INV-009). **Proven (Phase 3)** — real
  exponential-backoff `RETRY_WAIT`, retryable-vs-permanent failure
  classification, and exhaustion-driven `DEAD_LETTERED` all now exist and
  are tested against real PostgreSQL, including under adversarial
  stale-generation and rollback scenarios; see "Phase 3: What's
  Implemented" below.
- Database-enforced submission idempotency (TF-INV-008, TF-INV-016).
  **Proven (Phase 4)** — `Idempotency-Key` deduplication is enforced by a
  PostgreSQL unique constraint (not a check-then-act read), proven under
  real concurrent duplicate submissions, sequential retries, and simulated
  process restarts; see "Phase 4: What's Implemented" below.
- Deterministic cancellation-vs-completion race semantics (TF-INV-010).
- Scheduling that survives full process/fleet restarts (TF-INV-011).
- (Later phase) dependency-gated workflow/DAG execution (TF-INV-012).

The two bullets above with no "Proven" annotation are not implemented yet.
They are documented now, precisely, so that implementation has an
unambiguous contract to satisfy and external reviewers have something
falsifiable to check it against.

## Project Status

| Area | Status |
|---|---|
| Architecture & invariant documentation | **Done** (this repository, current state) |
| PostgreSQL schema | **Phase 2 done, still sufficient through Phase 6** (`jobs` table per Phase 1 already included `RETRY_WAIT`/`eligible_at`/`scheduled_at`/`cancel_requested`/`cancel_requested_at`/`terminal_at`/`last_error_class`; `job_attempts` added in Phase 2, migration `0002_create_job_attempts_table`, already allowed `FAILED_RETRYABLE`/`TIMED_OUT`/`CANCELLED`. Phase 3, Phase 4, and Phase 6 all required **no new migration** — see "Phase 3/4/6: What's Implemented" below) |
| API server | **Phase 1 done, extended in Phase 6** (`POST /jobs`, `GET /jobs/{id}`, `POST /jobs/{id}/cancel`; `GET /jobs/{id}` already surfaced `state`/`attempt_count`/`eligible_at`/`last_error`/`last_error_class`, so it needed no change to expose `RETRY_WAIT` and retry/DLQ status — Phase 6 added `scheduled_at` request/response support and the cancel endpoint) |
| Worker / claim / lease protocol | **Phase 2 done, hardened under load in Phase 5, extended in Phase 6**: multiple concurrent worker processes, lease expiration/reclaim, heartbeat renewal, fencing proven under real concurrent workers (not just the stale-credential mechanism), cooperative cancellation observation, and a fixed per-attempt execution-timeout ceiling distinct from lease renewal — see "Phase 2", "Phase 5", and "Phase 6: What's Implemented" below |
| Retry / backoff / DLQ | **Phase 3 done**: durable exponential backoff with equal jitter, retryable-vs-permanent failure classification via the handler contract, `RETRY_WAIT` claimable via the same claim query as `QUEUED`, and exhaustion-driven `DEAD_LETTERED` with preserved attempt history — see "Phase 3: What's Implemented" below |
| Idempotency enforcement | **Phase 4 done**: `Idempotency-Key` request header, database-unique-constraint-enforced deduplication (no check-then-act read), proven under 60-goroutine concurrent duplicate submissions and simulated process restarts; execution-side `job_id` identity documented and demonstrated — see "Phase 4: What's Implemented" below |
| Concurrency hardening | **Phase 5 done**: tens-of-workers/hundreds-of-jobs claim/reclaim/fencing/retry stress suites against real PostgreSQL, a connection-pool-constrained claim test, and a repeated-seed randomized crash/retry/success simulation — no new product code required, since Phase 1–4's short-transaction, fenced-lease design already satisfied every property tested; see "Phase 5: What's Implemented" below |
| Scheduling | **Phase 6 done**: `scheduled_at`/`eligible_at` future-execution semantics, PostgreSQL-authoritative eligibility (no in-memory scheduler), survives full process/fleet restart — see "Phase 6: What's Implemented" below |
| Cancellation / timeouts | **Phase 6 done**: `POST /jobs/{id}/cancel`, deterministic cancel-vs-completion race resolution (TF-INV-010), a real `execution_timeout` ceiling distinct from lease TTL, cooperative cancellation observation via heartbeat — see "Phase 6: What's Implemented" below for the explicit, documented limits (cooperative only, no forced termination of uncooperative handler code) |
| Workflow / DAG execution | Not started (staged for a later phase) |
| Observability | Not started beyond structured logs (claim, reclaim, heartbeat rejection, stale-completion rejection, retry scheduled, retries exhausted, permanent-failure dead-letter, new/duplicate idempotent submission, cancellation observed/acknowledged, and execution timeout are all logged — see "Phase 2", "Phase 3", "Phase 4", and "Phase 6: What's Implemented") |
| Test suite (unit/integration/concurrency/chaos) | **Phase 6 subset done**: unit (including a randomized property test), state-machine table, PostgreSQL integration, and real multi-goroutine concurrency tests (claim races, reclaim races, retry-eligibility races, idempotency-key submission races, scheduling races, cancellation races) at small scale (Phase 1–4) extended with tens-of-workers/hundreds-of-jobs stress variants (Phase 5) and scheduling/cancellation/timeout scenario coverage including SF-011/SF-012/SF-013 (Phase 6); sustained multi-hour chaos/load testing remains Phase 9 |

See [docs/roadmap.md](docs/roadmap.md) for the full phased plan, from
Phase 1 (single-node durable job engine) through Phase 10 (external-review
hardening and v1.0), including required invariants, required tests, and
completion criteria for each phase.

## Phase 1: What's Implemented

### Implemented now

- A `jobs` table (see [docs/data-model.md](docs/data-model.md)), created by
  the migration in `migrations/`. `job_attempts` is **not** created yet —
  deferred to Phase 2, per [docs/roadmap.md](docs/roadmap.md)'s explicit
  allowance ("no `job_attempts` yet ... `job_attempts` table introduced
  here if not already in Phase 1").
- `POST /jobs` and `GET /jobs/{id}` (`internal/api`). No other endpoint
  exists — no cancellation, idempotency keys, scheduling, or history
  endpoint yet.
- A single, synchronous worker process (`cmd/worker`, `internal/worker`)
  that claims one eligible job at a time, runs a registered `Handler`
  (`internal/handler`), and reports success or failure.
- The state machine subset `QUEUED -> RUNNING -> SUCCEEDED` and
  `RUNNING -> DEAD_LETTERED` on any failure (no `RETRY_WAIT`), per
  [docs/roadmap.md](docs/roadmap.md)'s Phase 1 scope. The full state
  machine's legality table (`internal/jobstate`) is implemented and
  exhaustively tested now, even though the database operations
  (`internal/store`) only ever drive the Phase 1 subset of it.
- Fenced completion (`lease_owner`/`lease_generation`/`state='RUNNING'`
  guard clauses, per [docs/worker-protocol.md](docs/worker-protocol.md)),
  wired from the start so Phase 2 does not need a schema or query-shape
  migration — but see "Phase 1 limitations" below for what this does and
  does not prove yet.

### Not implemented yet

- Multiple concurrent workers, lease expiration, and reclaim — **now done,
  see "Phase 2: What's Implemented" below.**
- Retry/backoff and real dead-letter classification (Phase 3) — every
  failure still goes straight to `DEAD_LETTERED`.
- ~~Idempotency-Key submission support (Phase 4) — the database unique
  constraint exists, but no API surface uses it yet.~~ **Closed in Phase
  4** — see "Phase 4: What's Implemented" below.
- ~~Scheduling, cancellation, and execution timeouts (Phase 6).~~ **Closed
  in Phase 6** — see "Phase 6: What's Implemented" below.
- Workflow/DAG execution (Phase 7), observability (Phase 8), chaos/load
  testing (Phase 9).

### How to run locally

```sh
docker compose up -d          # starts PostgreSQL on localhost:5432
cp .env.example .env && set -a && source .env && set +a
go run ./cmd/api &            # HTTP API on :8080
go run ./cmd/worker &         # single worker process
curl -X POST localhost:8080/jobs \
  -d '{"job_type":"demo.echo","payload":{"hello":"world"}}'
curl localhost:8080/jobs/<id-from-above>
```

`cmd/worker` registers one demonstration handler, `demo.echo`, which logs
its payload and always succeeds — useful for manual smoke testing, not a
real job type.

### How to run tests

```sh
make test        # go test -p 1 ./...
make test-race   # go test -race -p 1 ./...
```

Integration tests need a real PostgreSQL instance
([docs/testing-strategy.md](docs/testing-strategy.md) requires this — no
mocked driver). Two ways to provide one:

- Set `TASKFORGE_TEST_DATABASE_URL` (e.g. to the `docker compose up -d`
  instance above, or a CI service container).
- Leave it unset: tests start a real, temporary PostgreSQL server via
  [`embedded-postgres`](https://github.com/fergusstrange/embedded-postgres)
  automatically (no Docker or root required) — this is how the test suite
  runs in this project's own sandboxed development environment, where
  Docker was not available.

`-p 1` is required, not a style preference: every package's integration
tests share one PostgreSQL instance, so their test binaries must not run
concurrently (see `internal/testutil` and
[docs/testing-strategy.md](docs/testing-strategy.md)).

### Phase 1 guarantees

- **TF-INV-001** (accepted jobs cannot disappear): `POST /jobs` returns
  2xx if and only if the INSERT has committed (`internal/store.Insert` is
  a single statement/transaction; see
  `TestCreateJob_AcknowledgementImpliesDurableCommit`,
  `TestInsert_ReturnsCommittedRow`).
- **TF-INV-005** (terminal states never reopen), proven two ways: (a) the
  full state-transition legality table is exhaustively tested
  (`internal/jobstate`, all 36 `(from, to)` pairs); (b) database-backed,
  for the states Phase 1 actually reaches
  (`TestTerminalStates_RejectFurtherTransitions` for `SUCCEEDED` and
  `DEAD_LETTERED`).
- **TF-INV-013** (rollback never leaves a half-transitioned job): every
  transition is one SQL statement or one transaction; proven by forcing a
  mid-transaction constraint violation and asserting the row is unchanged
  (`TestFaultInjection_RollbackLeavesRowUnchanged`).
- A restarted process observes only durable state — no in-memory state is
  correctness-relevant (`TestRestart_RunningJobSurvivesFreshStoreInstance`,
  an early/minimal version of SF-018).
- The fencing *mechanism* (lease_owner/lease_generation guard clauses)
  correctly rejects a stale credential
  (`TestCompleteSuccess_RejectsWrongLeaseGeneration`,
  `TestCompleteSuccess_RejectsWrongLeaseOwner`) — see limitations below for
  what this does not yet prove.

### Phase 1 limitations (explicit, not hidden)

- ~~No crash recovery.~~ **Closed in Phase 2** — see "Phase 2: What's
  Implemented" below.
- ~~No proof of concurrent-worker safety.~~ **Closed in Phase 2** — see
  below.
- **No retries.** Any handler error dead-letters the job immediately, per
  [docs/roadmap.md](docs/roadmap.md)'s Phase 1/2 simplification (Phase 2's
  non-goals explicitly keep "failure = dead-letter"). Retry budgets,
  backoff, and real dead-letter classification are Phase 3.
- ~~**No duplicate-submission protection.** The `(job_type,
  idempotency_key)` unique constraint exists in the schema, but
  `POST /jobs` does not accept an `Idempotency-Key` yet.~~ **Closed in
  Phase 4** — see "Phase 4: What's Implemented" below.
- **No production readiness claim of any kind** — no observability beyond
  structured logs, no authentication, no load/chaos testing yet (Phase 5/9).

## Phase 2: What's Implemented

**Status: Experimental**, pending repeated CI runs — see "Proposed maturity
label" at the end of this section for why this is not yet claimed as
Hardening despite passing every scenario listed here, repeatedly, in local
testing.

### Implemented now

- **Lease acquisition and reclaim** (`internal/store.Claim`): the full
  claim query from [docs/worker-protocol.md](docs/worker-protocol.md),
  including the expired-lease `RUNNING` branch — the *same* query and the
  *same* short transaction handle a fresh `QUEUED`/`RETRY_WAIT` claim and a
  reclaim of a crashed worker's job, exactly as documented ("no separate
  reaper process required for correctness"). Reclaim strictly advances
  `lease_generation`.
- **The Lazy Dead-Letter Sweep** (`internal/store.sweepExpiredExhaustedLeases`),
  run in the same transaction immediately before every claim attempt: an
  expired-lease job that has already exhausted `max_attempts` is
  dead-lettered instead of being handed back out as a new `RUNNING`
  attempt — this is what keeps `attempt_count <= max_attempts` true even
  under repeated crash/reclaim cycles (TF-INV-006), with no retry-backoff
  policy implemented yet.
- **Heartbeat renewal** (`internal/store.Heartbeat`): the same fencing
  guard shape as completion (`lease_owner`/`lease_generation`/`state =
  'RUNNING'`); extends `lease_expires_at` forward only, for the current
  generation only, and rejects a stale generation, wrong owner, or
  non-`RUNNING` job (TF-INV-015).
- **The `job_attempts` ledger** (`migrations/0002_create_job_attempts_table`):
  one row per claim, written in the same transaction as the claim/reclaim
  that opened it; finalized (`finished_at`/`outcome`) in the same
  transaction as the completion call that closes it, or as `LEASE_EXPIRED`
  by whichever reclaim/sweep superseded it. This is Phase 2's scope per
  [docs/roadmap.md](docs/roadmap.md) ("`job_attempts` table introduced here
  if not already in Phase 1"); it does not yet back a history API endpoint
  (deliberately deferred — see [worker-protocol.md](docs/worker-protocol.md)
  "Deferred Endpoints").
- **Fenced, multi-worker-safe completion**: `CompleteSuccess`/
  `CompleteFailure` are unchanged in shape from Phase 1 but are now proven
  correct under real concurrent workers and real reclaim, not just a
  stale-credential unit test — see "Phase 2 guarantees" below.
- **The worker's ownership lifecycle** (`internal/worker.Worker.RunOnce`):
  claim → execute (with a background heartbeat goroutine renewing the
  lease every `execution_timeout_seconds / 3`) → complete, *or* claim →
  execute → heartbeat detects lost ownership → handler's context is
  cancelled and no completion call is attempted. The heartbeat goroutine
  is guaranteed to exit before `RunOnce` returns, on every code path
  (normal completion, lease loss, or caller context cancellation) — no
  goroutine/ticker leak.
- **Multiple concurrent worker processes** are now a supported, tested
  configuration (concurrency *hardening* at scale remains Phase 5, but
  correctness under N real concurrent workers racing for a small job pool
  is proven now — see the concurrency tests below).
- Structured logs for the events this phase's design is required to make
  observable: claim, reclaim (logged distinctly from a fresh claim once
  `lease_generation > 1`), heartbeat renewal, lease loss, and stale
  completion rejection (`internal/worker`).

### Not implemented yet

- Retry/backoff, `RETRY_WAIT`, and real dead-letter classification (Phase
  3) — every failure still dead-letters immediately, now correctly
  reclaimed if the crash happens before that report.
- ~~Idempotency-Key submission support (Phase 4).~~ **Closed in Phase 4**
  — see "Phase 4: What's Implemented" below.
- ~~Scheduling delay, cancellation, and a real `execution_timeout`
  distinct from lease TTL (Phase 6) — lease expiration is the only timeout
  mechanism that exists.~~ **Closed in Phase 6** — see "Phase 6: What's
  Implemented" below.
- Workflow/DAG execution (Phase 7), full metrics/tracing observability
  (Phase 8), sustained chaos/load testing (Phase 5/9).
- A `GET /jobs/{id}/history` endpoint over `job_attempts` (deliberately
  deferred, per [docs/worker-protocol.md](docs/worker-protocol.md)).

### How to run a multi-worker crash-recovery demonstration

```sh
docker compose up -d
cp .env.example .env && set -a && source .env && set +a
go run ./cmd/api &
go run ./cmd/worker &      # worker A
go run ./cmd/worker &      # worker B — a second, independent process

# Submit a job with a short lease TTL so reclaim is visible quickly.
curl -X POST localhost:8080/jobs \
  -d '{"job_type":"demo.echo","payload":{"hello":"world"},"execution_timeout_seconds":5}'

# Note the winning worker's PID from its logs, then kill THAT process
# specifically (not both) while the job is still RUNNING:
kill -9 <winning-worker-pid>

# Within ~5s the surviving worker's claim loop reclaims the job (its log
# line reads "job reclaimed after lease expiration") and completes it.
curl localhost:8080/jobs/<id-from-above>   # state: SUCCEEDED
```

If you kill the worker before it has claimed anything, the other worker
simply keeps polling normally — there is nothing to reclaim yet. The demo
above intentionally targets the worker that actually won the claim, to
force the reclaim path rather than relying on luck.

### How to run tests

```sh
make test        # go test -p 1 ./...
make test-race   # go test -race -p 1 ./...
```

Same PostgreSQL requirement and `-p 1` constraint as Phase 1 (see "How to
run tests" above) — this is unchanged in Phase 2. Concurrency tests
(`internal/store/lease_test.go`'s `TestClaim_ConcurrentWorkersRaceForSameJob`,
`TestClaim_ConcurrentReclaimRace`) use real goroutines against real pooled
PostgreSQL connections, not a mock — no additional tooling or command
beyond the two above is required to run them; they are part of the normal
suite.

### Phase 2 guarantees

- **TF-INV-002** (at most one valid lease per job), now proven under real
  concurrency, not just single-worker sequencing:
  `TestClaim_ConcurrentWorkersRaceForSameJob` (SF-006) and
  `TestClaim_ConcurrentReclaimRace` — N goroutines against a real
  PostgreSQL connection pool, asserting exactly one claim per job and no
  `lease_generation` issued twice.
- **TF-INV-003 / TF-INV-014** (a lost/superseded lease can never
  authoritatively complete a job, no matter how late or how many
  generations behind): `TestFencing_StaleWorkerCompletionRejectedAfterReclaim`
  (the canonical SF-008 sequence), `TestFencing_ArbitrarilyLateArrivalAcrossManyGenerations`
  (4+ generations), `TestFencing_StaleCompletionRejectedBeforeNewOwnerCompletes`.
- **TF-INV-004** (worker crashes cannot permanently strand a recoverable
  job): `TestClaim_ReclaimsExpiredLease` (SF-007),
  `TestReclaim_SurvivesFreshStoreInstance` (SF-018's actual crash-recovery
  case — a fresh process, not just a fresh in-memory reference, reclaims
  correctly), `TestRunOnce_LeaseLostDuringExecution_SkipsCompletion` (the
  same scenario at the worker-loop level).
- **TF-INV-006** (retry attempts respect configured limits), on the
  reclaim path specifically: `TestClaim_SweepDeadLettersAttemptExhaustedExpiredLease`
  proves an attempt-exhausted expired lease is dead-lettered, never
  handed back out as a fresh attempt.
- **TF-INV-013** (rollback never leaves a half-transitioned job), extended
  to the new multi-statement claim transaction:
  `TestClaim_RollbackOnAttemptConflictLeavesJobRowUnchanged` forces the
  `job_attempts` insert inside `Claim`'s transaction to fail and asserts
  the `jobs` row is completely untouched.
- **TF-INV-015** (heartbeats only extend, never transfer, a lease):
  `TestHeartbeat_ExtendsValidLease`, `TestHeartbeat_MonotonicAcrossRapidRenewals`,
  `TestHeartbeat_RejectsStaleGeneration`, `TestHeartbeat_RejectsWrongOwner`,
  `TestHeartbeat_NeverResurrectsOrReopensTerminalJob`.
- Both possible arrival orderings of heartbeat-vs-reclaim are proven
  deterministically (not by repeated-run luck):
  `TestHeartbeat_ThenReclaim_ValidLeaseIsNotStolen` and
  `TestReclaim_ThenHeartbeat_StaleHeartbeatRejected`.
- The specific documented boundary that a late-but-not-yet-superseded
  completion is accepted (expiry alone does not fence a worker; only a new
  generation does) is proven, not just asserted in prose:
  `TestCompleteSuccess_AcceptedAfterExpiryButBeforeReclaim`.
- Migrations upgrade an existing Phase 1 database cleanly:
  `TestUp_UpgradesPhase1SchemaToPhase2` starts from a database with only
  migration `0001` applied (including a pre-existing job row) and proves
  `migrate.Up` adds `job_attempts` without touching existing data.
- No heartbeat goroutine/ticker leak, on every exit path:
  `TestRunOnce_HeartbeatGoroutineDoesNotLeak`,
  `TestRunOnce_ContextCancellationStopsHeartbeatDeterministically`.
- Real multi-worker integration, end to end through `worker.Worker.Run`
  (not just direct `Store` calls): `TestRun_MultipleWorkersProcessSharedJobPool`
  in `internal/worker/lease_test.go` runs 3 independent `Worker` instances
  against a shared 15-job pool and asserts every job is executed exactly
  once, with no double-processing.

### Phase 2 limitations (explicit, not hidden)

- **At-least-once, not exactly-once, execution — now demonstrable, not just
  asserted.** A crashed worker's job is retried by another worker after
  reclaim; if the crashed worker's handler had already performed an
  external side effect before the crash, that side effect **may run
  again** on the reclaiming worker's attempt. TaskForge has no way to
  distinguish "crashed before the side effect" from "crashed after the
  side effect, before reporting success" — this is the fundamental limit
  documented in [ADR-0003](docs/adr/0003-at-least-once-execution-not-exactly-once.md),
  now backed by a real reclaim mechanism that makes the window concrete
  and testable rather than hypothetical. Phase 4's idempotency-key
  patterns are how a job handler protects itself; TaskForge itself does
  not and will not claim to prevent this.
- **TaskForge cannot forcibly halt a handler that ignores context
  cancellation.** When a lease is lost mid-execution, the worker cancels
  the `context.Context` passed to the handler and stops treating itself as
  the authoritative owner (no completion call is attempted) — but if the
  handler's own code does not check `ctx.Done()`, TaskForge has no way to
  physically kill it mid-side-effect. Losing authoritative ownership is
  detected and acted on promptly; physically stopping arbitrary handler
  code is a cooperative contract with the handler author, not something
  TaskForge can enforce unilaterally.
- **Still no retries, idempotency keys, scheduling, or cancellation** —
  unchanged from Phase 1, see "Not implemented yet" above. (Retries:
  closed in Phase 3. Idempotency keys: closed in Phase 4. See those
  phases' sections below.)
- **No concurrency hardening at scale.** Correctness under N *real*
  concurrent workers is proven (this phase's tests use 8 concurrent
  goroutines against small job pools), but sustained load with tens of
  workers and thousands of jobs, and randomized/overlapping crash
  injection, is Phase 5 scope.
- **No production readiness claim of any kind.**

### Proposed maturity label

[docs/roadmap.md](docs/roadmap.md) sets Phase 2's maturity as "Experimental
→ Hardening once SF-002/003/006/007/008 pass consistently under **repeated
CI runs**." Every scenario those invariants require has a passing,
deterministic test today, and the concurrency/timing-sensitive tests among
them were run repeatedly (3+ consecutive local runs, plus `-race`) without
a single flake during this phase's implementation — but "repeated CI runs"
means exactly that: repeated runs of *this project's actual CI pipeline*
over time, which has not yet happened for this code (it has not been
merged or pushed). This README therefore keeps Phase 2 at **Experimental**
for now; promotion to Hardening is a mechanical follow-up once CI has run
this suite repeatedly post-merge, not a judgment call this document should
make preemptively on the implementer's own local test runs.

## Phase 3: What's Implemented

**Status: Experimental**, for the same reason Phase 2 is: see "Proposed
maturity label" below.

Phase 3 ([docs/roadmap.md](docs/roadmap.md) "Retries, Backoff, DLQ") makes
retries durable, real, and correctly bounded — replacing Phase 1/2's
simplification ("every failure dead-letters immediately") with the full
`RUNNING -> RETRY_WAIT -> RUNNING` cycle and genuine
retryable-vs-permanent failure classification.

### Implemented now

- **Retryable-vs-permanent failure classification in the handler contract**
  (`internal/handler`): a `Handler` reports failure by returning an error
  wrapped with `handler.Retryable(err)` or `handler.Permanent(err)`.
  `handler.Classify` extracts the classification (walking any further
  `fmt.Errorf("...: %w", ...)` wrapping a handler adds on top, per
  [docs/retry-semantics.md](docs/retry-semantics.md)). TaskForge never
  infers retryability from string matching or error type — an error a
  handler did not explicitly classify is treated as **permanent** (dead-letters
  immediately, consuming no further retry budget), which is also exactly
  Phase 1/2's original behavior for every existing handler, so no
  pre-Phase-3 handler's behavior changed.
- **Durable exponential backoff with equal jitter** (`internal/retry`), per
  [docs/retry-semantics.md](docs/retry-semantics.md)'s exact formula:
  `raw_delay = min(max_backoff, base_delay * 2^(attempt-1))`, then
  `delay = (raw_delay/2) + random_uniform(0, raw_delay/2)`. Pure and
  DB-free (unit-tested without PostgreSQL — `internal/retry/backoff_test.go`),
  overflow-safe at extreme attempt counts, and its jitter source is
  injectable for deterministic tests. The computed **duration** (not an
  absolute timestamp) is what crosses into `internal/store`, which applies
  it as `eligible_at = now() + delay` with `now()` evaluated by PostgreSQL
  itself — per [docs/failure-model.md](docs/failure-model.md)'s Clock
  Model, no correctness-critical timestamp is ever compared against a
  worker's local clock.
- **`Store.CompleteRetryableFailure`** (`internal/store/retry.go`): the
  fenced `RUNNING -> RETRY_WAIT` / `RUNNING -> DEAD_LETTERED` transition
  from [docs/worker-protocol.md](docs/worker-protocol.md)'s "Retryable
  Failure" SQL, verbatim — the destination is chosen by a single SQL `CASE`
  comparing `attempt_count` to `max_attempts` inside the same fenced
  `UPDATE` that performs the write (never a separate read-then-decide step
  that could race a concurrent change to either value), with the
  `job_attempts` row for that exact attempt finalized (`FAILED_RETRYABLE`)
  in the same transaction (TF-INV-013). `Store.CompleteFailure`
  (Phase 1/2, unchanged) now serves specifically as the **permanent**
  failure path — its unconditional dead-letter behavior already matched
  [docs/worker-protocol.md](docs/worker-protocol.md)'s "Permanent Failure"
  section exactly, so it required no change.
- **Worker integration** (`internal/worker`): `Worker.RunOnce` classifies a
  handler's failure and calls `CompleteRetryableFailure` or
  `CompleteFailure` accordingly, logging `retry scheduled` (with the
  computed `eligible_at`), `retries exhausted`, or `permanent failure`. A
  worker that has lost its lease mid-execution (detected via the existing
  Phase 2 heartbeat mechanism) does not attempt either call — a stale
  generation is no more authoritative for scheduling a retry or a
  dead-letter than it is for reporting success (TF-INV-003/014, unchanged
  from Phase 2, now proven for the two new call paths too). No handler
  execution ever runs inside a database transaction, and the heartbeat
  goroutine's lifecycle (started/stopped deterministically around handler
  execution, no leaks) is unchanged.
- **Retry eligibility reuses the existing claim query unmodified**: a
  `RETRY_WAIT` job becomes claimable via the exact same
  `state IN ('QUEUED', 'RETRY_WAIT') AND eligible_at <= now()` predicate
  `QUEUED` jobs already used (`internal/store/claim.go`, unchanged since
  Phase 1) — retry backoff needed no new query, no new index, and no
  special-casing in the claim path, exactly as
  [docs/architecture.md](docs/architecture.md) and
  [docs/worker-protocol.md](docs/worker-protocol.md) designed it.
- **No new migration required.** Migration `0001` already permitted the
  `RETRY_WAIT` state and an `eligible_at`/`last_error_class` column with no
  restrictive `CHECK`, and migration `0002` already permitted the
  `FAILED_RETRYABLE` `job_attempts` outcome — both deliberately, per each
  migration's own comments, specifically to avoid a schema migration when
  Phase 3 arrived. `internal/store/retry_test.go`'s
  `TestSchema_RetryWaitStateAlreadySupportedByCheckConstraint` and
  `TestSchema_FailedRetryableOutcomeAlreadySupportedByCheckConstraint`
  prove this is tested, not merely asserted in prose. Migration history
  (`migrations/0001_*`, `migrations/0002_*`) is untouched.
- Structured logs for every event this phase's design requires observable:
  retry scheduled (with attempt number and computed `eligible_at`),
  permanent failure, retries exhausted, and dead-letter transition
  (`internal/worker`) — no payload contents are ever logged.

### Not implemented yet

- ~~Idempotency-Key submission support (Phase 4).~~ **Closed in Phase 4**
  — see "Phase 4: What's Implemented" below.
- ~~Scheduling delay, cancellation, and a real `execution_timeout`
  distinct from lease TTL (Phase 6).~~ **Closed in Phase 6** — see
  "Phase 6: What's Implemented" below.
- Workflow/DAG execution (Phase 7), full metrics/tracing observability
  (Phase 8), sustained chaos/load testing (Phase 5/9).
- Per-job-type backoff configuration — [docs/retry-semantics.md](docs/retry-semantics.md)
  and [docs/roadmap.md](docs/roadmap.md) both name this an explicitly
  deferred open question; v1 has exactly one, global `retry.Config` per
  worker (`Worker.SetRetryConfig`), not a per-`job_type` override.
  Production callers use the documented v1 defaults
  (`retry.DefaultConfig()`: 1s base delay, 300s cap) unless they construct
  a `Worker` with different values.
  In-process `Worker.SetRetryConfig`/`SetRandSource` overrides exist
  primarily so tests can use a fast, deterministic backoff window; there is
  no environment-variable or API surface for tuning this yet.
- A standalone, periodic background sweeper process. Phase 2's Lazy
  Dead-Letter Sweep (runs inside every `Claim` call, before the claim
  query itself) already keeps `attempt_count <= max_attempts` true on the
  reclaim path (TF-INV-006); [docs/architecture.md](docs/architecture.md)
  and [docs/roadmap.md](docs/roadmap.md) both describe a periodic sweeper
  as an optional promptness *optimization*, not a correctness requirement
  — building one was deliberately out of this phase's scope (see
  "Explicit Phase 4+ deferrals" implications below; this is really a
  Phase-3-optional item, not deferred to a numbered later phase).
- A manual `POST /jobs/{id}/retry` (DLQ replay) endpoint — documented as a
  deferred endpoint in [docs/worker-protocol.md](docs/worker-protocol.md)
  and not required by Phase 3's scope; a `DEAD_LETTERED` job remains
  terminal with no automatic or manual-via-API path back to execution in
  this codebase today.
- Handler panics are not caught, classified, or retried specially — an
  unrecovered panic during handler execution crashes the worker process
  exactly as it did in Phase 1/2, which degrades to an ordinary crash (F1/F2:
  the lease expires, and Phase 2's existing reclaim mechanism — not the new
  `RETRY_WAIT` path — picks the job back up under a new generation). No
  authoritative doc specifies catching handler panics, so Phase 3
  deliberately does not add a `recover()` that would turn a panic into a
  classified retryable/permanent outcome.

### How to run a retry demonstration

```sh
docker compose up -d
cp .env.example .env && set -a && source .env && set +a
go run ./cmd/api &
go run ./cmd/worker &

# demo.flaky (registered in cmd/worker) reports a retryable failure on
# every attempt before attempt_count reaches 3, then succeeds. A short
# execution_timeout_seconds is irrelevant here (this handler returns
# immediately); the interesting part is the delay BETWEEN attempts, which
# is real, computed backoff -- not a fixed retry interval.
curl -X POST localhost:8080/jobs \
  -d '{"job_type":"demo.flaky","payload":{},"max_attempts":5}'

# Poll -- the state cycles QUEUED -> RUNNING -> RETRY_WAIT -> RUNNING ->
# RETRY_WAIT -> RUNNING -> SUCCEEDED, attempt_count reaching 3 at success.
# eligible_at on each RETRY_WAIT response shows exactly when the next
# attempt becomes claimable (base delay 1s, so this completes in a few
# seconds total).
curl localhost:8080/jobs/<id-from-above>
```

To see exhaustion instead, submit with `"max_attempts":2` (fewer than
`demo.flaky`'s built-in 3-attempt failure threshold): the job reaches
`DEAD_LETTERED` after its 2nd attempt, with `last_error`/`last_error_class`
(`"RETRYABLE"`) populated from that final attempt — `GET /jobs/{id}` never
needs a separate endpoint to see this; the existing response fields already
carry it.

### How to inspect attempt history

There is still no `GET /jobs/{id}/history` endpoint (deliberately deferred,
per [docs/worker-protocol.md](docs/worker-protocol.md) "Deferred
Endpoints" — unchanged by Phase 3). Inspect `job_attempts` directly against
the database for now:

```sh
psql "$TASKFORGE_DATABASE_URL" -c \
  "SELECT attempt_number, lease_generation, worker_id, started_at, finished_at, outcome, error_class, error_message
   FROM job_attempts WHERE job_id = '<id>' ORDER BY attempt_number;"
```

Every attempt — including ones superseded by reclaim (`LEASE_EXPIRED`) and
ones that failed retryably before an eventual success or dead-letter — is
preserved and queryable, even after the job reaches a terminal state
(TF-INV-007, TF-INV-009).

### How to run tests

```sh
make test        # go test -p 1 ./...
make test-race   # go test -race -p 1 ./...
```

Same PostgreSQL requirement and `-p 1` constraint as Phase 1/2. New in
Phase 3: `internal/retry/backoff_test.go` (pure unit tests, no database at
all) and `internal/store/retry_test.go` /
`internal/worker/retry_test.go` (real PostgreSQL integration tests,
including a randomized property test and several adversarial
stale-generation/rollback scenarios) — all part of the normal suite, no
extra tooling or command required.

### Phase 3 guarantees

- **TF-INV-006** (retry attempts respect configured limits), now proven on
  the *explicit failure-report* path (Phase 2 only proved it on the
  reclaim/sweep path): `TestCompleteRetryableFailure_SF010_ExhaustionTransitionsToDeadLettered`,
  `TestCompleteRetryableFailure_ExhaustionExactlyAtMaxAttemptsBoundary`
  (the `max_attempts=1` boundary — exhaustion on the very first failure),
  and `TestProperty_AttemptCountNeverExceedsMaxAttempts` (a randomized
  property test across many `max_attempts` values and failure sequences,
  per [docs/roadmap.md](docs/roadmap.md)'s Phase 3 quality gate).
- **TF-INV-007** (attempt history durable and monotonic), extended to
  `FAILED_RETRYABLE` outcomes: `TestCompleteRetryableFailure_SF009_EventuallySucceeds`,
  `TestCompleteRetryableFailure_SF010_ExhaustionTransitionsToDeadLettered`
  (all `job_attempts` rows present, in order, after dead-lettering).
- **TF-INV-009** (dead-lettering preserves failure history), now proven for
  exhaustion-driven dead-lettering specifically (Phase 1/2 only proved it
  for the unconditional/permanent path):
  `TestCompleteRetryableFailure_AttemptOutcomeAndErrorClassRecorded` (the
  job-level `last_error_class = 'RETRYABLE'` even though exhaustion, not
  permanence, caused the dead-letter — the exact documented distinction
  from `CompleteFailure`'s `'PERMANENT'`).
- **TF-INV-003 / TF-INV-014**, extended to the new retry/dead-letter
  transition: `TestCompleteRetryableFailure_RejectsStaleGeneration`,
  `TestCompleteRetryableFailure_RejectsWrongOwner`,
  `TestCompleteRetryableFailure_RejectedAfterReclaim` (the SF-008 shape,
  now for a retryable-failure report), `TestCompleteFailure_PermanentRejectedAfterReclaim`
  (same, for a permanent failure), and the worker-loop-level
  `TestRunOnce_RetryableFailureRejectedAfterLeaseLoss`.
- **TF-INV-011**'s mechanism (eligibility evaluated by PostgreSQL's own
  clock, never a worker's), applied to `RETRY_WAIT`:
  `TestClaim_RetryWaitNotClaimableBeforeEligibility`,
  `TestClaim_RetryWaitClaimableAfterEligibility`.
- **TF-INV-002**, extended to concurrent claims of an eligible `RETRY_WAIT`
  job: `TestClaim_ConcurrentWorkersRaceForEligibleRetryWaitJob` (the SF-006
  shape, replayed from `RETRY_WAIT`), `TestClaim_MultipleWorkersOnlyOneWinsOnceEligible`.
- **TF-INV-013**, extended to the retry transition's two-statement
  transaction: `TestRetryTransition_RollbackLeavesJobAndAttemptConsistent`
  (forces a rollback between the job-row UPDATE and the `job_attempts`
  finalize UPDATE, asserts both rows are left completely unchanged).
- **TF-INV-004**, proven unaffected by the new retry machinery:
  `TestClaim_ReclaimStillWorksAfterPriorRetryCycle` (Phase 2's lease-expiry
  reclaim still works correctly on the second and later attempt of a job
  that has already been through one `RETRY_WAIT` cycle) and
  `TestRetryWait_SurvivesFreshStoreInstance` (a `RETRY_WAIT` job's backoff
  window survives a simulated full process restart — no in-memory retry
  timer is ever the source of truth).
- Backoff correctness itself, independent of any database:
  `TestRawDelay_MatchesDocumentedFormula`, `TestRawDelay_CappedAtMaxBackoff`,
  `TestRawDelay_OverflowSafeAtExtremeAttempts` (adversarial case #12),
  `TestEqualJitter_BoundsAndDeterminism`, `TestConfig_Validate`.

### Phase 3 limitations (explicit, not hidden)

- **Still no submission idempotency, cancellation, scheduling delay, or
  workflow execution** — unchanged from Phase 1/2, see "Not implemented
  yet" above. (Submission idempotency: closed in Phase 4, see below.)
- **No per-job-type backoff configuration.** One global `retry.Config` per
  worker process; see "Not implemented yet" above.
- **No manual DLQ replay.** A `DEAD_LETTERED` job is genuinely terminal in
  this codebase — the only documented path back to execution
  ([docs/execution-semantics.md](docs/execution-semantics.md),
  [docs/worker-protocol.md](docs/worker-protocol.md)) is submitting a
  **new** job that references the old one, and even that is not wired up
  as an API endpoint yet.
- **No concurrency hardening at scale** — unchanged from Phase 2; Phase 3's
  new concurrency tests use the same small, fixed worker/job counts
  Phase 2's did, for the same reason (sustained-load hardening is Phase 5).
- **The at-least-once duplicate-side-effect window is unchanged, not
  widened.** A `RETRY_WAIT`-driven retry re-invokes the handler exactly
  like a Phase 2 reclaim-driven retry does — [ADR-0003](docs/adr/0003-at-least-once-execution-not-exactly-once.md)'s
  limitation (a crash after a side effect but before acknowledgement may
  cause that side effect to run again) applies identically whether the
  next attempt arrives via reclaim or via backed-off `RETRY_WAIT`. Phase 3
  does not change, worsen, or fix this — idempotency primitives for
  handlers remain Phase 4 scope.
- **No production readiness claim of any kind.**

### Proposed maturity label

[docs/roadmap.md](docs/roadmap.md) sets Phase 3's maturity as "Hardening"
on completion. Every invariant and scenario Phase 3's roadmap entry
requires (TF-INV-006, TF-INV-007, TF-INV-009; SF-009, SF-010) has passing,
deterministic test coverage today — including the specific "property test
confirming `attempt_count` never exceeds `max_attempts` across randomized
failure sequences" the roadmap's quality gate names explicitly — and the
full suite (`go test -race -p 1 ./...`) was run repeatedly (3+ consecutive
local runs) without a flake during this phase's implementation. But exactly
as Phase 2's own section above explains, "Hardening" per
[docs/roadmap.md](docs/roadmap.md)'s Maturity Labels definition requires
surviving **repeated CI runs** of this project's actual pipeline over time
— which, like Phase 2, has not yet happened for this code (not yet merged
or pushed). This README therefore keeps the overall project status at
**Experimental** for now, consistent with how Phase 2 was handled;
promotion to Hardening is a mechanical follow-up once CI has run this suite
repeatedly post-merge, not a judgment call made preemptively here.

## Phase 4: What's Implemented

**Status: Experimental**, for the same reason Phase 2 and Phase 3 are —
see "Proposed maturity label" below.

Phase 4 ([docs/roadmap.md](docs/roadmap.md) "Idempotency") makes duplicate
*submission* provably impossible within its documented scope, and makes
the pre-existing at-least-once *execution* limitation an executable,
demonstrated fact rather than only a documented one — while explicitly
proving, not just asserting, that TaskForge still does not and cannot
guarantee exactly-once execution of arbitrary side effects.

### Implemented now

- **`Idempotency-Key` request header on `POST /jobs`**
  (`internal/api.CreateJob`/`parseIdempotencyKey`): optional, read
  case-insensitively, trimmed, and validated (empty/whitespace-only is
  treated as "no key supplied," not an error; longer than
  `api.MaxIdempotencyKeyLength` — 255 characters, matching `job_type`'s
  existing bound — is rejected with `400 Bad Request`). See
  [docs/idempotency.md](docs/idempotency.md) "Implementation Notes (Phase
  4)" for the exact rules.
- **`Store.InsertIdempotent`** (`internal/store/idempotency.go`): the
  INSERT-first, no-check-then-act-read pattern [docs/idempotency.md](docs/idempotency.md)
  and [ADR-0004](docs/adr/0004-idempotency-for-exactly-once-effects.md)
  require — the INSERT (with `idempotency_key` set in the SAME statement
  as the rest of the row, never a separate mapping write) is attempted
  directly; a unique-constraint violation against
  `idx_jobs_idempotency_key` is caught and resolved by re-reading and
  returning the already-committed existing row, never by a preceding
  `SELECT`. `Store.Insert` (all pre-Phase-4 callers) is now a thin wrapper
  around this with `IdempotencyKey` left `nil`, so no existing behavior or
  call site changed. **No new migration was required** — migration
  0001's `idx_jobs_idempotency_key` unique index
  (`UNIQUE (job_type, idempotency_key) WHERE idempotency_key IS NOT NULL`)
  was deliberately created ahead of this phase specifically so it would
  need none, exactly as that migration's own comment says.
- **First-write-wins, no conflict detection, per the documented v1
  decision**: a retried submission under the same key with a *different*
  payload is not rejected, not flagged, and does not update the existing
  row — [docs/idempotency.md](docs/idempotency.md)'s Open Questions
  section decided this explicitly, and Phase 4 implements exactly that
  decision rather than inventing new conflict-detection behavior.
- **Duplicate submission after a terminal state**: resubmitting a key
  whose job has already reached `SUCCEEDED`/`CANCELLED`/`DEAD_LETTERED`
  returns that job's current terminal representation — `InsertIdempotent`
  never performs an `UPDATE`, so TF-INV-005 (terminal states never reopen)
  is untouched by idempotency logic entirely, by construction.
- **Execution-side idempotency identity**: no code change was needed here
  — `job_id` (`job.Job.ID`, already passed to every `Handler.Execute`
  call since Phase 1) and `attempt_number` (`job.Job.AttemptCount`) are
  already TaskForge's stable identifiers per
  [docs/idempotency.md](docs/idempotency.md). Phase 4's contribution is
  proving `job_id` is stable across a retry and across a reclaim
  (`internal/worker/idempotency_test.go`'s
  `TestIdempotencyIdentity_JobIDStableAcrossRetry`/`...AcrossReclaim`) and
  documenting/demonstrating the pattern, per
  [docs/roadmap.md](docs/roadmap.md)'s Phase 4 scope ("building a
  reference example job handler demonstrating the pattern").
- **The exactly-once LOGICAL EFFECT demonstration**
  (`internal/worker/idempotency_test.go`): a matched pair of tests
  reconstructing SF-004's exact crash point (side effect performed,
  worker crashes before any completion call, job reclaimed and retried).
  `TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice` proves
  a non-idempotent side-effect double runs twice — the documented
  at-least-once limitation, made executable rather than only asserted.
  `TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect`
  runs the identical crash sequence, but the handler's effect goes
  through a `job_id`-keyed dedup table (the "Deduplication token table"
  pattern from [docs/idempotency.md](docs/idempotency.md)) — the handler
  still runs twice (asserted explicitly), but the dedup table ends with
  exactly one row. **This is exactly-once logical effect via
  application-level idempotency built on TaskForge's stable identity — it
  is not, and must never be read as, a claim that TaskForge achieves
  exactly-once execution in general.**
- **Response body**: `GET`/`POST /jobs` responses now include
  `idempotency_key` (omitted when absent) alongside the fields already
  returned since Phase 1 — part of the durable row per
  [docs/data-model.md](docs/data-model.md), useful for a caller
  confirming what key TaskForge recorded.
- Structured logs for the two events this phase's design requires
  observable: `new idempotent submission` and `duplicate submission
  detected` (`internal/api`), logged only when an `Idempotency-Key` was
  actually supplied — no log line changes for ordinary (no-key)
  submissions.

### Not implemented yet

- **Conflicting-key-reuse rejection.** Per
  [docs/idempotency.md](docs/idempotency.md)'s Open Questions, this is a
  deliberate v1 non-goal, not an oversight: TaskForge does not compare a
  retried submission's payload against the original, and never reports a
  distinct "conflict" status for a mismatched retry under the same key.
- **Idempotency key TTL/expiry.** Undecided per
  [docs/idempotency.md](docs/idempotency.md); a key is bound to its job
  row forever in v1 (job rows are never deleted).
- ~~Scheduling delay, cancellation, and a real `execution_timeout`
  distinct from lease TTL (Phase 6).~~ **Closed in Phase 6** — see
  "Phase 6: What's Implemented" below.
- Workflow/DAG execution (Phase 7), full metrics/tracing observability
  (Phase 8), sustained chaos/load testing (Phase 5/9).
- Per-job-type or configurable idempotency-key validation rules (length
  bound, empty-header handling) beyond the fixed Phase 4 defaults above —
  not requested by any authoritative doc, so not built speculatively.

### How to demonstrate duplicate submission

```sh
docker compose up -d
cp .env.example .env && set -a && source .env && set +a
go run ./cmd/api &

# First submission with a key: creates a job.
curl -i -X POST localhost:8080/jobs \
  -H 'Idempotency-Key: order-42' \
  -d '{"job_type":"demo.echo","payload":{"order_id":42}}'
# -> 201 Created, a fresh job id

# "Response lost, client retries": the identical key, same or even a
# DIFFERENT payload -- TaskForge returns the SAME job, not a new one.
curl -i -X POST localhost:8080/jobs \
  -H 'Idempotency-Key: order-42' \
  -d '{"job_type":"demo.echo","payload":{"order_id":42,"anything":"else"}}'
# -> 201 Created, the SAME job id as above -- no second job was created
```

Fire the second `curl` many times concurrently (or with `xargs -P`) against
the same key to see `TestInsertIdempotent_ConcurrentDuplicateSubmissions_SF005`'s
guarantee live: every response carries the same job id.

### How to demonstrate exactly-once logical effect

There is no HTTP-visible demonstration of this one (it is about a
*handler's* internal behavior, not the API surface) — run the paired test
directly:

```sh
go test ./internal/worker/... -run TestSF004 -v
```

`TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice` shows the
side effect happening twice (documented at-least-once limitation);
`TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect`
shows the same crash sequence ending with exactly one durable effect row,
because the handler used `job_id` as a dedup token. Read both tests'
comments in `internal/worker/idempotency_test.go` for the full walkthrough
— they are also the answer to "how would I make my own handler's side
effects idempotent," per [docs/idempotency.md](docs/idempotency.md).

### How to run tests

```sh
make test        # go test -p 1 ./...
make test-race   # go test -race -p 1 ./...
```

Same PostgreSQL requirement and `-p 1` constraint as Phase 1/2/3. New in
Phase 4: `internal/store/idempotency_test.go` (including a 60-goroutine
concurrency test), `internal/api/handlers_integration_test.go`'s new
`TestCreateJob_IdempotencyKey_*` tests (including a 25-goroutine
HTTP-boundary concurrency test), and `internal/worker/idempotency_test.go`
— all part of the normal suite, no extra tooling or command required. The
concurrency-sensitive new tests were run repeatedly (3+ consecutive local
runs, plus `-race`) without a flake during this phase's implementation.

### Phase 4 guarantees

- **TF-INV-008** (an idempotency key never creates two logical jobs), now
  proven under real concurrency at two layers:
  `TestInsertIdempotent_ConcurrentDuplicateSubmissions_SF005` (60
  goroutines, direct store calls) and
  `TestCreateJob_IdempotencyKey_ConcurrentDuplicates_ExactlyOneJobCreated`
  (25 goroutines, real HTTP requests through the actual router) — both
  assert exactly one job row and that every caller observes the identical
  `job_id`. The sequential "submit, response lost, retry" case (this
  task's explicitly required failure case) is
  `TestInsertIdempotent_SequentialDuplicate_ReturnsExistingJob` /
  `TestCreateJob_IdempotencyKey_SequentialDuplicateReturnsSameJob`.
- **TF-INV-016** (idempotency uniqueness enforced by the database, not
  application logic): `TestSchema_IdempotencyKeyUniqueConstraint`
  (`internal/store/store_test.go`, pre-existing since Phase 1) proves the
  constraint itself exists; `isIdempotencyKeyViolation`
  (`internal/store/idempotency.go`) is the only code path that ever
  reconciles a duplicate, and it does so by inspecting the database's own
  `23505` unique-violation error against the specific index name, never by
  a `SELECT`-then-`INSERT` sequence that could reintroduce a TOCTOU race.
- **TF-INV-005** (terminal states never reopen), extended to idempotency:
  `TestInsertIdempotent_DuplicateSubmissionAfterDeadLetter_ReturnsExistingJob`
  proves a duplicate submission against a `DEAD_LETTERED` job's key
  returns that job's current state, never reopening or duplicating it.
- **TF-INV-013** (rollback never leaves a half-transitioned/half-mapped
  state), extended to submission idempotency:
  `TestInsertIdempotent_RollbackLeavesNoPartialIdempotencyState` proves a
  rolled-back INSERT leaves neither a job row nor an idempotency mapping
  — by construction, since both are the same single-statement write, not
  two writes that could diverge.
- **Durability across restart**, extended to idempotency:
  `TestInsertIdempotent_SurvivesFreshStoreInstance` and
  `TestCreateJob_IdempotencyKey_ProcessRestartThenDuplicateReturnsExistingJob`
  — a fresh `*store.Store`/`*api.Handlers`/`httptest.Server` sharing only
  the durable database (no in-memory idempotency cache exists anywhere in
  this codebase) still deduplicates correctly.
- **Execution-side identity stability**, the documented mechanism behind
  [ADR-0004](docs/adr/0004-idempotency-for-exactly-once-effects.md):
  `TestIdempotencyIdentity_JobIDStableAcrossReclaim` and
  `TestIdempotencyIdentity_JobIDStableAcrossRetry` prove `job_id` is
  byte-identical across a reclaim/retry, even as `lease_generation`/
  `attempt_count` change.
- **The exactly-once-logical-effect argument, proven not just argued**:
  `TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice` +
  `TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect`
  — see "Implemented now" above.
- Scope isolation (`TestInsertIdempotent_DifferentJobTypeSameKey_CreatesSeparateJobs`)
  and the documented first-write-wins decision
  (`TestInsertIdempotent_ConflictingPayloadSameKey_FirstWriteWins`) are
  both proven, not just described in docs.

### Phase 4 limitations (explicit, not hidden)

- **TaskForge still guarantees at-least-once execution, never
  exactly-once, for arbitrary side effects.** Phase 4 does not change this
  in any way — it only makes the boundary between "submission dedup
  TaskForge solves completely" and "side-effect dedup the handler author
  is responsible for" executable and explicit, per
  [ADR-0003](docs/adr/0003-at-least-once-execution-not-exactly-once.md)
  and [ADR-0004](docs/adr/0004-idempotency-for-exactly-once-effects.md).
  A handler that does not use `job_id` (or an equivalent downstream
  idempotency mechanism) can and will have its side effect performed more
  than once under the same crash/reclaim/retry conditions Phase 2/3
  already documented.
- **No conflicting-payload detection or rejection** — a deliberate v1
  decision (see "Not implemented yet" above), not a gap.
- **No idempotency key TTL/expiry.**
- **Still no scheduling, cancellation, or workflow execution** — unchanged
  from Phase 1–3, see "Not implemented yet" above. (Scheduling and
  cancellation: closed in Phase 6, see below.)
- **No production readiness claim of any kind.**

### Proposed maturity label

[docs/roadmap.md](docs/roadmap.md) sets Phase 4's maturity as "Hardening"
on completion. Every invariant and scenario Phase 4's roadmap entry
requires (TF-INV-008, TF-INV-016; SF-005, and the "documents the
limitation" half of SF-004) has passing, deterministic test coverage
today — including the specific "concurrency test with 50+ simultaneous
duplicate-key submissions consistently yields exactly one job row" quality
gate the roadmap names explicitly (this implementation uses 60 at the
store level and 25 at the HTTP level) — and the full suite
(`go test -race -p 1 ./...`) was run repeatedly (3+ consecutive local
runs, including the concurrency-sensitive new tests specifically) without
a single flake during this phase's implementation. But exactly as Phase
2/3's own sections explain, "Hardening" per
[docs/roadmap.md](docs/roadmap.md)'s Maturity Labels definition requires
surviving **repeated CI runs** of this project's actual pipeline over time
— which, like Phase 2/3, has not yet happened for this code (not yet
merged or pushed). This README therefore keeps the overall project status
at **Experimental** for now, consistent with how Phase 2/3 were handled;
promotion to Hardening is a mechanical follow-up once CI has run this
suite repeatedly post-merge, not a judgment call made preemptively here.

## Phase 5: What's Implemented

Phase 5 ([docs/roadmap.md](docs/roadmap.md) "Concurrency Hardening") is
explicitly a proving phase, not a feature phase: its non-goal is "New
features — this phase is about proving Phase 1–4 guarantees hold under
load and adversarial timing, not adding capability." Consistent with that,
**no product code changed in this phase** — `internal/store` and
`internal/worker` are byte-for-byte what Phase 4 left them. What Phase 5
adds is two new test files exercising the existing claim/lease/retry/
idempotency mechanisms under real, sustained, multi-worker PostgreSQL
contention at a scale Phase 1–4's tests (small, hand-crafted two/three-
worker scenarios) did not attempt.

### Implemented now

- **Store-level stress suite** (`internal/store/concurrency_stress_test.go`):
  - `TestStress_SF006_ManyWorkersManyJobs_NoDoubleClaimNoGenerationReuse` —
    25 workers, 300 jobs, all released from one barrier so first-claim
    attempts genuinely overlap.
  - `TestStress_ManyWorkersRaceForOneJob` — 20 workers race a single job
    (adversarial case "N workers race for one job").
  - `TestStress_ClaimContention_JobToWorkerRatios` — fewer jobs than
    workers, more jobs than workers, and a roughly-equal case, table-driven.
  - `TestStress_SF007_SustainedConcurrentReclaimOfManyExpiredLeases` — 120
    jobs claimed and abandoned (simulated crash), all leases force-expired
    at once, 20 workers reclaim concurrently.
  - `TestStress_SF008_FencingHoldsUnderSustainedConcurrentCompletionAttempts`
    — 40 jobs each advanced through 4 generations; every stale generation's
    completion call and the one legitimate completion call are fired
    concurrently, all at once, for every job simultaneously (~160
    concurrent completion attempts against a connection-pool-bounded
    `*sql.DB`).
  - `TestStress_ConcurrentWorkersRaceForManyEligibleRetryWaitJobs` — 60
    `RETRY_WAIT` jobs become eligible simultaneously (DB-time
    fast-forwarded, never a sleep), 15 workers race for them.
  - `TestStress_ConcurrentClaimPressureWithTerminalJobsPresent` — 80
    already-terminal jobs (with adversarially long-expired lease
    timestamps left on the row) sit alongside 80 genuinely claimable jobs
    under 20-worker claim pressure; no terminal job is ever claimed.
  - `TestStress_ConcurrentSweepOfManyExhaustedExpiredLeases_NoDoubleDeadLetter`
    — 60 attempt-exhausted, lease-expired jobs swept concurrently by 15
    claimers; proves the Lazy Dead-Letter Sweep's lack of `SKIP LOCKED`
    (see "Concurrency model and locking strategy" below) causes lock
    waiting, not errors, deadlocks, or double dead-lettering.
  - `TestStress_ClaimProgressesUnderConstrainedConnectionPool` — 15 worker
    goroutines draining 60 jobs against a `*sql.DB` deliberately capped at
    3 open connections; proves claim/complete's single-short-transaction
    design degrades to queuing under pool pressure, never deadlock.
  - `TestStress_RandomizedCrashRetrySucceed_RepeatedSeeds_InvariantsHold` —
    a bounded, round-based, deterministically-synchronized simulation (5
    fixed seeds, 80 jobs, 12 workers/round, up to 30 rounds) where each
    claimed job is randomly resolved as success, retryable failure, or a
    simulated crash (abandoned, then force-expired for the next round);
    every seed's run is checked for full convergence to a terminal state,
    `attempt_count <= max_attempts`, gapless/monotonic `job_attempts`
    history, and strictly increasing `lease_generation` per job.
- **Worker-loop-level stress suite** (`internal/worker/concurrency_stress_test.go`):
  - `TestStress_ManyWorkersProcessLargeMixedJobPool` — 20 real
    `*worker.Worker` instances against 200 jobs with a realistic outcome
    mix (always-succeed, retry-then-succeed, always-permanently-fail);
    asserts exactly-once execution, correct terminal state per job, and
    `job_attempts` row counts matching `attempt_count` for every job.
  - `TestStress_ManyConcurrentLeaseLossRaces_NoStaleAuthoritativeCompletions`
    — 15 jobs each held open by a first wave of workers; all 15 leases
    force-expired and reclaimed concurrently by a second wave, which
    completes all 15 concurrently; only then are the first wave's handlers
    released to finish and attempt their (now-stale) completions — the
    worker-loop-level, many-jobs-at-once generalization of
    `TestRunOnce_LeaseLostDuringExecution_SkipsCompletion`.
  - `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
    — a 10-worker pool is shut down (context cancelled) while every worker
    is mid-execution and heartbeating; proves no heartbeat goroutine leaks,
    no worker fabricates a completion it lost the authority to make, every
    job is left cleanly `RUNNING` under its original lease (not corrupted),
    and every one of those jobs is still fully recoverable afterward via
    the normal lease-expiry/reclaim path.
- Every test above uses explicit synchronization (start barriers,
  `sync.WaitGroup`, DB-time manipulation via the existing
  `forceExpireLease`/`forceSetEligibleAt` helpers) — never a sleep-based
  race — per [docs/testing-strategy.md](docs/testing-strategy.md)'s
  determinism requirement.
- **Transaction-boundary audit** (no code change; confirms an existing
  property): every `internal/store` write remains a single short
  transaction (`Claim`, `CompleteSuccess`, `CompleteFailure`,
  `CompleteRetryableFailure`, `Heartbeat`), and the job handler always runs
  entirely outside any database transaction
  (`internal/worker.runWithHeartbeat` — heartbeats are their own separate
  short transactions issued while the handler runs, not nested inside a
  claim/completion transaction). This was true since Phase 2/3; Phase 5's
  contribution is re-confirming it under real concurrent load rather than
  by code inspection alone — a held lock or an accidentally
  handler-spanning transaction would have caused the stress suite above to
  time out or deadlock, and it did not, across repeated `-race` runs.
- **Connection-pool audit**: no `internal/store` or `internal/worker` code
  path holds a connection across a wait for another connection or across
  handler execution, so worker concurrency degrades to queuing (not
  deadlock) under a connection pool smaller than the worker count — proven
  by `TestStress_ClaimProgressesUnderConstrainedConnectionPool` and
  exercised incidentally by capping `TestStress_SF008_...`'s pool at 50
  connections for its ~160-way concurrent completion burst.
- **Adversarial audit findings during this phase's own test development**
  (both were bugs in the new *test* code, not in `internal/store` or
  `internal/worker`; both were fixed and the fixes are what ships here):
  - The initial randomized-chaos simulation shared one `math/rand.Rand`
    across concurrent worker goroutines; `math/rand.Rand` is not safe for
    concurrent use, and `go test -race` caught the resulting data race
    immediately. Fixed by switching to `internal/retry.Rand`, the
    mutex-guarded `RandSource` this codebase already uses for backoff
    jitter for exactly this reason.
  - The initial fencing-under-load test (`TestStress_SF008_...`) fired
    ~160 concurrent completion calls against an unbounded connection pool,
    which exceeded the test PostgreSQL instance's default
    `max_connections` under `-count=3` repeated runs. Fixed by capping that
    test's `*sql.DB` with `SetMaxOpenConns(50)` — a test-harness fix, not a
    product-code change, since `internal/store` itself never assumes an
    unbounded pool.

### Not implemented yet

- **No new product code or capability** — by design (Phase 5's explicit
  non-goal); everything the store/worker packages do is unchanged from
  Phase 4.
- **No sustained multi-hour chaos/load harness** — Phase 5's roadmap entry
  distinguishes "bounded, deterministic stress tests suitable for CI" (what
  this phase built) from the dedicated, longer-running chaos/load testing
  phase (Phase 9); this phase does not pull that forward.
- **No fabricated throughput/latency numbers.** The stress suite proves
  correctness under load, not performance — no benchmark claims are made
  here (Phase 9's quality gate is explicit that such numbers are "only
  published once actually measured").
- **No mathematically strict fairness guarantee.** The claim query's
  `ORDER BY priority DESC, eligible_at ASC` combined with `SKIP LOCKED`
  gives an approximate oldest/highest-priority-first ordering under
  contention, not a proven starvation-freedom bound; see "Fairness and
  starvation" below for what was actually checked.
- **Cancellation, scheduling, workflow execution** — unchanged from
  Phase 1–4, still Phase 6/7 work. (Scheduling and cancellation: closed
  in Phase 6, see below; workflow execution remains Phase 7.)

### Concurrency model and locking strategy (re-confirmed, not changed)

TaskForge's claim query (see [docs/worker-protocol.md](docs/worker-protocol.md))
uses `SELECT ... FOR UPDATE SKIP LOCKED` in its candidate subquery, so
concurrent claimers lock disjoint rows and never block waiting on each
other for the claim step itself — this is what Phase 5's `SF-006` stress
variants (25 workers/300 jobs, 20 workers/1 job) confirm holds at higher
concurrency than Phase 2's original tests exercised. The one step in the
claim transaction that does **not** use `SKIP LOCKED` is the Lazy
Dead-Letter Sweep (a plain `UPDATE ... WHERE state = 'RUNNING' AND
lease_expires_at < now() AND attempt_count >= max_attempts`), which can
cause concurrent claimers to briefly serialize on overlapping rows when
many attempt-exhausted, lease-expired jobs exist at once.
`TestStress_ConcurrentSweepOfManyExhaustedExpiredLeases_NoDoubleDeadLetter`
confirms this serialization is a latency consideration, not a correctness
one — no error, no deadlock, no double dead-lettering, across repeated
`-race` runs — and this phase makes no code change to it, since doing so
was not required to satisfy any TF-INV-* or SF-* requirement and would be
unrequested scope beyond what Phase 5's roadmap entry actually asks for.

### Fairness and starvation

[docs/roadmap.md](docs/roadmap.md)'s Phase 5 entry asks to "verify
contention behavior under `SKIP LOCKED` at higher concurrency (verifying
no lock convoy/starvation)," not to design or prove a fairness guarantee
TaskForge does not implement. What was checked:
`TestStress_ConcurrentClaimPressureWithTerminalJobsPresent` and the
job-to-worker-ratio tests confirm that a large pool of terminal or
irrelevant rows never prevents genuinely eligible jobs from being drained
by a concurrent worker pool, and the randomized-chaos simulation confirms
convergence to all-terminal within a bounded number of rounds across five
different seeds. No claim beyond that is made: TaskForge does not
implement or prove strict FIFO fairness across priorities/ages under
adversarial scheduling, and this is stated honestly here rather than
implied.

### How to run the Phase 5 stress suite

```sh
# The full new suite (store-level and worker-level), verbose:
go test -p 1 -run 'TestStress' -v ./internal/store/... ./internal/worker/...

# The same suite under the race detector, repeated 3x to catch flakes
# (this is what was actually run to validate this phase — see "Phase 5
# guarantees" below):
go test -race -p 1 -count=3 -run 'TestStress' ./internal/store/... ./internal/worker/...

# The full regression suite (Phase 1-5, everything):
go test -p 1 ./...
go test -race -p 1 ./...
```

### Phase 5 guarantees

- **TF-INV-002** (at most one valid lease per job) and **TF-INV-014**
  (stale generations fenced no matter how late or how many generations
  have advanced), re-proven under sustained load:
  `TestStress_SF006_ManyWorkersManyJobs_NoDoubleClaimNoGenerationReuse`,
  `TestStress_ManyWorkersRaceForOneJob`,
  `TestStress_ClaimContention_JobToWorkerRatios`,
  `TestStress_SF008_FencingHoldsUnderSustainedConcurrentCompletionAttempts`
  (40 jobs × 4 generations × concurrent completion attempts from every
  generation at once).
- **TF-INV-003** (a lost lease cannot authoritatively complete a job),
  re-proven at the worker-loop level with many simultaneous races (not
  just one hand-sequenced pair of workers):
  `TestStress_ManyConcurrentLeaseLossRaces_NoStaleAuthoritativeCompletions`.
- **TF-INV-004** (worker crashes cannot permanently strand a recoverable
  job), re-proven under sustained concurrent reclaim pressure and repeated
  randomized crash injection:
  `TestStress_SF007_SustainedConcurrentReclaimOfManyExpiredLeases`,
  `TestStress_RandomizedCrashRetrySucceed_RepeatedSeeds_InvariantsHold`,
  `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
  (recovery after a cooperative pool shutdown, not just a hard crash).
- **TF-INV-005/TF-INV-006/TF-INV-007/TF-INV-009** (terminal states never
  reopen; retry limits respected; attempt history gapless/monotonic;
  dead-lettering preserves failure history), all re-checked as end-of-run
  invariant assertions in
  `TestStress_RandomizedCrashRetrySucceed_RepeatedSeeds_InvariantsHold` and
  `TestStress_ManyWorkersProcessLargeMixedJobPool`, across many concurrently
  processed jobs rather than one hand-picked example each.
- **Quality gate from docs/roadmap.md** ("no job claimed twice, no stale
  completion accepted, no job stranded, across a sustained run ... with
  zero invariant violations"): met across this phase's full stress suite,
  run repeatedly (`-race`, `-count=3`) with zero failures and zero
  detected data races. The suite's cumulative scale across its ten new
  tests spans several thousand individual claim/complete/heartbeat
  operations against real PostgreSQL; no single test claims "thousands of
  jobs" in isolation, since [docs/roadmap.md](docs/roadmap.md)'s "e.g.,
  thousands of jobs, tens of workers" is stated as an illustrative scale,
  not a literal per-test minimum, and deliberately bounded per-test sizes
  are what keep this suite CI-suitable (per that phase's own "Load/Stress
  Boundary" guidance to prefer deterministic bounded tests over an
  unbounded load-test harness, which is Phase 9's job).
- The full pre-existing Phase 1–4 regression suite remains green,
  including under `-race`, with no test modified or weakened to
  accommodate this phase's additions.

### Phase 5 limitations (explicit, not hidden)

- **No new correctness guarantee was added** — Phase 5 re-verifies
  existing invariants under load; it does not extend TaskForge's
  guarantees beyond what Phase 1–4 already established. TaskForge still
  guarantees at-least-once execution, never exactly-once, exactly as
  before.
- **No sustained multi-hour or truly unbounded-scale chaos run** was
  performed — that is Phase 9's explicit scope, not Phase 5's; this
  phase's stress tests are deliberately bounded so they remain fast and
  deterministic enough for ordinary CI, per
  [docs/testing-strategy.md](docs/testing-strategy.md) and
  [docs/roadmap.md](docs/roadmap.md)'s own Phase 5/Phase 9 boundary.
- **No production connection-pool tuning guidance beyond what was
  tested** — `cmd/api`/`cmd/worker` still open a `*sql.DB` with Go's
  default pool settings (no `SetMaxOpenConns` call); Phase 5 proves the
  design does not *require* a large or unbounded pool to make progress,
  it does not prescribe a specific production pool size, which remains an
  operational tuning question outside this phase's scope.
- **No mathematically proven fairness/starvation bound** — see "Fairness
  and starvation" above.
- **Still no scheduling, cancellation, or workflow execution.** (Scheduling
  and cancellation: closed in Phase 6, see below.)
- **No production readiness claim of any kind.**

### Proposed maturity label

[docs/roadmap.md](docs/roadmap.md) sets Phase 5's maturity as "Stable (for
the single-job-engine core)" on completion, contingent on that document's
own quality gate: "no job claimed twice, no stale completion accepted, no
job stranded, across a sustained run ... with zero invariant violations."
That gate was met in this session's local runs — the full new stress suite,
plus the full pre-existing Phase 1–4 suite, passed repeatedly
(`go test -p 1 ./...`, then `go test -race -p 1 -count=1 ./...`, then the
new suite specifically under `-race -count=3`) with zero failures and zero
detected races. But exactly as Phase 2/3/4's own sections explain,
[docs/roadmap.md](docs/roadmap.md)'s Maturity Labels definition requires
surviving **repeated CI runs** of this project's actual pipeline over
time — which, like Phase 2/3/4, has not yet happened for this code (not yet
merged or pushed). This README therefore keeps the overall project status
at **Experimental** for now, consistent with how every prior phase was
handled; promotion to Stable is a mechanical follow-up once CI has run this
suite repeatedly post-merge, not a judgment call made preemptively here.

## Phase 6: What's Implemented

Phase 6 ([docs/roadmap.md](docs/roadmap.md) "Scheduling, Cancellation,
Timeouts") adds durable job scheduling, a `POST /jobs/{id}/cancel` endpoint
with deterministic cancel-vs-completion race resolution, and a real
execution-timeout ceiling distinct from lease duration — closing the three
gaps Phase 1–5 explicitly left open. All six job-level states from
[docs/execution-semantics.md](docs/execution-semantics.md) are now
reachable (`CANCELLED` was previously unreachable).

### Implemented now

- **Durable scheduled execution** (`internal/job.NewParams.ScheduledAt`,
  `internal/store.InsertIdempotent`): an optional `scheduled_at` field on
  `POST /jobs` is durably recorded as both the immutable `scheduled_at`
  audit column and the job's initial `eligible_at` (the live gating
  timestamp the claim query consults), in the same `INSERT` statement — no
  new schema, no new migration, no in-memory scheduler of any kind, per
  [docs/scheduling.md](docs/scheduling.md)'s "Scheduling Is a Column, Not a
  Service" design. The **exact same claim query** every other job uses is
  the only mechanism that ever makes a scheduled job eligible
  (TF-INV-011); a scheduled job durably survives an API server restart, a
  worker restart, or a full-fleet restart, because nothing about its
  eligibility ever depended on a running process (SF-013).
- **`POST /jobs/{id}/cancel`** (`internal/api.CancelJob`), per
  [docs/worker-protocol.md](docs/worker-protocol.md)'s documented
  contract exactly: a `QUEUED`/`RETRY_WAIT` job cancels directly and
  uncontested (`internal/store.CancelQueuedOrRetryWait`); a `RUNNING`
  job's cancellation is durably requested
  (`internal/store.RequestCancellation` sets `cancel_requested = true`,
  no lease fencing — this is a caller-facing request, not a worker
  action) and the response reports "requested, not yet confirmed"; an
  already-terminal job's cancellation is an idempotent no-op reporting
  the job's real terminal state. No check-then-act race window exists
  anywhere in this cascade — each step is its own fenced, conditional
  `UPDATE`.
- **Worker-side cancellation observation and acknowledgement**
  (`internal/worker.runWithHeartbeat`/`reportCancelled`,
  `internal/store.CompleteCancelled`): the worker's existing heartbeat
  loop (unchanged cadence since Phase 2) now also inspects the
  `cancel_requested` flag on every successful heartbeat's returned row.
  The instant it observes `true`, it cancels the handler's `context.Context`
  (so a cooperative handler — anything that checks `ctx.Done()`, which
  includes every handler shape this codebase's own test doubles use — can
  stop promptly) and, once the handler returns (however it returns),
  reports `CompleteCancelled` instead of whatever the handler itself
  returned. `CompleteCancelled` is fenced exactly like every other
  completion (`lease_owner`/`lease_generation`/`state='RUNNING'`, plus
  `cancel_requested = true`), so TF-INV-003/014 apply identically: a
  stale generation can never acknowledge a cancellation any more than it
  can report success.
- **TF-INV-010's race rule, made structural rather than special-cased**:
  `CompleteCancelled` and
  `CompleteSuccess`/`CompleteFailure`/`CompleteRetryableFailure`/`CompleteTimeout`
  all share the identical `WHERE ... state = 'RUNNING'` guard on the same
  row. PostgreSQL serializes concurrent `UPDATE`s to that row by
  construction, so whichever commits first wins and the other's guard no
  longer matches — no new locking, token, or negotiation mechanism was
  needed to satisfy "first durable write to commit wins."
- **A real `execution_timeout` ceiling, distinct from lease duration**
  (`internal/worker.runWithHeartbeat`): the handler's execution context is
  now `context.WithTimeout(ctx, execution_timeout_seconds)` — a **fixed**
  deadline set once at the start of the attempt and never renewed — while
  the **lease** (`lease_expires_at`) continues to be renewed by every
  successful heartbeat exactly as before. These are deliberately
  independent mechanisms that happen to share one configured duration and
  start time: a job can heartbeat forever and keep its lease valid, but it
  still cannot run past `execution_timeout_seconds`, closing the gap
  [docs/failure-model.md](docs/failure-model.md) F9 previously left open
  ("Yes, handled" was aspirational until this phase). When the deadline
  fires, the worker reports the new `internal/store.CompleteTimeout`
  outcome (`TIMED_OUT`), treated as retryable by default per
  [docs/retry-semantics.md](docs/retry-semantics.md) — scheduled for
  retry, or dead-lettered on exhaustion, via the exact same
  attempt-ceiling/backoff decision as any other retryable failure
  (`CompleteTimeout` shares its SQL with `CompleteRetryableFailure`,
  factored into a single `completeRetryableOutcome` helper —
  `internal/store/retry.go`).
- **Explicit, tested cooperative-cancellation and timeout limitation**:
  neither cancellation nor execution-timeout enforcement can physically
  terminate a handler that never checks `ctx.Done()`. This is proven, not
  merely asserted:
  `TestRunOnce_HandlerIgnoresCancellation_StillAcknowledgesCancelled` and
  `TestRunOnce_ExecutionTimeout_HandlerIgnoresContext_StillReportsTimeout`
  (`internal/worker`) both use a handler that deliberately ignores `ctx`
  and confirm the worker still correctly classifies and reports the
  durable outcome once the handler eventually returns — TaskForge detects
  and reports promptly; it does not and cannot forcibly kill arbitrary
  handler code.
- **No new migration required** — `scheduled_at`, `eligible_at`,
  `cancel_requested`, `cancel_requested_at`, and `terminal_at` have all
  existed on `jobs` since migration `0001`; `job_attempts`'s outcome
  `CHECK` constraint already permitted `'TIMED_OUT'` and `'CANCELLED'`
  since migration `0002` — both deliberately, per each migration's own
  comment, specifically so this phase would need none. Proven, not
  asserted: `TestSchema_TimedOutOutcomeAlreadySupportedByCheckConstraint`
  (`internal/store/timeout_test.go`).
- Structured logs for every new event this phase's design requires
  observable: cancellation observed via heartbeat, cancellation
  acknowledged, execution timeout exceeded, retry-after-timeout scheduled,
  and dead-letter-after-timeout-exhaustion (`internal/worker`) — no
  payload contents are ever logged.

### Not implemented yet

- **Workflow-level cancellation** — explicit Phase 6 non-goal per
  [docs/roadmap.md](docs/roadmap.md); Phase 7 scope.
- **A supervisory process for a heartbeating job whose worker never
  self-reports `TIMED_OUT`** — [docs/execution-semantics.md](docs/execution-semantics.md)
  documents this exact gap as an open question rather than papering over
  it: if a worker process's own execution-timeout enforcement somehow
  fails to fire (e.g. the worker process itself is wedged in a way that
  also prevents its own `context.WithTimeout` from being observed — an
  extremely narrow window, since Go's own runtime drives context
  cancellation independent of the handler), the job simply keeps
  heartbeating until an operator intervenes or the lease eventually
  expires. This is documented, not solved, per that document's explicit
  allowance.
- **No schedule-limit validation** (e.g. a maximum allowed `scheduled_at`
  distance into the future) — not required by any authoritative doc, so
  not built speculatively; a caller may schedule arbitrarily far ahead.
- **No priority-aware or per-job-type execution-timeout configuration
  surface** beyond the existing per-submission
  `execution_timeout_seconds` field — unchanged from Phase 1.
- **No `POST /jobs/{id}/reschedule`** or any mutation of an already-durable
  `scheduled_at`/`eligible_at` beyond what claim/retry/cancellation already
  do — not documented, not built.

### How to demonstrate scheduling

```sh
docker compose up -d
cp .env.example .env && set -a && source .env && set +a
go run ./cmd/api &
go run ./cmd/worker &

# Schedule a job 2 minutes in the future.
curl -X POST localhost:8080/jobs \
  -d '{"job_type":"demo.echo","payload":{},"scheduled_at":"'"$(date -u -d '+2 minutes' +%Y-%m-%dT%H:%M:%SZ)"'"}'

# GET immediately: state is QUEUED, eligible_at matches scheduled_at, and
# no worker claims it yet no matter how long you poll before that time.
curl localhost:8080/jobs/<id-from-above>

# After 2 minutes, the exact same worker loop -- no restart, no special
# action -- claims and executes it normally.
```

### How to demonstrate cancellation

```sh
# Cancel a job that hasn't been claimed yet -- immediate CANCELLED.
curl -X POST localhost:8080/jobs \
  -d '{"job_type":"demo.echo","payload":{}}'
curl -X POST localhost:8080/jobs/<id>/cancel   # -> state: CANCELLED

# Cancel a RUNNING job -- request is durable but not yet confirmed.
curl -X POST localhost:8080/jobs \
  -d '{"job_type":"demo.echo","payload":{},"execution_timeout_seconds":10}'
# (claim it with a worker, then, while it is executing:)
curl -X POST localhost:8080/jobs/<id>/cancel   # -> state: RUNNING, cancel_requested: true
# Poll GET /jobs/<id> -- once the worker's next heartbeat observes the
# flag, the job transitions to CANCELLED (or, if the handler finished
# first, whatever it legitimately completed as -- see TF-INV-010).
curl localhost:8080/jobs/<id>
```

### How to run tests

```sh
make test        # go test -p 1 ./...
make test-race   # go test -race -p 1 ./...

# The new Phase 6 tests specifically, verbose:
go test -p 1 -v ./internal/store/... -run 'Sched|Cancel|Timeout|SF012'
go test -p 1 -v ./internal/worker/... -run 'Cancel|ExecutionTimeout'
go test -p 1 -v ./internal/api/...    -run 'ScheduledAt|CancelJob'

# Repeated, race-detected runs of the race-sensitive new tests (this is
# what this phase's own quality gate requires, and what was actually run
# to validate it -- see "Phase 6 guarantees" below):
go test -race -count=3 ./internal/store/...  -run 'SF012|Cancel|Timeout|Sched'
go test -race -count=3 ./internal/worker/... -run 'Cancel|ExecutionTimeout|LongRunningJob'
```

Same PostgreSQL requirement and `-p 1` constraint as every prior phase. No
new tooling or command is required beyond what Phase 1–5 already
established.

### Phase 6 guarantees

- **TF-INV-010** (cancellation race is deterministic): both forced
  interleavings proven directly —
  `TestSF012_CancelCommitsFirst_CompletionRejected` and
  `TestSF012_CompletionCommitsFirst_CancellationRejected`
  (`internal/store/cancellation_test.go`), plus the extended races this
  task's adversarial-audit list requires:
  `TestSF012_CancelRacesRetryableFailure_FirstCommitWins`,
  `TestSF012_CancelRacesDeadLetter_FirstCommitWins`, and the timeout
  analogue `TestCompleteTimeout_RacesCancellation_FirstCommitWins`
  (`internal/store/timeout_test.go`). Every one of these calls the
  competing completion methods in a controlled, deterministic order (each
  Store completion call is a single, immediately-committed transaction,
  so sequential calls directly force each documented interleaving — no
  sleep, no flaky timing dependency), run repeatedly under `-race` with
  zero flakes.
- **TF-INV-011** (scheduled jobs don't run early): proven with real
  PostgreSQL, never a wall-clock sleep —
  `TestInsertIdempotent_FutureScheduledAt_DurablyRecordedAndNotClaimable`,
  `TestInsertIdempotent_ScheduledJob_ClaimableAfterEligibilityArrives`
  (DB-time manipulation via the new `forceSetScheduledEligibility`
  helper, mirroring Phase 3's `forceSetEligibleAt`), and
  `TestInsertIdempotent_ScheduledJob_ManyWorkersRaceAtEligibility` (20
  real goroutines racing the instant a scheduled job becomes eligible —
  the scheduling analogue of SF-006).
- **SF-013** (scheduled job survives restart):
  `TestInsertIdempotent_ScheduledJob_SurvivesFreshStoreInstance` — a
  fresh `*store.Store` sharing only the database still correctly refuses
  to claim before eligibility and claims correctly after, proving no
  in-memory scheduler state exists anywhere to lose.
- **TF-INV-005**, extended to the new `CANCELLED` reachability path and to
  scheduling: `TestCancelQueuedOrRetryWait_AlreadyTerminal_RejectedAsStale`,
  `TestCancelQueuedOrRetryWait_DuplicateOnAlreadyCancelled_RejectedAsStale`,
  and `TestInsertIdempotent_ScheduledJob_CancelledBeforeEligibility_NeverExecutes`
  (a cancelled scheduled job never becomes claimable even once its
  original `eligible_at` passes).
- **TF-INV-003/014**, extended to the cancellation-acknowledgement and
  timeout completion paths: `TestCompleteCancelled_RejectsStaleGeneration`,
  `TestCompleteTimeout_RejectsStaleGeneration`, and the worker-loop-level
  `TestRunOnce_LeaseLostTakesPriorityOverPendingCancellation` /
  `TestRunOnce_ExecutionTimeout_FiresAfterLeaseAlreadyLost` (adversarial
  case #13, "timeout fires after worker lost ownership") — a stale
  generation can never override cancellation, and a stale generation's
  late timeout report is rejected exactly like any other stale
  completion.
- **Cancellation cannot bypass retry limits or reopen a dead-lettered
  job**: `TestSF012_CancelRacesRetryableFailure_FirstCommitWins`,
  `TestSF012_CancelRacesDeadLetter_FirstCommitWins`, and
  `TestCancelQueuedOrRetryWait_RetryWait_NeverBecomesClaimableAfterEligibilityArrives`
  (a cancelled `RETRY_WAIT` job never later executes just because its
  already-scheduled retry `eligible_at` arrives — the exact
  "cancelled → retry eligible → executes again" sequence this phase's
  requirements explicitly forbid, disproven directly).
- **Cancellation-vs-reclaim race**:
  `TestCancelRacesReclaim_StaleGenerationCannotAcknowledge` — a
  cancellation requested against a generation that is then reclaimed
  before acknowledgement leaves the reclaimed generation to proceed
  normally, with `cancel_requested` correctly reset (per
  [docs/worker-protocol.md](docs/worker-protocol.md)'s documented
  per-generation cancellation-intent decision), and the stale
  generation's late acknowledgement attempt rejected.
- **Idempotency preserved after cancellation** (Phase 4 guarantees,
  extended to the new `CANCELLED` terminal state specifically — Phase 4's
  own tests predated `CANCELLED` being reachable):
  `TestInsertIdempotent_DuplicateSubmissionAfterCancellation_ReturnsExistingJob`
  (store level) and
  `TestCreateJob_IdempotencyKey_DuplicateAfterCancellation_ReturnsExistingJob`
  (HTTP level) — a duplicate submission after cancellation returns the
  same, still-cancelled job, never a new one.
- **TF-INV-013**, extended to the two new completion paths:
  `TestCompleteCancelled_RollbackLeavesRowUnchanged`,
  `TestCompleteTimeout_RollbackLeavesRowUnchanged` — a forced
  mid-transaction failure leaves the row byte-for-byte unchanged.
- **Lease vs. execution-timeout are genuinely independent, proven, not
  just asserted**: `TestRunOnce_ExecutionTimeout_FiresIndependentlyOfLeaseRenewal`
  drives a cooperative handler past its `execution_timeout_seconds`
  ceiling while heartbeats keep renewing the lease throughout (the lease
  never expires, `lease_generation` never advances), and the timeout
  still fires — proving these are two distinct mechanisms sharing a
  configured duration, not the same check performed twice.
- **Multi-worker/concurrency hardening extended to Phase 6's new
  machinery** (`internal/store/phase6_stress_test.go`):
  `TestStress_CancellationSurvivesConcurrentCompletionAttemptsAcrossManyGenerations`
  (20 jobs, each driven through 3 stale generations plus cancellation
  under the current generation, then 260 concurrent completion attempts
  from every stale generation fired at once — every one rejected, every
  job remains `CANCELLED`) and
  `TestStress_ManyEligibleScheduledJobsClaimedExactlyOnceUnderPressure`
  (40 eligible scheduled jobs and 40 cancelled scheduled jobs claimed
  under 15-worker pressure — exactly the eligible ones are claimed,
  exactly once each, never a cancelled one).
- The full pre-existing Phase 1–5 regression suite remains green,
  including under `-race`, with exactly one test's *parameters* adjusted
  (not its assertions weakened) to account for the new execution-timeout
  ceiling — see "A note on `TestRunOnce_LongRunningJob_HeartbeatKeepsLeaseAlive`"
  below.

### A note on `TestRunOnce_LongRunningJob_HeartbeatKeepsLeaseAlive`

This Phase 2 test (SF-017: a long-running, heartbeating job completes
successfully across multiple lease-renewal cycles) used
`ExecutionTimeoutSeconds: 1` with the handler held open for 1200ms — a
choice that made sense before Phase 6 existed, because
`execution_timeout_seconds` was *only* the lease duration then; running
"past" it via heartbeat renewal, with no consequence, was exactly the
documented gap [docs/failure-model.md](docs/failure-model.md) F9 named as
not yet handled. Phase 6 closes that gap: `execution_timeout_seconds` is
now *also* a fixed, unrenewed per-attempt ceiling. Continuing to hold the
handler open for 1200ms against a 1-second ceiling would now correctly
trigger `TIMED_OUT`, which would silently invalidate this test's original
assertion (`SUCCEEDED`) for a reason unrelated to what the test actually
verifies (lease renewal across multiple heartbeat cycles). The test's
*parameters* were updated (`ExecutionTimeoutSeconds: 3`, held open for
2500ms — still crossing 2 heartbeat cycles, now safely under the new
ceiling); its *assertions* were not weakened in any way, and
`TestRunOnce_ExecutionTimeout_FiresIndependentlyOfLeaseRenewal`
(`internal/worker/timeout_test.go`) now directly proves the specific
behavior (timeout firing independent of successful lease renewal) that
would have made the old parameters fail.

### Phase 6 limitations (explicit, not hidden)

- **TaskForge can request cooperative cancellation; it cannot guarantee
  physical termination of arbitrary handler code.** A handler that never
  checks `ctx.Done()` will run to its own natural completion regardless of
  a cancellation request or an exceeded execution timeout — TaskForge
  detects and durably reports the outcome promptly once the handler
  returns, but cannot forcibly stop it mid-flight. If the handler already
  performed an external side effect before returning, that side effect is
  not undone, exactly as [docs/vision.md](docs/vision.md)'s core thesis
  states TaskForge never can.
- **Scheduling precision is bounded by worker poll interval and queue
  contention, not wall-clock-accurate timer firing** — unchanged from
  [docs/scheduling.md](docs/scheduling.md)'s documented, deliberate
  non-goal (no sub-second real-time scheduling guarantee).
- **No supervisory fallback if a worker process's own execution-timeout
  enforcement never fires** (an extremely narrow window — see "Not
  implemented yet" above) — the job remains `RUNNING` until lease expiry
  or operator intervention, per
  [docs/execution-semantics.md](docs/execution-semantics.md)'s explicitly
  documented open question.
- **No workflow-level cancellation** — Phase 7 scope, per this phase's
  explicit non-goal.
- **`cancel_requested` still resets on reclaim** (unchanged Phase 2
  decision, per [docs/worker-protocol.md](docs/worker-protocol.md)'s Open
  Questions): a cancellation requested against a generation that is then
  reclaimed (crash before acknowledgement) must be re-issued against the
  new generation if still desired — proven in
  `TestCancelRacesReclaim_StaleGenerationCannotAcknowledge`.
- **No production readiness claim of any kind.**

### Proposed maturity label

[docs/roadmap.md](docs/roadmap.md) sets Phase 6's maturity as "Hardening →
Stable once race tests are proven flake-free across repeated CI runs."
Every invariant and scenario this phase's roadmap entry requires
(TF-INV-010, TF-INV-011; SF-011, SF-012, SF-013) has passing, deterministic
test coverage today, including the specific quality gate
[docs/roadmap.md](docs/roadmap.md) names explicitly — "deterministic race
test (SF-012) passes under both forced interleavings, repeatably" — proven
via `TestSF012_CancelCommitsFirst_CompletionRejected` and
`TestSF012_CompletionCommitsFirst_CancellationRejected`, run 3+ consecutive
times under `-race` with zero flakes during this phase's implementation,
alongside the full pre-existing Phase 1–5 suite. But exactly as every
prior phase's own section explains, [docs/roadmap.md](docs/roadmap.md)'s
Maturity Labels definition requires surviving **repeated CI runs** of this
project's actual pipeline over time — which, like every prior phase, has
not yet happened for this code (not yet merged or pushed). This README
therefore keeps the overall project status at **Experimental** for now,
consistent with how every prior phase was handled; promotion to Stable is
a mechanical follow-up once CI has run this suite repeatedly post-merge,
not a judgment call made preemptively here.

## Documentation Map

| Document | Contents |
|---|---|
| [docs/vision.md](docs/vision.md) | What TaskForge is/is not, primary users, core reliability thesis, precise lifecycle terminology |
| [docs/architecture.md](docs/architecture.md) | Component overview, why no broker/Redis/Kubernetes dependency |
| [docs/failure-model.md](docs/failure-model.md) | Every assumed failure mode, handled-in-v1 vs. out-of-scope |
| [docs/invariants.md](docs/invariants.md) | Numbered system invariants (TF-INV-001 through TF-INV-016), each with a violating example and enforcement mechanism |
| [docs/execution-semantics.md](docs/execution-semantics.md) | The authoritative job state machine and transition table |
| [docs/data-model.md](docs/data-model.md) | Conceptual PostgreSQL schema, constraints, indexing strategy |
| [docs/worker-protocol.md](docs/worker-protocol.md) | Claim/heartbeat/completion SQL, fencing mechanics, API contract |
| [docs/retry-semantics.md](docs/retry-semantics.md) | Attempt numbering, backoff policy, dead-letter transition, crash-timing analysis |
| [docs/idempotency.md](docs/idempotency.md) | Submission idempotency vs. execution/side-effect idempotency |
| [docs/scheduling.md](docs/scheduling.md) | Delayed/scheduled job semantics, clock model |
| [docs/workflows.md](docs/workflows.md) | DAG execution design (staged for a later phase) |
| [docs/observability.md](docs/observability.md) | Metrics, structured logs, trace boundaries |
| [docs/testing-strategy.md](docs/testing-strategy.md) | Test categories and the invariant-to-test matrix |
| [docs/scenario-corpus.md](docs/scenario-corpus.md) | 18 named, deterministic test scenarios (SF-001 through SF-018) |
| [docs/roadmap.md](docs/roadmap.md) | Phased implementation plan with entry/exit criteria per phase |
| [docs/adr/](docs/adr/README.md) | Architecture decision records — the real tradeoffs behind the design |

## Technology Direction

- **Go** as the implementation language (Phase 1 implemented; see
  `go.mod`).
- **PostgreSQL** as the sole durable source of truth — no additional
  infrastructure (Kafka, Redis, Kubernetes-specific tooling) unless a
  documented limitation of this design genuinely requires it. See
  [ADR-0001](docs/adr/0001-postgresql-as-source-of-truth.md) and
  [ADR-0006](docs/adr/0006-database-backed-queue-first.md).

## Contributing / Reviewing

This project is built to withstand skeptical review from backend,
platform, infrastructure/SRE, and distributed-systems engineers. If you are
reviewing this repository: the invariants in
[docs/invariants.md](docs/invariants.md) and the scenarios in
[docs/scenario-corpus.md](docs/scenario-corpus.md) are the intended target
for scrutiny — if you can find a sequence of events that violates one of
the stated invariants under the documented design, that is exactly the kind
of finding this documentation set exists to surface before implementation
begins.
