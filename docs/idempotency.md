# Idempotency

Status: foundational. Separates two distinct concerns that are frequently
(and dangerously) conflated: making job **submission** safe to retry, and
making job **execution/side effects** safe to repeat.

## Two Distinct Kinds of Idempotency

### 1. Submission Idempotency (TaskForge's responsibility, fully solved in v1)

**Problem**: A client calls `POST /jobs` but the response is lost (network
timeout, client crash before reading the response). The client does not
know whether the job was created. If it naively retries, it risks creating
a duplicate logical job.

**Solution**: The client supplies an `Idempotency-Key` header. TaskForge
guarantees that within the key's scope, at most one job is ever created
(TF-INV-008), enforced by a database constraint, not application logic
(TF-INV-016).

**Scope**: `(job_type, idempotency_key)`. The key is scoped per job type
because the same key value used by two different callers/features for
different `job_type`s should not collide — there is no global idempotency
namespace. If a caller wants a broader scope, they are responsible for
constructing a sufficiently unique key themselves (e.g., prefixing with
their own domain).

**Exact behavior of `POST /jobs` with `Idempotency-Key: X`**:

1. First call: no existing row for `(job_type, X)`. INSERT succeeds. Job is
   created, its durable state returned.
2. Retried call (same key, same or different payload): the INSERT attempt
   hits the unique constraint. The API server catches the unique-violation
   error, re-reads the existing row by `(job_type, X)`, and returns **that**
   row's current representation with the same success status code the
   original request would have produced. **The retried call does not
   create a second job, and does not re-validate or compare the payload
   against the original** — the first submission with a given key wins,
   and TaskForge does not attempt to detect or reject a "conflicting" retry
   that supplies a different payload under the same key. This is a
   documented, deliberate simplification (see Open Questions) — callers
   are responsible for using a fresh key when they genuinely mean a
   different job.
3. Concurrent retried calls (two requests with the same key racing each
   other): the unique constraint ensures only one INSERT commits; the
   other observes the constraint violation and follows the same re-read
   path. No check-then-act race window exists because there is no
   preceding "check if exists" read — the INSERT is attempted directly.
   This is what makes TF-INV-008 hold even under true concurrency, not just
   sequential retries.

**No `Idempotency-Key` supplied**: every `POST /jobs` call creates a new
job. TaskForge does not deduplicate by payload similarity — that is a much
harder and fundamentally heuristic problem, explicitly out of scope.

### 2. Execution / Side-Effect Idempotency (the handler author's responsibility, enabled but not solved by TaskForge)

**Problem**: As established in [retry-semantics.md](retry-semantics.md) and
[vision.md](vision.md), a job may be retried after its side effect already
occurred (crash between side effect and acknowledgement). TaskForge cannot
prevent the handler from running twice — it can only make it possible for
the handler to make the *effect* happen once.

**What TaskForge provides**: stable, durable identifiers that a handler can
use as an idempotency token against downstream systems:

