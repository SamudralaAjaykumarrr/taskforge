# Phase 13 Implementation Plan — Workload Governance + Retention

Status: **PLANNING ONLY. No code written, no migration authored, no ADR
committed.** This document is the pre-implementation plan for Phase 13 —
[docs/enterprise-roadmap.md](enterprise-roadmap.md) "Phase 13 — Workload
Governance + Retention" — produced by inspecting the repository's own
authoritative documents (roadmap, security model, data model, invariants,
compatibility policy, observability, and the merged Phase 12 plan/code) at
`main`@`b0664a2`. It does not redefine Phase 13's scope; that scope remains
authoritative in [enterprise-roadmap.md](enterprise-roadmap.md) and, where
cited, [security-model.md](security-model.md),
[reference-analysis.md](reference-analysis.md), and
[enterprise-readiness.md](enterprise-readiness.md). Where this document and
those disagree, they win.

This plan follows the same discipline the merged
[phase-12-plan.md](phase-12-plan.md) established: every design choice is
either **settled** (a concrete recommendation, ready to implement) or an
explicitly labeled **open decision (OD-N)** requiring resolution — including
two hard **blockers** that must clear before any implementation PR is
opened (§18): one mandated directly by
[enterprise-roadmap.md](enterprise-roadmap.md)'s own Phase 13 text (OD-1,
the fairness/concurrency ADR), the other inherited from
[phase-12-plan.md](phase-12-plan.md)'s OD-8 — a cross-phase governance
commitment this plan did not create and cannot unilaterally waive or
resolve (OD-3). Nothing here allocates or renumbers an invariant ID beyond
what the roadmap already, explicitly, tentatively names.

> **Post-planning update**: the cross-phase governance pass OD-3 named as a
> blocker has since landed in [invariants.md](invariants.md). This phase's
> fairness property is confirmed as **`TF-INV-019`**, not `TF-INV-017` as
> tentatively named below and in `enterprise-roadmap.md` — `017`/`018` were
> allocated to Phase 12's already-merged G2 (principal isolation) and G7
> (tenant-scoped idempotency) instead. The property's wording and its
> algorithm-independence are unchanged; only the ID moved. This update does
> not otherwise alter this plan's content, so the "tentative"/"TF-INV-017"
> language below is left as originally written except where noted.

> **Post-ADR update**: the other hard blocker, OD-1 (the concurrency/
> fairness ADR), is also resolved: **[ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md)**
> selects slot-table concurrency limiting + `last_claimed_at` fairness,
> defines TF-INV-019's concrete bound as E − 1 service opportunities (not
> a wall-clock SLA), and decides the reclaim/slot-ownership question
> (§6a/§7 below) that this plan itself left open. The ADR was authored
> against a three-pass concurrency/performance evidence package
> ([phase-13-concurrency-evidence-v2.md](phase-13-concurrency-evidence-v2.md),
> [phase-13-concurrency-evidence-v3.md](phase-13-concurrency-evidence-v3.md))
> satisfying the roadmap's own "only after a concurrency and performance
> analysis" precondition. As of this update the ADR file exists in the
> working tree but has **not yet been committed**; §20's exit criterion
> ("exists, committed before implementation") is not yet checked off for
> that reason — see §18 OD-1 below. This update does not otherwise alter
> this plan's content.

