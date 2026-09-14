# Transactional Enqueue (Phase 11)

Status: **implemented** — docs/enterprise-roadmap.md Phase 11
("Transactional Enqueue & API Contract Hardening"). This document specifies
the exact guarantee the `txenqueue` Go package provides, the guarantee it
explicitly does **not** provide, and the recommended pattern for the case
it does not cover.

## The problem this closes

Before this phase, `POST /jobs` and `internal/store.CreateJob` always
opened their own connection: there was no TaskForge-provided way for a
caller to guarantee "enqueue this job if and only if my own business write
commits" when both live in the same PostgreSQL database. A caller who
needed that guarantee had no choice but to build their own
outbox-and-relay mechanism even for the simplest case — two writes to the
*same* database.

## What is provided: same-PostgreSQL-transaction atomicity

The `txenqueue` package (repository root, `github.com/SamudralaAjaykumarrr/taskforge/txenqueue`)
lets a caller enqueue a job using a `pgx.Tx` (`github.com/jackc/pgx/v5`,
an **interface**, not `*pgx.Tx` — a pointer-to-interface design would be a
type error) the caller already owns:

```go
tx, err := pool.Begin(ctx)
// ... caller's own business-data write, using tx ...
job, created, err := txenqueue.New().EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{
    PrincipalID: callerPrincipalID, // required as of Phase 12 -- see below
    JobType:     "invoice.charge",
    Payload:     payload,
})
// ... caller decides commit or rollback ...
err = tx.Commit(ctx) // or tx.Rollback(ctx)
```

Guarantee: if the caller's transaction commits, the business write and the
job both become durable; if it rolls back (for this reason or any other,
at any later point), neither does. This is exactly PostgreSQL's own
transaction atomicity — `txenqueue` does not add, weaken, or coordinate
anything beyond one more statement inside a transaction the caller already
controls. See `txenqueue/txenqueue_test.go` for the executable proof
(commit, rollback, insert failure, and idempotency-under-concurrency
cases) against a real PostgreSQL instance.

Ownership is strict: `EnqueueTx` never calls `Commit` or `Rollback` on the
caller's `tx`, never opens a second connection, and never starts an
independent top-level transaction. See `internal/store.InsertTx`'s doc
comment for the SAVEPOINT-based mechanism it uses internally to recover
from an idempotency-key conflict without ending the caller's transaction.

A job enqueued this way, once committed, is an ordinary row in the one
`jobs` table (docs/data-model.md) — claimed by the same worker claim query,
subject to the same leases, fencing, retries, dead-lettering, scheduling,
and workflow mechanisms as a job submitted via `POST /jobs`. There is no
second queue table and no parallel execution path.

Submission-idempotency (docs/idempotency.md, TF-INV-008/TF-INV-016) works
the same way here as everywhere else, including inside a caller-owned
transaction: the database unique index is authoritative, not a second,
weaker application check, and this holds under concurrent transactions
racing on the same key. As of Phase 12 that index is
`UNIQUE (principal_id, job_type, idempotency_key)` — so two callers
supplying different `PrincipalID`s with the same `JobType` and
`IdempotencyKey` get two independent jobs (see "Principal identity"
below).

## Isolation level and idempotency conflicts

`EnqueueTx`'s idempotency-key conflict recovery (`internal/store.InsertTx`)
works by attempting the `INSERT` inside a `SAVEPOINT`, and, if it loses a
uniqueness race, rolling back to that savepoint and re-reading the winning
row **using the caller's own `tx`** (so it can hand back a job the caller's
transaction can still see and use). Under **READ COMMITTED** (PostgreSQL's
default, and the isolation level `EnqueueTx`'s existing tests exercise by
default), this always works: each statement in a READ COMMITTED transaction
gets a fresh snapshot, so the fallback re-read always sees the
already-committed conflicting row.

Under **REPEATABLE READ** or **SERIALIZABLE**, this is not guaranteed. A
transaction's snapshot is fixed at (or before) its first statement.
PostgreSQL's own unique-index enforcement is *not* governed by that
snapshot -- it always checks the latest committed data -- so the `INSERT`
still correctly detects the conflict and fails. But the fallback re-read,
constrained to run inside the caller's own already-fixed snapshot, can be
unable to see the row that won the race, if that row committed *after* the
caller's snapshot was fixed. When this happens, `EnqueueTx` never reports a
false success and never returns `store.ErrNotFound`'s "no such job"
meaning (the job genuinely exists, durably, in another already-committed
transaction -- this transaction simply cannot see it yet). Instead it
returns `ErrMustRetryTransaction` (see "Error contract" below): the caller
must roll back `tx` and retry its entire business transaction from the
beginning. `txenqueue` never retries anything itself -- no second
connection, no hidden retry loop, no partial retry of `tx` -- transaction
ownership and retry timing remain entirely the caller's, exactly as for
every other `EnqueueTx` error.

