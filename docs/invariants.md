# System Invariants

Status: foundational. This is the most important document in the
repository. Every implementation decision must be checked against this list.
Every invariant must have at least one test in
[testing-strategy.md](testing-strategy.md)'s invariant-to-test matrix.

Invariant IDs are stable once assigned and are never reused, even if an
invariant is later retired (a retired invariant is marked `RETIRED` with a
reason, not deleted, so historical references stay valid).

## Format

Each invariant documents:
- **Property**: the precise rule.
- **Why it matters**: what breaks if it is violated.
- **Violating example**: a concrete sequence of events that would break it.
- **Implementation mechanism**: what enforces it.
- **Test strategy**: how it is proven (cross-referenced to
  [testing-strategy.md](testing-strategy.md)).

---

### TF-INV-001 — Accepted jobs cannot disappear

**Property**: Once a job submission has been acknowledged to the caller (HTTP
2xx from `POST /jobs`), the job's durable row exists and will reach an
observable, queryable state at all times until a terminal state, with no gap
during which it is neither queued, leased, nor terminal.

**Why it matters**: A caller that received a success response must be able
to trust that the job is real. A system that acknowledges before persisting
can lose work silently — the worst possible failure mode for a job engine.

**Violating example**: API server returns 200 to the client, then crashes
before the INSERT transaction commits. The job never existed durably, but
the caller believes it was accepted.

**Implementation mechanism**: The HTTP response is only written after the
INSERT transaction commits (see [ADR-0006](adr/0006-database-backed-queue-first.md)).
There is no "accept now, persist later" path anywhere in the API server.

**Test strategy**: Integration test that kills the API process at the byte
level right after commit vs. right before, and asserts response is sent iff
commit succeeded. See [testing-strategy.md](testing-strategy.md) "process
restart tests."

---

### TF-INV-002 — At most one valid lease per job

**Property**: At any instant, at most one `(lease_owner, lease_generation)`
pair is considered valid ownership of a given job. `lease_generation` is
strictly increasing per job across successive claims.

**Why it matters**: Without this, two workers could both believe they own
the same job and both perform its side effects concurrently, or both write
conflicting completion records.

**Violating example**: Two workers both run `SELECT` to find eligible jobs,
both see the same row, and both issue an `UPDATE` claiming it, without a
mechanism to guarantee only one succeeds.

**Implementation mechanism**: Claim is a single atomic
`UPDATE ... WHERE state IN (...) AND ... RETURNING`, combined with
`SELECT ... FOR UPDATE SKIP LOCKED` in the candidate-selection subquery (see
[worker-protocol.md](worker-protocol.md)). PostgreSQL's row-level locking
guarantees only one transaction's UPDATE can affect a given row for a given
claim cycle.

**Test strategy**: Concurrency test — N workers hammering the same small
pool of jobs, assert `SUM(rows claimed) == COUNT(jobs)` and no
`lease_generation` value is issued twice for the same job. See
[testing-strategy.md](testing-strategy.md) "concurrency tests," scenario
SF-006.

---

### TF-INV-003 — A lost lease cannot authoritatively complete a job

**Property**: A worker whose lease is no longer the current one for a job
(because it expired and/or a new generation was issued) cannot cause any
durable state transition on that job.

**Why it matters**: This is what prevents "split-brain" completion — a slow
or paused worker finishing and reporting success/failure after another
worker has already taken over and possibly already completed the job
differently.

**Violating example**: Worker A holds generation 5, pauses (GC, VM suspend),
its lease expires, Worker B claims generation 6 and completes the job.
Worker A wakes up and calls "complete" with generation 5; if this succeeded,
it could overwrite Worker B's result or double-count a side effect
acknowledgement.

**Implementation mechanism**: Every completion/heartbeat write is a
conditional `UPDATE ... WHERE id = ? AND lease_owner = ? AND
lease_generation = ? AND state = 'RUNNING'`. If the affected row count is 0,
the caller is stale and the write is rejected. See
[worker-protocol.md](worker-protocol.md).

**Test strategy**: Deterministic fencing test — construct exactly the
sequence above and assert Worker A's completion call reports rejection. See
[scenario-corpus.md](scenario-corpus.md) SF-008.

---

### TF-INV-004 — Worker crashes cannot permanently strand a recoverable job

**Property**: Any job whose owning worker has crashed (silently stopped
heartbeating) will, within a bounded time (lease TTL), become eligible for
claiming by another worker, provided the job has not exhausted its retry
budget.

**Why it matters**: Without this, a single worker crash could leave jobs
stuck in `RUNNING` forever, silently halting progress.

**Violating example**: A job's lease never expires (bug in expiry logic, or
no reclaim path), so a crashed worker's job sits in `RUNNING` indefinitely.

**Implementation mechanism**: `lease_expires_at` is set on every claim/
heartbeat; the claim query itself treats `state='RUNNING' AND
lease_expires_at < now()` as an eligible candidate alongside `QUEUED`/
`RETRY_WAIT` rows (see [worker-protocol.md](worker-protocol.md)). No
separate "is the worker alive" check exists or is needed — liveness is
purely lease-expiry-based.

**Test strategy**: Worker-crash simulation test — kill a worker mid-job,
advance time past `lease_expires_at`, assert another worker claims it. See
[scenario-corpus.md](scenario-corpus.md) SF-002/SF-003/SF-007.

---

### TF-INV-005 — Terminal states never become non-terminal

