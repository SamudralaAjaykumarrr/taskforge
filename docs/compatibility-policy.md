# Compatibility / Evolution Policy

Status: **review document, mostly PROPOSED, partially implemented.**
This document originally marked every rule `PROPOSED` because none of it
was implemented, tested, or proven. As of Phase 11
(docs/enterprise-roadmap.md), the "API Evolution" and "Transactional
Enqueue" sections below are now implemented and proven — each such rule is
labeled "Status: implemented" at its section header, with the executable
test(s) that prove it. Every rule not so labeled remains `PROPOSED`: a
specification of what TaskForge *should* adopt, not a claim that it already
holds. Treating a still-PROPOSED guarantee as an existing one would be
exactly the kind of unsupported claim [vision.md](vision.md) and
[invariants.md](invariants.md) exist to prevent — cite only the
sections explicitly marked implemented as evidence of a shipped
compatibility guarantee.

## Why This Document Exists Now

TaskForge has gone through exactly one schema-evolution event of real
consequence — migration `0003_create_workflow_tables`, adding two new
tables with no changes to `jobs` itself — with zero in-flight production
traffic to preserve across it. Phases 3, 4, and 6 each explicitly required
"no new migration" by deliberate upfront schema design (columns like
`RETRY_WAIT`, `eligible_at`, `cancel_requested`, and `last_error_class` were
all present in migration `0001`, unused until later phases needed them).
This is good foresight, but it also means TaskForge has never actually been
forced to solve a hard compatibility problem — old worker talking to new
server, a job payload shape changing while old rows of the old shape are
still in flight, or a rolling deployment with two server versions live at
once. This document names the rules that will be needed before that first
real test arrives in production, so the policy is decided deliberately
rather than under incident pressure.

## API Evolution

Status: **implemented** as of Phase 11 (docs/enterprise-roadmap.md).

- **An API version prefix now exists**: every job/workflow endpoint is
  served under both `/v1/...` (the current canonical surface) and its
  original unprefixed path (`internal/api.NewRouter`). The unprefixed
  surface is not removed or redirected by this phase — it remains fully
  functional, and carries a `Deprecation: @1788998400` response header
  (RFC 9745, https://www.rfc-editor.org/rfc/rfc9745 -- Deprecation is an
  HTTP Structured Field Item whose value MUST be a Date, serialized
  `@<unix-seconds>`; the bare `Deprecation: true` this codebase originally
  shipped was not valid RFC 9745 syntax and is corrected here.
  `1788998400` is the fixed Phase 11 deprecation *effective* date,
  `2026-09-10T00:00:00Z` -- not a removal/sunset date; see below) plus a
  structured `deprecated_route_used` log line on every request, per the
  deprecation policy below. A future, separately-decided release may
  remove it, no sooner than the deprecation window this document already
  specifies.
- **Additive changes** (a new optional request field, a new response
  field) do not require a version bump. **Proven, not merely assumed**:
  `POST /jobs`'s and `POST /workflows`'s JSON decoders no longer call
  `DisallowUnknownFields` (audit finding: they previously did, which
  contradicted this very rule — closed by Phase 11) — an unrecognized
  request field is silently ignored, not rejected, in both directions
  (an old-shape request missing a newer optional field, and a
  new-shape request carrying a field this server version does not yet
  recognize). See `internal/api/handlers_phase11_test.go`'s
  `TestCreateJob_UnknownFieldsAreIgnored_RoundTrip` and
  `TestCreateWorkflow_UnknownFieldsAreIgnored`. Documented clients are
  still expected to ignore unknown response fields (unchanged, untested
  in this direction — this codebase does not ship a client that could
  regress it).