Proven against real PostgreSQL, for all three isolation levels, in
`txenqueue/errors_test.go`:
`TestEnqueueTx_ReadCommitted_DuplicateAcrossSeparateTransactions_ResolvesToExistingRow`,
`TestEnqueueTx_RepeatableRead_IdempotencyConflictOutsideSnapshot_MapsToMustRetry`,
`TestEnqueueTx_Serializable_IdempotencyConflictOutsideSnapshot_MapsToMustRetry`.

## Error contract

Every non-nil error `EnqueueTx` returns is classifiable, via `errors.Is`,
as exactly one of four sentinels (`txenqueue/errors.go`), or is
`context.Canceled`/`context.DeadlineExceeded` passed through unchanged so a
caller's existing ctx-cancellation checks keep working:

- `ErrInvalidRequest` -- `req` failed ordinary submission validation
  (`internal/job.ValidateSubmission`, the same rules `POST /jobs` uses, plus
  the Phase 12 `PrincipalID` requirement below); `tx` was never touched. `Error()` additionally includes the specific,
  safe, human-readable validation reason (the same stable wording a `POST
  /jobs` 400 body would show).
- `ErrInvalidTransaction` -- `tx` itself cannot be used: it is `nil` (an
  ordinary nil `pgx.Tx` interface value is detected before any statement is
  issued and never panics), or it is a transaction PostgreSQL/pgx has
  already ended.
- `ErrMustRetryTransaction` -- see "Isolation level and idempotency
  conflicts" above; also covers a genuine PostgreSQL serialization
  failure/deadlock (SQLSTATE `40001`/`40P01`) reported directly against the
  insert.
- `ErrEnqueueFailed` -- any other unexpected persistence failure.

In every case, the underlying `internal/store`/PostgreSQL error's own text
-- SQL, SQLSTATE, constraint names, driver-internal wording, a `"store:
..."` prefix -- is deliberately never included in the returned error's
`Error()` string. This is a public-API error-leakage requirement
(docs/enterprise-roadmap.md Phase 11): a caller of this package can safely
log, return, or otherwise surface `EnqueueTx`'s error without leaking
TaskForge's internal persistence details. Proven in
`txenqueue/errors_test.go`.

## What is explicitly NOT provided: cross-database atomicity

`txenqueue` provides **no guarantee whatsoever** across:

- two different PostgreSQL databases (even on the same server),
- two different PostgreSQL instances/servers,
- any non-PostgreSQL system — MySQL, DynamoDB, an external SaaS/HTTP API,
  a message broker, or any other independent transactional resource.

TaskForge does not implement, and this document does not claim, a
distributed transaction, two-phase commit, or any other cross-database
coordination mechanism. If a caller's business data lives in a different
database than TaskForge's, `txenqueue` cannot help, by construction —
there is no such thing as one PostgreSQL transaction spanning two separate
database connections.

## The correct pattern for a different database: transactional outbox

1. In the caller's **own** database transaction, write the business row
   **and** an "outbox" row (e.g. `job_type`, `payload`,
   `idempotency_key`, a `dispatched` flag) in the same transaction. This
   is an ordinary same-database transaction in the caller's own system —
   no TaskForge involvement yet.
2. A separate relay process — **the caller's own**, not something
   TaskForge provides or builds — polls the outbox table for
   undispatched rows and calls TaskForge's canonical `POST /v1/jobs`
   endpoint (docs/compatibility-policy.md "API Evolution" -- the legacy,
   deprecated unprefixed `POST /jobs` remains available too, but new
   integration guidance should target the canonical `/v1/` surface, not
   the deprecated one), supplying the same `Idempotency-Key` the outbox row
   was written with. Because submission is idempotent (docs/idempotency.md),
   the relay can safely retry a call whose response was lost, without
   risking a duplicate job.
3. Once the relay observes a successful (or idempotent-duplicate)
   response, it marks the outbox row dispatched.

This is a well-understood pattern precisely because no generic mechanism
can make a single write atomic across two independent transactional
systems — the outbox turns "one atomic cross-system operation" into "one
atomic same-system write, plus an at-least-once, idempotent relay," which
is achievable. TaskForge intentionally does not ship a relay
implementation: the relay's polling cadence, failure handling, and
outbox-table shape are the caller's own application concern, not a
TaskForge feature.

## Non-claims (explicit, for an enterprise reviewer's checklist)

TaskForge does **not** claim, via this phase or any other:

- a distributed transaction across two databases,
- two-phase commit,
- exactly-once execution of an arbitrary external side effect (see
  docs/idempotency.md — this was already true before Phase 11 and remains
  unchanged),
