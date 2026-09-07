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

**Test strategy**: State-machine table test enumerating all
`(from_state, to_state)` pairs and asserting the transition function rejects
every pair not in the allowed table. See
[testing-strategy.md](testing-strategy.md) "state-machine table tests,"
scenario SF-015.

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
with `state = QUEUED` but is only made *eligible* (`eligible_at` set to a
real timestamp, or an equivalent gating column) once a predecessor
completion trigger evaluates all required predecessors as satisfied. See
[workflows.md](workflows.md). This invariant is not enforceable in v1
because workflows do not exist until Phase 7; it is recorded now so the
Phase 7 design is bound by it from the start.

**Test strategy**: (Phase 7) DAG scenario tests for fan-out/fan-in,
including failed/cancelled predecessor propagation.

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
| TF-INV-012 | Workflow deps gate execution | No (Phase 7+) |
| TF-INV-013 | Rollback never half-transitions | Yes |
| TF-INV-014 | Stale generations always fenced | No |
| TF-INV-015 | Heartbeats only extend, never transfer | No |
| TF-INV-016 | Idempotency enforced by DB constraint | No |

See [testing-strategy.md](testing-strategy.md) for the full
invariant-to-test matrix and [execution-semantics.md](execution-semantics.md)
for the state machine these invariants constrain.