**Property**: `SUCCEEDED`, `CANCELLED`, and `DEAD_LETTERED` are terminal.
No transition out of a terminal state exists, in the state machine or in
any code path.

**Why it matters**: This is the property most external reviewers will
specifically try to break. A job that "un-completes" undermines every
downstream system that trusted the terminal state (e.g., a billing job that
reports `SUCCEEDED` twice, or reopens after being marked `CANCELLED`).

**Violating example**: A stale worker's retry logic runs after the job was
already marked `SUCCEEDED` by another attempt, and transitions it to
`RETRY_WAIT` because it doesn't know the job already finished.

**Implementation mechanism**: (a) the state machine in
[execution-semantics.md](execution-semantics.md) defines terminal states
with zero outbound transitions; (b) every transition-writing query includes
`AND state = 'RUNNING'` (or the specific expected pre-state) in its `WHERE`
clause, so a transition attempt against an already-terminal row affects zero
rows; (c) fencing (TF-INV-003) prevents a stale worker from even attempting
a transition with valid credentials.

**Durable proof, independent of the code paths above**: `internal/invariant`
proves this property directly against durable state (not just "the code
looks right") via two independent, wall-clock-free checks, both under this
same TF-INV-005 ID:

1. **Currently reopened**: `jobs.terminal_at IS NOT NULL` but the job's
   CURRENT `state` is not `SUCCEEDED`/`CANCELLED`/`DEAD_LETTERED`. `terminal_at`
   is written to non-NULL only in the same statement that also sets `state`
   to one of those three, so a stale `terminal_at` next to a non-terminal
   `state` is exactly the artifact a buggy reclaim of an already-terminal row
   would leave behind — a same-row, same-statement comparison that needs no
   cross-transaction time ordering.
2. **Historically reopened**: a `job_attempts` row exists whose
   `attempt_number` is greater than `jobs.terminal_attempt_count` (see the
   `terminal_attempt_count` column, [data-model.md](data-model.md), added by
   migration 0004). Case 1 alone misses a job that was terminalized,
   illegitimately reclaimed, and then reached a terminal state a SECOND
   time — at that point `state` reads terminal again and `terminal_at` has
   been overwritten with the second terminalization's own timestamp, so
   case 1 sees nothing wrong. `terminal_attempt_count` is captured exactly
   once, on the transition that FIRST made the job terminal, and is never
   written again, so any `job_attempts.attempt_number` found above it is
   proof a new attempt was opened after the job had already, durably,
   reached a terminal state at least once — regardless of what
   `state`/`terminal_at` read now. This covers all three terminal states
   uniformly, including `CANCELLED` reached with zero `job_attempts` rows at
   all (e.g. `CancelQueuedOrRetryWait`, or a workflow's cascade-cancel of an
   unclaimed dependent): `terminal_attempt_count` is still captured as `0`
   on that transition, so any later `attempt_number > 0` is caught the same
   way.

Neither check ever compares a wall-clock timestamp written by one
transaction against one written by a different transaction — both compare
durable integer/enum columns on rows serialized by PostgreSQL's own
row-level locking. An earlier version of this check instead compared
`job_attempts.started_at` against `jobs.terminal_at` across two different
transactions/rows and produced real false positives under sustained load
(PostgreSQL's `now()` is not guaranteed strictly monotonic across an
NTP-slew-style bounded correction — see the Clock Model in
[failure-model.md](failure-model.md)); this is exactly why case 1/2 above
compare only columns written together, in the same statement, on the same
row.

**Marker self-validation**: because `terminal_attempt_count` is
correctness-critical to case 2, this same check also reports the marker's
own state being malformed, independent of whether anything is currently
reopened — a terminal row with `terminal_attempt_count IS NULL` (case 2
silently disabled for that row), a negative `terminal_attempt_count`, or a
`terminal_attempt_count` exceeding the job's current `attempt_count` are
all durably impossible under every current terminalization path and are
reported the moment they are found, rather than being silently trusted.
See `internal/invariant/invariant.go`'s `checkNoAttemptAfterTerminal` doc
comment for exactly which malformed shapes this covers, and why no
database `CHECK` constraint enforces them instead (migration 0004's own
comment).

**Migration 0004 limitation**: a job row that was already illegitimately
reopened *before* migration 0004 ran cannot have its true original
`attempt_count` reconstructed from existing durable data — the migration's
backfill can only record "`attempt_count` as of the backfill," not the
provably-true first-terminalization value, for any row terminalized before
the column existed. Every row terminalized after the migration gets the
full, race-free guarantee described above.

**Test strategy**: State-machine table test enumerating all
`(from_state, to_state)` pairs and asserting the transition function rejects
every pair not in the allowed table (see
[testing-strategy.md](testing-strategy.md) "state-machine table tests,"
scenario SF-015), plus `internal/invariant`'s durable-state checks above,
proven directly against a real PostgreSQL instance by
`internal/invariant/invariant_test.go` (reopening, reopen-then-re-terminalize,
the clock-skew false-positive regression, and each malformed-marker shape)
and exercised continuously by every seeded chaos campaign in
`internal/chaos`.

---

### TF-INV-006 — Retry attempts respect configured limits

**Property**: A job's `attempt_count` never exceeds its `max_attempts`
before transitioning to a terminal state. Once `attempt_count >=
max_attempts` and the latest attempt failed, the job transitions to
`DEAD_LETTERED`, never back to `RETRY_WAIT`.

**Why it matters**: Without an enforced ceiling, a permanently-failing job
retries forever, wasting resources and masking the fact that it needs human
attention.

**Violating example**: An off-by-one in the retry-eligibility check allows
one extra attempt past `max_attempts`, or (worse) never checks the ceiling
at all.

**Implementation mechanism**: The transition decision after a failed attempt
is computed in the same transaction that records the attempt outcome:
`IF attempt_count >= max_attempts THEN DEAD_LETTERED ELSE RETRY_WAIT`. See
[retry-semantics.md](retry-semantics.md).

**Test strategy**: Property test — for random `max_attempts` and a
handler that always fails retryably, assert the job reaches exactly
`max_attempts` attempts and then `DEAD_LETTERED`, never more. See
[scenario-corpus.md](scenario-corpus.md) SF-010.

---

### TF-INV-007 — Attempt history is durable and monotonic

**Property**: Every attempt (claim → outcome) produces exactly one durable
`job_attempts` row. `attempt_number` values for a given job are gapless,
strictly increasing integers starting at 1, and existing rows are never
updated after being written (append-only).

**Why it matters**: Auditability. Debugging a production incident or a
disputed billing job requires a trustworthy, tamper-evident history of what
was actually attempted and when.

**Violating example**: A retry reuses the same `attempt_number` as a prior
attempt, or an attempt row is mutated after the fact to hide a failure.

**Implementation mechanism**: `job_attempts.attempt_number` is set from the
job's `attempt_count` at claim time (post-increment), inserted once per
claim, and the table has no `UPDATE` code path for existing rows in the
attempt lifecycle — only `finished_at`/`outcome`/`error_*` columns are
filled in by the *same* attempt's completion call, fenced by
`lease_generation` (TF-INV-003), so a stale attempt cannot mutate a row that
a newer attempt has already superseded. See [data-model.md](data-model.md).

**Test strategy**: Table test asserting `job_attempts.attempt_number` for a
job is exactly `[1, 2, ..., attempt_count]` with no gaps or duplicates after
any sequence of crash/retry test scenarios.

---

### TF-INV-008 — An idempotency key never creates two logical jobs

**Property**: Within its defined scope (`(job_type, idempotency_key)`), a
given idempotency key maps to exactly one job row, regardless of how many
times a submission with that key is retried, including concurrently.

**Why it matters**: This is what makes `POST /jobs` safe to retry over an
unreliable network — the entire point of accepting an `Idempotency-Key`.

**Violating example**: Two concurrent `POST /jobs` requests with the same
key both pass a "check if exists" read before either has inserted, and both
insert, creating two jobs.

**Implementation mechanism**: A unique constraint on
`(job_type, idempotency_key)` where `idempotency_key IS NOT NULL`; the
INSERT is attempted directly (not preceded by a check-then-act read), and a
unique-violation error is caught by the API server, which then re-reads and
returns the existing row. See [idempotency.md](idempotency.md).

**Test strategy**: Concurrency test firing N concurrent submissions with an
identical key and asserting exactly one job row exists afterward. See
[scenario-corpus.md](scenario-corpus.md) SF-005.

---

### TF-INV-009 — Dead-lettering preserves failure history

**Property**: A job transitioning to `DEAD_LETTERED` retains its full
`job_attempts` history and a non-null final failure reason
(`last_error`/`last_error_class`) describing why it stopped being retried.

**Why it matters**: A dead-lettered job with no explanation is undebuggable
and un-actionable for the human operator who has to decide whether to
resubmit it.

**Violating example**: Dead-lettering logic clears `last_error` or does not
propagate the terminating attempt's failure reason onto the job row.

**Implementation mechanism**: The same transaction that transitions a job to
`DEAD_LETTERED` copies the terminating attempt's `error_message`/
`error_class` onto the job row's `last_error`/`last_error_class` columns; no
deletion of `job_attempts` rows ever occurs (append-only, per TF-INV-007).

**Test strategy**: Scenario test asserting a dead-lettered job's
`last_error` matches its final `job_attempts` row and all prior attempts
are still queryable. See [scenario-corpus.md](scenario-corpus.md) SF-010.

---

### TF-INV-010 — Cancellation has deterministic race semantics

**Property**: When cancellation races with worker completion, exactly one
outcome wins, and the rule is deterministic and documented: **the first
write to reach a durable terminal/committed state wins**; a cancellation
request that loses the race is not applied (the job is not forcibly
reverted from `SUCCEEDED`/other terminal states); a completion attempt that
loses the race (job already `CANCELLED`) is rejected by the same fencing
check as TF-INV-003.

**Why it matters**: Without a documented, deterministic rule, cancel/complete
races produce nondeterministic, untestable behavior — exactly what
distinguishes a hobby implementation from a reviewable one.

**Violating example**: A cancellation request and a worker's success report
arrive "simultaneously," and depending on unspecified internal ordering, the
job sometimes ends up `SUCCEEDED` and sometimes `CANCELLED` for the
*identical* input timing, with no way to reason about which will happen.

**Implementation mechanism**: Both cancellation and completion are
conditional `UPDATE`s guarded by `WHERE state = 'RUNNING'` (or `QUEUED`/
`RETRY_WAIT` for pre-claim cancellation). PostgreSQL serializes concurrent
UPDATEs to the same row; whichever transaction commits first wins, and the
second one's `WHERE` clause no longer matches (state has changed), so it
affects zero rows and is reported to its caller as "no-op, job already in
terminal state X." See [execution-semantics.md](execution-semantics.md)
"Cancellation and Timeouts."

**Test strategy**: Deterministic interleaving test that holds one
transaction open past the other's commit to force both orderings, asserting
exactly one terminal state results either way. See
[scenario-corpus.md](scenario-corpus.md) SF-012.

---

### TF-INV-011 — Scheduled jobs do not execute before eligibility

**Property**: A job is never claimed while `eligible_at > now()`, where
`now()` is PostgreSQL's clock (see [failure-model.md](failure-model.md)
Clock Model).

