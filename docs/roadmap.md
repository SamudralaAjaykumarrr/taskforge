# Roadmap

Status: foundational. Phased implementation plan. Each phase is scoped so
it can be built, tested, and reviewed incrementally — no phase requires
"finishing everything" before it can be verified against the invariants it
claims to satisfy.

## Maturity Labels

- **Foundation** — architecture/documentation only, no code. *(current
  status — see [README.md](../README.md))*
- **Experimental** — code exists, core happy path works, invariants for
  this phase are not yet fully tested.
- **Hardening** — invariants for this phase have test coverage; edge cases
  still being found and fixed.
- **Stable** — invariants for this phase are proven by the scenario corpus
  and have survived chaos/load testing without violation.

---

## Foundation — Architecture Only *(current phase)*

**Scope**: This document set. No application code.

**Non-goals**: Anything executable.

**Completion criteria**: All documents in [README.md](../README.md)'s docs
map exist, are internally consistent (state machine ↔ invariants ↔ failure
model ↔ roadmap), and contain no unresolved contradictions. ✅ as of this
writing — see the review notes accompanying this document set.

---

## Phase 1 — Single-Node Durable Job Engine

**Scope**:
- PostgreSQL schema: `jobs` table only (no `job_attempts` yet — deferred to
  make Phase 1 minimal, or included immediately if trivial; see note
  below).
- Submission API (`POST /jobs`, `GET /jobs/{id}`) — no idempotency key
  support yet.
- Durable state machine limited to `QUEUED → RUNNING → SUCCEEDED` and a
  simple `RUNNING → DEAD_LETTERED` on any failure (no retry machinery,
  no `RETRY_WAIT` yet — a failure goes straight to `DEAD_LETTERED`).
- A single worker process, synchronous claim-execute-complete loop, no
  concurrency hardening yet (single worker instance assumed).
- Basic lease fields exist in the schema (`lease_owner`,
  `lease_generation`, `lease_expires_at`) even though single-worker
  operation does not yet stress them — this avoids a schema migration
  between Phase 1 and Phase 2.

**Non-goals**: Retries, backoff, DLQ semantics beyond "failure = dead",
multiple concurrent workers, cancellation, scheduling delay, idempotency
keys, observability beyond basic logs.

**Required invariants**: TF-INV-001, TF-INV-005 (trivially, since there's
only one failure path), TF-INV-013.

**Required tests**: Unit tests for request validation; integration test for
submit → claim → complete happy path (SF-001); fault-injection test for
TF-INV-013 (SF-014, simplified to the single-worker case).

**Quality gates**: `go vet`/lint clean; integration tests pass against a
real PostgreSQL instance; no in-memory state holds anything
correctness-relevant (verifiable by restarting the process mid-test and
asserting state is unaffected — an early, minimal version of SF-018).

**Completion criteria**: A job can be submitted, executed once, and its
terminal state observed via `GET /jobs/{id}`, entirely through durable
state, surviving a restart of the single worker process between claim and
completion (job then requires manual re-submission at this phase, since
reclaim doesn't exist yet — this limitation is explicitly acceptable for
Phase 1 and closed in Phase 2).

**Maturity label on completion**: Experimental.

---

## Phase 2 — Worker Leases and Heartbeats

**Scope**:
- Lease acquisition via the real claim query
  ([worker-protocol.md](worker-protocol.md)), including
  `FOR UPDATE SKIP LOCKED`.
- Lease expiration and reclaim (the `RUNNING` + expired-lease branch of the
  claim query).
- Heartbeating for long-running jobs.
- Fencing via `lease_generation` on all completion/heartbeat writes.
- Multiple worker processes now supported (though concurrency *hardening*
  under adversarial load is Phase 5).
- `job_attempts` table introduced here if not already in Phase 1.

**Non-goals**: Retry backoff policy (still failure = dead-letter, but now
correctly reclaimed on crash before that point), idempotency keys,
scheduling delay, cancellation.

**Required invariants**: TF-INV-002, TF-INV-003, TF-INV-004, TF-INV-014,
TF-INV-015 (all newly meaningful once multiple workers/leases exist), plus
continued TF-INV-001, TF-INV-005, TF-INV-013.

**Required tests**: SF-002, SF-003, SF-006, SF-007, SF-008, SF-016, SF-017.

**Quality gates**: Concurrency test suite (N workers, M jobs) passes
deterministically, repeated runs; stale-completion rejection is
observable and logged.

**Completion criteria**: A worker crash mid-job is recovered by another
worker without operator intervention, and a stale worker's late completion
call is provably rejected (SF-008 passes reliably, not just once).