- cross-database atomicity of any kind.

## Package surface

`txenqueue` is intentionally small:

- `type Store struct{}` / `func New() *Store` — no database connection or
  other state of its own.
- `func (*Store) EnqueueTx(ctx, tx pgx.Tx, req EnqueueRequest) (*Job, bool, error)`
- `type EnqueueRequest struct{ PrincipalID, JobType, Payload, MaxAttempts, ExecutionTimeoutSeconds, IdempotencyKey, ScheduledAt }`
  — field-for-field the same contract as `POST /jobs` (docs/worker-protocol.md),
  validated by the exact same shared rules (`internal/job.ValidateSubmission`).
  `PrincipalID` is required as of Phase 12 — see the section below.
- `type Job struct{ ID uuid.UUID }` — a small, package-owned projection of
  the durable row's identity, **not** a type alias onto
  `internal/job.Job` (Phase 11 audit correction: an earlier draft of this
  package aliased the internal type directly, which would have coupled
  this package's public compatibility surface to `internal/job`'s entire
  durable row shape -- free to change as later phases add columns -- for
  no benefit, since a caller already knows the submission parameters it
  supplied and only needs the row's assigned identity back). A caller that
  needs a job's full current state fetches it the same way any other
  caller does: `GET /v1/jobs/{id}`, using `ID.String()`.
- Four public error sentinels (`ErrInvalidRequest`, `ErrInvalidTransaction`,
  `ErrMustRetryTransaction`, `ErrEnqueueFailed`, `txenqueue/errors.go`) --
  see "Error contract" above.

It is not `internal/store` exposed publicly (that package remains
unimportable outside this module by construction) and it is not a general
TaskForge client library or a replacement for the HTTP API — it is one
additional, narrow, optional entry point for the specific same-database
integration shape described above.

## Principal identity (Phase 12) — a breaking change

`EnqueueRequest.PrincipalID` is **required**. A zero value is rejected with
the existing `ErrInvalidRequest` sentinel (no new error type — this is an
invalid request, and that sentinel already means exactly that), and the
check runs alongside ordinary submission validation, **before `tx` is
touched at all** — so this method's documented "`tx` was never touched"
contract for `ErrInvalidRequest` holds unchanged. It is also checked
*before* the `nil`-`tx` check, so an invalid request against a `nil`
transaction is still reported as an invalid request, consistent with every
other validation-first path here.

**There is no default.** TaskForge does not resolve an unset `PrincipalID`
to a system principal, to "the first principal it finds," or to anything
else. A caller that cannot say who it is enqueuing for does not enqueue.
The system principal (`00000000-...-0001`) exists solely as the identity
that migration `0007` backfills pre-Phase-12 rows to; it holds no API key,
nothing can authenticate as it, and no application code path ever assigns
it, and it cannot be given one -- migration `0005`'s
`api_keys_no_system_principal` CHECK constraint and
`principal.Store.CreateAPIKey`'s guard both refuse. A test asserts this
package's source contains no reference to it at all, so a fallback cannot
appear by accident rather than by decision.

Idempotency is scoped to the principal accordingly: two different callers
using the same `JobType` and `IdempotencyKey` get two independent jobs
(`UNIQUE(principal_id, job_type, idempotency_key)`), and the
conflict-recovery re-read is principal-scoped too — a globally-scoped read
there would have handed one caller the other's job.

**Why this is a hard break with no compatibility shim**, stated as a
decision rather than an oversight: a repository-wide search found zero
callers of this package outside its own tests when Phase 12 was
implemented, and the project is labelled Experimental. A separately named
`EnqueueTxUnscoped`-style adapter that resolves to the system principal and
logs on every call is documented as a **contingency** for a real integrator
who genuinely cannot supply a principal yet — it is deliberately **not
built**, and if it ever is, it must be a function whose name says what it
does, never something reachable by accident through `EnqueueTx`.

The caller decides where `PrincipalID` comes from. A service handling its
own authenticated traffic should pass the principal it authenticated, not a
hardcoded constant.

## Cross-references

- docs/enterprise-roadmap.md Phase 11 — scope, proof obligations, exit
  criteria.
- docs/compatibility-policy.md — how this fits TaskForge's broader
  API/schema evolution posture.
- docs/idempotency.md — submission-idempotency semantics, unchanged and
  reused here.
- docs/invariants.md TF-INV-001, TF-INV-008, TF-INV-013, TF-INV-016 — the
  invariants this phase's proof obligations extend to the transactional
  entry point.
- `internal/store/tx.go`, `txenqueue/txenqueue.go`,
  `txenqueue/txenqueue_test.go` — implementation and executable proof.