- **Breaking changes** (removing/renaming a field, changing a field's
  type or semantics, changing a status code's meaning) require a new
  version prefix and a documented deprecation window for the old one.
- **Request body size limit, implemented**: `POST /jobs` and
  `POST /workflows` both reject a request body larger than
  `api.MaxRequestBodyBytes` (1 MiB) with `413 Request Entity Too Large`,
  enforced via `http.MaxBytesReader` *before* JSON decoding begins (so an
  oversized body is rejected while it is still being read, not only after
  it has been fully buffered) — see
  `TestCreateJob_OversizedBodyRejected413` /
  `TestCreateWorkflow_OversizedBodyRejected413`.
- **`max_attempts` upper bound, decided**: TaskForge deliberately enforces
  **no arbitrary product/operational** upper bound on caller-supplied
  `max_attempts` beyond the value PostgreSQL's `jobs.max_attempts` column
  (`INTEGER`, a 32-bit signed integer) can actually represent (see the
  storage-representability bound immediately below). This is an explicit
  policy decision (docs/security-model.md's "Abusive retry workload" P2,
  docs/retry-semantics.md "Open Questions"), not an oversight: a large
  `max_attempts` amplifies retry-scheduling churn for that one job and
  cannot bypass TF-INV-006's ceiling or create additional jobs -- but,
  corrected (Phase 11 audit finding): this is **not** a claim that a large
  `max_attempts` is free or isolated. Every retry attempt still consumes
  shared worker, database, and scheduling/claim-query resources, so a
  sufficiently large configured retry budget can create real
  shared-resource pressure that could be felt by other jobs/tenants
  contending for the same claim query and worker pool. That operational/
  governance concern is an accepted, explicitly **deferred** risk for
  Phase 13's workload-governance work (per-queue/tenant concurrency limits
  and fairness), not something this phase claims is harmless. No arbitrary
  numeric cap is imposed *for that reason* -- one that could reject a
  previously-valid caller with a legitimate need for many attempts -- but
  the risk itself is real and unmitigated until Phase 13.