**Maturity label on completion**: Experimental → Hardening once SF-002/
003/006/007/008 pass consistently under repeated CI runs.

---

## Phase 3 — Retries, Backoff, DLQ

**Scope**:
- `RETRY_WAIT` state, backoff computation
  ([retry-semantics.md](retry-semantics.md)), `max_attempts` enforcement.
- Real `DEAD_LETTERED` semantics (distinct from Phase 1's "failure = dead"
  shortcut): retryable vs. permanent failure classification, attempt
  history preserved.
- Optional background sweeper for prompt DLQ transition on expired,
  attempt-exhausted leases (the optimization noted in
  [architecture.md](architecture.md), not required for correctness).

**Non-goals**: Idempotency keys, cancellation, scheduling delay beyond what
retry backoff already exercises, per-job-type backoff configuration (open
question in [retry-semantics.md](retry-semantics.md)).

**Required invariants**: TF-INV-006, TF-INV-007, TF-INV-009.

**Required tests**: SF-009, SF-010.

**Quality gates**: Property test confirming `attempt_count` never exceeds
`max_attempts` across randomized failure sequences.

**Completion criteria**: A job that fails repeatedly is retried with
correct backoff timing and correctly dead-letters with full history once
exhausted.

**Maturity label on completion**: Hardening.

---

## Phase 4 — Idempotency

**Scope**:
- `Idempotency-Key` support on `POST /jobs`, unique constraint enforcement.
- Documentation and worked examples of execution/side-effect idempotency
  patterns for job authors ([idempotency.md](idempotency.md)) — this is
  guidance, not a TaskForge-enforced mechanism, but the phase includes
  building a reference example job handler demonstrating the pattern.
- Adversarial concurrent-submission test suite.

**Non-goals**: Any attempt to enforce exactly-once side effects — this
phase's purpose is explicitly to cement (in tests and docs) that TaskForge
does not and cannot do this, while making submission idempotency airtight.

**Required invariants**: TF-INV-008, TF-INV-016.

