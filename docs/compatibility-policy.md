# Compatibility / Evolution Policy

Status: **mostly implemented and proven, as of Phase 14.** This document
originally marked every rule `PROPOSED` because none of it was
implemented, tested, or proven. As of Phase 11 (docs/enterprise-roadmap.md),
the "API Evolution" and "Transactional Enqueue" sections were the first to
become implemented and proven. Phase 14 (docs/enterprise-roadmap.md,
[phase-14-plan.md](phase-14-plan.md), [ADR-0010](adr/0010-expand-migrate-contract.md))
converts nearly everything else that remained `PROPOSED` into proven
policy: Database Migrations (expand/migrate/contract, machine-checked),
Rolling Server Upgrades, Old/New Worker-Server Compatibility, the
unregistered-`job_type` sub-claims of the two deployment-durability
sections, the Backward Compatibility Policy statement, and the
Deprecation Policy. Each such rule is labeled "Status: implemented" at its
section header, with the executable test(s) that prove it. Only "PROPOSED:
Job Payload/Schema Evolution" remains genuinely `PROPOSED` (the
`schema_version` convention is documentation guidance, not an enforced
mechanism — Phase 14 adds a demonstrated failure mode if it is violated,
but does not change the guidance itself into a requirement), and only the
legacy unprefixed API routes' actual removal/Sunset *date* remains
undecided (the deprecation *policy* itself is now adopted). Treating a
still-PROPOSED guarantee as an existing one would be exactly the kind of
unsupported claim [vision.md](vision.md) and [invariants.md](invariants.md)
exist to prevent — cite only the sections explicitly marked implemented as
evidence of a shipped compatibility guarantee.

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

## Database Migrations

Status: **implemented** as of Phase 14 ([enterprise-roadmap.md](enterprise-roadmap.md),
[phase-14-plan.md](phase-14-plan.md), [ADR-0010](adr/0010-expand-migrate-contract.md)).

- **Adopted primary model: expand / migrate / contract**, formally, per
  [ADR-0010](adr/0010-expand-migrate-contract.md) — this **replaces** any
  universal "every migration must have a working down migration" rule; no
  such rule is adopted, anywhere, for this project. Concretely:
  1. **Expand** — ship an additive schema change (new nullable/defaulted
     column, new table, new index) that both the old and new binary can
     coexist with.
  2. **Migrate** — backfill/migrate data if required, with old and new
     binaries both still running against the expanded schema.
  3. **Contract** — only in a later, separate migration, once every
     reader/writer of the old shape is confirmed stopped, remove or alter
     the old shape.
  Migration ordering is a required deployment sequence: schema migration
  first (confirmed applied), worker/server binary deployment second —
  never the reverse, since a new binary's queries may reference
  columns/tables that do not yet exist under the old schema. This was
  already informally true (`migrate.Up` runs automatically at process
  startup in both `cmd/api` and `cmd/worker`); Phase 14's two-binary-version
  proof (SF-066/067, below) is what makes deviating from it demonstrably
  unsafe rather than merely assumed so.
- **Forward-compatibility rule, implemented**: a migration that adds a
  column gives it a default or allows `NULL`, so an old server binary that
  does not know about the column can still write the row (proved for real,
  separately-compiled binaries by SF-066/067); a migration that removes a
  column ships only after a server version that stops reading it has been
  deployed and confirmed live.