- `job_id` — stable across all retries of the same logical job. The natural
  choice for a handler that needs one idempotency token per logical job
  regardless of attempt count (e.g., "charge this customer for this
  invoice" should happen once per job, not once per attempt).
- `(job_id, attempt_number)` — unique per attempt. Useful if a handler
  wants to detect that a *specific* attempt is being replayed rather than
  reason about the whole job (rare; most handlers want the `job_id`-only
  token).

**Patterns a handler can use** (TaskForge does not implement these on the
handler's behalf, because they depend on the downstream system):

- **Downstream idempotency key**: pass `job_id` as an `Idempotency-Key` to a
  downstream HTTP API that itself supports idempotent requests (e.g., most
  payment processors). This pushes the exactly-once guarantee to a system
  that already solved it for its own domain.
- **Database unique constraint**: if the side effect is itself a database
  write the handler controls, a unique constraint on `job_id` in that
  table prevents a duplicate row from a replayed attempt.
- **Transactional outbox**: if the handler's side effect is "write a
  message that something else will eventually deliver," writing that
  message in the same transaction as a `job_id`-keyed dedup row makes the
  write-then-deliver step idempotent even if the handler itself is
  replayed. This pattern is documented here for completeness; TaskForge
  does not provide outbox infrastructure — it is the handler's own
  concern, potentially built on the same PostgreSQL instance.
- **Deduplication token table**: a handler-owned table
  `(job_id PRIMARY KEY, performed_at)` that the handler checks/inserts
  before performing its side effect, inside its own transaction if the side
  effect is transactional, or as a best-effort check if it is not (e.g., an
  outbound HTTP call cannot be made transactional with a local dedup check
  — the classic dual-write problem, which TaskForge does not solve because
  no system can solve it generically without the downstream side
  cooperating).

**What TaskForge explicitly does NOT claim**: it cannot guarantee an
arbitrary external side effect (an email send with no dedup support, a
non-idempotent third-party API call) happens exactly once. Any documentation,
README, or marketing material for this project that claims "exactly-once
execution" without qualifying it as "exactly-once *logical effect*, when the
handler cooperates" is wrong and should be corrected on sight — see
[vision.md](vision.md) core thesis and
[ADR-0003](adr/0003-at-least-once-execution-not-exactly-once.md)/
[ADR-0004](adr/0004-idempotency-for-exactly-once-effects.md).

## Summary Table

| | Submission idempotency | Execution/side-effect idempotency |
|---|---|---|
| Who solves it | TaskForge | The job handler author |
| Mechanism | `Idempotency-Key` + DB unique constraint | `job_id` as a token into downstream idempotency mechanisms |
| Guarantee strength | Absolute (TF-INV-008, DB-enforced) | Only as strong as the handler's own implementation |
| Failure mode if unused | Duplicate job rows from retried submissions | Duplicate external side effects from retried attempts |

## Cross-References

- Retry crash-timing analysis: [retry-semantics.md](retry-semantics.md)
- Invariants: TF-INV-008, TF-INV-016 in [invariants.md](invariants.md)
- Schema: `UNIQUE (job_type, idempotency_key)` in [data-model.md](data-model.md)

## Open Questions

- Should TaskForge validate that a retried submission's payload matches the
  original (and reject/warn on mismatch) rather than silently ignoring the
  new payload? Current v1 decision: no — first-write-wins with no
  comparison, to keep the constraint simple and avoid defining a payload
  equality semantics for arbitrary JSON. Revisit if real usage shows this
  causes confusing surprises.
- Idempotency key TTL/expiry (should old keys ever become reusable?) is
  undecided; v1 has no expiry, meaning a key is bound to its job row
  forever (rows are never deleted). This is acceptable at v1 scale and
  revisited if storage growth becomes a concern.

## Implementation Notes (Phase 4)

These are Phase 4 API-layer decisions this document did not previously
pin down. They are implementation choices, not new correctness contracts —
nothing here changes TF-INV-008/TF-INV-016 or the semantics above.

- **Header name**: `Idempotency-Key` (`internal/api`'s
  `idempotencyKeyHeader`), read case-insensitively per HTTP header
  semantics.
- **Empty/whitespace-only header**: treated as "no key supplied," not a
  validation error — operationally indistinguishable from a caller who did
  not mean to send one. Each such submission creates its own job, exactly
  as if the header were omitted entirely.
- **Maximum length**: 255 characters (`api.MaxIdempotencyKeyLength`),
  matching `job_type`'s existing bound. A longer value is rejected with
  `400 Bad Request` before any database call — the `idempotency_key`
  column itself is unbounded `TEXT` (see [data-model.md](data-model.md));
  this cap exists only to keep the key comparable/loggable at the API
  layer, not because the schema requires it.
- **Duplicate submission after a terminal state**: resubmitting the same
  key after the job it maps to has reached `SUCCEEDED`, `CANCELLED`, or
  `DEAD_LETTERED` still returns that job's current (terminal)
  representation — never a new job, and never an attempt to reopen the
  terminal row (TF-INV-005 is untouched: `InsertIdempotent` only ever
  performs an `INSERT` or a plain read, never an `UPDATE`).
- **How to demonstrate duplicate submission**: see
  `internal/store/idempotency_test.go`'s
  `TestInsertIdempotent_ConcurrentDuplicateSubmissions_SF005` (50+
  concurrent duplicate-key submissions, store level) and
  `internal/api/handlers_integration_test.go`'s
  `TestCreateJob_IdempotencyKey_ConcurrentDuplicates_ExactlyOneJobCreated`
  (same proof at the real HTTP boundary) and
  `TestCreateJob_IdempotencyKey_SequentialDuplicateReturnsSameJob` (the
  "submit, response lost, retry" case).
- **How to demonstrate exactly-once logical effect**: see
  `internal/worker/idempotency_test.go`'s
  `TestSF004_DuplicateExecutionWithoutIdempotency_EffectRunsTwice` (the
  limitation, made concrete and executable) paired with
  `TestSF004Companion_JobIDKeyedDedupTable_AvoidsDuplicateLogicalEffect`
  (the same crash sequence, but the handler's effect is deduplicated via a
  `job_id`-keyed table) — the exact "at-least-once execution + application
  idempotency = exactly-once logical effect" argument made in this
  document's "Execution / Side-Effect Idempotency" section above, proved
  rather than only asserted.