**Required tests**: SF-005, SF-004 (specifically the "documents the
limitation" half of SF-004).

**Quality gates**: Concurrency test with 50+ simultaneous duplicate-key
submissions consistently yields exactly one job row.

**Completion criteria**: Duplicate submissions are provably deduplicated;
the duplicate-side-effect limitation is demonstrated by a passing test
(SF-004) rather than merely asserted in prose.

**Maturity label on completion**: Hardening.

---

## Phase 5 — Concurrency Hardening

**Scope**:
- Realistic multi-worker load: tens of concurrent workers against a shared
  job pool.
- Contention behavior under `SKIP LOCKED` at higher concurrency (verifying
  no lock convoy / starvation).
- Worker recovery drills under randomized crash injection (not just the
  single-crash scenarios of Phase 2, but repeated/overlapping crashes).

**Non-goals**: New features — this phase is about proving Phase 1–4
guarantees hold under load and adversarial timing, not adding capability.

**Required invariants**: Re-verification of TF-INV-002, 003, 004, 014
specifically under sustained concurrent load (not just crafted two-worker
scenarios).

**Required tests**: Extended/stress variants of SF-006, SF-007, SF-008.

**Quality gates**: No job claimed twice, no stale completion accepted, no
job stranded, across a sustained run (e.g., thousands of jobs, tens of
workers, randomized crash injection) with zero invariant violations.

**Completion criteria**: A long-running concurrency stress run completes
with all invariant assertions intact.

**Maturity label on completion**: Stable (for the single-job-engine core).

---

## Phase 6 — Scheduling, Cancellation, Timeouts

**Scope**:
- `scheduled_at`/`eligible_at` scheduling semantics
  ([scheduling.md](scheduling.md)).
- Cancellation endpoint and race resolution
  ([execution-semantics.md](execution-semantics.md) TF-INV-010).
- `execution_timeout_seconds` enforcement and the heartbeating long-running
  job path.

**Non-goals**: Workflow-level cancellation (Phase 7).

**Required invariants**: TF-INV-010, TF-INV-011.

**Required tests**: SF-011, SF-012, SF-013.

**Quality gates**: Deterministic race test (SF-012) passes under both
forced interleavings, repeatably.

**Completion criteria**: Scheduled jobs never execute early; cancellation
races resolve deterministically per the documented rule.

**Maturity label on completion**: Hardening → Stable once race tests are
proven flake-free across repeated CI runs.

---

## Phase 7 — Workflow/DAG Execution

**Scope**: Full implementation of [workflows.md](workflows.md):
`workflow_instances`/`workflow_nodes` tables, dependency-gated eligibility,
failure propagation, workflow-level cancellation.

**Non-goals**: OR/any-of fan-in, compensation/rollback edges, dynamic
workflow modification — all explicitly deferred per
[workflows.md](workflows.md) open questions.

**Entry criteria**: Phases 1–6 at Stable/Hardening maturity — workflows are
built on the single-job engine's guarantees and should not be started
before those guarantees are proven, per
[workflows.md](workflows.md) rationale.

**Required invariants**: TF-INV-012.

**Required tests**: New DAG-specific scenarios (fan-out, fan-in, diamond
dependency, failed-parent propagation, workflow cancellation) to be added
to [scenario-corpus.md](scenario-corpus.md) at the start of this phase.

**Quality gates**: A diamond-dependency workflow (A→B, A→C, B+C→D)
executes nodes in correct order under concurrent workers, with a failed
parent correctly cancelling dependents.

**Completion criteria**: TF-INV-012 has passing test coverage equivalent in
rigor to the single-job invariants.

**Maturity label on completion**: Experimental (workflows are new
surface area; do not inherit Stable from the underlying engine
automatically).

---

## Phase 8 — Observability

**Scope**: Full metrics/logging/tracing implementation per
[observability.md](observability.md).

**Non-goals**: Vendor-specific dashboards or alerting rules — this phase
produces the instrumentation, not an opinionated ops runbook.

**Required invariants**: None directly (observability doesn't create new
correctness properties) but every metric must be validated against the
scenario corpus (e.g., SF-007 must produce a
`taskforge_lease_expirations_total` increment).

**Required tests**: Metric-assertion tests attached to existing scenarios
rather than a new scenario set.

**Quality gates**: Every metric in [observability.md](observability.md) is
emitted and independently verified against at least one scenario.

**Completion criteria**: An operator can answer "how many jobs are
dead-lettered right now" and "what is our current claim latency" without
querying the database directly.

**Maturity label on completion**: Stable.

---

## Phase 9 — Chaos, Load, and Failure Testing

**Scope**: Sustained chaos testing (randomized, continuous fault injection
combining crashes, network delay, DB unavailability) and load testing
(sustained high job volume) run together over long durations.

**Non-goals**: New features.

**Required invariants**: All of them, simultaneously, under combined
stress — this phase exists specifically to find interaction effects
between invariants that isolated scenario tests might miss.

**Required tests**: A chaos-test harness driving randomized combinations of
every fault in [failure-model.md](failure-model.md)'s "handled in v1" list,
run for extended durations, with continuous invariant assertion (not just
end-of-run checks).

**Quality gates**: Zero invariant violations across a multi-hour chaos run;
documented throughput/latency numbers under load (no fabricated
benchmarks — numbers are only published once actually measured).

**Completion criteria**: The project can defend, with evidence, every claim
in [vision.md](vision.md) and [invariants.md](invariants.md) under
sustained adversarial conditions.

**Maturity label on completion**: Stable.

---

## Phase 10 — External-Review Hardening and v1.0

**Scope**: Address findings from external/skeptical review (the target
audience named in [vision.md](vision.md)); close any open questions still
outstanding across the docs; finalize v1.0 API stability guarantees.

**Non-goals**: New capability beyond what review findings require.

**Required invariants**: All — this phase is a hardening pass, not a
feature phase.

**Required tests**: Whatever the review process surfaces as missing.

**Quality gates**: No open critical/high-severity findings from review
remain unaddressed or undocumented as an explicit, deliberate non-goal.

**Completion criteria**: v1.0 tag — a system whose documented guarantees
match its tested behavior, reviewable by the audience defined in
[vision.md](vision.md).

**Maturity label on completion**: Stable, v1.0.

---

## Phase Dependency Summary

```
Foundation -> Phase 1 -> Phase 2 -> Phase 3 -> Phase 4 -> Phase 5
                                                              |
                                                              v
                                          Phase 6 -----> Phase 7
                                             |               |
                                             v               v
                                          Phase 8 -------> Phase 9 -> Phase 10
```

Phases 1–5 are strictly sequential (each depends on guarantees proven by
the previous one). Phase 6 depends on Phase 5 (concurrency-hardened base).
Phase 7 depends on Phase 6 being at least Hardening maturity. Phases 8 and
9 apply across whatever functional scope exists at the time and are
revisited after Phase 7, not strictly blocked by it — observability work in
particular can and should start incrementally alongside earlier phases in
practice, even though it is formally sequenced last for documentation
clarity.
