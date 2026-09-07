# TaskForge

**Status: Experimental (Phase 1 — Single-Node Durable Job Engine — and
Phase 2 — Worker Leases and Heartbeats — both complete).**
Phases 1 and 2 of [docs/roadmap.md](docs/roadmap.md) are implemented: a
durable PostgreSQL-backed job engine with HTTP submission, multiple
concurrent worker processes, fenced lease-based ownership with heartbeat
renewal and automatic crash recovery via lease expiration/reclaim, and the
`QUEUED -> RUNNING -> SUCCEEDED` / `RUNNING -> DEAD_LETTERED` subset of the
documented state machine. It is **not** distributed beyond a single
PostgreSQL instance, and does not implement retry backoff/DLQ
classification, cancellation, scheduling delay, or idempotency keys yet —
see "Phase 1: What's Implemented" and "Phase 2: What's Implemented" below
for the exact boundary. Everything else described in this README past those
sections remains a design target for later phases, not a demonstrated
capability. TaskForge guarantees **at-least-once** execution, never
exactly-once for arbitrary external side effects — see "Phase 2 guarantees"
below for the specific, tested duplicate-side-effect window this phase
closes and the one it deliberately leaves open.

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
  mechanism.**
- Automatic crash recovery via lease expiration — no permanently stranded
  jobs (TF-INV-004). **Proven (Phase 2).**
- Heartbeat renewal without transferring or shortening a lease
  (TF-INV-015). **Proven (Phase 2).**
- Durable, monotonic retry history with configurable backoff and dead-letter
  behavior (TF-INV-006, TF-INV-007, TF-INV-009). **Partially proven**: the
  `job_attempts` ledger exists and is populated (Phase 2), and TF-INV-006 is
  enforced on the reclaim path (an attempt-exhausted expired lease is
  dead-lettered, never re-reclaimed) — but real backoff/`RETRY_WAIT` and
  dead-letter failure classification are Phase 3.
- Database-enforced submission idempotency (TF-INV-008, TF-INV-016).
- Deterministic cancellation-vs-completion race semantics (TF-INV-010).
- Scheduling that survives full process/fleet restarts (TF-INV-011).
- (Later phase) dependency-gated workflow/DAG execution (TF-INV-012).

None of these are implemented yet. They are documented now, precisely,
so that implementation has an unambiguous contract to satisfy and external
reviewers have something falsifiable to check it against.

## Project Status

| Area | Status |
|---|---|
| Architecture & invariant documentation | **Done** (this repository, current state) |
| PostgreSQL schema | **Phase 2 done** (`jobs` table per Phase 1; `job_attempts` added in Phase 2, migration `0002_create_job_attempts_table`) |
| API server | **Phase 1 done** (`POST /jobs`, `GET /jobs/{id}` only — unchanged in Phase 2, per Phase 2's explicit non-goal of API expansion) |
| Worker / claim / lease protocol | **Phase 2 done**: multiple concurrent worker processes, lease expiration/reclaim, heartbeat renewal, fencing proven under real concurrent workers (not just the stale-credential mechanism) — see "Phase 2: What's Implemented" below |
| Retry / backoff / DLQ | Not started (failures still go straight to `DEAD_LETTERED`, no `RETRY_WAIT`; the reclaim path's Lazy Dead-Letter Sweep enforces `attempt_count <= max_attempts` as an infrastructure precondition, not a retry policy) |
| Idempotency enforcement | Not started (schema constraint exists; no `Idempotency-Key` API support) |
| Scheduling | Not started |
| Cancellation / timeouts | Not started (lease TTL bounds a stuck attempt, but there is no `execution_timeout` classification distinct from lease expiry yet, and no cancellation endpoint) |
| Workflow / DAG execution | Not started (staged for a later phase) |
| Observability | Not started beyond structured logs (claim, reclaim, heartbeat rejection, stale-completion rejection are all logged — see "Phase 2: What's Implemented") |
| Test suite (unit/integration/concurrency/chaos) | **Phase 2 subset done**: unit, state-machine table, PostgreSQL integration, and real multi-goroutine concurrency tests (claim races, reclaim races) now exist; sustained-load chaos testing remains Phase 5/9 |

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
- Idempotency-Key submission support (Phase 4) — the database unique
  constraint exists, but no API surface uses it yet.
- Scheduling, cancellation, and execution timeouts (Phase 6).
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
- **No duplicate-submission protection.** The `(job_type, idempotency_key)`
  unique constraint exists in the schema, but `POST /jobs` does not accept
  an `Idempotency-Key` yet.
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
- Idempotency-Key submission support (Phase 4).
- Scheduling delay, cancellation, and a real `execution_timeout`
  distinct from lease TTL (Phase 6) — lease expiration is the only timeout
  mechanism that exists.
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
  unchanged from Phase 1, see "Not implemented yet" above.
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