- **Lock-safety discipline, implemented and measured, not merely
  asserted**: no migration in this project's history holds an `ACCESS
  EXCLUSIVE` lock across a full-table scan or write — see
  [data-model.md](data-model.md)'s "Phase 12 migration lock profile" and
  "Phase 13 migration lock profile" sections for the exact, measured lock
  behavior of every migration to date. This is a documented discipline for
  every migration going forward, not a one-time audit; a new migration
  that reintroduces the pattern those sections retracted (an `ACCESS
  EXCLUSIVE` statement sharing a transaction with a full-table scan) is a
  regression against this policy.
- **Down-migration requirement, implemented as a machine-checked
  classification, not an ad hoc judgment call**: a down migration is
  required to actually work, and to be exercised in CI, **only** for
  migrations classified `data-safe-reversible` — those where reversing
  cannot destroy information already written under the new shape. A
  migration that is not genuinely safe to reverse is classified
  `forward-fix-only`: its remediation path is a new, later migration that
  fixes forward, never a down migration pretending to honestly undo it.
  Concretely:
  - Every `.down.sql` file's first line carries a
    `-- taskforge:down-migration-status: data-safe-reversible` or
    `forward-fix-only` marker, parsed and validated by
    `internal/migrate.Migrations()` — a file with neither value, or an
    unrecognized one, fails the check. All fourteen existing migrations
    are labeled today: `0001`, `0002`, `0003`, `0005` (whole-table
    creation — reversing them would destroy all data ever written to that
    table) are `forward-fix-only`; `0004`, `0006`–`0014` are
    `data-safe-reversible`.
  - `internal/migrate.Down(ctx, db, version)` (new, additive) runs one
    migration's `.down.sql` inside a transaction and removes its
    `schema_migrations` row on success — symmetric with `applyOne`'s
    handling of `.up.sql`. `internal/migrate.UpTo(ctx, db, targetVersion)`
    (new, additive) applies pending migrations up to and including a given
    version, for tooling that must step through an expand/migrate/contract
    sequence one migration at a time. Neither is wired into `cmd/api`'s or
    `cmd/worker`'s own startup path (both call only `migrate.Up`,
    unchanged) — down migrations remain a manual, deliberate operator
    action, never auto-run against production.
  - **CI enforcement**: for every `data-safe-reversible` migration, an
    up→down→up-again cycle reproduces an identical schema
    (`TestMigrations_SF060_DataSafeReversibleSubsetRoundTripsCleanly`,
    `internal/migrate/reversibility_test.go`) — this runs as an ordinary
    `go test`, which is itself the CI enforcement (no separate script is
    needed): `.github/workflows/ci.yml`'s existing `go test -p 1 ./...`
    step already runs it on every push and pull request. Migration
    `0010`'s "honest, not blanket-safe" case is still classified
    `data-safe-reversible` under this scheme — CI still runs its down
    migration; the migration's own SQL, not CI, decides whether that run
    succeeds against real, possibly-divergent data.
  - `internal/migrate.Migrations()` failing on any unlabeled or
    mislabeled file is itself the proof that "every forward-fix-only
    migration is explicitly labeled" holds
    (`TestMigrations_SF061_EveryFileCarriesAValidDownMigrationStatusMarker`).

## Rolling Server Upgrades

Status: **implemented** as of Phase 14 ([phase-14-plan.md](phase-14-plan.md)).
TaskForge's stateless-API-server / stateless-worker architecture
([architecture.md](architecture.md)) is structurally favorable to rolling
upgrades (no server-local state to migrate) — this is now a proven claim,
not merely a structural argument:

- **Two-binary-version integration proof, implemented**: a real, pinned
  `cmd/worker` binary from immediately before Phase 13's schema change
  (`794abbb57a7e2a965d556700468930a1ebed23e4`, built via a detached `git
  worktree` — no knowledge of `queue_name` at the Go type level at all)
  and one from immediately after it
  (`c17f89c592c803a8f7d2fbd61cde566467ebe062`) run concurrently against
  one shared, real PostgreSQL database while migrations `0011`→`0014` are
  applied one at a time (`internal/migrate.UpTo`), with
  `internal/invariant.Checker` re-run and finding zero violations at every
  intermediate stage — the full duration of the expand/migrate/contract
  window, not just its two endpoints. See `test/compat/two_binary_test.go`,
  `TestCompat_SF066_SF067_TwoBinaryVersionAcrossExpandMigrateContractWindow`.
- **Graceful-drain procedure, implemented and tested at the real
  OS-process level**, extending what Phase 5's
  `TestStress_WorkerPoolGracefulShutdown_*` already proves at the
  goroutine level:
  - `cmd/worker`: SIGTERM stops accepting new claims immediately; an
    already-in-flight job may finish within a configurable grace period
    (`TASKFORGE_WORKER_DRAIN_TIMEOUT`, default 30s) via the additive,
    opt-in `Worker.SetDrainTimeout` — `cmd/worker` is the only caller that
    opts in; every existing caller of `internal/worker.Worker` (including
    every pre-Phase-14 test) keeps today's immediate-cancellation
    behavior, unchanged, by not calling it. If the grace period elapses
    first, no completion of any kind is reported — the job's lease is left
    to expire and is reclaimed through the existing, unmodified
    TF-INV-004 path, indistinguishable from an ordinary crash at that
    instant. See `test/procs/worker_drain_test.go` (SF-064, SF-065,
    SF-069) and `internal/worker/drain_test.go` (SF-064a, and SF-064b: the
    pre-existing Phase 5 stress test, re-run unmodified as a mandatory
    regression check).
  - `cmd/api`: `http.Server.Shutdown` (deadline `TASKFORGE_API_SHUTDOWN_TIMEOUT`,
    default 10s, reproducing the pre-Phase-14 hardcoded value) closes
    listeners immediately and waits for active handlers up to the
    deadline; once `Shutdown` returns — whether every handler finished or
    the deadline passed — the process cancels an explicit
    `http.Server.BaseContext` and force-closes any remaining connection
    (`srv.Close`), so a handler still running past the deadline is
    forcibly ended rather than silently abandoned to run to whatever
    completion it eventually reaches, unobserved. See
    `test/procs/api_shutdown_test.go` (SF-063, SF-070).
- A rolling deploy that kills in-flight work mid-deploy is not a safe
  rolling upgrade; the drain procedure above is part of this guarantee,
  not a separate concern, exactly as this document previously proposed.

## Old Worker / New Server and New Worker / Old Server Compatibility

Status: **implemented** as of Phase 14 ([phase-14-plan.md](phase-14-plan.md)):

- **Old worker, new server** (a worker binary lags behind a schema
  change): proven directly by the two-binary-version harness above — a
  real `cmd/worker` binary compiled before `queue_name` existed at the Go
  type level continues claiming and completing jobs at every intermediate
  migration stage, with no error and no stall
  (`TestCompat_SF066_SF067_TwoBinaryVersionAcrossExpandMigrateContractWindow`,
  SF-067) — a real-binary strengthening of the pre-existing in-process
  `SF-046` proof.
- **New worker, old server** (a worker binary is ahead of the deployed
  schema): a new worker calling a query that references a column not yet
  present under the currently-deployed schema still fails outright — this
  is exactly why the migration-ordering rule above (migrate first, deploy
  worker second) is the required, adopted deployment sequence, not left to
  operator judgment. No migration in this project's history has shipped a
  binary ahead of its required schema, so this direction is a documented
  operational rule rather than a scenario this project has needed to
  recover from.
- **Repurposed `job_type` across an incompatible payload shape**: the
  roadmap's own explicitly named gap — this is not prevented (payload
  remains opaque `jsonb`, see "PROPOSED: Job Payload/Schema Evolution"
  below) but the resulting failure mode is now documented and demonstrated:
  a handler's own validation/unmarshal failure against an incompatibly
  repurposed payload surfaces as an ordinary classified failure
  (dead-lettered), never a panic, a silent no-op, or an infinite retry
  loop. See `test/compat/two_binary_test.go`,
  `TestCompat_SF068_RepurposedJobTypeAcrossIncompatiblePayloadShape`
  (SF-068).

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
  TaskForge already treats idempotency. This guidance itself remains
  PROPOSED (unchanged by Phase 14) — what Phase 14 adds is a demonstrated
  answer to what happens if it is violated anyway: see SF-068 below.
- **The failure mode if this guidance is violated is now documented and
  demonstrated, not silently assumed safe (Phase 14,
  [phase-14-plan.md](phase-14-plan.md) §14, SF-068)**: a `job_type`
  string repurposed for an incompatible payload shape mid-deploy (a job
  queued under the old shape, claimed by a worker running a handler that
  now expects the new shape) fails as an ordinary classified error —
  whatever the handler's own unmarshal/validation logic does, surfaced
  through `internal/handler`'s normal `Retryable`/`Permanent`
  classification — never a panic, a silent no-op, or an infinite retry
  loop. See `test/compat/two_binary_test.go`,
  `TestCompat_SF068_RepurposedJobTypeAcrossIncompatiblePayloadShape`. This
  documents the failure mode; it does not prevent it — job authors remain
  responsible for never repurposing a `job_type` string this way.

## Long-Running Scheduled Jobs Across Deployments

Status: **implemented** as of Phase 14 for the unregistered-`job_type`
sub-claim ([phase-14-plan.md](phase-14-plan.md) §6.3); the `schema_version`-
in-payload guidance below remains a recommendation, not an enforced
mechanism.

- A job scheduled far in the future (`scheduled_at`) is durable
  ([scheduling.md](scheduling.md)) and will survive any number of
  deployments between submission and eligibility, by construction — this
  part is already proven (TF-INV-011, SF-013).
- **The unregistered-`job_type` failure mode is now documented and
  proven, not merely undocumented behavior**: a claimed job whose
  `job_type` has no registered handler is deterministically dead-lettered
  via `store.CompleteFailure` with `handler.ErrNoHandler` as the permanent
  error, logged as `event="permanent_failure"` — this was already
  implemented as an ordinary part of Phase 3's retry/DLQ machinery and
  proven by `internal/worker/worker_test.go`'s
  `TestRunOnce_NoHandlerRegistered`; Phase 14's own contribution is
  documenting it here and extending the same proof to a workflow node's
  underlying job (SF-062, below). TaskForge does not otherwise detect a
  handler's behavior changing *incompatibly* (rather than being removed
  outright) between submission and execution — this remains the job
  author's responsibility. Job authors should never repurpose a
  `job_type` string for an incompatible payload shape; see "Job
  Payload/Schema Evolution" below for the demonstrated failure mode if
  this guidance is violated anyway.

## Workflows Surviving Deployments

Status: **implemented** for the unregistered-`job_type` sub-claim, as of
Phase 14 ([phase-14-plan.md](phase-14-plan.md) §6.3, SF-062); no
workflow-definition-versioning system is adopted (unchanged, deliberate
non-goal).

- A workflow instance's structure (`workflow_instances`/`workflow_nodes`,
  including `depends_on`) is fixed at submission time and is durable
  ([workflows.md](workflows.md): "v1-of-workflows assumes the full DAG is
  known at submission time" — dynamic modification is an explicit
  deferral). This means an in-flight workflow's *shape* cannot be
  affected by a deployment, by construction — a real strength.
- **A workflow node's job_type with no registered handler now has a
  proven, defined outcome**: the node's underlying job dead-letters
  through the identical `ErrNoHandler`/`CompleteFailure` path a plain job
  uses, and TF-INV-012's completion-propagation cascade correctly fails
  the enclosing workflow — proven by
  `TestRunOnce_SF062_WorkflowNodeWithNoRegisteredHandler`
  (`internal/worker/worker_test.go`), which confirms this holds even
  though workflow completion propagation is a different code path than a
  plain job's terminal transition. If a workflow's node handlers change
  behavior incompatibly (rather than being removed outright) between
  submission and a given node's eventual execution, the same
  payload-shape guidance in "Job Payload/Schema Evolution" applies,
  node-by-node.
- No workflow-definition versioning concept exists (there is no
  "workflow template" or named/versioned workflow definition in the schema
  at all — every `POST /workflows` call submits a fully concrete DAG of
  already-resolved `job_type`/`payload` pairs, per [workflows.md](workflows.md)).
  This is arguably correct simplicity for TaskForge's current DAG model
  (there is no abstract "workflow definition" to version — only concrete
  instances), and this review does not recommend inventing a
  workflow-definition-versioning system without a demonstrated need for
  one.

## Backward Compatibility Policy (General Statement)

Status: **formally adopted** as of Phase 14 ([phase-14-plan.md](phase-14-plan.md)
§4 item 8). Within a major version, TaskForge:

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
   [observability.md](observability.md)'s cardinality discipline). Phase
   14's own two additions (`taskforge_worker_drain_duration_seconds`,
   `taskforge_worker_drain_timed_out_total`) are the first real test of
   this rule once adopted: both are new metric names, and no existing
   Phase 8/12/13 metric's name or meaning changed to add them.
5. Formally adopts the migrate-first, deploy-second ordering rule (see
   "Database Migrations" above) as the required deployment sequence, not
   left to operator judgment.

This is a general statement of intent this project now holds itself to;
it does not itself invent a semantic-versioning scheme or a tagged-release
process — see [phase-14-plan.md](phase-14-plan.md) §19 OD-7 for why "one
minor version of skew" is defined operationally (in terms of adjacent
phase-boundary commits) rather than in terms of a git tag that does not
yet exist.

## Deprecation Policy

Status: **formally adopted** as of Phase 14
([phase-14-plan.md](phase-14-plan.md) §4 item 8, §9). Any future
deprecation must (a) be announced in `docs/roadmap.md` or a successor
changelog, (b) remain functional for at least one full minor-version
cycle, (c) emit a structured log warning (not merely a comment) when the
deprecated surface is used, so operators can measure actual usage before
removal, mirroring the existing discipline of never silently changing
behavior that [invariants.md](invariants.md) already embodies for
correctness properties.

This phase's job is to state, in writing, that this mechanism is now the
*adopted policy* for any future deprecation — not merely what Phase 11
happened to build once. The mechanism itself was already implemented in
Phase 11: the `Deprecation: @1788998400` response header and the
`deprecated_route_used` structured log line on every legacy-route request
(see "API Evolution" above). Phase 14 does **not** itself decide to start
the removal clock for that specific already-in-flight legacy-route
deprecation — no Sunset date is set here — it only formalizes the policy
those future decisions must follow.

## What Is Marked PROPOSED vs. What Actually Exists Today

To be unambiguous, restating what is **not** proposed but already true
today, because it was built correctly from the start or proven by Phase
14:

- Schema additions so far have never required breaking an existing server
  version (verified: migrations `0001`→`0004`, and Phases 3/4/6/11 needing
  zero new migrations; Phase 14 additionally proves this for real,
  separately-compiled binaries across the Phase 13 `queue_name` boundary —
  SF-066/067).
- Scheduled-job and workflow-structure durability across restarts/deploys
  is already proven (TF-INV-011, TF-INV-012, SF-013, SF-029), and the
  unregistered-`job_type` failure mode for both a plain job and a workflow
  node is now proven too (SF-062).
- Job payload opacity is an existing, deliberate architectural property,
  not a proposal. The failure mode if a `job_type` is repurposed for an
  incompatible payload shape anyway is now demonstrated, not merely
  assumed (SF-068) — the guidance to avoid doing so remains PROPOSED
  guidance, not an enforced mechanism.
- **As of Phase 11**: the `/v1/` API version prefix, unknown-request-field
  tolerance, the request-body size limit, the `max_attempts`
  no-upper-bound policy decision, and same-PostgreSQL-transaction
  atomicity (`txenqueue`) are all **implemented and proven**, not
  proposed — see the "API Evolution" and "Transactional Enqueue" sections
  above.
- **As of Phase 14**: expand/migrate/contract (machine-checked, CI-enforced),
  the two-binary-version rolling-upgrade proof, old/new worker-server
  compatibility, the migrate-first/deploy-second ordering rule, the
  graceful worker/API drain contract (real OS-process SIGTERM/SIGKILL
  tests), the general Backward Compatibility Policy statement, and the
  Deprecation Policy are all **implemented and proven**, not proposed —
  see the "Database Migrations", "Rolling Server Upgrades", "Old Worker /
  New Server and New Worker / Old Server Compatibility", "Backward
  Compatibility Policy", and "Deprecation Policy" sections above.

Only two items in this document remain **PROPOSED and unimplemented**:
the job-payload `schema_version` guidance (documentation-only by design,
not a TaskForge-enforced mechanism), and the actual removal/Sunset date
for the legacy unprefixed API routes (Phase 14 formalizes the deprecation
*policy*; it does not itself decide to start that specific clock).
