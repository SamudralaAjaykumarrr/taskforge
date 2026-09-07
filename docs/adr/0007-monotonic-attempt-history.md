# ADR-0007: Monotonic, Append-Only Attempt History

Status: Accepted

## Context

Debugging a failed or disputed job (e.g., "did this billing job actually
run twice?") requires trustworthy history of every attempt made against
it. We need to decide how attempt history is stored and mutated: as a
single mutable "last attempt" record on the job row, or as a durable,
independently-addressable history. See TF-INV-007 in
[invariants.md](../invariants.md).

## Decision

Every claim produces exactly one row in `job_attempts`
(`(job_id, attempt_number)` unique, 1-based, gapless), and that row is
never deleted and never has its identity-defining columns
(`attempt_number`, `lease_generation`, `worker_id`, `started_at`) mutated
after insert. Only the terminal fields of *that same attempt's own row*
(`finished_at`, `outcome`, `error_class`, `error_message`) are filled in
once, by the completion call for that specific attempt, itself fenced by
`lease_generation` (see [worker-protocol.md](../worker-protocol.md)). The
job row's own fields (`state`, `attempt_count`, etc.) are the current
summary; `job_attempts` is the immutable ledger behind that summary.

## Alternatives Considered

- **Store only the most recent attempt's outcome on the job row itself, no
  separate history table.** Rejected: this destroys exactly the audit
  trail the task requires (TF-INV-007, TF-INV-009) — an operator
  investigating a dead-lettered job would have no way to see *why* earlier
  attempts failed, only the last one, making root-cause diagnosis of
  intermittent failures much harder.
- **Store history as a JSON array column on the job row
  (`attempts jsonb[]`), appended to in place.** Rejected: mutating a JSON
  array column under concurrent/retried writes is harder to make correctly
  atomic and query-efficient than a proper relational table with its own
  primary key and indexes, and it does not naturally support the
  `UNIQUE (job_id, attempt_number)` constraint that gives TF-INV-007 its
  database-level enforcement (paralleling the reasoning in
  [ADR-0004](0004-idempotency-for-exactly-once-effects.md)/TF-INV-016
  about pushing invariants into constraints rather than trusting
  application discipline).
- **Allow attempt rows to be updated freely (not just their terminal
  fields) for correction purposes.** Rejected: "corrections" to historical
  attempt records are indistinguishable from silently rewriting history to
  hide a failure — exactly what TF-INV-007's "tamper-evident" property
  exists to prevent. If a recorded error message is wrong, that's a bug in
  the recording code, fixed going forward, not a reason to allow rewriting
  the past.

## Consequences

- Positive: `job_attempts` is a complete, independently queryable audit
  log — `GET /jobs/{id}/history` (a deferred but designed-for endpoint, see
  [worker-protocol.md](../worker-protocol.md)) is a simple indexed query,
  not a reconstruction from ambiguous partial data.
- Positive: The append-only property makes fault-injection testing simpler
  — a test can assert "this row's identity-defining columns are unchanged"
  as a straightforward equality check rather than reasoning about which
  fields were allowed to change.
- Negative: Slightly more storage over the life of a frequently-retried
  job (one row per attempt rather than one summary field) — accepted as
  the correct tradeoff for a project whose central value proposition is
  auditable reliability.

## Failure Implications

This decision is what makes TF-INV-009 ("dead-lettering preserves failure
history") meaningful rather than aspirational — without an append-only
ledger, "preserves history" would only mean "preserves the last error,"
which is a materially weaker and less useful guarantee for the operators
this project is built for (see [vision.md](../vision.md) primary users).
