# ADR-0009: Phase 13 Concurrency Limiting and Fairness (OD-1)

Status: Accepted

## Context

[enterprise-roadmap.md](../enterprise-roadmap.md) "Phase 13 — Workload
Governance + Retention" requires per-queue/tenant concurrency limits and a
fairness guarantee, but is deliberately explicit that it does **not**
pre-commit to an algorithm: *"specify the required fairness/liveness
property first... and require an ADR to select the simplest correct
PostgreSQL-native scheduling algorithm only after a concurrency and
performance analysis... Hatchet's group-key round-robin is one candidate
worth evaluating in that ADR, not a foregone conclusion."* The roadmap's
own fairness property text: *"no queue/tenant with pending capacity-
eligible work is starved for longer than a documented bound while another
queue/tenant is making progress."* This is one of two hard blockers
[phase-13-plan.md](../phase-13-plan.md) §18 names before any Phase 13
implementation PR may open (the other, OD-3, is already resolved — see
[invariants.md](../invariants.md) "Cross-Phase Governance Additions").

[phase-13-plan.md](../phase-13-plan.md) §6a — planning only, not this
ADR — narrows the field to three PostgreSQL-native concurrency-limit
candidates and two fairness candidates, in increasing order of departure
from the existing claim-query idiom (`internal/store/claim.go`'s `SELECT
... FOR UPDATE SKIP LOCKED`):

- **Concurrency limiting**: (1) a per-queue `pg_advisory_xact_lock`, (2) a
  slot-table semaphore (a `queue_slots` row per unit of configured
  capacity, claimed via the same `FOR UPDATE SKIP LOCKED` idiom as job
  selection), (3) `SERIALIZABLE` isolation with client-side retry.
- **Fairness**: (A) Hatchet-style group-key round robin, (B) ordering
  claim candidates by each queue's own `last_claimed_at` ascending.

This ADR selects among exactly these candidates — it does not introduce
new architecture, per the roadmap's own instruction and this repository's
[docs/adr/README.md](README.md) convention that an ADR records a decision
actually weighed against a real, already-identified alternative.

**Evidence base**: three measurement passes, each correcting defects the
previous one's own review found, culminating in an independent review
that returned **ADR READY**:

1. [phase-13-concurrency-evidence.md](../phase-13-concurrency-evidence.md)
   ("v1") — the first pass; **superseded**, contains invalid fairness and
   performance conclusions (query-plan defects that misattributed cost),
   kept unedited for provenance.
2. [phase-13-concurrency-evidence-review.md](../phase-13-concurrency-evidence-review.md)
   — an independent review of v1, verdict **REVISE EVIDENCE**, naming six
   specific defects (an unindexed cross-table sort mischaracterized as
   "lock contention," a production baseline measured against the wrong
   index, undercounted `SERIALIZABLE` retries, an advisory-lock
   round-trip-count confound, an untested `LIMIT 50` cap, and others).
3. [phase-13-concurrency-evidence-v2.md](../phase-13-concurrency-evidence-v2.md)
   ("v2") — every REVISE-EVIDENCE finding fixed and re-measured, each
   verified via `EXPLAIN (ANALYZE, BUFFERS)` against this project's actual
   PostgreSQL 16 instance before being trusted. Reversed the round-robin
   lean the review pass's own preliminary numbers had suggested.
4. [phase-13-concurrency-evidence-v3.md](../phase-13-concurrency-evidence-v3.md)
   ("v3") — the dedicated TF-INV-019 bound sweep: 46 runs (18 queue-count,
   18 capacity-configuration, 10 adversarial) against the recommended
   integrated mechanism, deriving and empirically confirming the bound
   this ADR adopts.
5. A final independent review of v3 (conducted in-session, not a separate
   committed document) verified the 46-run dataset directly against
   `results-v3-sweep.json`, re-derived the bound's proof from
   `tools/phase13bench/claim_fairness.go` rather than accepting the
   write-up's prose, and returned **ADR READY**, with two precision
   corrections to how this ADR must state the bound (both incorporated
   below — see "Exact fairness semantics").

`tools/phase13bench/` (the harness behind all three passes) is
analysis-only: not part of TaskForge's build, not imported by any
`internal/` or `cmd/` package, never touches the real `jobs` table, and
remains uncommitted, per every pass's own explicit instruction not to
stage evidence-gathering code as production.

## Decision

1. **Concurrency limiting**: the **slot-table semaphore**.
2. **Fairness**: **`last_claimed_at` ascending** ordering.
3. **TF-INV-019**: a **service-opportunity bound of E − 1**, not a
   wall-clock SLA — see "TF-INV-019: exact definition" below for E's
   precise meaning.
4. **Reclaim/slot ownership**: a reclaimed job **retains and reuses** its
   original capacity-slot binding; reclaim never triggers new slot
   acquisition.

Each is justified on its own below, with the rejected alternatives and
their specific failure modes in "Alternatives Considered."

### Concurrency mechanism: slot-table

**Selected.** A new table, `queue_slots(queue_name, slot_index,
held_by_job_id)`, with exactly one row per unit of a queue's configured
`concurrency_limit`. A fresh claim atomically binds a job to a free slot
in the same `FOR UPDATE SKIP LOCKED` statement that selects the job
(candidate-job-row × free-slot-row cross join yields zero rows, so the
claim fails closed — not partially — if no slot remains); a reclaim never
touches this table at all (see "Reclaim/slot-ownership semantics").

**Why**: measured across v2 and v3 (`tools/phase13bench/claim_concurrency.go`,
`experiments.go`, `sweep.go`):

- **Exact enforcement in every tested configuration.** Zero
  concurrency-limit violations across every exactness run in v2 (four
  candidates, capacity 5, 30 workers, mixed fresh/reclaim load) and every
  one of v3's 46 integrated runs (capacity 1–10, workers 3–20, queue
  count 2–60, including five adversarial scenarios) — verified by direct
  query against the raw result files in this ADR's final review, not
  taken from the write-up alone.
- **Strongest throughput among the three viable candidates**: peaked at
  ~1,974 claims/s (25 workers, single hot queue), 2.4–2.8× either
  advisory-lock variant's plateau (v2 §D2), confirmed not to be a
  round-trip-count artifact by comparing against an optimized,
  round-trip-matched advisory-lock implementation built specifically to
  control for that confound (v2 §A3).
- **Avoids the advisory-lock convoy.** `pg_locks` sampling under
  40-worker contention showed 35.3 of ~37.6 active connections
  continuously waiting on the advisory lock (v2 §D3) — a structural
  property of a single session-level lock serializing all admission
  decisions for a queue, independent of the configured limit's actual
  value. Slot-table showed 0.10–0.18 average waiting locks under
  identical load.
- **Avoids `SERIALIZABLE`'s abort/retry waste.** Corrected accounting
  (every retry counted, not a single boolean per successful call — v2
  §A5) shows a true abort rate of 62–97%, rising with concurrency, with
  throughput *falling* as workers increase — the opposite of a usable
  concurrency mechanism under the exact flooding scenario Phase 13
  exists to defend against.
- **Zero violations under the full integrated combination** (slot-table
  capacity limiting + `last_claimed_at` fairness + reclaim, together, at
  every v3 configuration) — the concurrency proof holds not just in
  isolation but combined with everything else this ADR selects.

**Operational cost, accepted**: every terminalization call site
(`CompleteSuccess`, `CompleteFailure`, `CompleteRetryableFailure`,
`CompleteCancelled`, `CompleteTimeout`, the Lazy Dead-Letter Sweep) must
release its held slot in the same transaction that terminalizes the job —
an enumerable, testable new responsibility, not an open-ended one (see
"Required implementation proof obligations"). Isolation between
queues is measured as 5–13% throughput degradation under concurrent
hot-queue flooding, worse than advisory lock's ~0% (v2 §D7/§G) —
reproducible across five repeated runs but not mechanistically isolated
via a dedicated micro-benchmark; a real, accepted cost, not a reason to
reverse the selection (see "Consequences").

### Fairness mechanism: `last_claimed_at` ascending

**Selected.** One new column, `queue_state.last_claimed_at` (or its
production-schema equivalent — see [phase-13-plan.md](../phase-13-plan.md)
§7 for the strawman `queue_limits`/`rate_limit_buckets` shape this ADR's
`queue_state` concept should be reconciled with at implementation time).
The claim query orders eligible candidates by this value ascending,
selecting the queue least recently served; a successful claim (fresh or
reclaim — see below) updates it to the current time.

**Why**:

- **The corrected, indexed implementation avoids v1's invalid
  cross-table-sort defect.** v1 ordered by a column on a *joined* table,
  which PostgreSQL cannot satisfy from any index on the jobs table —
  confirmed via `EXPLAIN` to force a full sequential scan and (at scale)
  a disk-spilling sort on every claim attempt, which the independent
  review found had been misattributed as "row-lock contention." v2's
  corrected implementation (`claim_fairness.go`'s `perQueueCandidate` +
  `claimFairnessV2`) fetches each queue's own best candidate via a
  `LATERAL` subquery restricted to that queue — confirmed via `EXPLAIN`
  to use the intended per-queue index, costing O(subscribed queues), not
  O(backlog size) — before any conclusion was drawn from it.
- **Round-robin's failure mode is real and reproduced, not a fluke of one
  run.** The evaluated round-robin implementation — a faithful,
  correctly-indexed rendering of "compare each queue's own oldest pending
  candidate, serve whichever is globally oldest," the natural
  PostgreSQL-native reading of a group-key round robin — is vulnerable to
  **backlog-age dominance**: a continuously flooded queue's own oldest
  unclaimed row ages without bound for as long as claim throughput trails
  the flood's insert rate, eventually beating any periodically-served
  sparse queue's much-younger single pending item. Measured at 2 queues:
  sparse-queue p50 wait 1.8–2.5 *seconds* under round-robin vs. 3–4
  *milliseconds* under `last_claimed_at` (v2 §D5/§F, 6/6 runs, reversing
  the review pass's own earlier, differently-confounded reading). At 60
  queues the gap **widens**, not narrows: round-robin left 59 of 59
  sparse queues simultaneously stalled at some sampled point in every
  run.
- **`last_claimed_at` resets fairness state on every service event**,
  independent of how old or deep any competing queue's backlog has
  grown — this is precisely why it does not share round-robin's failure
  mode, confirmed by the E−1 proof below, which depends only on
  `last_claimed_at` strictly increasing on service, never on backlog age.
- **Measured together with slot-table concurrency limiting and reclaim**,
  not asserted from a separate half-experiment: v2 §D6 (6 integrated
  runs, 2 shapes) and v3's full 46-run sweep both exercise the combined
  mechanism directly.

### TF-INV-019: exact definition

**The bound is a service-opportunity count, E − 1 — not a wall-clock
time.** This is deliberate: v3 §3/§4 show wall-clock wait scales clearly
and non-trivially with queue count and configuration, so no fixed
millisecond number is defensible as an invariant (see "Wall-clock
interpretation" below for why a number still matters operationally, just
not as the invariant's own text).

> **TF-INV-019 (concrete)**: a queue or tenant with pending,
> capacity-eligible work — within a single worker-subscription pool — is
> served within at most **E − 1** other eligible queues' service
> opportunities, where E is defined precisely as follows.

**Worker-subscription pool.** The set of workers whose `TASKFORGE_WORKER_
QUEUES` subscriptions cause them to compete for the same job rows —
practically, the set of workers passed the same (or overlapping) queue
list as the `subscribed` parameter to the claim query. For the default,
common case (`TASKFORGE_WORKER_QUEUES` unset, subscribing to everything
per [phase-13-plan.md](../phase-13-plan.md) §14), every worker in the
deployment forms **one** pool, and E is deployment-wide. **This bound is
evidenced only within one such pool.** A deployment that partitions
workers into disjoint or differently-overlapping subscription sets
creates multiple, potentially-interacting pools that this ADR's evidence
does not cover — see "Known limitations," and do not read this ADR as
claiming the bound for that topology.

**E, precisely**: the number of distinct `queue_name` values within one
worker-subscription pool that are, or become, simultaneously part of that
pool's fairness roster — counted as a **static roster size**, not a
live/instantaneous "currently eligible" count. This distinction is
load-bearing: the E−1 proof (below) holds because any other queue can be
credited against a given queue's wait **at most once**, no matter how
many times that other queue's own eligibility toggles in the meantime —
so the quantity that actually bounds the wait is "how many distinct
queues were ever eligible and served during this wait," which is safely
upper-bounded by the pool's total queue count, not by whatever happens to
be eligible at any single instant. A durable invariant check implemented
against a live/fluctuating count would be checking the wrong quantity.

**Roster membership — entry**: a queue enters the roster the instant it
has at least one row that is both **pending** and **capacity-eligible**
(both defined next), within the pool's subscription scope.

**Roster membership — exit**: a queue leaves the roster the instant it no
longer has any such row — either its last eligible row was claimed (no
pending work remains) or it lost capacity eligibility (no free slot, and
its current best candidate is not itself a reclaim, which needs none).

**"Pending"**: a job row in state `QUEUED` with `eligible_at <= now()`,
or in state `RUNNING` with an expired lease (`lease_expires_at < now()`)
and `attempt_count < max_attempts` — TaskForge's existing two-branch claim
eligibility (`internal/store/claim.go`'s `claimQuery`), unchanged by this
ADR.

**"Capacity-eligible"**: for a `QUEUED` candidate, the queue currently has
at least one free slot in `queue_slots`. For a `RUNNING` (reclaim)
candidate, capacity-eligibility is automatic and unconditional — see
"Reclaim/slot-ownership semantics": a reclaim never needs a *new* free
slot, because it already holds the one it was admitted under.

**"Service opportunity"**: exactly one successful claim — fresh or
reclaim, no distinction — from any roster queue: one commit of the claim
transaction that transitions a job to `RUNNING` and updates that queue's
`last_claimed_at` to the commit time.

**When the E−1 counter begins, and resets**: begins the instant a queue
enters the roster, with its current `last_claimed_at` value fixed at
whatever it already was (see "newly eligible queue" below for a queue
with no prior value). It increases by one, for a given other roster
queue, the first time that other queue has a service opportunity after
this queue's `last_claimed_at` was last set — and by construction, a
given other queue can only ever increment this counter once per waiting
streak (proof below). It resets to zero the instant this queue itself
receives a service opportunity.

**Temporary capacity ineligibility — accounting effect**: if the waiting
queue Q itself temporarily loses capacity eligibility mid-wait, Q's own
`last_claimed_at` is untouched (only a real service event changes it), so
Q's position in the ordering is exactly preserved; Q is simply not a
candidate during that window, and no service opportunity is credited
against or attempted for it then. If a *different*, competing queue R
loses and regains capacity eligibility during Q's wait, R still counts
**at most once** against Q's bound — R's first post-wait-start service
(whenever it happens) is the only one that can count, since R's own
`last_claimed_at` is thereafter newer than Q's fixed value regardless of
R's later eligibility state.

**Newly eligible queue — how it begins accounting**: a queue with no
prior `last_claimed_at` (never served before, or newly configured)
defaults to the earliest possible value (`-infinity`, per this project's
own tested schema default). It is therefore immediately the most-stale
roster member the instant it becomes pending and capacity-eligible, and
is served next among ties — its very first turn costs it zero E−1 wait,
since nothing can be "more stale" than a queue that has never been
served.

**Proof (re-stated precisely, matching the final review's re-derivation,
not v3's original, slightly looser phrasing)**: while queue Q remains
continuously in the roster with a fixed `last_claimed_at` value v, any
other roster queue R served at time T > (the moment v was set) receives a
new `last_claimed_at` = T > v — strictly larger, so R cannot be selected
ahead of Q again while v remains fixed (the query always selects the
globally smallest value among roster candidates). Once every other roster
queue has had one such service since v was set, all of them have values
> v, and Q — uniquely smallest — must be selected next. Reclaims
participate in this identically to fresh claims (the `last_claimed_at`
update in `claim_fairness.go` is unconditional on claim type) — **a
reclaimed queue's turn consumes exactly one service opportunity, exactly
like a fresh claim, and neither creates a separate accounting channel nor
exempts that queue from consuming its own turn.**

**Empirical confirmation**: a purpose-built metric
(`MaxOtherQueuesServedBetweenTurnsSparse`, `tools/phase13bench/sweep.go`)
counted this quantity directly from the true ordered sequence of served
queues — not inferred from wall-clock wait — and it equalled exactly
E − 1, with zero exceptions, across all 46 runs of v3's sweep: 18
queue-count-sweep runs (2–60 queues), 18 capacity-configuration runs
(capacity 1–10, workers 3–20), and 10 adversarial-stress runs (an
overwhelmingly hot queue, more than 50 queues, a worker pool that changes
size mid-run, a queue that becomes eligible only mid-run, and a queue
that temporarily loses and regains capacity eligibility). Re-verified
directly against `results-v3-sweep.json` as part of this ADR's own
authorship, not reused from the write-up without re-checking.

### Wall-clock interpretation

**TF-INV-019's invariant text is E − 1 service opportunities. It is not,
and must not be read as, a millisecond number.** Converting the
service-opportunity count into wall-clock time requires multiplying by
the deployment's own achieved time-per-claim-round, which v3 §3 measured
as **not constant** — it grew roughly 7× (2.55ms to 18.38ms per
opportunity) as queue count rose 30× (2 to 60) in that pass's own
environment, for two identified, mechanistic (not merely correlational)
reasons: the per-claim candidate-selection query costs O(number of
roster queues) per attempt (one `LATERAL` evaluation per queue), and a
fixed-size worker pool's attention dilutes as queue count rises.

> **Formula, not a constant** (v3 §5b, adopted verbatim as this ADR's own
> operational guidance): wall-clock wait ≈ **(E − 1) × r**, where r is
> the deployment's own measured mean time between successful claims under
> its actual roster size, capacity configuration, and worker pool — never
> a number this ADR or the evidence package supplies on a deployment's
> behalf. An operator or SLA wanting a millisecond figure must measure r
> against their own production-representative configuration.

`docs/phase-13-concurrency-evidence-v3.md`'s specific millisecond figures
are single-node-WSL2-VM numbers (§C of v2, unchanged caveats) and must
not be cited as production capacity-planning numbers by this ADR, its
implementation, or any operator documentation derived from it.

### Reclaim/slot-ownership semantics

**Decision: a reclaimed expired job retains and reuses its previous
capacity-slot ownership (option A). It never acquires a new slot through
the same allocation path as a fresh claim (option B is rejected).**

`internal/store/claim.go`'s existing claim query already treats a
reclaim (an expired-lease `RUNNING` row with attempt budget remaining) as
one branch of the same eligibility check as a fresh `QUEUED` claim, both
producing an admission to `RUNNING` under a new `lease_generation` —
unchanged, unmodified by this ADR. What Phase 13 adds is the question of
what happens to that job's **capacity accounting** across the reclaim.

**Why A, not B**: a reclaimed job never stopped occupying one unit of its
queue's concurrency budget — only its owning worker crashed. The job is a
**continuation of the same admission** under a new lease generation, not
a new admission event. Option B — requiring a reclaim to acquire capacity
through the identical slot-allocation path as a fresh claim — would be
wrong in two concrete, evidenced-against ways: (1) if the job's original
slot binding is not first released, the same logical job could end up
counted against two slots, silently shrinking effective capacity over
time — precisely the "slot leak" failure mode
[phase-13-plan.md](../phase-13-plan.md) §19 already names for
terminalization call sites, now shown by this evidence pass to apply
identically to the reclaim path if implemented carelessly; (2) if the
original slot is explicitly released first, there is a real window in
which the job is admitted (`RUNNING`) but holds zero slots — a
capacity-accounting inconsistency with no compensating benefit, since the
concurrency invariant (never more than the configured limit concurrently
`RUNNING`) does not require reclaim to go through fresh admission at all
— it only requires the running count never exceed the limit, which option
A satisfies trivially: the count never changes across a reclaim, because
the job was already counted.

**How reclaim participates in E − 1 fairness accounting**: identically to
a fresh claim (see "TF-INV-019: exact definition" above) — a reclaim
still updates `last_claimed_at` and still consumes exactly one of the
queue's own service opportunities. Slot-ownership semantics and fairness
accounting are independent concerns; this ADR's choice on the former does
not change the latter (the E−1 proof references only `last_claimed_at`
values, never slot state).

**Lease ownership vs. capacity-slot reservation — kept distinct**: this
ADR's mechanism has two separate pieces of state that must not be
conflated. **Lease ownership** — `(lease_owner, lease_generation,
lease_expires_at)` — is about *which worker* may currently act on a job,
governed entirely by [ADR-0002](0002-lease-based-worker-ownership.md),
unmodified by this ADR; it changes on every claim and every reclaim.
**Capacity-slot reservation** — the `queue_slots` binding — is about
*whether the queue has room* for this job at all, and under Option A it
changes far less often: once on admission, once on the job's actual
terminal transition, never in between. A reclaim changes the first and
never touches the second. Confusing the two — treating a lease-generation
change as if it were itself a capacity event — is exactly the mistake
Option B would make.

**Stale slot ownership after lease expiry/crash — corrected**: a slot
held by a job that is merely awaiting reclaim (expired lease, attempt
budget remaining) is not stale — the job is still logically in flight,
and the *next* successful reclaim keeps it attributed to the same job id,
exactly as described above. **But this is not the only way a `RUNNING`
job with an expired lease can end up in front of the claim query.** If
the job's attempt budget is exhausted (`attempt_count >= max_attempts`)
while its lease is expired, `internal/store/claim.go`'s existing Lazy
Dead-Letter Sweep — not the reclaim branch — moves it straight to
`DEAD_LETTERED` ahead of any claim attempt
([phase-13-plan.md](../phase-13-plan.md) §7/§17 already name this call
site as needing a slot-release step once slot-table is selected). This
path is **not** "already required, unchanged by this ADR" the way
ordinary terminalization is: **`queue_slots` does not exist before Phase
13, so a durable, idempotent slot release on the sweep's own transition
is a genuinely new requirement this ADR's mechanism introduces at that
specific call site, and it was never built, exercised, or measured by
this evidence pass at any point across v1, v2, or v3.** Left unaddressed,
Option A's own logic ("the slot stays with the job until it reaches an
actual terminal state") is satisfied *literally* but not *safely*: the
job does reach `DEAD_LETTERED`, but if that transition's own release step
is missing or non-atomic with it, the slot is permanently orphaned —
capacity silently and durably lost for that queue, with no future event
that will ever release it. This is not a hypothetical: this pass's own
disposable benchmark database — which never implemented the sweep at
all — was found, on direct inspection during the final independent
review, holding 22 jobs in exactly this state (`RUNNING`, lease expired
by tens of minutes, `attempt_count == max_attempts`), with every slot in
that run's 8 queues consumed as a result. That failure belongs to the
benchmark's own incompleteness, not to Option A's design — but it is
concrete proof that the sweep-releases-its-slot step cannot be assumed
correct by inheritance; it is a new, specific requirement this ADR
imposes and Phase 13 implementation must build and test explicitly (see
SF-058 below). Every *other* terminalization path (success, failure,
cancellation) is unaffected by this correction — those were always
reached through the ordinary completion API, already covered by the
existing "every terminalization call site" language.

**Proof obligation implementation must satisfy**: (1) the reclaim branch
of the production claim query must never issue a write to the slot table
(or its equivalent) — an automated test must assert this directly, not
merely observe zero violations under load (SF-052); (2) the Lazy
Dead-Letter Sweep's own transition to `DEAD_LETTERED` must release the
swept job's slot **durably and idempotently, in the same transaction as
the state transition** — durable, so a crash between the sweep's commit
and any later inspection cannot leave the slot ambiguous; idempotent, so
a sweep that runs again (e.g., after a crash and restart) against a job
already `DEAD_LETTERED` does not attempt to release an already-free slot
in a way that could corrupt a *different* job's later, legitimate claim
on that same slot index — an explicit new obligation, SF-058, not implied
by SF-052 alone; (3) since the benchmark's own load-based testing checked
only the aggregate `count(RUNNING) <= limit`, the real implementation
must assert the stronger, per-row invariant `count(RUNNING) ==
count(held slots)` directly (SF-053, strengthened below) — an aggregate
match can hide two offsetting errors (one job wrongly holding two slots,
another wrongly holding none), which a per-row check cannot.

### Compatibility behavior

Unchanged by this ADR, explicitly preserved:

- **Workers with no queue-subscription configuration** (`TASKFORGE_WORKER_
  QUEUES` unset) retain today's exact queue-blind behavior — claiming
  from every queue, including reclaiming every queue's expired leases —
  matching [phase-13-plan.md](../phase-13-plan.md) §14's compatibility
  requirement. This ADR's concurrency/fairness mechanisms are additive
  predicates on the existing claim query's selection, not a replacement
  of it.
- **§6b's reclaim/queue-subscription rule** (a worker's subscription
  filter applies identically to a fresh claim and a reclaim of an
  expired lease) is unchanged and was independently re-verified against
  the corrected schema in v2's correctness-check suite
  (`reclaim_respects_subscription_boundary`,
  `reclaim_succeeds_for_subscribed_worker`, both passing).
- **TF-INV-001 through TF-INV-018 are structurally untouched.** The
  entire evidence base's `bench_jobs` schema (every fairness/concurrency
  experiment across all three passes) has no `lease_generation` column
  at all — confirmed by direct schema inspection during this ADR's final
  review — meaning none of this evidence line's claim/reclaim/fairness
  mechanics can touch TF-INV-002/003/014's fencing mechanism; only the
  separate, unmodified `prod_jobs` reconstruction (used solely to measure
  today's actual baseline, §D4 of v2) models `lease_generation` at all,
  and it reuses `internal/store/claim.go`'s query verbatim. This is
  **confirmed by inspection, not proven exhaustively** — the claim is
  scoped exactly that far, not further.
- **Phase 12's principal isolation, tenant-scoped idempotency,
  authentication, and least-privilege database roles** are not touched
  by this ADR. Concurrency/fairness limits are an admission-selection
  concern layered in front of the existing engine, exactly as Phase 12's
  authentication sits in front of it ([phase-13-plan.md](../phase-13-plan.md)
  §9): a principal denied capacity by a concurrency cap is throttled, not
  unauthorized, and this ADR does not conflate the two in any shared code
  path, consistent with that section's own requirement.

## Alternatives Considered

### Advisory lock (`pg_advisory_xact_lock` per queue) — rejected

- **Correctness**: exact — zero concurrency-limit violations measured
  (v2 §D1), same as every candidate. Not rejected on correctness grounds.
- **Contention behavior**: a genuine, measured convoy. 35.3 of ~37.6
  active connections continuously waiting on the lock under 40-worker
  contention (v2 §D3, `pg_locks` sampling, not inferred) — a structural
  property of a single session-level lock serializing every admission
  decision for a queue, unrelated to the configured limit's value. A
  queue's *realized* concurrency is capped by lock serialization, not by
  the operator's configured `concurrency_limit` — directly undermining
  the roadmap's own "per-queue/tenant-scoped concurrency limits" goal as
  a performance property, not just a throughput inconvenience.
- **Performance evidence**: plateaus hard at 600–840/s past 5–10 workers
  regardless of added capacity (v2 §D2), 2.4–2.8× below slot-table's
  peak, confirmed not to be a round-trip-count artifact via a dedicated
  optimized variant built specifically to rule that out.
- **Operational complexity**: lowest of the three — no new schema, no new
  terminalization responsibility. This is real and was weighed; it does
  not outweigh the structural throughput ceiling for the general-purpose
  choice, though it remains a legitimate narrow option for a deployment
  with many low-volume queues where the convoy never binds — not selected
  here because Phase 13's stated P0 threat model
  ([security-model.md](../security-model.md) §1, "tenant starvation") is
  exactly a hot, flooding queue, the scenario where advisory lock performs
  worst.

### `SERIALIZABLE` isolation with retry — rejected

- **Correctness**: exact in principle (textbook serializable-snapshot
  isolation) and exact in every measured run — not rejected on
  correctness grounds.
- **Contention behavior**: the dominant cost is **invisible to
  `pg_locks`** — a serialization failure is a commit-time abort, not a
  wait state. The corrected counter (v2 §A5, every retry counted) shows
  62–97% of attempted transactions abort, rising with concurrency.
- **Performance evidence**: throughput **falls** as worker count rises
  (peaking at 5 workers, then monotonically declining) — the opposite
  direction of both other candidates, and specifically the wrong
  direction under Phase 13's own flooding threat model.
- **Operational complexity**: requires a retry loop on the single hottest
  query in the system, a class of failure mode (retry-storm amplification
  under exactly the load Phase 13 exists to defend against) this codebase
  has no prior precedent for and this ADR does not want to introduce one
  for.

### Round-robin fairness (group-key / backlog-age ordering) — rejected

- **Correctness**: not a correctness rejection — round-robin never
  produced a concurrency-limit violation or admitted more than the
  configured capacity.
- **Failure mode, confirmed real and reproduced**: backlog-age
  dominance under sustained flooding (see "Fairness mechanism" above) —
  6/6 reproduced runs at two queue counts, the gap *widening* rather than
  narrowing as queue count grows, the opposite of what a fairness
  mechanism should do at scale.
- **Not overstated**: the specific implementation evaluated is a
  faithful, correctly-indexed rendering of the natural PostgreSQL-native
  reading of group-key round robin (compare each queue's own oldest
  candidate, serve globally-oldest) — not a strawman. A genuine
  turn-taking round robin with explicit, persistent per-queue rotation
  state is a materially different, more expensive design that was never
  built or tested here; this ADR rejects the round-robin candidate **as
  evaluated**, not every conceivable round-robin-flavored design. If a
  future ADR wants to revisit round-robin specifically (e.g., to match
  Hatchet's own behavior more closely for a stated reason), it needs its
  own implementation and its own measurement, not a re-reading of this
  evidence.

## Evidence

Full data: [phase-13-concurrency-evidence-v2.md](../phase-13-concurrency-evidence-v2.md),
[phase-13-concurrency-evidence-v3.md](../phase-13-concurrency-evidence-v3.md),
raw results in `tools/phase13bench/results-v2.json` and
`tools/phase13bench/results-v3-sweep.json`. Headline figures, each cited
above with its specific section:

| Claim | Evidence |
|---|---|
| Slot-table: zero limit violations | v2 §D1 (4 candidates), v3 (46/46 runs) |
| Slot-table: peak throughput ~1,974/s | v2 §D2 |
| Advisory lock: convoy (~35/38 connections waiting) | v2 §D3, `pg_locks` |
| `SERIALIZABLE`: 62–97% true abort rate | v2 §D2, corrected counter |
| Round-robin: backlog-age dominance, 6/6 runs | v2 §D5/§F |
| `last_claimed_at`: E−1 bound, zero exceptions | v3 §2/§5a, 46/46 runs |
| Integrated (slot-table + `last_claimed_at` + reclaim): zero violations | v2 §D6, v3 (46/46 runs) |
| Wall-clock cost per opportunity is not constant (2.55ms→18.38ms, 2→60 queues) | v3 §3 |

All environment caveats (single-node WSL2 VM, not production hardware —
absolute numbers are not portable, relative comparisons within one pass
are) carry forward unchanged from v2 §C.

## Consequences

**Positive**:
- Concurrency limiting is exact and the highest-throughput of the three
  viable candidates, under every tested configuration including the full
  integrated (concurrency + fairness + reclaim) combination.
- TF-INV-019 gets a concrete, provable, hardware-independent bound
  (E − 1) instead of an unproven or borrowed constant — the first of
  TaskForge's invariants whose bound is a service-opportunity count
  rather than a state-machine impossibility, a genuinely new invariant
  *shape* for this project, not just a new invariant.
- Reclaim/slot-ownership is decided explicitly, with a stated proof
  obligation, rather than left as an implicit assumption an implementer
  could get wrong in either direction.

**Negative, accepted**:
- Slot-table's 5–13% measured queue-isolation cost under concurrent
  hot-queue flooding (v2 §D7/§G) is real, reproducible, and not
  mechanistically explained — accepted because it is smaller than
  advisory lock's throughput ceiling under the same threat model, not
  because it is fully understood.
- Every terminalization call site gains a new responsibility (slot
  release) — an enumerable, testable surface, not an open-ended one, but
  a real new place a future regression could introduce a slot leak. This
  is not purely hypothetical: this ADR's own drafting found the Lazy
  Dead-Letter Sweep's release step specifically untested throughout this
  evidence package (see "Reclaim/slot-ownership semantics"), which is why
  SF-058 exists as its own named obligation rather than being assumed
  covered by the general case.
- TF-INV-019's bound requires a deployment to measure its own wall-clock
  translation factor (r) to get an operationally meaningful number — this
  ADR deliberately does not supply one, which is more honest but less
  immediately actionable than a borrowed constant would have been.
- The bound's evidence is scoped to a single worker-subscription pool;
  a partitioned-worker deployment does not yet have direct evidence for
  how (or whether) the bound composes across pools.

## Required implementation proof obligations

Continuing [phase-13-plan.md](../phase-13-plan.md) §16's scenario
numbering (currently proposed through SF-050):

| # | Scenario | Proves |
|---|---|---|
| SF-051 (proposed) | Slot-table concurrency-limit exactness under concurrent claim attempts, mixed fresh/reclaim load | Never more than the configured limit concurrently `RUNNING` — the `SKIP LOCKED`-based stress harness pattern (Phase 5), extended with reclaim traffic. |
| SF-052 (proposed) | Reclaim never writes `queue_slots` (or its production equivalent) | Direct assertion on the reclaim code path, not just an absence of violations under load — this ADR's specific reclaim/slot-ownership proof obligation. |
| SF-053 (proposed, **strengthened**) | **Per-row** slot/job consistency: every `RUNNING` job's id appears in exactly one held `queue_slots` row, and every held `queue_slots` row's `held_by_job_id` names exactly one currently-`RUNNING` job for that queue — checked per row, not only as the aggregate `count(RUNNING) == count(held slots)` this evidence pass's own benchmark did not check at all (only `count(RUNNING) <= limit`). The aggregate form is necessary but not sufficient — it cannot distinguish a correct state from two offsetting errors (one job wrongly holding two slots, another wrongly holding none); the per-row form can. | The stronger invariant required for the real implementation; supersedes the original, aggregate-only phrasing of this obligation. |
| SF-054 (proposed) | `last_claimed_at` fairness: a flooded queue cannot starve a continuously-pending, capacity-eligible sparse queue beyond E − 1 other queues' service opportunities | Direct trace of served-queue order (the `MaxOtherQueuesServedBetweenTurns`-style check this evidence pass built), not inferred from wall-clock wait alone. |
| SF-055 (proposed) | A queue that temporarily loses and regains capacity eligibility does not have its `last_claimed_at` position disturbed, and a competing queue's service during that window counts at most once against any other queue's bound | The specific accounting rule stated in "TF-INV-019: exact definition." |
| SF-056 (proposed) | A newly-configured or never-before-served queue is served on its first opportunity without waiting behind any existing queue's backlog | The `-infinity` default / newly-eligible-queue rule. |
| SF-057 (proposed) | Reclaim participates in `last_claimed_at` accounting identically to a fresh claim | The uniform "service opportunity" definition, both branches of the claim query. |
| SF-058 (proposed, **new**) | Sweep/expired-lease terminalization releases the associated capacity slot durably and idempotently, including the attempt-budget-exhaustion case specifically: construct a job whose lease has expired and whose `attempt_count` has reached `max_attempts` while `RUNNING`; confirm the Lazy Dead-Letter Sweep transitions it to `DEAD_LETTERED` and releases its slot in the same transaction (durable — a crash immediately after commit must not leave the slot ambiguous); confirm a second, later sweep pass against the same already-`DEAD_LETTERED` job is a no-op that does not re-release (or otherwise disturb) a slot that may since have been legitimately reclaimed by a different job (idempotent); confirm a subsequent claim attempt for that queue can successfully use the freed slot. | Closes the specific gap this ADR's own drafting found: neither SF-052 (reclaim never writes slots) nor SF-053 (per-row consistency) alone requires exercising this path, and this evidence pass's own benchmark database was found, on inspection, holding 22 jobs permanently stuck in exactly this unaddressed state — see "Reclaim/slot-ownership semantics" above. |
| SF-059 (proposed, **new**) | Fault injection: force a crash/rollback between a job's terminal state transition (success, failure, cancellation, or dead-letter) and its slot-release write; confirm PostgreSQL's own transaction atomicity means the row is left exactly as it was before the transition began — never `DEAD_LETTERED` (or otherwise terminal) with its slot still held, and never slot-released with the job row still non-terminal | Proves terminal transition and slot release cannot leave stranded capacity after a crash, mirroring TF-INV-013's own SF-014 precedent ("Rollback never leaves a half-transitioned job") extended to the slot-table mechanism this ADR adds. |
| — | Full Phase 1–12 regression suite and `internal/chaos` campaigns, unchanged, against the Phase-13-modified schema and claim query | [phase-13-plan.md](../phase-13-plan.md) §10's "no regression" requirement — TF-INV-001–018 untouched in practice, not just by inspection. |

## Known limitations / future hardening

Carried forward from v3 §7/L, reclassified by this ADR's own final
review rather than accepted uncritically:

- **Not evidenced for partitioned/heterogeneous worker-subscription
  pools** — every one of the 46 v3 runs used one uniform pool where all
  workers see all queues in that run. A deployment that partitions
  workers by queue subscription is a real, plan-supported configuration
  ([phase-13-plan.md](../phase-13-plan.md) §6b/§8/§14) this ADR's bound
  does not directly cover. Future hardening, not a blocker to this ADR
  or to Phase 13 implementation for the common (unpartitioned) case.
- **"Tighter capacity reduces tail latency" is an observed but
  mechanistically unconfirmed pattern** (v3 §4/§6) — does not affect the
  bound (confirmed capacity-independent across 18 capacity-sweep runs)
  or this ADR's decisions. Future hardening only.
- **No full worker-count × queue-count cross-sweep** — both dimensions
  were independently varied with zero bound exceptions in either; a
  cross-product sweep would sharpen the wall-clock formula's precise
  shape, not the algorithmic bound itself. Future hardening only.
- **No production-scale benchmark** (hundreds of workers, long duration,
  realistic execution times). Does not block this ADR's mechanism/bound
  selection — the algorithmic bound is a property of the ordering rule,
  not of scale, and the rejected candidates' failure modes (advisory
  lock's convoy, round-robin's backlog-age dominance) are rooted in
  properties that do not improve at larger scale. **Reclassified here as
  a Phase 13 implementation exit criterion** — [phase-13-plan.md](../phase-13-plan.md)
  §20's own exit criteria already expect a real regression/scale pass
  before the phase ships; this ADR does not relax that expectation.

## Failure Implications

- If the reclaim branch of a future implementation is changed to acquire
  a new slot (this ADR's rejected option B) without also correctly
  releasing the old one first, the result is a silent slot leak —
  effective capacity shrinks over time with no obvious symptom until an
  operator notices `taskforge_queue_running` plateauing below
  `taskforge_queue_concurrency_limit` with pending work waiting
  ([phase-13-plan.md](../phase-13-plan.md) §19's own named risk, now
  confirmed to apply to the reclaim path specifically, not just
  terminalization).
- If the Lazy Dead-Letter Sweep's transition to `DEAD_LETTERED` does not
  release its job's slot durably and idempotently (SF-058), the failure
  is worse than an ordinary slot leak: the affected job has *already*
  exhausted its retry budget by definition, so nothing will ever reclaim
  or otherwise touch it again — the slot is permanently, silently gone
  for the life of the queue, not merely until the next crash-recovery
  cycle. This is confirmed reachable, not speculative: this ADR's own
  evidence package found exactly this state (22 jobs, `RUNNING`, expired
  leases, exhausted attempt budgets, unreleased slots) in its own
  benchmark database, because the benchmark never implemented the sweep
  at all. A production implementation that likewise omits or gets this
  step wrong will accumulate stranded capacity at a rate proportional to
  how often jobs exhaust their retry budget — silent, cumulative, and
  with the same `taskforge_queue_running`-plateau symptom as the bullet
  above, but with no path to recovery short of an operator manually
  reprovisioning the queue's slots.
- If a future change makes the fairness ordering query fall back to a
  sequential scan (the exact defect the review found and v2 fixed twice —
  once for the fairness candidates, once for a related capacity-check
  query), this ADR's throughput claims do not hold, though the
  correctness/bound claims (exactness, E−1) would likely still hold at
  degraded speed — the two are independently proven, not one dependent on
  the other's performance.
- If TF-INV-019's bound is ever cited with a specific millisecond number
  copied from this evidence package's own environment rather than
  measured against the deployment in question, that number is wrong by
  construction — this ADR's own "Wall-clock interpretation" section
  exists specifically to prevent that error, and any documentation
  derived from this ADR should preserve the same distinction.
- If a worker-subscription-partitioned deployment is put into production
  citing this ADR's bound as evidenced for that topology, it is not —
  see "Known limitations." That configuration needs its own measurement
  before TF-INV-019 can be claimed for it with the same confidence.