**Why it matters**: This is the entire contract of "scheduled/delayed jobs."
Violating it silently breaks any caller relying on delayed execution (e.g.,
a reminder sent early).

**Violating example**: A worker's claim query uses the worker's local clock
instead of the database's, and a fast/skewed worker clock claims a job
early.

**Implementation mechanism**: The claim query's `WHERE eligible_at <=
now()` predicate is evaluated entirely inside PostgreSQL; no
application-level time comparison ever gates eligibility. See
[scheduling.md](scheduling.md).

**Test strategy**: Scenario test scheduling a job in the future, advancing
only the database's clock context (via test fixtures), and asserting no
claim succeeds before `eligible_at`. See [scenario-corpus.md](scenario-corpus.md)
SF-013.

---

### TF-INV-012 — Dependency-gated workflow tasks wait for predecessors

**Property**: (Phase 7+) A workflow node with declared dependencies is never
eligible for claiming until all of its required predecessor nodes have
reached a state that satisfies the declared dependency condition (default:
`SUCCEEDED`).

**Why it matters**: This is the entire contract of DAG execution — running
step B before step A completes defeats the purpose of declaring a
dependency at all.

**Violating example**: Node B is enqueued as soon as the workflow starts,
ignoring that it depends on Node A, and races with A's execution.

**Implementation mechanism**: A workflow node's underlying job is inserted
with `state = QUEUED`, and is only made *eligible* by advancing
`eligible_at` off a fixed far-future sentinel value once a
predecessor-completion propagation step (running inside the SAME
transaction as the predecessor's own completion/cancellation/dead-letter
transition) evaluates all required predecessors as satisfied. See
[workflows.md](workflows.md) for the exact mechanism as implemented,
including the concurrent-fan-in row-locking argument. Enforced starting
Phase 7.

**Test strategy**: DAG scenario tests for fan-out/fan-in (including
concurrent completion), retrying/failed/cancelled predecessor
propagation, stale-generation fencing extended to workflow propagation,
workflow-level cancellation, and restart durability — see
[scenario-corpus.md](scenario-corpus.md) SF-019 through SF-030 and
[testing-strategy.md](testing-strategy.md)'s Phase 7 section.

---

### TF-INV-013 — Rollback never leaves a half-transitioned job

**Property**: If any durable state transition's transaction is rolled back
for any reason, the job row is left exactly as it was before the
transaction began — never in a state that is neither the old nor the new
value.

**Why it matters**: Partial writes are the classic source of "impossible"
production states that take hours to debug.

**Violating example**: A transition that updates `state` and
`lease_expires_at` in two separate statements, where a crash between them
commits the first but not the second, leaving the row internally
inconsistent (e.g., `state='RUNNING'` but `lease_expires_at` still null).

**Implementation mechanism**: Every state transition is a single SQL
statement (a single `UPDATE`) or, where multiple tables are involved (job +
attempt row), a single database transaction with no intermediate commit
points. PostgreSQL's atomicity guarantees this by construction as long as
no transition is ever split across transactions.

**Test strategy**: Fault-injection test that forces a rollback mid-transition
(e.g., a deliberate constraint violation or killed connection) and asserts
the row is byte-for-byte identical to its pre-transaction value. See
[scenario-corpus.md](scenario-corpus.md) SF-014.

---

### TF-INV-014 — A stale lease holder is fenced after a newer lease is acquired

**Property**: Once a job's `lease_generation` has advanced (a new worker
successfully claimed it), no write carrying an older `lease_generation` can
succeed, regardless of ordering or timing of the stale write's arrival.

**Why it matters**: This is the generalized form of TF-INV-003, stated
specifically for the multi-generation race — it must hold no matter how
many generations have advanced or how late the stale write arrives (even
long after the job has reached a terminal state via a later generation).

**Violating example**: Job goes through generations 5 (Worker A, lease
lost), 6 (Worker B, completes job → `SUCCEEDED`), and Worker A's
long-delayed completion call for generation 5 arrives after that — it must
still be rejected, not just "usually" rejected.

**Implementation mechanism**: Identical mechanism to TF-INV-003
(`lease_generation` equality check in the `WHERE` clause of every
transition-writing query) — stated as its own invariant because it must
hold for *arbitrarily late* arrivals and *arbitrarily many* intervening
generations, not just the immediately-next one.

**Test strategy**: Extended fencing test with 3+ generations and
out-of-order arrival of stale completion calls. See
[scenario-corpus.md](scenario-corpus.md) SF-008.

---

## Additional Invariants

### TF-INV-015 — Heartbeats can only extend a lease, never shorten or transfer it

**Property**: A heartbeat call from the current lease holder can only move
`lease_expires_at` forward in time, and only succeeds under the same
fencing check as completion (`lease_owner`/`lease_generation`/`state='RUNNING'`
match). A heartbeat can never change `lease_owner` or `lease_generation`.

**Why it matters**: Heartbeats are the mechanism that lets long-running jobs
avoid spurious reclaim; if a heartbeat could be forged or could shorten a
lease, it would either strand long jobs (starvation) or let a stale worker
resurrect its ownership (split-brain).

**Violating example**: A heartbeat handler accepts a `worker_id` alone
(without checking `lease_generation`), letting a stale worker "refresh" a
lease it no longer holds.

**Implementation mechanism**: Heartbeat is `UPDATE jobs SET
lease_expires_at = now() + lease_duration, heartbeat_at = now() WHERE id = ?
AND lease_owner = ? AND lease_generation = ? AND state = 'RUNNING'` — same
fencing shape as completion.

**Test strategy**: Fencing test issuing a heartbeat with a stale generation
and asserting rejection; property test asserting `lease_expires_at` is
non-decreasing across successive successful heartbeats from the same
generation.

### TF-INV-016 — Idempotency key uniqueness is enforced by the database, not application logic

**Property**: The guarantee in TF-INV-008 holds even under arbitrary
application-level bugs, concurrent process crashes, or race conditions in
API server code, because it is enforced by a PostgreSQL unique constraint,
not by a check-then-act pattern in Go code.

**Why it matters**: Correctness properties that depend on application code
"remembering" to check something correctly are fragile under refactors.
Pushing the invariant into a database constraint makes it unconditional.

**Violating example**: A future refactor removes the "catch unique
violation and re-read" logic, replacing it with a `SELECT` followed by an
`INSERT` — reintroducing the TOCTOU race that TF-INV-008 exists to prevent.

**Implementation mechanism**: `UNIQUE (job_type, idempotency_key)` constraint
in the schema (see [data-model.md](data-model.md)); this document exists so
that constraint is never treated as optional/removable in a future
migration without deliberately revisiting this invariant.

**Test strategy**: Schema test asserting the constraint exists; the
concurrency test for TF-INV-008 doubles as a regression test for this.

---

## Cross-Phase Governance Additions (Phase 12 / Phase 13 reconciliation)

Phase 12 ([phase-12-plan.md](phase-12-plan.md) OD-8) and Phase 13
([phase-13-plan.md](phase-13-plan.md) OD-3) both deferred formal
`TF-INV-0NN` allocation to a dedicated, cross-phase governance pass rather
than deciding it unilaterally inside either phase's implementation PR.
This section is that pass's result.

**Method**: not every acceptance test, security control, or operational
expectation becomes a numbered invariant. An existing `TF-INV-*` entry
proves a durable, checkable property of the job/workflow state machine
itself — "this exact row-level condition can never hold" — verified
directly against PostgreSQL state, independent of which code path got
there. A new ID is allocated only when a candidate guarantee is of that
same kind: a durable state-machine safety property, not a request-time
access-control gate, a deployment/config requirement, a compatibility
contract, or an SLO. Phase 12's own [phase-12-plan.md](phase-12-plan.md)
§11 "16 verification points" already proves this distinction is real in
practice: most of them (deny-by-default auth, credential hygiene, TLS
boundary, audit logging, least-privilege roles) are proven by dedicated
non-`TF-INV` test suites and are not weakened, duplicated, or renumbered
by being left out of this registry.

### Phase 12's G1–G8: disposition

| Guarantee | Disposition | Why |
|---|---|---|
| G1 — No unauthenticated read/write | **Not an invariant.** Security requirement. | Request-time access-control gate, not a durable state property; already proven by `internal/api/structure_phase12_test.go`'s structural absence-of-behavior tests and verification point 5 — the same rigor class as a `TF-INV`, deliberately kept in its own category so this document stays scoped to job/workflow state-machine correctness. |
| **G2 — Ownership-scoped access** | **Invariant. → TF-INV-017.** | Durable, SQL-enforced, zero-row-affected property of the exact kind TF-INV-002/003 already are. See below. |
| G3 — Distinct trust boundaries | **Not an invariant.** Architectural/security design decision. | Not a runtime-checkable state property — a statement about which layer authenticates whom, documented in `security-model.md` "Trust Domains" and `phase-12-plan.md` §4. |
| G4 — Least-privilege PostgreSQL roles | **Not an invariant.** Security/operational configuration requirement. | Provable against `deploy/postgres-roles.sql`'s grants (`internal/migrate/phase12_roles_test.go`), but it bounds blast radius rather than stating a job-state safety property; a compromised-but-scoped worker role is a defense-in-depth control, not a "this state can never occur" guarantee. |
| G5 — Transport security | **Not an invariant.** Operational/deployment expectation. | Explicitly not enforced by TaskForge itself (`phase-12-plan.md` G5: "TaskForge does not terminate TLS itself") — an external deployment requirement, not a property of this system's own state. |
| G6 — Audit trail | **Not an invariant.** Operational/observability expectation. | A logging-completeness requirement, not a state-machine safety property. |
| **G7 — Tenant-scoped idempotency** | **Invariant. → TF-INV-018.** | Extends TF-INV-008/016's durable, DB-constraint-enforced uniqueness guarantee to the tenant-scoped key. See below. |
| G8 — Credential hygiene | **Not an invariant.** Security requirement. | Proven by verification points 3/4/10 (never-plaintext, constant-time comparison, never-logged); a credential-handling discipline, not a job-state property. |

`TF-INV-001` through `TF-INV-016` are unchanged by this reconciliation —
not one word of their existing text is altered. Where a Phase 12
guarantee narrows or extends the *scope* of an existing invariant (G7 on
TF-INV-008/016) rather than stating a wholly new property, that narrowing
is recorded as its own new ID below rather than by editing the original
entry, per this document's own append-only convention (see header).

---

### TF-INV-017 — A principal can act only on the jobs and workflows it owns

**Property**: For a non-admin principal, every read, cancel, and list
operation against a job or workflow is scoped to
`jobs.principal_id = <authenticated caller>` (or the workflow-instance
equivalent) at the SQL statement itself. An attempt by principal B against
a resource owned by principal A affects zero durable rows and is
indistinguishable, in its response, from the resource not existing at
all. An admin principal is the sole documented exception.

**Why it matters**: Without this, one tenant's credentials could read or
cancel another tenant's jobs — a direct confidentiality and integrity
breach, and exactly the failure mode multi-tenancy exists to prevent. It
is the durable-state counterpart to Phase 12's authentication guarantee
(G1): authentication proves *who* is asking; this invariant proves the
data layer itself, not just a handler check, refuses to answer for anyone
but the asker.

**Violating example**: Principal B calls `POST /jobs/{A's job id}/cancel`.
If the query lacked a `principal_id` predicate (or checked it only in a
handler, after an unscoped read), B's request could mutate A's job, or a
response could leak that a given ID belongs to *someone* even if not to B.

**Implementation mechanism**: The ownership predicate lives in the same
SQL statement that reads or mutates the row — `internal/store`'s
`GetByID`, `CancelQueuedOrRetryWait`, `RequestCancellation`, `GetWorkflow`,
and `CancelWorkflow` all carry an `AND ($n::boolean OR principal_id =
$n+1)` clause (admin bypass OR ownership match), not a handler-level
filter applied after the fact. `txenqueue.EnqueueTx` requires a non-zero
`PrincipalID` and attributes the job to exactly that principal, with no
fallback/default path. See [phase-12-plan.md](phase-12-plan.md) §4a, G2.

**Test strategy**: Store-layer, zero-row-mutation proofs in
`internal/store/principal_scoping_phase12_test.go`
(`TestGetByID_IsPrincipalScoped`,
`TestCancelQueuedOrRetryWait_IsPrincipalScoped_MutatesZeroRows`,
`TestRequestCancellation_IsPrincipalScoped_MutatesZeroRows`,
`TestGetWorkflow_IsPrincipalScoped`,
`TestCancelWorkflow_IsPrincipalScoped_TouchesNoNodeJob`,
`TestCreateWorkflow_AttributesInstanceAndEveryNodeJobToOnePrincipal`);
HTTP-boundary, indistinguishable-from-nonexistent proofs in
`internal/api/handlers_phase12_test.go`
(`TestGetJob_CrossPrincipal_IndistinguishableFromNonexistent`,
`TestGetWorkflow_CrossPrincipal_IndistinguishableFromNonexistent`,
`TestCancelJob_CrossPrincipal_MutatesZeroRows`,
`TestCancelJob_CrossPrincipal_RunningJob_NeverSetsCancelRequested`,
`TestCancelWorkflow_CrossPrincipal_MutatesZeroRows`,
`TestAdminPrincipal_MayReadAndCancelAnyPrincipalsResources`); enqueue-path
proofs in `txenqueue/principal_phase12_test.go`
(`TestEnqueueTx_AttributesJobToSuppliedPrincipal`,
`TestEnqueueTx_PrincipalID_Required_RejectsZeroValue`,
`TestEnqueueTx_NoDefaultSystemPrincipalPath`,
`TestTxenqueuePackage_ContainsNoSystemPrincipalFallback`). These are
Phase 12 verification points 6, 7, and 8
([phase-12-plan.md](phase-12-plan.md) §11), already passing at merge; this
entry only assigns them a formal invariant ID, adding no new test.

---

### TF-INV-018 — Idempotency keys are unique within their tenant scope

**Property**: Within the scope `(principal_id, job_type, idempotency_key)`,
a given idempotency key maps to exactly one job row, regardless of how
many times a submission with that key is retried, including concurrently
and including by two different principals presenting the *same*
`(job_type, idempotency_key)` pair. `principal_id` is `NOT NULL`, so no
NULL-widening collision window exists across tenants or for
not-yet-authenticated submissions.

**Why it matters**: This is TF-INV-008/016's guarantee, carried forward
under multi-tenancy. Without the tenant dimension in the uniqueness scope,
two different callers who happen to choose the same `idempotency_key` for
the same `job_type` would collide into a single job row — one tenant's
retry-safety key silently becoming a cross-tenant confused-deputy bug,
letting one principal observe or cancel a job it did not submit.

**Violating example**: Principal A and principal B both submit
`job_type="send_email"` with `idempotency_key="user-42-welcome"`
(coincidentally identical, or supplied by a shared client library with a
predictable key scheme). Without tenant scoping, B's request would be
treated as a duplicate of A's and return A's job.

**Implementation mechanism**: `UNIQUE (principal_id, job_type,
idempotency_key)` — the same unique-constraint-not-application-logic
pattern TF-INV-016 already establishes, re-keyed to include
`principal_id`; `jobs.principal_id NOT NULL` closes the NULL-widening
case. See [phase-12-plan.md](phase-12-plan.md) §5, G7 ("Idempotency
constraint change").

**Test strategy**: `internal/store/principal_scoping_phase12_test.go`
(`TestGetByIdempotencyKey_IsPrincipalScoped`,
`TestInsertIdempotent_ConflictRecoveryStillWorksAfterIndexRename`,
`TestInsert_ZeroPrincipalIDIsRejectedByTheDatabase`,
`TestConcurrentTwoPrincipalIdempotency_ExactlyOneRowPerPrincipal`);
`internal/api/handlers_phase12_test.go`
(`TestIdempotency_IsScopedPerPrincipal`);
`txenqueue/principal_phase12_test.go`
(`TestEnqueueTx_IdempotencyIsScopedPerPrincipal`). Phase 12 verification
point 2 ([phase-12-plan.md](phase-12-plan.md) §11), already passing at
merge; this entry only assigns it a formal invariant ID.

---

### TF-INV-019 — No queue/tenant is starved beyond its documented bound

Status: **Reserved, algorithm-independent.** Phase 13
([phase-13-plan.md](phase-13-plan.md)) has not been implemented; this ID
is confirmed now, ahead of implementation, so Phase 13's implementation PR
has a stable, non-tentative ID to cite. `docs/enterprise-roadmap.md` and
`docs/phase-13-plan.md` previously cited this property as "tentatively
TF-INV-017" — see the reconciliation note above for why `017`/`018` went
to Phase 12's G2/G7 instead, pushing this property to the next available
ID. Confirming the ID now does **not** decide, endorse, or foreclose any
candidate fairness/concurrency mechanism — that choice remains Phase 13
OD-1's mandatory ADR, still unwritten.

**Property**: No queue or tenant with pending, capacity-eligible work is
starved beyond the bound that Phase 13's governing ADR proves for its
selected algorithm, under that ADR's own stated assumptions (run
duration, load shape, queue count), while another queue or tenant
continues to make progress. This is never claimed as an unconditional or
indefinite guarantee — only the bound the ADR actually proves.

**Why it matters**: Without a bounded fairness guarantee, a single
high-volume queue or tenant can monopolize worker capacity indefinitely,
starving every other queue/tenant even though they have eligible,
capacity-respecting work waiting — the core failure mode Phase 13's
governance work exists to close.

**Violating example**: Queue "bulk-export" floods the claim pool; queue
"user-notifications" has pending, capacity-eligible work but is never
claimed for the duration of a test run long enough to exceed whatever
bound the ADR's algorithm is supposed to guarantee.

**Implementation mechanism**: Not yet decided — Phase 13 OD-1 (mandatory
ADR, [phase-13-plan.md](phase-13-plan.md) §18) selects the concurrency/
fairness mechanism; this invariant governance pass deliberately does not
select one on the ADR's behalf.

**Test strategy**: A Phase-5-style concurrency stress test with two or
more queues — one flooded, one starved under the pre-Phase-13 model —
asserting both make bounded-wait-time progress under the selected
mechanism, over the tested run's duration (per
[phase-13-plan.md](phase-13-plan.md) §10 and
[enterprise-roadmap.md](enterprise-roadmap.md) Phase 13 "Invariants /
proof obligations"). The exact test does not exist yet and is Phase 13
implementation-PR scope, not this governance pass's.

---

### Phase 13 guarantees NOT allocated a `TF-INV` ID

Recorded here so the reconciliation is complete, not just the additions:

| Candidate | Disposition | Why |
|---|---|---|
| Concurrency-limit exactness (never more than N concurrently `RUNNING` jobs for a capped queue) | **Not a new invariant.** Proof obligation, tested the same way Phase 5 already proves exclusivity (TF-INV-002-style `SKIP LOCKED` stress test). | [phase-13-plan.md](phase-13-plan.md) §10 already states this explicitly: "not itself a new numbered invariant, but a proof obligation." This governance pass ratifies that call. |
| Backpressure liveness (`429`/`503` + `Retry-After` eventually succeeds; a queue merely at its cap does not itself trigger either code) | **Not an invariant.** Operational/SLO expectation. | A retry-eventually-succeeds liveness property and a status-code-classification rule, not a durable state-machine safety property. |
| Retention/idempotency "no second clock" interaction | **Not a new invariant.** Proof obligation against the existing TF-INV-008/TF-INV-016/TF-INV-018 uniqueness guarantee. | Retention must not let cleanup outpace the idempotency window; this is a correctness requirement *on* the existing idempotency invariants once Phase 13 implements retention, not a new property in its own right. |
| Retention/TF-INV-005 interaction (pruning `job_attempts` must not disable the historical-reopen check) | **Not a new invariant.** Proof obligation against TF-INV-005. | [phase-13-plan.md](phase-13-plan.md) §10 identifies this as a required test (its SF-045) against TF-INV-005's existing durable check, not a new property. |
| `queue_name` validation, retention sweeper role scoping, `503` admission semantics | **Not invariants.** Ordinary acceptance criteria / operational configuration. | Implementation-detail acceptance tests once Phase 13 is built; none states a durable state-machine safety property in its own right. |

This governance pass resolves [phase-12-plan.md](phase-12-plan.md) OD-8
and [phase-13-plan.md](phase-13-plan.md) OD-3: the invariant namespace is
now one coherent sequence, `TF-INV-001` through `TF-INV-019`, with `017`
and `018` allocated to Phase 12's already-merged G2/G7 and `019` confirmed
(non-tentative) for Phase 13's still-unimplemented fairness property.
Phase 13's concurrency/fairness *algorithm* (OD-1) remains open and is
explicitly not decided by this pass.

### Phase 14 reviewed: no new invariant added

Phase 14 (Upgrade & Compatibility Proof, [phase-14-plan.md](phase-14-plan.md)
§11) was reviewed against this same method and adds **no new numbered
invariant**. Its proof obligation is the existing set, `TF-INV-001`
through `TF-INV-019`, holding throughout a mixed-binary-version window
(`test/compat`'s two-binary-version harness, SF-066/067) and a graceful
SIGTERM/SIGKILL drain sequence (`test/procs`, SF-063 through SF-070) — a
breadth requirement across a new adversarial *condition*, not a new
state-machine property. Candidates considered and their disposition:

| Candidate | Disposition | Why |
|---|---|---|
| Expand/migrate/contract, data-safe-reversible vs. forward-fix-only migration classification | **Not an invariant.** Schema-tooling/CI-enforcement policy. | [ADR-0010](adr/0010-expand-migrate-contract.md) governs this; it is a statement about how migrations are authored and verified, not a durable row-level state property. |
| Graceful-drain grace period (`Worker.SetDrainTimeout`, `dispositionDraining`) | **Not a new invariant.** Proof obligation against the existing TF-INV-004. | A drain-timeout expiry is deliberately made indistinguishable, from the job's and the store's perspective, from an ordinary worker crash — TF-INV-004's already-proven lease-expiry/reclaim mechanism is what makes it safe, not a new property. |
| `cmd/api` `BaseContext`/`srv.Close` shutdown-deadline behavior | **Not an invariant.** Process-lifecycle/operational contract. | Governs when a process stops serving a request, not a durable state-machine transition. |
| Old-worker/new-server (and vice versa) schema compatibility | **Not a new invariant.** Proof obligation across every existing `TF-INV-*`. | This is exactly the breadth requirement this section already describes — the two-binary harness is a stronger *test* of existing invariants under a new condition, not a new invariant in its own right. |

### Phase 15 reviewed: no new invariant added

Phase 15 (PostgreSQL HA / Backup / DR Proof, [phase-15-plan.md](phase-15-plan.md)
§21) was reviewed against this same method and adds **no new numbered
invariant**. Its proof obligation is the existing set, `TF-INV-001`
through `TF-INV-019`, holding across (a) a database restored via
point-in-time recovery (`test/dr/backup_restore_test.go`, SF-071/SF-075)
and (b) a database that has just undergone a live standby-promotion
failover drill (`test/dr/failover_drill_test.go`, SF-073/SF-074) — a
breadth requirement across a new adversarial condition (a *database
instance* change, not merely a code-version change), exactly the same
category this section already used to close out Phase 14. Candidates
considered and their disposition:

| Candidate | Disposition | Why |
|---|---|---|
| "A restore reproduces a database where every existing `TF-INV-*` holds" | **Not a new invariant.** Proof obligation against the existing set. | Breadth requirement — every existing invariant, proven against a new database instance rather than a new adversarial code path. Drilled: `TestDR_SF071_SF075_BackupRestoreDrill_PITR` runs `internal/invariant.Checker.CheckAll` against the restored database directly; zero violations. |
| "Fencing (TF-INV-002/003/014) holds across a standby promotion" | **Not a new invariant.** Proof obligation against TF-INV-002/003/014 specifically. | Fencing is a property of the data (`lease_owner`/`lease_generation` and the conditional-`UPDATE` mechanism that reads/writes them), which a promoted standby serves identically — proven, not merely argued: `TestDR_SF073_SF074_FailoverDrill_LiveTraffic` puts a job actively in-flight at the instant the primary is stopped and observes it complete correctly against the promoted standby, with zero invariant violations across the drill. |
| "TaskForge resumes writes within a measured, bounded window after promotion" | **Not an invariant.** Operational/SLO-shaped expectation, timing-only. | No durable row-level state property is being asserted — a stopwatch measurement, recorded in [disaster-recovery.md](disaster-recovery.md) §5.2, not a state-machine property. |
| "A WAL archive gap fails recovery loudly rather than silently skipping" | **Not an invariant.** PostgreSQL's own documented behavior, not a TaskForge-enforced property. | TaskForge builds no code path here — `TestDR_SF072_WALArchiveGap_RecoveryFailsLoudly` observes PostgreSQL's own `FATAL: recovery ended before configured recovery target was reached`, nothing `internal/invariant.Checker` (or any TaskForge mechanism) checks. |

---

## Summary Table

| ID | Property (short) | Terminal to state machine? |
|---|---|---|
| TF-INV-001 | Accepted jobs cannot disappear | No |
| TF-INV-002 | At most one valid lease per job | No |
| TF-INV-003 | Lost lease cannot complete a job | No |
| TF-INV-004 | Crashes cannot permanently strand jobs | No |
| TF-INV-005 | Terminal states never reopen | Yes |
| TF-INV-006 | Retry attempts respect limits | No |
| TF-INV-007 | Attempt history durable and monotonic | No |
| TF-INV-008 | Idempotency key maps to one job | No |
| TF-INV-009 | Dead-lettering preserves failure history | No |
| TF-INV-010 | Cancellation race is deterministic | Yes |
| TF-INV-011 | Scheduled jobs don't run early | No |
| TF-INV-012 | Workflow deps gate execution | No |
| TF-INV-013 | Rollback never half-transitions | Yes |
| TF-INV-014 | Stale generations always fenced | No |
| TF-INV-015 | Heartbeats only extend, never transfer | No |
| TF-INV-016 | Idempotency enforced by DB constraint | No |
| TF-INV-017 | Principal isolation (ownership-scoped access) | No |
| TF-INV-018 | Idempotency uniqueness is tenant-scoped | No |
| TF-INV-019 | No queue/tenant starved beyond documented bound (Phase 13, reserved) | No |

See [testing-strategy.md](testing-strategy.md) for the full
invariant-to-test matrix and [execution-semantics.md](execution-semantics.md)
for the state machine these invariants constrain.