- **`max_attempts` storage-representability bound, implemented**:
  independent of the policy decision above, `internal/job.ValidateSubmission`
  rejects any caller-supplied `max_attempts` greater than
  `job.MaxRepresentableMaxAttempts` (`math.MaxInt32`, matching the
  `jobs.max_attempts` `INTEGER` column's range) with an ordinary
  400/invalid-request response, at every submission entry point
  (`POST /jobs`, `POST /workflows`, `txenqueue`) -- before any INSERT is
  attempted. This closes an API-contract hole (Phase 11 audit finding): Go's
  `int` is 64-bit on every platform TaskForge ships for, so a caller-supplied
  value could previously be a valid Go `int` (passing a naive `>= 1` check)
  while being outside PostgreSQL's `INTEGER` range, which would otherwise
  have surfaced as an internal 500 once the INSERT itself failed. This is a
  storage-representability bound, distinct from (and not a replacement for)
  the no-arbitrary-operational-cap decision above -- see
  `internal/job/validate_test.go` and
  `internal/api/handlers_phase11_test.go`'s
  `TestCreateJob_MaxAttempts_LargestRepresentableValue_Accepted` /
  `TestCreateJob_MaxAttempts_FirstUnrepresentableValue_Rejected400NotDBError`.
- PROPOSED deprecation window: a minimum of one full minor-version release
  cycle with the old surface still functional and a `Deprecation`/`Sunset`
  response header (or equivalent), before removal. The header mechanism
  itself is now implemented (see above); the *removal* decision for the
  legacy unprefixed routes remains PROPOSED/future — Phase 11 does not
  remove them, and this document does not set a Sunset date.

## Transactional Enqueue: Same-Database Atomicity Only (Phase 11)

Status: **implemented**. See docs/transactional-enqueue.md for the full
specification; summarized here because it is itself a compatibility-shaped
guarantee (what TaskForge does and does not promise about a caller's own
transaction boundary).

The `txenqueue` Go package (repository root) lets a caller enqueue a job
using a `pgx.Tx` the caller already owns, so a business-data write and the
job insert commit together or neither does — **when both live in the same
PostgreSQL database**. This required no schema change: the existing `jobs`
table (docs/data-model.md) already supports a plain `INSERT` from any
transaction, including one a caller opened, so no migration was added for
this phase.

**Phase 13 update**: `EnqueueTx`'s underlying insert now also upserts a
`queue_state` row (migration `0013`) in the same statement and transaction,
which changes the *minimum PostgreSQL privilege* an external integrator's
own custom database role needs — a compatibility-relevant operational
change, though not a Go API break (the new `QueueName` request field is
optional and defaults identically to pre-Phase-13 behavior). See
docs/transactional-enqueue.md "Required database privilege" for the exact
grant required and why, and `txenqueue/queue_state_privilege_test.go` for
the proof against real PostgreSQL.

This guarantee is explicitly **not** extended, and cannot be, across two
different databases or any non-PostgreSQL system. TaskForge does not
implement distributed transactions or two-phase commit. The documented
pattern for that case is the transactional outbox (docs/transactional-enqueue.md;
also referenced from docs/idempotency.md's execution-idempotency
patterns, which already named it for a different reason — a handler's own
side effect — before this phase existed). The outbox relay's own call
target is the canonical `POST /v1/jobs` route, not the deprecated legacy
`POST /jobs` alias, per the "API Evolution" rules above.

`txenqueue`'s public compatibility surface is deliberately small and
Go-internal-detail-free: its `Job` return type is a package-owned
projection (`struct{ ID uuid.UUID }`), not a type alias onto
`internal/job.Job`, so TaskForge's internal durable-row shape is free to
change without being part of this package's public contract; and every
error `EnqueueTx` returns is classifiable via `errors.Is` against one of
four sentinel errors (`txenqueue/errors.go`), never the raw
`internal/store`/PostgreSQL error text — see
docs/transactional-enqueue.md "Error contract."

## PROPOSED: Database Migrations

- **Current practice (works, but untested under load):** migrations are
  additive-only so far — every phase from 3 through 9 required zero schema
  changes, and the one real schema change (`0003`) added new tables rather
  than altering `jobs`. This has never been stress-tested against a
  concurrently-running old server version still writing to the old schema
  shape.
- **PROPOSED primary model: expand / migrate / contract**, not a universal
  "every migration must have a working down migration" rule. Concretely:
  1. **Expand** — ship an additive schema change (new nullable column, new
     table, new default) that both the old and new binary can coexist
     with.
  2. **Migrate** — backfill/migrate data if required, with old and new
     binaries both still running against the expanded schema.
  3. **Contract** — only in a later, separate release, once every reader/
     writer of the old shape has been confirmed stopped, remove/alter the
     old shape.
  This is the same rolling-compatible sequencing the two forward-
  compatibility rules below already imply; expand/migrate/contract names it
  as the general policy rather than restating it per-rule.
- PROPOSED rule: every migration must be forward-compatible with the
  immediately-preceding server version for the duration of a rolling
  deployment — i.e., a migration that adds a column must give it a
  default or allow `NULL`, so an old server binary that does not know
  about the column can still write the row; a migration that removes a
  column must first ship a server version that stops reading it, deployed
  and confirmed live, before a later migration drops it (this is exactly
  the "expand" then "contract" split above).
- PROPOSED rule: no migration may lock the `jobs` or `job_attempts` tables
  for a duration incompatible with continuous claim-query traffic (e.g., a
  full-table `ALTER TABLE ... ADD COLUMN ... NOT NULL DEFAULT` rewrite on a
  large table). This has not been tested at any table size beyond Phase
  9's soak run scale (hundreds of rows).
- PROPOSED rule (replaces a universal down-migration requirement): a
  **down migration is required only where rollback is genuinely
  data-safe** — i.e., where reversing it cannot destroy information already
  written under the new shape (a purely additive column with no writes yet
  migrated into it, for example). A migration that is not genuinely
  data-safe to reverse (e.g., one that drops a column, or one where new
  rows have already been written in a shape the old schema cannot
  represent) should be **forward-fixed** by a new, later migration rather
  than reversed — a down migration cannot honestly reconstruct data it
  never had. CI should run down migrations only for the subset marked
  data-safe-reversible, and that subset must be explicitly labeled as such
  in the migration file itself, not assumed. A blanket "every migration
  must have a tested down migration" rule is actively misleading for
  destructive changes, because it implies a recoverability property the
  down migration cannot actually deliver.

## PROPOSED: Rolling Server Upgrades

Not implemented, not tested. TaskForge's stateless-API-server /
stateless-worker architecture ([architecture.md](architecture.md)) is
structurally favorable to rolling upgrades (no server-local state to
migrate), but "structurally favorable" is not the same claim as "proven."

- PROPOSED requirement before this can be claimed as a real guarantee: an
  integration test that runs two different binary versions of `cmd/api`
  (or `cmd/worker`) concurrently against the same database and asserts
  correct behavior throughout, for the full duration of the expand/migrate/
  contract window (not just at the two endpoints of a migration).
- PROPOSED requirement: a documented, operator-facing graceful-drain
  procedure (SIGTERM stops accepting new claims/requests, waits for
  in-flight work up to a timeout) is part of this guarantee, not a separate
  concern — a rolling upgrade that kills in-flight work mid-deploy is not
  actually a safe rolling upgrade. This formalizes, at the operator-process
  level, what Phase 5's `TestStress_WorkerPoolGracefulShutdown_*` already
  proves at the goroutine level. See
  [enterprise-roadmap.md](enterprise-roadmap.md) Phase 14, where both the
  mixed-version proof and this drain procedure are scheduled to land
  together.

## PROPOSED: Old Worker / New Server and New Worker / Old Server Compatibility

This is presently **untested in both directions**:

- **Old worker, new server** (a worker binary lags behind a schema/API
  change): the worker only ever talks to PostgreSQL directly via
  `internal/store`, not through the API server — so "new server" mostly
  means "new schema." An old worker's `Claim`/`Heartbeat`/`Complete*`
  queries are shaped against a specific set of columns; a new migration
  that adds a column with a safe default should not break an old worker's
  queries (PROPOSED rule, not yet tested), but a migration that changes an
  existing column's type or semantics would.
- **New worker, old server** (a worker binary is ahead of the deployed
  schema): a new worker calling a query that references a column that does
  not exist yet in the currently-deployed schema will fail outright. This
  is why PROPOSED database-migration ordering (migrate first, deploy worker
  second) matters and should be documented as the required deployment
  sequence, not left to operator judgment.
- No test exercises either direction today. This is the single largest
  concrete gap this document identifies — it is entirely plausible for a
  real deployment.

## PROPOSED: Job Payload/Schema Evolution

`payload` is an opaque `jsonb` column TaskForge never validates
([data-model.md](data-model.md): "Opaque to TaskForge; interpreted by the
job handler"). This means:

- TaskForge itself imposes **no** payload versioning requirement or
  constraint — by design, and correctly so; a job engine should not need
  to understand payload semantics.
- The consequence, not currently documented anywhere, is that **the job
  handler author is entirely responsible** for payload schema
  compatibility across a deployment where old-shape payloads may still be
  in the queue (submitted before a handler upgrade) when a new handler
  version is deployed that expects a new payload shape.
- PROPOSED guidance (documentation only, no code change): recommend job
  authors embed an explicit `schema_version` field inside `payload` and
  have handlers branch on it, exactly the same pattern already recommended
  for idempotency in [idempotency.md](idempotency.md). This is guidance
  for job authors, not a TaskForge-enforced mechanism — consistent with how
  TaskForge already treats idempotency.

## PROPOSED: Long-Running Scheduled Jobs Across Deployments

- A job scheduled far in the future (`scheduled_at`) is durable
  ([scheduling.md](scheduling.md)) and will survive any number of
  deployments between submission and eligibility, by construction — this
  part is already proven (TF-INV-011, SF-013).
- What is **not** addressed: if the handler for that job's `job_type` is
  removed or its behavior changes incompatibly between submission and
  execution, TaskForge has no mechanism to detect or reject this — the
  job will simply be claimed and handed to whatever handler is currently
  registered for that `job_type` string, or fail to find one at all
  (behavior in that case is not documented). PROPOSED: document the
  expected failure mode when a claimed job's `job_type` has no registered
  handler, and recommend job authors never repurpose a `job_type` string
  for an incompatible payload shape.

## PROPOSED: Workflows Surviving Deployments

- A workflow instance's structure (`workflow_instances`/`workflow_nodes`,
  including `depends_on`) is fixed at submission time and is durable
  ([workflows.md](workflows.md): "v1-of-workflows assumes the full DAG is
  known at submission time" — dynamic modification is an explicit
  deferral). This means an in-flight workflow's *shape* cannot be
  affected by a deployment, by construction — a real strength.
- What is **not** addressed: if a workflow's node `job_type`s are backed by
  handlers that change behavior (or are removed) between workflow
  submission and a given node's eventual execution, the same gap as
  "long-running scheduled jobs" above applies, node-by-node.
- No workflow-definition versioning concept exists (there is no
  "workflow template" or named/versioned workflow definition in the schema
  at all — every `POST /workflows` call submits a fully concrete DAG of
  already-resolved `job_type`/`payload` pairs, per [workflows.md](workflows.md)).
  This is arguably correct simplicity for TaskForge's current DAG model
  (there is no abstract "workflow definition" to version — only concrete
  instances), and this review does not recommend inventing a
  workflow-definition-versioning system without a demonstrated need for
  one.

## PROPOSED: Backward Compatibility Policy (General Statement)

PROPOSED: adopt a policy stating that within a major version, TaskForge:

1. Never removes a documented API field or endpoint without a deprecation
   window (see "API Evolution" above).
2. Never changes the meaning of an existing job/workflow state or
   `job_attempts` outcome value.
3. Never changes the retry backoff formula's default constants without a
   documented, opt-in migration path — [retry-semantics.md](retry-semantics.md)'s
   own "Open Questions" section already flags per-job-type backoff
   configuration as deferred; that deferral should not be resolved by
   silently changing the global default.
4. Never repurposes an existing metric name for a different meaning
   (only adds new metrics or new labels on existing ones, per
   [observability.md](observability.md)'s cardinality discipline).

## PROPOSED: Deprecation Policy

PROPOSED, not yet needed in practice (TaskForge has not yet deprecated
anything): any deprecation must (a) be announced in `docs/roadmap.md` or a
successor changelog, (b) remain functional for at least one full
minor-version cycle, (c) emit a structured log warning (not merely a
comment) when the deprecated surface is used, so operators can measure
actual usage before removal, mirroring the existing discipline of never
silently changing behavior that [invariants.md](invariants.md) already
embodies for correctness properties.

## What Is Marked PROPOSED vs. What Actually Exists Today

To be unambiguous, restating what is **not** proposed but already true
today, because it was built correctly from the start:

- Schema additions so far have never required breaking an existing server
  version (verified: migrations `0001`→`0004`, and Phases 3/4/6/11 needing
  zero new migrations).
- Scheduled-job and workflow-structure durability across restarts/deploys
  is already proven (TF-INV-011, TF-INV-012, SF-013, SF-029).
- Job payload opacity is an existing, deliberate architectural property,
  not a proposal.
- **As of Phase 11**: the `/v1/` API version prefix, unknown-request-field
  tolerance, the request-body size limit, the `max_attempts`
  no-upper-bound policy decision, and same-PostgreSQL-transaction
  atomicity (`txenqueue`) are all **implemented and proven**, not
  proposed — see the "API Evolution" and "Transactional Enqueue" sections
  above.

Everything else in this document — rolling-upgrade proof, old/new
worker-server compatibility testing, payload `schema_version` convention,
and the deprecation window's actual *removal* decision for the legacy
unprefixed routes — remains **PROPOSED and unimplemented** as of this
review (targeted at Phase 14, docs/enterprise-roadmap.md).