**A note on sourcing, added by this correction pass**: this plan draws on
three distinct kinds of requirement and does not treat them as
interchangeable. (1) **Roadmap-defined requirements** — stated directly in
[enterprise-roadmap.md](enterprise-roadmap.md)'s Phase 13 section (named
queues, concurrency limits, the fairness ADR, rate limiting, the `429`/`503`
split, retention itself) — are quoted or closely paraphrased below, never
extended in substance. (2) **Cross-phase constraints inherited from
earlier, already-merged guarantees** — Phase 12's `principal_id`/
idempotency scoping ([security-model.md](security-model.md),
[data-model.md](data-model.md)), Phase 11's API-versioning/compatibility
rules ([compatibility-policy.md](compatibility-policy.md)), and TF-INV-005's
durable mechanism ([invariants.md](invariants.md)) — constrain *how*
Phase 13's roadmap-defined requirements may be implemented without being
Phase-13-specific requirements themselves; §3's table marks each one by
citing its actual document. (3) **Design conclusions this plan itself
introduces** — e.g. §6b's reclaim/queue-subscription rule, §10's
TF-INV-005 interaction proof, §7's schema — are this planning pass's own
analysis, explicitly flagged as such (*"this plan"*, *"surfaced by this
plan's own review"*) rather than attributed to the roadmap or any other
source that does not actually say them.

---

## 1. Exact Phase 13 objective

Close two confirmed, related gaps in one phase, per
[enterprise-roadmap.md](enterprise-roadmap.md) "Phase 13 — Workload
Governance + Retention":

1. **Workload governance**: TaskForge today has one global claimable pool
   (`idx_jobs_claimable`), `priority` as a claim-query tie-break only, no
   named queues, no per-queue/per-tenant concurrency limit, no rate
   limiting, no fairness guarantee, and no backpressure signal. A single
   flooding tenant or job type can starve every other caller sharing the
   deployment (confirmed by direct code inspection — no matches for
   `rate.limit`/`ratelimit`/`quota` anywhere in the codebase;
   [security-model.md](security-model.md) §1 "Tenant starvation" rates this
   **P0** the moment more than one logical tenant shares a deployment).
2. **Retention**: `jobs`, `job_attempts`, and workflow rows grow forever —
   there is no lifecycle/cleanup policy anywhere in the schema or
   documentation (confirmed: no `DELETE` statement targeting these tables
   exists outside test fixtures).

Both are grouped into one phase, per the roadmap's own stated reasoning, not
by convenience: **a retention policy that deletes rows without respecting
the tenant/governance model built alongside it would be designed twice** —
retention must know about `principal_id`-scoped idempotency (Phase 12) and
about whatever queue/tenant concept governance introduces, or it risks
reopening exactly the cross-tenant collision window Phase 12 closed (§10).

## 2. Why this follows Phase 12

[enterprise-roadmap.md](enterprise-roadmap.md)'s sequencing diagram places
Phase 13 immediately after Phase 12 for a structural reason, not merely
numeric order: **per-tenant fairness and per-tenant concurrency limits
require a tenant/principal concept to scope against, and Phase 12 is what
introduced it** (`jobs.principal_id`, `workflow_instances.principal_id`,
both `NOT NULL` as of migration `0009`). Before Phase 12, "per-tenant" had
no schema-level referent. [security-model.md](security-model.md) §1 states
this explicitly for the "Tenant starvation" finding: *"A principal concept
exists (`jobs.principal_id`, NOT NULL) — but nothing acts on that
distinction... Phase 12 deliberately builds the identity Phase 13 needs, not
the governance itself."* Retention has the same dependency in the opposite
direction: it must not prune a row while its `idempotency_key` is still
inside its tenant-scoped dedup-authoritative window (§10, §11), and that
scoping (`(principal_id, job_type, idempotency_key)`) also only exists as of
Phase 12.

Two secondary prerequisites, both already satisfied:

- **Phase 11** (API versioning): queue-name and rate-limit fields land on
  the already-versioned `/v1/` surface, not a surface that then needs a
  second breaking migration to add a version prefix.
- **`security-model.md` §1's "Abusive retry workload" and Phase 12's
  observability-only auth-failure mitigation** are both explicitly deferred
  *by name* to Phase 13 — this phase is where those deferrals are meant to
  be closed, not a fresh, unrelated concern.

## 3. Authoritative source for every major requirement

| Requirement | Authoritative source |
|---|---|
| Overall scope, in-scope/non-scope list, invariant/test/exit-criteria checklist | [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 13 — Workload Governance + Retention" (the single source of truth for this phase; quoted extensively below) |
| Tenant-starvation P0 finding, `max_attempts` shared-resource-contention deferral | [security-model.md](security-model.md) §1 |
| Rate limiting explicitly deferred from Phase 12 by name | [phase-12-plan.md](phase-12-plan.md) §3; [observability.md](observability.md) `taskforge_auth_failures_total` note |
| `principal_id` schema, tenant-scoped idempotency mechanics | [data-model.md](data-model.md) "Table: `jobs`", "Idempotency constraint change (G7)" in [phase-12-plan.md](phase-12-plan.md) |
| TF-INV-001 through TF-INV-019 (fairness property confirmed as TF-INV-019, not the roadmap's original tentative TF-INV-017 — see post-planning update above) | [invariants.md](invariants.md) (current registry, now through 019 via the cross-phase governance pass); [enterprise-roadmap.md](enterprise-roadmap.md) Phase 13 section (citations updated to match) |
| Invariant-ID allocation must be a cross-phase governance PR, not decided unilaterally | [phase-12-plan.md](phase-12-plan.md) OD-8 |
| Expand/migrate/contract migration model, lock-safety discipline for `jobs`/`job_attempts` | [compatibility-policy.md](compatibility-policy.md) "PROPOSED: Database Migrations"; [data-model.md](data-model.md) "Phase 12 migration lock profile" (the precedent this phase's migrations must match) |
| Claim query shape or the "candidate" CTE + `FOR UPDATE SKIP LOCKED` idiom | `internal/store/claim.go` (`claimQuery`); [worker-protocol.md](worker-protocol.md) |
| Reclaim eligibility (an expired-lease `RUNNING` job) follows the same queue-subscription filter as a fresh claim (§6b) | **This plan's own design conclusion**, not a roadmap-stated requirement — derived from direct inspection of `internal/store/claim.go`'s existing `claimQuery` (one `WHERE` clause, two `OR`'d branches), TF-INV-004's actual property in [invariants.md](invariants.md), and [enterprise-roadmap.md](enterprise-roadmap.md)'s "preserve all Phase 1–12 behavior and guarantees" instruction for this planning pass. Surfaced by independent review (H1); §6b. |
| Rejection of strict-priority draining; Hatchet's group-key round robin as one (not the) candidate fairness algorithm | [reference-analysis.md](reference-analysis.md) "Priorities / fairness" row, "Explicitly Rejected Capabilities" |
| Faktory precedent for staging (governance before rate limiting) | [reference-analysis.md](reference-analysis.md) "Faktory" summary |
| Backpressure / overload signal split (`429` vs `503`) | [enterprise-roadmap.md](enterprise-roadmap.md) Phase 13 "Exact scope"; [slo.md](slo.md) "Overload behavior" row; [enterprise-readiness.md](enterprise-readiness.md) §3 item 4 |
| Cardinality/logging discipline for any new metric/log field this phase adds | [observability.md](observability.md) "Cardinality policy, audited" |
| No CASCADE on `job_attempts.job_id`/`workflow_nodes.job_id` FKs (retention delete ordering) | `migrations/0002_create_job_attempts_table.up.sql`, `migrations/0003_create_workflow_tables.up.sql` (both plain `REFERENCES`, no `ON DELETE`) |
| TF-INV-005's durable reopening-detection mechanism (constrains retention's `job_attempts` pruning) | [invariants.md](invariants.md) TF-INV-005 "Historically reopened" |
| No AGENTS/HANDOFF/CLAUDE instruction files exist in this repository | Direct repository search (none found) — this plan follows only the documents above |

## 4. In-scope work

Reproduced and organized from [enterprise-roadmap.md](enterprise-roadmap.md)
Phase 13 "Exact scope" (verbatim scope, not paraphrased away from it):

**Workload governance**
- Named queues (`queue_name`), routable independently of `job_type`, with
  explicit worker queue-subscription/routing semantics.
- Per-queue/per-tenant(principal)-scoped concurrency limits, enforced in the
  claim query.
- Fairness: a documented bound ("no queue/tenant with pending,
  capacity-eligible work is starved beyond a documented bound while another
  queue/tenant is making progress"), backed by a **mandatory ADR** selecting
  the scheduling algorithm only after a concurrency/performance analysis —
  not pre-committed here (§18 OD-1, §6).
- Rate limiting: static, per-queue (optionally per-tenant) submission rate
  limits at the API layer, staged after the concurrency/fairness mechanism.
- Backpressure/overload signal: `429` (caller/tenant/queue admission or
  rate-limit policy) vs. `503` (system/service capacity), both carrying
  `Retry-After`, with ordinary concurrency-cap saturation (job still
  enqueues and waits) explicitly **not** a rejection condition.

**Retention**
- Terminal job retention (`SUCCEEDED`/`DEAD_LETTERED`/`CANCELLED` `jobs`
  rows).
- Attempt-history retention (`job_attempts`), independently paced.
- Workflow retention (`workflow_instances`/`workflow_nodes`), analogous to
  jobs.
- Safe batch cleanup (bounded, batched deletion; no single unbounded
  `DELETE`; must not starve claim-query traffic).
- Cleanup observability (metrics/logs for what was pruned, when, how much).
- Index/vacuum implications, documented.
- Optional archive/export boundary, specified if offered.
- Idempotency-safety: retention must not delete a row whose
  `idempotency_key` is still inside its dedup-authoritative window.

## 5. Explicit non-goals / deferred work

Reproduced from [enterprise-roadmap.md](enterprise-roadmap.md) Phase 13
"Explicit non-scope", plus this plan's own scoping calls flagged
`(this plan)`:

- No dynamic/adaptive rate limiting based on real-time load — static,
  operator-configured limits only.
- No cross-queue global, unconditional fairness guarantee — only the
  bounded property the required ADR proves, under stated assumptions (run
  duration, load shape, queue count); "indefinite" fairness is never
  claimed.
- No per-job-type backoff configuration (tracked separately in
  [retry-semantics.md](retry-semantics.md) "Open Questions", unrelated to
  queue governance).
- No periodic/cron scheduling (explicitly deferred per
  [reference-analysis.md](reference-analysis.md)).
- No automatic archival/data-warehouse infrastructure beyond the optional,
  specified export boundary.
- No full RBAC, OIDC, or per-`job_type` policy engine (unchanged carry-over
  from Phase 12's own non-scope, still out of scope here).
- No change to `lease_owner`/`lease_generation` fencing semantics, or to
  any TF-INV-001–016 mechanism.
- **`(this plan)`** No pruning of `job_attempts` rows belonging to a
  **non-terminal** job, however many attempts it has accumulated — retention
  applies only to attempts belonging to an already-terminal `jobs` row (§7,
  §10). Pruning a still-in-flight job's closed-but-superseded attempt rows
  is a plausible future extension the roadmap's "independently" language
  does not forbid, but this plan does not design it: it has no
  entry in the roadmap's own required-tests list, and doing it safely
  needs its own TF-INV-005/007 interaction analysis distinct from the one
  this plan resolves for terminal jobs (§10).
- **`(this plan)`** No self-service HTTP API for queue/rate-limit
  configuration — operator-tool-managed (`cmd/taskforge-admin`), mirroring
  Phase 12's key-management precedent ([phase-12-plan.md](phase-12-plan.md)
  §3's "no self-service HTTP API" non-goal, extended here for consistency,
  not because the roadmap names it for Phase 13 specifically).
- **`(this plan)`** No fix to Phase 12's known carry-over defects (`401` vs
  `503` for a database outage during auth verification; unbounded
  `last_used_at` write concurrency) — those are named in
  [security-model.md](security-model.md) §5 as Phase-12-owned follow-ups,
  not Phase 13 scope, though §19 notes the opportunistic overlap with this
  phase's own `503` mechanism.

## 6. Architecture changes

```
        [ any authenticated HTTP client ]
                     |
                     v
   +---------------------------------------------+
   |  cmd/api                                     |
   |  +----------------+   +--------------------+ |
   |  | auth middleware |-->| admission/rate-    | |  <- NEW: per-queue/
   |  | (Phase 12,      |   | limit check (NEW)  | |     tenant 429, or
   |  |  unchanged)     |   +--------------------+ |     system-capacity
   |  +----------------+            |               |     503 (§9)
   |                                v               |
   |                     handlers (attach            |
   |                     queue_name, unchanged        |
   |                     principal/ownership path)    |
   +---------------------------------------------+
                     |
                     v  (INSERT, now carrying queue_name)
   +--------------------------------------------------------+
   |                     PostgreSQL                          |
   |  jobs (+ queue_name)     workflow_instances (+ analog)  |
   |  queue_limits (NEW)      rate_limit_buckets (NEW)       |
   +--------------------------------------------------------+
                     ^
                     |  claim query, NOW queue- and
                     |  concurrency-limit-aware (§6a)
   +--------------------------------+
   |  Worker Pool                    |
   |  subscribed to a configured      |
   |  subset of queue_name values     |
   |  (NEW, internal/worker + config) |
   +--------------------------------+

   +--------------------------------------------------------+
   |  NEW: retention sweeper (own process or ticker, §18     |
   |  OD-5) — batched DELETE against jobs/job_attempts/      |
   |  workflow_instances/workflow_nodes, never touching a    |
   |  row still inside its idempotency-authoritative window  |
   +--------------------------------------------------------+
```

**No new network-facing component for governance** — the admission/rate
check and the queue-aware claim query are both additions to the existing
`internal/api` and `internal/store` layers, matching
[architecture.md](architecture.md)'s "smallest architecture" philosophy and
Phase 12's own precedent (§4a there: push correctness into the existing SQL
statement, not a new service).

**One new operational component**: the retention sweeper. Whether it is a
ticker inside an existing binary or a new standalone binary is **open**
(§18 OD-5) — either way it is a batch-DELETE loop, not a new network
listener, and it reads/writes only `jobs`, `job_attempts`,
`workflow_instances`, `workflow_nodes`.

### 6a. Concurrency-limit and fairness mechanism — scoped, not pre-selected

The roadmap is explicit that this phase **"does not pre-commit to Hatchet's
(or any other system's) exact algorithm"** and **requires an ADR** "to
select the simplest correct PostgreSQL-native scheduling algorithm only
*after* a concurrency and performance analysis." This plan does not
override that instruction. What follows is the concurrency/performance
context that ADR needs, not a decision made here.

**The problem, stated precisely**: enforcing "never more than N
concurrently `RUNNING` jobs for queue Q" cannot be done by a plain
`SELECT count(*) ... WHERE queue_name = Q AND state = 'RUNNING'` inside the
claim query's `WHERE` clause. Two concurrent claim transactions can both
observe `count = N-1` (below the limit) before either commits, and both
then claim — the classic check-then-act race this codebase's own idiom
(`internal/store/idempotency.go`'s comment, cited approvingly in
[phase-12-plan.md](phase-12-plan.md) §4a) exists specifically to avoid.
Three PostgreSQL-native candidates for the ADR to evaluate, in increasing
order of departure from the existing claim-query idiom:

1. **Per-queue advisory lock** (`pg_advisory_xact_lock(hashtext(queue_name))`
   held for the claim transaction's duration): serializes claims *within* a
   queue while leaving different queues fully concurrent. Simple, but an
   advisory lock is a session-level primitive outside the row-level-locking
   idiom every other invariant in this codebase relies on, and its
   contention behavior under many workers claiming the same hot queue is
   the kind of thing the ADR's performance analysis must actually measure,
   not assume.
2. **Slot-table semaphore** (a new `queue_slots(queue_name, slot_index,
   held_by_job_id)` row per unit of concurrency capacity; claiming a job
   also claims a free slot row via the same `FOR UPDATE SKIP LOCKED`
   pattern `claimQuery` already uses, and completion releases it): stays
   entirely within the existing row-lock idiom (§4a's "push it into the
   WHERE clause" pattern, one step further — push it into a *second*
   `FOR UPDATE SKIP LOCKED` join), makes the current concurrency count an
   O(1) fact (count of non-NULL `held_by_job_id` rows) rather than a scan,
   and composes naturally with fairness ordering (§6b). Cost: a slot table
   whose row count scales with configured concurrency capacity (not job
   volume), and completion/dead-letter/cancel paths must all release the
   held slot in the same transaction that terminalizes the job — a new
   responsibility for every terminalization call site.
3. **`SERIALIZABLE` isolation with retry** on the claim transaction: closest
   to "just add a `WHERE` predicate," but trades an explicit mechanism for
   PostgreSQL's serialization-failure/retry machinery, which this
   codebase's claim path (`SKIP LOCKED`, deliberately *not*
   `SERIALIZABLE` — see `worker-protocol.md`) has never used, and would add
   a retry loop to the single hottest query in the system.

**Fairness candidates**, layered on whichever concurrency mechanism the ADR
selects: Hatchet's group-key round robin
([reference-analysis.md](reference-analysis.md) "Priorities / fairness"
row) is the one named candidate, evaluated (not adopted sight-unseen) per
that same row; a simpler Postgres-native alternative worth the ADR's
consideration is ordering claim candidates by each queue's own
`last_claimed_at` (ascending) ahead of `priority`/`eligible_at` — a
durable, single-column "who hasn't been served recently" signal requiring
one new timestamp column, updated in the same claim statement, with no new
table. **Faktory's strict-priority-drain model is explicitly rejected**
(roadmap language, reiterated here) as a known starvation anti-pattern,
independent of which candidate the ADR picks.

### 6b. Reclaim eligibility and queue subscription — settled, not an ADR question

Unlike §6a's concurrency/fairness mechanism, this is not left to the
required ADR: it follows directly from inspecting the existing reclaim
path plus the roadmap's own "preserve all Phase 1–12 behavior and
guarantees" instruction for this planning pass, so it is resolved here.
**Added by this correction pass** — the original version of this plan did
not address it, an independent review of this document found the gap, and
this section is this plan's own conclusion, not a roadmap quotation.

**Current behavior, inspected directly against `internal/store/claim.go`**:
`claimQuery` is a single statement with one `WHERE` clause covering two
`OR`'d branches — `(state IN ('QUEUED','RETRY_WAIT') AND eligible_at <=
now())` for a fresh claim, `OR (state = 'RUNNING' AND lease_expires_at <
now() AND attempt_count < max_attempts)` for a reclaim of an expired lease
(TF-INV-004's mechanism, [invariants.md](invariants.md)). Today there is no
`queue_name` predicate on either branch, and no distinction in how a
worker becomes eligible to claim a fresh job versus reclaim an abandoned
one — both are the same query, decided by whichever worker's transaction
wins the `FOR UPDATE SKIP LOCKED` race.

**The Phase 13 rule**: a worker's `TASKFORGE_WORKER_QUEUES` subscription
filter (§8) applies identically to both branches of the claim query. A
worker may reclaim an expired lease for a job in queue Q if and only if it
is also eligible to freshly claim from queue Q. There is no separate,
looser rule for reclaim — the subscription filter is one authorization
boundary, not two.

**Why not the looser alternative** (let any worker reclaim any queue's
expired lease regardless of its own subscription, so liveness is never at
risk): that would let a worker an operator deliberately excluded from
queue Q — for isolation, compliance, or resource-locality reasons, which
is the entire point of introducing named queues (§4) — end up *executing*
queue Q's job the moment its lease happens to expire. That is precisely
the unauthorized queue execution this plan must not create: queue
subscription would then be an admission filter for new work only, silently
defeated by any transient lease expiry, not an actual execution boundary.
Applying one consistent predicate to both branches is what makes "a
worker only ever runs jobs from queues it is subscribed to" a real
guarantee instead of one that holds only in the common case.

**Why this does not strand work, and does not weaken TF-INV-004**:
TF-INV-004's property is that an abandoned job "become[s] eligible for
claiming by another worker" — eligibility, not a guarantee that a
specific, deliberately-excluded worker will claim it
([invariants.md](invariants.md)). TaskForge has never guaranteed a job
gets claimed if zero workers exist, or if zero workers recognize its
`job_type`; an operator who runs no worker capable of a job is already,
today, responsible for that job never progressing, and that has never been
treated as an invariant violation. Phase 13 adds exactly one more instance
of the same, pre-existing class of operator responsibility: **every
`queue_name` a deployment actually uses must have at least one
currently-running worker subscribed to it** (directly, or via §14's
default of an unset `TASKFORGE_WORKER_QUEUES` subscribing to everything),
or that queue's jobs — new and reclaimed alike — are not served. This is a
new, explicit operational requirement this plan adds (§13, §15), not a
silent gap: the default means a deployment that does no queue
configuration at all reclaims exactly as it does today, with zero risk of
accidental stranding, and an operator who does partition their worker
fleet by queue takes on the same staffing responsibility they already
carry for `job_type` coverage. See §10 for the TF-INV-004 no-regression
argument extended to this interaction, §13/§14 for the failure-scenario
and compatibility consequences, and §16 (SF-049) for the required test.

## 7. Data-model / schema changes

All additive, per the expand/migrate/contract model
([compatibility-policy.md](compatibility-policy.md)). Concrete recommendation
below; the concurrency-limit table's exact shape depends on §6a's ADR and
may need a follow-up migration once that ADR lands (flagged inline).

### `jobs.queue_name` (and `workflow_instances.queue_name`)

```sql
ALTER TABLE jobs ADD COLUMN queue_name text NOT NULL DEFAULT 'default';
ALTER TABLE workflow_instances ADD COLUMN queue_name text NOT NULL DEFAULT 'default';
```

**Unlike Phase 12's `principal_id`, this does not need the four-migration
expand/backfill/validate/contract dance.** `principal_id` needed that split
because each row's correct value differed (the real submitting principal,
requiring a data-dependent backfill) and the column had to end up
`NOT NULL` with a validated `CHECK`. `queue_name` needs neither: every
existing row's correct value is the *same* literal, `'default'`, and
PostgreSQL 16 (this project's target — `docker-compose.yml`) has supported
a non-volatile-default `ADD COLUMN ... NOT NULL DEFAULT '...'` as a
catalog-only operation since version 11 (no table rewrite, no full-table
scan) — the exact optimization [data-model.md](data-model.md)'s "Phase 12
migration lock profile" section already relies on for `0006`'s reasoning
about catalog-only operations. This is called out explicitly, by name,
because a future reviewer familiar with Phase 12's six-file story might
otherwise assume this needs the same split; it does not, and this migration
should **not** be over-engineered into one it doesn't need. A single
`.up.sql` file, `ACCESS EXCLUSIVE` held only for the catalog-only `ADD
COLUMN` (comparable to Phase 12's `0006`/`0008` timings, low-single-digit
milliseconds), is correct and sufficient.

Index (replacing, not modifying, `idx_jobs_claimable`):

```sql
CREATE INDEX idx_jobs_claimable_by_queue
  ON jobs (queue_name, priority DESC, eligible_at ASC)
  WHERE state IN ('QUEUED', 'RETRY_WAIT');
```

Whether the old `idx_jobs_claimable` (no `queue_name` column) is dropped in
the same migration or a later one is itself an expand/migrate/contract
question: the old index remains correct (just non-optimal once queries
start filtering by `queue_name`) until the claim query is actually changed
to use the new one, so the safe sequence is **add the column and new index
first, cut the claim query over, then drop the old index in a later,
`ACCESS EXCLUSIVE`-isolated file** — mirroring Phase 12's `0009`/`0010`
split precedent exactly (new index built and proven live before the old one
is retired).

### Per-queue/tenant concurrency and rate-limit state (shape pending §6a's ADR)

A strawman, **not final** until the ADR selects a mechanism (§6a):

```sql
-- Configuration (operator-set, static per Phase 13's own non-goal on
-- dynamic/adaptive limits):
CREATE TABLE queue_limits (
    id                 uuid PRIMARY KEY,  -- application-supplied, matching
                                           -- this codebase's existing
                                           -- convention (uuid.New() in Go,
                                           -- e.g. internal/store/claim.go,
                                           -- internal/store/idempotency.go)
                                           -- rather than a DB-side default
    queue_name         text NOT NULL,
    principal_id       uuid NULL REFERENCES principals(id),
    concurrency_limit  int  NULL,   -- NULL = unlimited
    rate_limit_per_sec numeric NULL,
    rate_limit_burst   int  NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- principal_id NULL means "queue-wide default" (§13's misconfiguration
-- row depends on being able to distinguish this from a per-principal
-- override). This is deliberately NOT expressed as
-- PRIMARY KEY (queue_name, principal_id): PostgreSQL forces every
-- PRIMARY KEY column NOT NULL, so a NULL-containing primary key cannot
-- represent the "queue-wide" row this table needs at all -- an earlier
-- draft of this plan specified exactly that broken shape, caught by
-- independent review, corrected here rather than merely re-labeled.
-- Two partial unique indexes give the same guarantee (at most one
-- queue-wide row, at most one row per (queue_name, principal_id) pair)
-- without a NULL-containing key:
CREATE UNIQUE INDEX idx_queue_limits_scoped
  ON queue_limits (queue_name, principal_id) WHERE principal_id IS NOT NULL;
CREATE UNIQUE INDEX idx_queue_limits_default
  ON queue_limits (queue_name) WHERE principal_id IS NULL;

-- Rate-limit runtime state, durable across restart (roadmap: "Rate-limit
-- state itself must survive a worker/API-server restart... consistent
-- with ADR-0001"). A single-statement, race-free token-bucket refill:
CREATE TABLE rate_limit_buckets (
    scope_key      text PRIMARY KEY,   -- e.g. "queue:emails" or "principal:<uuid>:queue:emails"
    tokens         numeric NOT NULL,
    last_refill_at timestamptz NOT NULL DEFAULT now()
);
```

`rate_limit_buckets` is deliberately **not** the same table as
`queue_limits`: the former is static operator configuration, the latter is
continuously-mutated runtime state, and conflating them would put
high-churn `UPDATE` traffic on the same rows an admin CLI reads/writes —
the same table-role separation this codebase already draws between
`principals` (identity, low-churn) and `api_keys` (also low-churn, but a
distinct concern) in Phase 12.

**If §6a's ADR selects the slot-table concurrency mechanism** (§6a
candidate 2), `queue_limits.concurrency_limit` becomes the source of truth
for how many `queue_slots` rows exist per queue (provisioned/deprovisioned
by the same admin-tool path that sets the limit), and every
terminalization call site (`CompleteSuccess`, `CompleteFailure`,
`CompleteRetryableFailure`, `CompleteCancelled`, `CompleteTimeout`, the Lazy
Dead-Letter Sweep, workflow cascade-cancellation) gains a slot-release step
in the same transaction — a concrete, enumerable list of `internal/store`
call sites (§17), not an open-ended search.

### Idempotency scoping: unaffected

`UNIQUE(principal_id, job_type, idempotency_key)` does **not** gain
`queue_name` — two submissions with the same key from the same principal
are the same logical job regardless of which queue either request names;
adding `queue_name` to the constraint would let a caller "escape"
deduplication by resubmitting the identical key against a different queue,
which is not a capability this phase intends to grant and the roadmap does
not request. This is stated explicitly to close off an easy but wrong
design a queue-name column might otherwise invite.

## 8. API / CLI / config changes

**API (additive, versioned surface, per** [compatibility-policy.md](compatibility-policy.md)**):**
- `POST /jobs`, `POST /workflows`: new optional request field
  `queue_name` (string; omitted/empty → `"default"`). Unknown-field
  tolerance (Phase 11) already covers an old client omitting it and a new
  client's server ignoring it if rolled back — no version bump required.
- New response headers on the two backpressure codes: `429` (admission/
  rate-limit policy) and `503` (system capacity), both carrying
  `Retry-After` (seconds), per roadmap §"Exact scope".
- `GET /jobs/{id}` / `GET /workflows/{id}` response bodies gain
  `queue_name` (additive field, no version bump).

**CLI (`cmd/taskforge-admin`, extending Phase 12's precedent of
operator-tool-only lifecycle management, §5):**
- `set-queue-limit -queue=<name> [-principal=<uuid>] -concurrency=<N>`
- `set-rate-limit -queue=<name> [-principal=<uuid>] -rate=<per-sec> -burst=<N>`
- `show-queue-state` (current limits + live concurrency/rate-bucket state,
  read-only operator visibility — this is the CLI-side complement to §12's
  metrics, for an operator who wants a point-in-time snapshot without a
  Prometheus query).

**Worker config (`internal/config`, extending the existing `FromEnv`
pattern):**
- `TASKFORGE_WORKER_QUEUES` — comma-separated list of `queue_name` values
  this worker process claims from. **Unset means "claim from every queue,"
  not "claim only from `default`"** — see §14's compatibility analysis for
  why this default direction is load-bearing, not arbitrary. **This
  restriction governs both branches of `claimQuery` identically — a fresh
  QUEUED/RETRY_WAIT claim and a reclaim of an expired `RUNNING` lease
  alike** (§6b): a worker never reclaims an expired lease for a queue it
  is not itself eligible to freshly claim from. See §6b for the full
  rule and its justification, §10 for the TF-INV-004 interaction, and
  §15 for the operational staffing obligation this creates.

**API config:**
- `TASKFORGE_MAX_INFLIGHT_SUBMISSIONS` — bounds concurrent in-flight
  `POST /jobs`/`POST /workflows` handler executions in `cmd/api`; exceeding
  it is the `503` system-capacity signal (§9, §18 OD-6). Default value is
  an open operational question, not fixed by this plan (mirrors
  `ActiveWorkerWindow`'s "implementation decision" precedent in
  `internal/config`).

**Retention config:**
- `TASKFORGE_RETENTION_ENABLED` (default `false` — retention is an
  opt-in operator decision, not a silently-enabled behavior change for
  existing deployments; see §14).
- `TASKFORGE_RETENTION_TERMINAL_JOB_TTL`, `TASKFORGE_RETENTION_WORKFLOW_TTL`
  (`time.Duration`-parsed, per the existing `FromEnv` convention).
- **`TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL`** (`time.Duration`-parsed,
  optional). The roadmap requires attempt-history retention be
  "independently paced" from terminal-job retention specifically because
  `job_attempts` rows "accumulate faster than terminal job rows" (§2,
  §4, [enterprise-roadmap.md](enterprise-roadmap.md) Phase 13 "Exact
  scope") — a roadmap-defined requirement, not this plan's own
  invention. This is the config surface that satisfies it: **default,
  if unset, equals `TASKFORGE_RETENTION_TERMINAL_JOB_TTL`** (attempts
  live exactly as long as their job unless an operator opts into a
  shorter value), so a deployment that never sets it gets exactly one
  clock, not two — the same "no redundant clock that can drift"
  discipline §10 applies to the idempotency window, applied here to a
  different concern (audit-history volume, not dedup correctness; the
  two are unrelated, since idempotency authority is scoped to the
  `jobs` row alone, never to `job_attempts` — see §10). **Validation**:
  the admin tooling and the sweeper itself must reject/refuse a
  configured `TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL` greater than
  `TASKFORGE_RETENTION_TERMINAL_JOB_TTL` — attempts must never outlive
  their job. Even absent that validation, `job_attempts.job_id
  REFERENCES jobs(id)` with no `ON DELETE CASCADE` (§3's citation of
  migrations `0002`/`0003`) means PostgreSQL itself refuses to delete a
  `jobs` row while its `job_attempts` rows still exist, so a
  misconfiguration here fails loudly (a foreign-key violation, surfaced
  as `taskforge_retention_sweep_errors_total`, §12) rather than
  silently — a backstop this plan relies on but does not treat as a
  substitute for the sweeper always pruning a terminal job's
  `job_attempts` at or before that job row's own deletion. See §13, §16
  (SF-050).
- `TASKFORGE_RETENTION_BATCH_SIZE` (rows per batch-delete transaction;
  default TBD by load testing, not fixed here).
- `TASKFORGE_RETENTION_SWEEP_INTERVAL`.
- **Deliberately no separate `TASKFORGE_RETENTION_IDEMPOTENCY_WINDOW`** —
  see §10's "no second clock" resolution: idempotency dedup authority for a
  job is defined to last exactly as long as the job row itself exists, so
  `TASKFORGE_RETENTION_TERMINAL_JOB_TTL` **is** the idempotency window,
  not a second, independently-tunable value that could drift out of sync
  with it. (This is a different clock from
  `TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL` above, and for a different
  reason: idempotency correctness genuinely must not have two clocks,
  while attempt-history pacing is explicitly requested as independently
  tunable by the roadmap itself — the two decisions are not in tension.)

## 9. Authorization / security / trust implications

- **No new principal type, no change to authentication** (Phase 12's
  `internal/principal` is unchanged). Queue/rate-limit configuration is
  operator-tool-managed (§8), not reachable through any authenticated HTTP
  route — consistent with Phase 12's "no self-service HTTP API" precedent
  and this phase's own §5 non-goal.
- **Per-principal rate-limit/concurrency scoping must not become a new
  cross-tenant disclosure surface.** A `429` response body must not reveal
  *another* tenant's queue depth or limit configuration — the response
  states only that the caller's own request was throttled and when to
  retry (`Retry-After`), mirroring Phase 12's "no existence disclosure"
  discipline ([security-model.md](security-model.md) "Avoid unnecessary
  existence disclosure") extended from job/workflow identity to
  governance state.
- **`503` (system capacity) must not be principal-scoped at all** — by
  construction it says nothing about any specific tenant, so it carries no
  disclosure risk analogous to Phase 12's ownership-`404` reasoning (there
  is no per-resource ID at stake, exactly like Phase 12's scope-mismatch
  `403` reasoning in [phase-12-plan.md](phase-12-plan.md) §6a).
- **Retention must not create a privilege-escalation or blast-radius
  change** for the least-privilege PostgreSQL roles Phase 12 introduced
  (`taskforge_api`, `taskforge_worker`, `deploy/postgres-roles.sql`). The
  retention sweeper needs `DELETE` on `jobs`/`job_attempts`/
  `workflow_instances`/`workflow_nodes` — a privilege **neither existing
  role currently needs or should keep** (per
  [security-model.md](security-model.md) §2's own forward-reference: *"workers
  do not need `DELETE` on `jobs` if Phase 13's retention cleanup runs under
  its own, narrower-scoped role"*). **This plan adopts that framing as a
  requirement**: a third, even-more-restricted PostgreSQL role
  (`taskforge_retention`, granted `SELECT`/`DELETE` on exactly the four
  retention-eligible tables and nothing else) is required, not merely
  suggested — see §17.
- **Fairness/concurrency limits are not an authorization mechanism.** A
  principal denied capacity by a concurrency cap or rate limit is not
  "unauthorized" — it is throttled. This phase must not conflate `429` with
  `401`/`403` in any shared code path, log event, or metric.
- **No change to TF-INV-001–016 or to Phase 12's G1–G8 guarantees** —
  governance and retention sit in front of / after the existing engine,
  exactly as Phase 12's auth sits in front of it; this phase does not touch
  lease/fencing/idempotency-uniqueness mechanics themselves (only the claim
  query's *selection* predicate, never its fencing predicate).

## 10. Invariant / proof obligations

**New invariant (ID confirmed by the post-planning cross-phase governance
pass — see the note at the top of this document and §18 OD-3):**
**TF-INV-019** (the roadmap originally, tentatively, named this
TF-INV-017; that number went to Phase 12's G2 instead):
*"no queue/tenant with pending, capacity-eligible work is starved beyond
the documented bound while another queue/tenant is making progress"* — the
bound is whatever §6a's ADR proves for its selected algorithm, under stated
assumptions (run duration, load shape, queue count); this is **never**
claimed as an unconditional or indefinite guarantee.

**Concurrency-limit exactness** (not itself a new numbered invariant, but a
proof obligation the roadmap requires with the same rigor): never more than
N concurrently `RUNNING` jobs for a capped queue, proven under the same
`SKIP LOCKED`-based concurrent-claim stress harness Phase 5 already
established (`internal/store/concurrency_stress_test.go` pattern).

**Backpressure liveness**: a caller receiving `429`/`503` and retrying
after `Retry-After` succeeds — no permanent lockout from a transient
signal. A queue merely at its configured concurrency cap (no admission/rate
threshold crossed) must **not** itself trigger either code — jobs still
enqueue and wait their turn.

**Retention / idempotency interaction — the roadmap's own named risk,
resolved here rather than left open:**

> "The retention design must state explicitly how long an idempotency key
> remains dedup-authoritative relative to how long its owning job row is
> retained, and must not let cleanup outpace that window."
> — [enterprise-roadmap.md](enterprise-roadmap.md) Phase 13

**Resolution — no second clock.** A job's `idempotency_key` is
dedup-authoritative for **exactly** as long as its `jobs` row exists.
There is no independently-configured "idempotency retention window"
alongside the job-retention TTL — introducing one would create exactly the
drift risk the roadmap warns about (two config values that must always
agree, with no mechanism forcing them to). `TASKFORGE_RETENTION_TERMINAL_JOB_TTL`
(§8) **is** the idempotency window by construction: as long as the
retention sweeper only ever deletes a `jobs` row after that TTL has
elapsed since `terminal_at`, and `Store.GetByIdempotencyKey`'s
conflict-recovery re-read continues to query the live `jobs` table
(unchanged), there is no way for cleanup to "outpace" the window — the
window's boundary and the deletion's precondition are the same value read
from the same place. **Required test**: submit a duplicate request against
a job whose `terminal_at` is old but still inside
`TASKFORGE_RETENTION_TERMINAL_JOB_TTL`, confirm dedup still fires; run the
retention sweep; confirm the row is gone; confirm a *new* submission with
that same key now creates a fresh row rather than colliding with a deleted
one (this last assertion is new relative to the roadmap's own stated test
list and closes a gap that list does not explicitly name: a resubmission
*after* legitimate deletion must succeed, not error).

**Retention / TF-INV-005 interaction — a proof obligation this plan
surfaces, not named explicitly in the roadmap's own text:**

TF-INV-005's second, "historically reopened" durable check
([invariants.md](invariants.md)) detects an illegitimate reopening by
finding a `job_attempts` row whose `attempt_number` exceeds the job's own
`terminal_attempt_count` — a column on the immutable `jobs` row itself,
never on `job_attempts`. Pruning **legitimate, already-accounted-for**
`job_attempts` rows for an already-terminal job (every row with
`attempt_number <= terminal_attempt_count`) cannot disable this check: any
*future* illegitimate reopening still produces a *new* `job_attempts` row
with `attempt_number > terminal_attempt_count`, and that row is recent —
never a candidate for deletion by an age-based retention sweep. This is
exactly why §5 and §7 restrict `job_attempts` pruning to **terminal jobs
only, and never above `terminal_attempt_count`** (which for a terminal job
is, by definition, every row it has): the check's soundness depends on the
immutable marker column, not on retained historical rows, so pruning is
safe by construction under this restriction — but only under it. **Required
test** (new, not in the roadmap's own list, added here because the
interaction is real and previously undocumented): prune all `job_attempts`
rows for a terminal job down to zero, then durably insert one anomalous
row above `terminal_attempt_count` directly (simulating an illegitimate
reopening bypassing normal code paths, the same technique
`internal/invariant`'s existing TF-INV-005 tests already use), and assert
`internal/invariant`'s checker still flags it. This is a genuine
correctness dependency between two phases' work that the roadmap's Phase
13 section does not itself call out, and it is exactly the kind of
cross-document contradiction/gap this planning pass exists to surface
rather than silently assume away.

**No regression to TF-INV-001 through TF-INV-016**: this phase adds a
selection predicate (queue/concurrency filter) and an admission check
in front of the existing engine; it must not touch the claim query's
fencing/transition logic, `lease_generation` arithmetic, or any completion
path's `WHERE` clause beyond what §6a's chosen slot-release mechanism (if
selected) adds as a same-transaction side effect. A full Phase 1–12
regression pass is required before this phase's exit criteria are declared
met, mirroring every prior phase's own discipline.

**TF-INV-004 interaction, specifically — surfaced by this plan's own
review, not named in the roadmap's text:** the queue-subscription filter
is exactly a "selection predicate" on `claimQuery`, and `claimQuery` has
two branches, not one — a fresh QUEUED/RETRY_WAIT claim and a reclaim of
an expired `RUNNING` lease, the latter being TF-INV-004's actual
mechanism. §6b resolves the interaction: the filter applies identically
to both branches (a worker's queue subscription is one authorization
boundary, not two), and TF-INV-004's own property — a job "become[s]
eligible for claiming by *another worker*," never a guarantee that a
specific, deliberately-unsubscribed worker will claim it — is preserved
unchanged. What this phase adds is a new precondition for that
eligibility to be realized in practice: every actively-used `queue_name`
must have at least one currently-subscribed worker (§6b, §13, §15),
exactly as TaskForge has always implicitly required at least one worker
recognizing a given `job_type`. Neither is a TF-INV-004 regression;
both are pre-existing categories of operator responsibility that
Phase 13's queue concept extends by one dimension. **Required test**:
SF-049 (§16).

## 11. Migration requirements and rollback concerns

Following [compatibility-policy.md](compatibility-policy.md)'s
expand/migrate/contract model and Phase 12's own measured-lock discipline
(never assert a lock property without measuring it — the two retracted
claims in [data-model.md](data-model.md) "Phase 12 migration lock profile"
are the standing cautionary precedent):

1. **Expand**: add `queue_name` (§7, single catalog-only migration, no
   backfill needed — every existing row's correct value is the literal
   default), add `queue_limits`/`rate_limit_buckets` (new tables, no
   existing-table lock at all), add the new `idx_jobs_claimable_by_queue`
   index. Whether the new index's `CREATE INDEX` (non-`CONCURRENTLY`, since
   every migration runs inside a transaction per TF-INV-013, exactly as
   Phase 12's `0009` could not use `CONCURRENTLY` either) takes `SHARE` and
   blocks writes for its duration **must be measured against this
   migration's actual file**, not assumed safe by analogy — Phase 12's
   `0009` measured a real write-blocking stall doing exactly this kind of
   index build; this phase's index build deserves the same
   `TestPhase1X_...BlocksWritesAndTheClaimQuery`-style test, not a repeated
   optimistic claim.
2. **Migrate**: cut the claim query over to filter by worker-subscribed
   queue names and (once §6a's ADR lands) the selected concurrency
   mechanism. Old and new worker binaries can coexist during this window:
   an old worker binary's claim query has no `queue_name` predicate and
   will keep claiming from every queue exactly as before, which is
   **why** the default subscription semantics in §8/§14 must be "claim
   everything unless configured otherwise" — the old binary's behavior and
   the new binary's *default* behavior must be the same thing.
3. **Contract**: drop the old `idx_jobs_claimable` index only after the new
   one is confirmed live and every worker fleet member is running
   queue-aware code — a separate, later migration file, isolated for its
   own `ACCESS EXCLUSIVE` `DROP INDEX`, mirroring Phase 12 `0010`'s
   precedent exactly (drop-only files are cheap catalog operations but must
   never share a transaction with the index build they retire, or the
   `DROP`'s wait queues behind the `CREATE`'s commit).

**Down migrations**: `queue_name`, `queue_limits`, `rate_limit_buckets`,
and the new claimable index are all genuinely additive with no destructive
data loss on rollback — **data-safe-reversible**, per
[compatibility-policy.md](compatibility-policy.md)'s label requirement, and
should ship ordinary tested `.down.sql` files (drop column/table/index).
The later contract migration that drops `idx_jobs_claimable` is, like Phase
12's `0010`, **not** safely reversible in the general case if the old index
was already relied upon by a not-yet-upgraded reader — but unlike Phase
12's idempotency-index swap, there is no data-uniqueness reason a rebuild
could fail; its `down` can simply recreate the old index unconditionally
(no honest-failure caveat needed, unlike `0010`'s).

**Rollback concern specific to this phase**: if §6a's ADR selects the
slot-table mechanism, provisioning/deprovisioning `queue_slots` rows on an
operator's `set-queue-limit` call is **not** a schema migration at all — it
is ordinary DML issued by `cmd/taskforge-admin`, and must be idempotent
(re-running the same limit-set command must converge to the same slot
count, not accumulate or duplicate slots). This is a runtime-tool
correctness requirement, not a `internal/migrate` concern, and should be
tested as such.

## 12. Observability requirements

New metrics (cardinality-audited against
[observability.md](observability.md)'s existing discipline — `queue_name`
is operator-configured and bounded, exactly like the existing `job_type`
label, so it is safe as a label; `principal_id` is **not** safe as a label,
per the existing "no `job_id`/`idempotency_key`/high-cardinality identifier
as a metric label" rule, so any per-principal governance signal is a
structured log field, never a metric label):

| Metric | Type | Purpose |
|---|---|---|
| `taskforge_queue_depth` | gauge, labeled `queue_name`, `state` | Live per-queue backlog — the queue-scoped analog of the existing `taskforge_jobs_by_state`. |
| `taskforge_queue_running` | gauge, labeled `queue_name` | Current concurrently-`RUNNING` count per queue — the operator-visible half of the concurrency-limit proof. |
| `taskforge_queue_concurrency_limit` | gauge, labeled `queue_name` | The configured cap (from `queue_limits`), so an operator can see "running / limit" without a second query. |
| `taskforge_admission_rejections_total` | counter, labeled `code` (`429`/`503`), `reason` | Split by which of the two backpressure codes fired and why — mirrors `taskforge_auth_failures_total{reason}`'s Phase 12 precedent exactly (server-side detail, uniform caller-facing behavior). |
| `taskforge_retention_rows_deleted_total` | counter, labeled `table` (`jobs`/`job_attempts`/`workflow_instances`/`workflow_nodes`) | What retention actually deleted — the roadmap's own explicit "an operator must be able to see retention actually running, not infer it silently happened" requirement. |
| `taskforge_retention_sweep_duration_seconds` | histogram | How long each batch/sweep cycle takes — informs `TASKFORGE_RETENTION_BATCH_SIZE` tuning. |
| `taskforge_retention_sweep_errors_total` | counter | Sweep failures (e.g., a batch aborted by a lock timeout) — must not be silent. |

New structured log events (extending
[observability.md](observability.md)'s existing vocabulary, not replacing
it): `queue_limit_exceeded`, `rate_limited` (carrying `actor` per Phase
12's precedent — this **is** a submit-path rejection, so the same audit
discipline applies), `admission_rejected_capacity` (the `503` path — no
`actor` needed, since it is not principal-scoped, per §9),
`retention_sweep_started`/`retention_sweep_completed` (with row counts),
`retention_batch_deleted`.

**What this phase does not change**: none of Phase 8's existing metrics
change name, type, or meaning ([compatibility-policy.md](compatibility-policy.md)
"Never repurposes an existing metric name for a different meaning"); this
phase only adds new series.

## 13. Failure modes and adversarial cases

Reproduced from [enterprise-roadmap.md](enterprise-roadmap.md) Phase 13
"Failure scenarios to guard against", each paired with this plan's
mechanism:

| Failure scenario | Mechanism this plan provides |
|---|---|
| A single tenant/queue submits a sustained flood | Per-queue/tenant concurrency cap (§6a) + rate limit (§7/§8) — other queues keep making measurable progress. |
| A queue's concurrency limit is set to 0 or misconfigured | Must fail closed with a clear operator-visible error, not silently accept unlimited concurrency — `cmd/taskforge-admin set-queue-limit -concurrency=0` must be rejected or must genuinely mean "admit nothing," never silently ignored; this plan requires the admin tool to reject `0` explicitly with a distinct error from "unset/unlimited" (`NULL`), since the two are easy to conflate in the schema (§7) and must not be conflated in code. |
| Rate-limit state must survive a restart | `rate_limit_buckets` is a PostgreSQL table (§7), never in-memory-only — consistent with ADR-0001. |
| A retention batch-cleanup job runs during peak claim-query load and degrades claim latency | Batched, bounded-size deletes (§11, §17), measured (not assumed) lock behavior per migration/query, and a documented scheduling recommendation (quiet-window guidance, mirroring Phase 12's `0009` operator obligation in [data-model.md](data-model.md)) rather than a claim of zero impact. |
| A retention cleanup pass deletes a job row whose idempotency key a legitimate late-arriving duplicate still depends on | Resolved structurally by §10's "no second clock" design — the deletion precondition *is* the window boundary. |
| **(this plan, not named by the roadmap's own list)** Retention prunes `job_attempts` history needed by `internal/invariant`'s TF-INV-005 reopening check | Resolved by §10's terminal-only, never-above-`terminal_attempt_count` pruning restriction. |
| **(this plan)** An operator upgrades to Phase 13 without reconfiguring existing workers | §14's default-subscribes-to-everything worker semantics — no job becomes silently unclaimable. |
| **(this plan)** A `429`/`503` response accidentally discloses another tenant's queue state | §9's response-shape discipline (no cross-tenant detail in either body). |
| **(this plan, surfaced by independent review — H1)** An operator partitions the worker fleet by queue subscription such that some actively-used `queue_name` ends up with zero currently-subscribed workers | Not a TaskForge-enforced invariant — the same pre-existing class of risk as zero workers recognizing a given `job_type` today (§6b). This plan's mitigation is operational, not code: §15 documents the staffing requirement explicitly, and §12's `taskforge_queue_depth`/`taskforge_queue_running` gauges are the operator-visible signal for a queue accumulating work (fresh or reclaim-eligible) with nothing draining it. The §14 default (unset `TASKFORGE_WORKER_QUEUES` = subscribe to everything) means this risk exists only when an operator opts into queue partitioning, never in the common case. |
| **(this plan — M2)** An operator configures `TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL` longer than `TASKFORGE_RETENTION_TERMINAL_JOB_TTL`, so the sweeper would try to delete a `jobs` row while its `job_attempts` rows are still inside their own, longer window | Must be rejected by the admin tooling/config validation, not silently accepted (§8). Backstopped even if misconfigured anyway: `job_attempts.job_id`'s FK has no `ON DELETE CASCADE` (§3), so PostgreSQL itself refuses the `jobs` row deletion while `job_attempts` rows for it remain — a loud, observable sweep error (`taskforge_retention_sweep_errors_total`), never silent data loss or an orphaned reference. |

## 14. Compatibility / backward-compatibility requirements

- **Every pre-Phase-13 job/workflow row gets `queue_name = 'default'`**
  (the column's own default), so no existing row becomes unclaimable or
  changes queue identity on upgrade.
- **An old (pre-Phase-13) worker binary continues to function unchanged**
  against a post-Phase-13 schema: its claim query has no `queue_name`
  predicate and simply keeps claiming from every queue, exactly as
  documented for the "old worker, new server" case in
  [compatibility-policy.md](compatibility-policy.md).
- **A new (Phase-13) worker binary with `TASKFORGE_WORKER_QUEUES` unset
  must reproduce old-worker behavior exactly — claim from every queue, not
  only `"default"`.** This is stated as a hard requirement, not a
  preference: if the new default were "claim only `default`," an operator
  who upgrades worker binaries without also updating configuration would
  silently strand any job submitted to a non-default queue, which is a
  correctness regression relative to Phase 1–12's actual (queue-blind)
  behavior — [enterprise-roadmap.md](enterprise-roadmap.md)'s own
  "Preserve all Phase 1–12 behavior and guarantees" instruction for this
  planning pass makes this non-negotiable, not merely convenient.
- **Reclaim of an expired lease is subject to the same queue-subscription
  filter as a fresh claim (§6b), not a looser one.** An old
  (pre-Phase-13) worker binary, and a new worker binary with
  `TASKFORGE_WORKER_QUEUES` unset, both reclaim from every queue exactly
  as they do today — nothing narrows for either. Only a newly configured,
  deliberately queue-restricted worker is narrowed, and only for queues
  it was never eligible to freshly claim from either, so no previously
  reclaimable job becomes unreclaimable by any worker that could reclaim
  it before this phase. This is the same compatibility argument as the
  bullet above, extended to the reclaim branch specifically because §6b
  establishes it is a separate branch of the same query, not an
  afterthought.
- **`POST /jobs`/`POST /workflows` remain backward-compatible** for a
  caller that never sends `queue_name` — it defaults to `"default"`,
  exactly matching every existing row's backfilled value, so an old client
  and a new client's un-set-`queue_name` requests are indistinguishable in
  outcome.
- **No breaking change to any existing response shape** — `queue_name` is
  an additive response field (Phase 11's unknown-field-tolerance already
  proves a client ignoring it is safe).
- **Retention is opt-in by default** (`TASKFORGE_RETENTION_ENABLED=false`)
  — an operator who takes no action after upgrading does not lose any
  historical row they were not already prepared to lose; this is a
  deliberate, conservative default distinct from Phase 12's own hard-cutover
  precedent (OD-6 there was justified by "no established external caller
  base for the *authentication* boundary" — retention instead deletes an
  operator's *own historical data*, which is a strictly higher-consequence,
  harder-to-undo action than rejecting an unauthenticated request, so the
  same hard-cutover reasoning does not transfer and is not applied here).
- **No change to any Phase 1–12 metric or log event's name/meaning**
  (§12).

## 15. Deployment / operational considerations

- **New required PostgreSQL role**: `taskforge_retention` (§9, §17),
  provisioned by an extension to `deploy/postgres-roles.sql`, granted only
  `SELECT`/`DELETE` on the four retention-eligible tables.
- **Retention scheduling window**: per the roadmap's own failure-scenario
  guidance and Phase 12's `0009` precedent, an operator should be able to
  run (or schedule) the sweeper during a quiet/maintenance window sized by
  their own benchmark against a realistically-sized copy of their data —
  this plan documents that obligation; it cannot verify or enforce it from
  inside the process, exactly as Phase 12's TLS/role/migration-timing
  obligations are documented-but-unverifiable ([security-model.md](security-model.md)
  "Deployment boundary summary after Phase 12" is the precedent for this
  framing).
- **Worker fleet reconfiguration is an operator action, not automatic**:
  restricting which queues a given worker pool serves (`TASKFORGE_WORKER_QUEUES`)
  requires a deliberate config change and restart; this plan does not
  invent a dynamic/hot-reloadable subscription mechanism (consistent with
  §5's "no dynamic/adaptive" non-goal, extended here from rate limits to
  subscription config for the same reason: static, operator-driven,
  auditable).
- **New operational requirement (§6b — surfaced by independent review,
  H1): every `queue_name` actually in use must have at least one
  currently-subscribed worker**, satisfied automatically by the §14
  default (unset `TASKFORGE_WORKER_QUEUES` subscribes to everything) and
  requiring deliberate operator attention only once queue partitioning is
  opted into. This is not enforced or verifiable by TaskForge from inside
  the process — the same category as this section's other
  documented-but-unverifiable obligations — and its absence does not
  corrupt state; it silently stops progress on the affected queue,
  including reclaim of its own expired leases (§10's TF-INV-004
  interaction), until an operator notices via §12's `taskforge_queue_depth`/
  `taskforge_queue_running` gauges and corrects the subscription
  configuration.
- **Queue/rate-limit configuration changes take effect on next read**, with
  no cache and no TTL in the enforcement path — mirroring Phase 12's
  revocation-takes-effect-on-next-lookup precedent
  ([phase-12-plan.md](phase-12-plan.md) §6a) — so an operator tightening a
  limit does not need a redeploy.
- **Monitoring**: the metrics in §12 are the operator's primary signal;
  this plan does not ship an opinionated alerting-rule set, consistent with
  [observability.md](observability.md)'s own "produces the instrumentation,
  not an opinionated ops runbook" framing for Phase 8.

## 16. Deterministic test / proof matrix

Continuing the scenario-corpus numbering (`docs/scenario-corpus.md`
currently runs through **SF-036**); this plan proposes **SF-037 onward**,
final numbering to be confirmed at implementation time against whatever the
corpus has grown to by then:

| # | Scenario | Proves |
|---|---|---|
| SF-037 (proposed) | Two-queue fairness stress: one flooded, one starved under the pre-Phase-13 model | Starved queue makes bounded-wait-time progress under the ADR-selected algorithm, over the tested run's duration (roadmap's own required test). |
| SF-038 (proposed) | Concurrency-limit exactness under concurrent claim attempts | Cap is never exceeded, `SKIP LOCKED`-based stress harness (Phase 5 pattern). |
| SF-039 (proposed) | `429` fires only once a configured rate/admission threshold is crossed, never at ordinary concurrency-cap saturation | The roadmap's explicit "reaching cap is not itself a rejection condition" rule. |
| SF-040 (proposed) | `503` fires only under configured system-capacity overload (§8 OD-6's semaphore), retry after `Retry-After` succeeds | Backpressure liveness (§10). |
| SF-041 (proposed) | Misconfigured `concurrency_limit = 0` fails closed with a distinct, clear operator error | §13's "fail closed, not silently unlimited" requirement. |
| SF-042 (proposed) | Rate-limit token-bucket state survives a process restart | Durable-not-in-memory requirement (§7, §13). |
| SF-043 (proposed) | Retention batch cleanup completes without starving concurrent claim-query traffic | Roadmap's own required test; measured, not assumed (§11). |
| SF-044 (proposed) | Idempotency dedup still fires for a row inside its retention window; a duplicate submitted *after* legitimate deletion creates a fresh row rather than erroring | §10's "no second clock" resolution, including the gap this plan adds beyond the roadmap's own stated test. |
| SF-045 (proposed) | Pruning a terminal job's `job_attempts` to zero does not prevent `internal/invariant`'s TF-INV-005 check from detecting a subsequent simulated illegitimate reopening | §10's TF-INV-005/retention interaction proof, not named in the roadmap's own text. |
| SF-046 (proposed) | An old (pre-Phase-13) worker binary and a new worker binary with `TASKFORGE_WORKER_QUEUES` unset both claim from every queue identically | §14's compatibility requirement. |
| SF-047 (proposed) | A worker configured with a queue subset never claims a job outside that subset, even under contention with an unrestricted worker | §6/§8's subscription semantics, adversarial direction. |
| SF-048 (proposed) | `429`/`503` response bodies never reveal another tenant's queue depth, limit configuration, or existence | §9's disclosure discipline, extending Phase 12's existence-disclosure test pattern. |
| SF-049 (proposed, added by independent review — H1) | A worker configured with a restricted queue subset never reclaims an expired lease for a job outside that subset, even though the job is otherwise reclaim-eligible (`lease_expires_at < now()`, attempt budget remaining); a worker with `TASKFORGE_WORKER_QUEUES` unset reclaims it identically to pre-Phase-13 behavior | §6b/§10's TF-INV-004/queue-subscription resolution, both the adversarial direction (no unauthorized queue execution via reclaim) and the compatibility direction (§14). |
| SF-050 (proposed, added by independent review — M2) | Attempt-history retention is independently paced: with `TASKFORGE_RETENTION_JOB_ATTEMPTS_TTL` set shorter than `TASKFORGE_RETENTION_TERMINAL_JOB_TTL`, a terminal job's `job_attempts` rows are pruned while its `jobs` row still exists; a misconfigured value longer than the job TTL is rejected by validation, and if bypassed, fails loudly via the `job_attempts.job_id` foreign key rather than corrupting state | §8's config surface for the roadmap's "independently paced" attempt-history requirement, and its FK-backstop failure mode (§13). |
| — | The required governance ADR itself (§6a, §18 OD-1), committed before implementation begins | Roadmap's own explicit exit-criterion precondition — not a test, a document. |
| — | Full Phase 1–12 regression suite, `internal/chaos` campaigns, unchanged | §10's "no regression" requirement. |

All new tests run against real PostgreSQL, per this project's unbroken
existing discipline (no mocked-database shortcut).

> **Post-ADR reconciliation**: [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md)
> continues this table's numbering from its `SF-050` ceiling — `SF-051`
> through `SF-059` are defined there, not duplicated here, to avoid the
> two-copies drift risk this document avoids elsewhere (§20's own stated
> reasoning). `SF-038` above (this plan's own generic, pre-ADR
> "concurrency-limit exactness under concurrent claim attempts"
> placeholder) is refined, not duplicated or superseded in substance, by
> the ADR's mechanism-specific `SF-051`; the ADR additionally requires
> `SF-052`/`SF-053` (reclaim never writes the slot table; per-row
> slot/job consistency) and, added during the ADR's own drafting after a
> gap was found by direct inspection of the evidence pass's own benchmark
> database, `SF-058`/`SF-059` (the Lazy Dead-Letter Sweep's terminal
> transition releases its job's slot durably and idempotently, including
> the attempt-budget-exhaustion case specifically; and a fault-injection
> proof that terminal transition and slot release cannot leave stranded
> capacity after a crash) — see the ADR's "Required implementation proof
> obligations" for the authoritative text of all of these.

## 17. Files / packages expected to change

- **New**: `internal/governance/` (or similarly named package) —
  `queue_limits`/`rate_limit_buckets` types and store methods; kept
  HTTP-agnostic, matching the existing `internal/job`/`internal/principal`
  split.
- **New**: `internal/retention/` — the sweeper's batch-delete logic,
  independent of whether §18 OD-5 places it in a new binary or an existing
  one's ticker.
- **New migrations**: `0011_add_queue_name.{up,down}.sql`,
  `0012_create_queue_limits_and_rate_limit_buckets.{up,down}.sql`,
  `0013_create_claimable_by_queue_index.{up,down}.sql`, plus — only once
  §6a's ADR is committed — whatever migration the selected concurrency
  mechanism needs (e.g., a `queue_slots` table), and a later, separate
  contract migration dropping the old `idx_jobs_claimable` index (§11).
  Exact numbering to be confirmed against whatever `migrations/` has grown
  to by implementation time (currently ends at `0010`).
- **`internal/store/claim.go`**: `claimQuery` gains a `queue_name`
  predicate (subscribed-queue filter), applied identically to **both**
  `OR`'d branches of its existing `WHERE` clause — the fresh-claim branch
  and the expired-lease reclaim branch alike (§6b) — and, once §6a
  resolves, the selected concurrency-check join; `Claim`'s signature
  likely gains a `queueNames []string` parameter (an internal,
  non-public-API change — `internal/` is unimportable outside this
  module, so this carries none of `txenqueue`'s external-compatibility
  stakes).
- **`internal/store/complete.go`, `cancellation.go`, and the Lazy
  Dead-Letter Sweep in `claim.go`**: each terminalization call site gains a
  slot-release step — no longer conditional (§7's original "only if
  §6a's ADR selects the slot-table mechanism" note is resolved: it did —
  see [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md)). The
  Lazy Dead-Letter Sweep's own release specifically must be durable and
  idempotent (SF-058) — the ADR's own drafting found this exact call site
  untested by every prior evidence pass, not merely one of an
  interchangeable list.
- **`internal/api/handlers.go`, `workflow_handlers.go`**: accept/attach
  `queue_name` on submission; new admission/rate-limit check ahead of the
  existing insert path (§6, §9).
- **`internal/worker/worker.go`**: gains queue-subscription configuration
  threading (§8), defaulting per §14.
- **`internal/config/config.go`**: new fields/env vars per §8.
- **`internal/metrics`**: new collectors/counters per §12.
- **`cmd/taskforge-admin`**: new subcommands per §8.
- **New (or existing binary extended)**: the retention sweeper entry
  point — resolved by §18 OD-5.
- **`deploy/postgres-roles.sql`**: new `taskforge_retention` role (§9,
  §15).
- **`docs/data-model.md`, `docs/security-model.md`,
  `docs/observability.md`, `README.md`**: updated once implemented, same
  discipline as every prior phase ("this planning pass does not pre-claim
  completion" — Phase 12's own phrasing in
  [phase-12-plan.md](phase-12-plan.md) §13, reused deliberately here).
- **`docs/invariants.md`**: **not** modified by this plan or by Phase 13's
  eventual implementation PR directly — see §18 OD-3; any `TF-INV-0NN`
  allocation is the separate, cross-phase governance PR Phase 12's OD-8
  already named.
- **New ADR**: `docs/adr/0009-<title>.md` (next available number; current
  highest is `0008`) — the mandatory fairness/concurrency-mechanism
  decision record (§6a, §18 OD-1), required **before** the implementation
  PR, not part of it.

## 18. Unresolved architectural decisions

Two of these are **hard blockers** the roadmap itself requires to clear
before implementation starts — not ordinary open questions to resolve
opportunistically during implementation.

| ID | Topic | Status | This plan's position |
|---|---|---|---|
| **OD-1** *(blocker, now resolved)* | Concurrency-limit enforcement mechanism + fairness algorithm | **Resolved** — [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md), drafted against a three-pass concurrency/performance evidence package (not yet committed; see the post-ADR update above) | §6a's three concurrency-mechanism candidates and two fairness candidates were evaluated exactly as scoped here, not extended: ADR-0009 selects the slot-table semaphore (candidate 2) and `last_claimed_at` ascending ordering (candidate B), rejecting advisory lock, `SERIALIZABLE`+retry, and round-robin with specific, measured failure modes for each. TF-INV-019's bound is defined as E − 1 service opportunities (a parametric, hardware-independent form), and the reclaim/slot-ownership question this plan's own §6a/§7 left implicit is now decided explicitly (a reclaim retains its original slot; never re-acquires). |
| **OD-2** | Per-principal (not just per-queue) concurrency/rate limits | Open | Roadmap hedges with "where supported." Schema (§7) supports both from day one (`queue_limits` keyed by `(queue_name, principal_id)`); whether principal-scoped enforcement ships in the same implementation PR or a fast-follow is the ADR's/implementer's call, not fixed here. |
| **OD-3** *(blocker, now resolved)* | Formal `TF-INV-0NN` allocation for Phase 13's fairness property (and Phase 12's still-unallocated G1–G8) | **Resolved** — the cross-phase governance pass landed in [invariants.md](invariants.md) "Cross-Phase Governance Additions": Phase 12's G2 → `TF-INV-017`, G7 → `TF-INV-018`; G1/G3/G4/G5/G6/G8 remain symbolic guarantees proven by verification points, not invariants; Phase 13's fairness property is confirmed as `TF-INV-019` (not the roadmap's tentative `TF-INV-017`, since `017`/`018` went to Phase 12 instead). | This blocker is cleared. The property's wording is unchanged from §10 below; only its ID moved from tentative `TF-INV-017` to confirmed `TF-INV-019`. The concurrency/fairness *algorithm* itself (OD-1) remains open — this governance pass deliberately did not decide it. |
| **OD-4** | Default worker queue-subscription semantics | Recommended, not yet approved | §14: unset `TASKFORGE_WORKER_QUEUES` means "claim everything," for compatibility. Recommended with a firm rationale; still listed as open pending explicit sign-off since it is a behavioral default future operators will rely on. **Extended by this correction pass**: this same subscription filter governs reclaim of an expired lease identically to a fresh claim, not a looser rule (§6b) — one behavioral default, one sign-off, not two. |
| **OD-5** | Retention sweeper's process shape | Open | Candidates: (a) a ticker inside `cmd/worker` (fewest binaries, but couples maintenance load to the claim-serving process); (b) a standalone `cmd/taskforge-retention` binary, operator/cron-invoked (cleanest isolation, one more binary to deploy/document); (c) a ticker inside `cmd/api`. No recommendation is fixed here; (b) is favored for isolating maintenance-window scheduling (§15) from request/claim serving, but this trades off operational simplicity the ADR/implementer should weigh. |
| **OD-6** | `503` system-capacity admission mechanism | Recommended, not yet approved | §8: an in-process bounded semaphore (`TASKFORGE_MAX_INFLIGHT_SUBMISSIONS`) in `cmd/api`. Default threshold value intentionally not fixed (mirrors `ActiveWorkerWindow`'s precedent of leaving a numeric default to implementation-time judgment). |
| **OD-7** | `job_attempts` retention scope | Recommended, not yet approved | §5, §10: terminal jobs only, never above `terminal_attempt_count`. Pruning a still-in-flight job's superseded attempts is explicitly out of scope for this plan (§5), not merely deferred silently. |
| **OD-8** | `queue_name` validation | Recommended, not yet approved | Free-form `text`, no registry/allow-list table — mirrors the existing `job_type` precedent exactly (no `job_types` table exists either). |
| **OD-9** | Rate-limit scope-key composition (`queue` only, `principal` only, or both) | Open | §7's `rate_limit_buckets.scope_key` is a free-form text key precisely so this composition question does not require a schema change either way it is answered — but the exact key format (and whether both a queue-level and a principal-level bucket both apply to one request, and if so how they combine) is unresolved and should be settled in or alongside OD-1's ADP, since it interacts with the same concurrency/fairness analysis. |

## 19. Risks and carry-over items

- **Risk**: §6a's chosen mechanism could underperform under real production
  concurrency (advisory-lock contention on a hot queue; slot-table
  contention if slot count is small relative to worker count). This is
  exactly why the roadmap mandates a concurrency/performance analysis
  *before* algorithm selection, not after — this plan does not shortcut
  that analysis with an untested recommendation.
- **Risk**: retention is inherently destructive and, unlike every prior
  phase's additive schema work, this phase's core deliverable is designed
  to delete data. A bug in the batch-delete predicate (e.g., an off-by-one
  in the idempotency-window boundary, or a queue/predicate mismatch between
  §10's "no second clock" design and its actual SQL implementation) is not
  recoverable from backups alone without Phase 15's (not-yet-built) PITR
  procedure. This raises the bar for the required test matrix (§16) beyond
  the roadmap's own minimum list, which is why §16 adds SF-044/SF-045
  beyond what the roadmap itself names.
- **Risk**: the slot-table mechanism (if selected) adds a new
  responsibility to every terminalization call site (§7, §17) — a
  regression there (a forgotten slot release on one failure path) would
  silently shrink effective concurrency capacity over time without an
  obvious symptom until an operator notices `taskforge_queue_running`
  plateauing below `taskforge_queue_concurrency_limit` with pending work
  waiting. §12's metrics are the intended detection signal; a dedicated
  "slot leak" test should be part of whichever ADR selects this mechanism.
- **Carry-over from Phase 12, opportunistically related but not in this
  phase's scope**: [security-model.md](security-model.md) §5 names two
  known Phase 12 follow-ups — the `401`-vs-`503` database-outage-during-auth
  mapping, and unbounded `last_used_at` write concurrency. Phase 13
  introduces its own, purpose-built `503` semantics (§8 OD-6); an
  implementer may find it natural to revisit Phase 12's `401`/`503`
  mapping using the same reasoning at the same time, but this plan does
  **not** fold that fix into Phase 13's scope — it remains a Phase-12-owned
  item unless separately re-scoped.
- **Carry-over**: [security-model.md](security-model.md) §1's
  "Abusive retry workload" P2 (large caller-supplied `max_attempts`
  amplifying shared-resource contention) is explicitly named as "accepted
  as a deferred risk pending Phase 13's governance work" — this plan's
  concurrency limits (§6a, §7) are the intended mitigation once
  implemented, but this plan does not add a new, separate `max_attempts`
  cap; the existing Phase 11 storage-representability bound is unchanged
  (per §5's "no change to any Phase 1–12 mechanism" framing) and
  contention is expected to be bounded indirectly, via per-queue/tenant
  concurrency caps, not by a new direct limit on the field itself.
- **Risk, explicit**: this plan's OD-1/OD-3 blockers mean Phase 13's actual
  implementation timeline depends on two artifacts this document does not
  produce (the ADR, the governance PR). Treating this plan as
  "implementation-ready" for the parts gated behind those blockers would
  be inaccurate; it is implementation-ready for everything *except* the
  exact concurrency/fairness mechanism and the formal invariant ID, both of
  which have concrete, bounded next steps (§18) rather than open-ended
  ambiguity.

## 20. Exact exit criteria

Adopted verbatim from [enterprise-roadmap.md](enterprise-roadmap.md) Phase
13 "Enterprise exit criteria" as the acceptance bar (unchanged, not
restated with different wording, to avoid the two-copies drift risk Phase
12's own §12 explicitly avoided the same way):

- [ ] An ADR selecting the Phase 13 fairness/scheduling algorithm exists,
      committed before implementation, stating the concurrency/performance
      analysis behind the choice. **(§18 OD-1 — hard blocker.)** **Drafted**:
      [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md) exists in
      the working tree and states the required analysis (three evidence
      passes, see the post-ADR update above). Left unchecked because this
      criterion's own text requires the ADR be **committed**, which it is
      not yet — do not mark this done on the strength of the draft alone.
- [ ] A named-queue or per-job-type/tenant concurrency-limit mechanism
      exists with a passing test demonstrating one queue/tenant cannot
      starve another beyond the documented bound (a bounded, not
      indefinite, guarantee).
- [ ] Rate limiting exists for at least one dimension (queue or tenant) and
      is durable across a restart.
- [ ] `POST /jobs` returns a documented `429`/`503` + `Retry-After` only
      under configured admission/rate/capacity overload, verified by test,
      with the two codes' semantics kept distinct.
- [ ] A terminal-record retention/lifecycle policy is documented and
      implemented for `jobs`, `job_attempts`, and workflow rows, with
      batched cleanup, cleanup observability, and a proven
      idempotency-safe retention window.

**Added by this plan, as preconditions this planning pass identified that
the roadmap's own checklist does not spell out as separate line items but
that block the checklist above from being honestly checkable:**

- [x] The invariant-ID governance PR (§18 OD-3) has landed, so this
      phase's fairness property has a confirmed, non-tentative ID before
      its own exit criteria are declared met. **Done**: confirmed as
      `TF-INV-019` in [invariants.md](invariants.md) — see the
      post-planning update at the top of this document. This does not by
      itself satisfy any other exit-criteria checkbox on this list; it
      only clears the ID-allocation precondition.
- [ ] The TF-INV-005/retention interaction proof (§10, SF-045) passes,
      confirming retention does not silently weaken an existing, numbered
      invariant's durable detection mechanism.
- [ ] The idempotency-window "no second clock" test (§10, SF-044),
      including the post-deletion-resubmission case this plan adds beyond
      the roadmap's own stated test, passes.
- [ ] A full Phase 1–12 regression suite and `internal/chaos` campaign run
      with zero invariant violations against the Phase-13-modified schema
      and claim query.

---

## Cross-references

[enterprise-roadmap.md](enterprise-roadmap.md) (Phase 13's authoritative
scope), [security-model.md](security-model.md) §1/§2/§7,
[data-model.md](data-model.md), [invariants.md](invariants.md),
[compatibility-policy.md](compatibility-policy.md),
[observability.md](observability.md), [reference-analysis.md](reference-analysis.md),
[enterprise-readiness.md](enterprise-readiness.md) §3/§13,
[slo.md](slo.md) "Overload behavior", [phase-12-plan.md](phase-12-plan.md)
(precedent for format, migration-lock discipline, and the OD-8 governance
deferral this plan extends).
