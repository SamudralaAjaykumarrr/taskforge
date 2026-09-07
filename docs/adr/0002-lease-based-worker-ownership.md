# ADR-0002: Lease-Based Worker Ownership with Fencing Generations

Status: Accepted

## Context

Multiple workers compete for the same pool of jobs. We need a way to grant
exclusive, time-bounded ownership of a job to exactly one worker at a time,
that survives worker crashes without permanent manual intervention, and
that cannot be exploited by a worker that has actually lost ownership but
doesn't yet know it (e.g., after a long GC pause or VM suspend). See
[failure-model.md](../failure-model.md) F8, F11, F12, F20 and
[invariants.md](../invariants.md) TF-INV-002/003/004/014.

## Decision

Ownership is a **lease**: `(lease_owner, lease_generation, lease_expires_at)`
on the job row. A worker acquires a lease by claiming the job (which
increments `lease_generation`), must renew it via heartbeat before
`lease_expires_at` for long-running work, and every subsequent write
(heartbeat, completion) must present the exact `(lease_owner,
lease_generation)` it was issued, checked via a conditional `UPDATE ...
WHERE lease_owner = ? AND lease_generation = ?`. A worker whose lease has
expired and been reassigned a new generation is unconditionally fenced out
— its writes affect zero rows, regardless of how late they arrive.

## Alternatives Considered

- **Worker ID alone, no generation counter.** Rejected: a worker ID alone
  cannot distinguish "the current holder of this lease" from "a previous
  holder who happened to have the same ID reconnecting" or, more commonly,
  "the same worker process attempting a stale write after it should have
  already given up." A generation counter is what turns "is this the right
  worker?" into "is this the right *instance of ownership*?" — the latter
  is the actually-necessary check. This exact gap is why TF-INV-014 is
  stated as its own invariant distinct from TF-INV-003.
- **Distributed lock via external service (Redis `SETNX`, etcd lease,
  ZooKeeper ephemeral node).** Rejected for the reasons in
  [ADR-0001](0001-postgresql-as-source-of-truth.md) — it would split
  authoritative ownership state from job state across two systems that
  must agree, and PostgreSQL already provides everything needed
  transactionally.
- **No lease at all — hold a database transaction open for the entire job
  execution, using the transaction itself as the "lock."** Rejected
  explicitly and strongly: this was called out as unacceptable in the
  original task brief and is unacceptable in practice — it holds
  connections and locks for however long an arbitrary external side effect
  takes (seconds to potentially much longer), starving connection pools
  and making crash detection indistinguishable from "still legitimately
  working." Leases exist specifically so ownership can outlive a short
  claim transaction while execution happens outside any transaction. See
  [worker-protocol.md](../worker-protocol.md) "Why Not Hold a Transaction
  Open."
- **Timestamp-only leases without a generation counter (just check
  `lease_expires_at > now()`).** Rejected: this alone does not prevent the
  specific race where Worker A's write arrives in a narrow window where
  the lease *appears* still validly held by generation N in a naive
  timestamp-only check, but generation N+1 has already been issued. The
  generation counter is the precise, race-free fencing token; a timestamp
  comparison alone has TOCTOU gaps.

## Consequences

- Positive: The canonical "Worker A pauses, Worker B takes over, Worker A
  wakes up" scenario ([worker-protocol.md](../worker-protocol.md),
  [scenario-corpus.md](../scenario-corpus.md) SF-008) has a precise,
  testable, deterministic resolution.
- Positive: No external coordination service is needed; the mechanism is a
  handful of columns and a `WHERE` clause shape reused for every
  transition.
- Negative: Lease TTL tuning is a real operational tradeoff — too short and
  legitimate slow jobs get spuriously reclaimed (duplicate work risk, see
  [retry-semantics.md](../retry-semantics.md)); too long and a genuine
  crash takes longer to recover from (TF-INV-004's "bounded time" is
  directly set by this TTL). This tradeoff is not eliminated by the design,
  only made explicit and configurable per job via
  `execution_timeout_seconds`.
- Negative: A crashed worker cannot signal "I'm giving up" — recovery is
  always via timeout, never an explicit release, because a crashed process
  cannot signal anything by definition. A cooperative, non-crashed worker
  *can* explicitly release early in principle, but v1 does not implement a
  distinct "voluntary release" API — a worker that wants to give up simply
  reports a retryable failure through the normal completion path.

## Failure Implications

Every failure mode in [failure-model.md](../failure-model.md) involving
worker liveness (F1, F2, F9, F10, F11, F12, F13, F20) is handled by this
single mechanism. If the fencing check were ever bypassed or implemented
incorrectly (e.g., checking `lease_owner` but not `lease_generation`), the
system would silently regress into the exact split-brain completion bug
this ADR exists to prevent — this is why TF-INV-003 and TF-INV-014 are
both individually tested (see
[testing-strategy.md](../testing-strategy.md)) rather than assumed to be
implied by each other.
