# Enterprise Roadmap

Status: **review document, PROPOSED.** This is the authoritative sequencing
of work needed to close TaskForge's enterprise gaps, produced during the
`enterprise-readiness-review` review. It implements nothing. Every phase
below is scoped, justified, and given proof obligations — none is started.
This roadmap continues the project's existing phase numbering (Architecture
Foundation, Phases 1–9 complete); proposed work begins at **Phase 10**.

This roadmap deliberately contains **nine phases**, not dozens. Each groups
multiple related capabilities from [enterprise-readiness.md](enterprise-readiness.md),
[security-model.md](security-model.md), [compatibility-policy.md](compatibility-policy.md),
[slo.md](slo.md), and [reference-analysis.md](reference-analysis.md) into one
coherent unit of proof, following this review's own instruction: prioritize
proof and operational maturity over feature count. Ordering follows
dependency and leverage, not the order gaps happen to be numbered in
[enterprise-readiness.md](enterprise-readiness.md) §3.

## Sequencing Logic

```
Phase 10 (Supply-Chain)          — zero prerequisites, runs first & cheaply
      |
Phase 11 (Enqueue + API Contract) — stabilizes the API surface before
      |                             anything else adds fields to it
      v
Phase 12 (Security & Trust)      — introduces the principal/identity
      |                             concept that governance needs for
      |                             per-tenant fairness
      v
Phase 13 (Workload Governance)   — queues/concurrency/rate limits/fairness,
      |                             built on Phase 12's principal concept
      v
Phase 14 (Upgrade & Compat Proof) — proves the API/schema stabilized in
      |                             11–13 can evolve safely going forward
      v
Phase 15 (Tracing & Diagnostics) — independent of 11–14, but benefits from
      |                             Phase 12's actor concept for audit trails
      v
Phase 16 (PostgreSQL HA/DR)      — independent, operational; can run in
      |                             parallel with 14–15 if staffing allows
      v
Phase 17 (Performance/Saturation) — needs 13's backpressure signal and 15's
      |                             metrics to be measured meaningfully
      v
Phase 18 (External Validation)   — needs everything above to be a real
                                    release-candidate review, not theater
```

Phase 10 has no prerequisites and should start immediately, in parallel with
whatever else the team is doing — it is CI-only work with no product code
risk. Phases 16 (PostgreSQL HA/DR) and 15 (Tracing) are largely independent
of each other and of 13–14 and may be reordered or run concurrently if two
work-streams are available; they are listed in this order because DR
evidence is more frequently the literal first question in an enterprise
security/ops review than tracing is.

---

## Phase 10 — Supply-Chain & Release Hardening

### Why it matters
[enterprise-readiness.md](enterprise-readiness.md) §9 and [security-model.md](security-model.md)
§6 both name this a P0/P1: zero dependency vulnerability scanning, no SBOM,
no artifact provenance, and no security gate at all in
`.github/workflows/ci.yml` (confirmed: only gofmt/vet/build/test/test-race).
Per [reference-analysis.md](reference-analysis.md), this is also the
cheapest possible gap-closer identified in this entire review — GitHub-native
tooling, no new infrastructure, no new runtime dependency, and it can run
starting today without touching `internal/` at all.

### Exact scope
- Add `govulncheck` as a required CI job (fails the build on a known-exploitable
  vulnerability in a direct or transitive dependency).
- Add a Dependabot configuration for Go module updates.
- Generate an SBOM (CycloneDX or SPDX) for each build artifact.
- Add `actions/attest` with `subject-path` (binaries) and `sbom-path` to
  produce a signed GitHub Artifact Attestation for at least one tagged
  release.
- Document the minimal release process this implies (tag → build → attest →
  publish) even if no release has happened yet.

### Explicit non-scope
- No container image / Dockerfile is required by this phase (TaskForge
  currently ships no container image at all; if one is added later, this
  phase's attestation step extends to it, but building one is not in scope
  here).
- No code signing of source, no reproducible-builds infrastructure.
- No change to `go.mod` dependencies themselves unless `govulncheck` surfaces
  an actual finding.

### Prerequisites
None. This phase can start immediately.

### Invariants / proof obligations
- CI fails when a direct dependency has a known, exploitable CVE
  (`govulncheck` exit code wired to CI failure, not just a warning log).
- Every tagged release has an attached, `gh attestation verify`-passing
  attestation and an SBOM.

### Failure scenarios to guard against
- A new CVE is disclosed in `pgx`, `google/uuid`, or `prometheus/client_golang`
  after this phase ships — CI on the next PR (or a scheduled Dependabot scan)
  must surface it, not silently pass.
- A release artifact is downloaded from an untrusted mirror or tampered
  build — `gh attestation verify` against the real artifact must fail to
  verify a tampered one.

### Tests / evidence required
- A CI run log showing `govulncheck` executing and passing/failing correctly
  (a deliberately reverted dependency with a known CVE, tested once in a
  branch, is acceptable evidence it actually gates).
- At least one tagged release with a verifiable attestation and SBOM,
  demonstrated via `gh attestation verify`.

### Enterprise exit criteria
- [ ] `govulncheck` (or equivalent) runs in CI and fails the build on a
      known-exploitable vulnerability in a direct dependency.
- [ ] Dependabot is configured and has opened at least one real update PR.
- [ ] An SBOM is generated and attached to at least one tagged release.
- [ ] That release's attestation verifies via `gh attestation verify`.

---

## Phase 11 — Transactional Enqueue & API Contract Hardening

### Why it matters
Two compounding problems, both confirmed by direct code inspection: (1)
there is no way for a caller to enqueue a job atomically with their own
business-logic write — `CreateJob` always opens its own connection
([enterprise-readiness.md](enterprise-readiness.md) is silent on this by
omission; the internal audit performed for this review confirms it directly
against `internal/store`/`internal/api/handlers.go`) — and (2) the API has
no version prefix, no documented request-size limit, and no confirmed
behavior for unknown fields, meaning every later phase that touches the API
(security headers, governance fields, tracing headers) would otherwise be
retrofitting an unversioned surface. [reference-analysis.md](reference-analysis.md)
identifies River's `InsertTx` as the single highest-leverage, lowest-risk
capability in this entire review: it requires no new architecture, only a
`Store` API change.

### Exact scope
- Add a `Store` API that accepts an external `*pgx.Tx` for job insertion
  (`InsertIdempotentTx`-shaped), alongside the existing pool-based path —
  additive, not a breaking change.
- Introduce an explicit API version prefix (`/v1/...`) for all existing
  endpoints, with the current unprefixed routes either redirecting or
  removed per whatever migration window is chosen (see
  [compatibility-policy.md](compatibility-policy.md) "API Evolution").
- Enforce a documented request body size limit (`http.MaxBytesReader`) on
  `POST /jobs` and `POST /workflows`, returning a clear `413`.
- Audit and document (with a test) that the JSON decoder ignores unknown
  request fields, and that documented response consumers are expected to
  ignore unknown response fields — closing the "has this ever been tested"
  gap [compatibility-policy.md](compatibility-policy.md) flags.
- Document an upper bound (or explicit "no bound, operator-configured"
  policy) on caller-supplied `max_attempts`, closing the "abusive retry
  workload" P2 in [security-model.md](security-model.md) §1.

### Explicit non-scope
- No authentication/authorization (Phase 12).
- No named queues, priorities, or rate limiting (Phase 13).
- No change to the job state machine, lease/fencing mechanism, or any
  Phase 1–9 invariant.
- No workflow-definition versioning (Phase 14) — this phase versions the
  *API*, not workflow definitions.

### Prerequisites
None beyond Phase 10 running in parallel (no hard dependency).

### Invariants / proof obligations
- A job inserted via the transactional API is visible (claimable) if and
  only if the caller's transaction commits — a rollback must leave no job
  row behind. This must be proven under concurrent commit/rollback races,
  in the same spirit as Phase 4's idempotency concurrency tests.
- All existing TF-INV-001 through TF-INV-016 invariants continue to hold
  unchanged (this phase touches only the enqueue entrypoint and HTTP
  routing, not the state machine).
- A request exceeding the size limit is rejected before full-body JSON
  decoding is attempted (protects against unbounded memory use while
  decoding, not just after).

### Failure scenarios to guard against
- Caller starts a transaction, calls the transactional enqueue API, then
  the caller's process crashes before commit — the job must not become
  claimable (proven via the same crash-injection primitives Phase 9's
  `internal/chaos` already provides against real PostgreSQL).
- A 50MB `payload` is submitted — the server must reject it with `413`
  before OOM-risking buffering, not merely slow down.
- An old client (pre-versioning) is pointed at the new `/v1/` routes without
  being updated — behavior must be a clear 404/redirect, not silent
  misbehavior.

### Tests / evidence required
- Concurrent commit/rollback test proving transactional enqueue atomicity
  (extends the pattern of Phase 4's `TestConcurrent*Idempotency` tests).
- A request-size-limit test asserting `413` and bounded memory use for an
  oversized payload.
- A round-trip test proving an old-shape client request (missing new
  optional fields) and a new-shape client request (extra unknown fields)
  both succeed unchanged.

### Enterprise exit criteria
- [ ] A caller can enqueue a job inside their own PostgreSQL transaction via
      a documented TaskForge API, and a rollback leaves no claimable job.
- [ ] All endpoints are served under `/v1/`, with the old surface's fate
      (redirect vs. removed) documented in [compatibility-policy.md](compatibility-policy.md).
- [ ] `POST /jobs` and `POST /workflows` reject oversized bodies with `413`.
- [ ] A passing test demonstrates unknown-field tolerance in both directions.

---

## Phase 12 — Security & Trust Boundaries

### Why it matters
This is the single most-cited blocker across every review document: no
authentication, no authorization, no transport security, no worker identity
model anywhere in the codebase (confirmed by direct code search across
`internal/` and `cmd/` — zero hits for auth/RBAC/JWT/TLS constructs).
[security-model.md](security-model.md) rates application security, worker
trust, and transport security as uniformly "Absent." Per
[reference-analysis.md](reference-analysis.md)'s Faktory finding, the right
scope for a *first* security phase is a shared-credential/API-key model plus
mandatory TLS — not a full RBAC/OIDC system nobody has asked for yet.

### Exact scope
- **Authentication**: API-key-based authentication for all write endpoints
  (`POST /jobs`, `POST /jobs/{id}/cancel`, `POST /workflows`,
  `POST /workflows/{id}/cancel`) and, at minimum, for `GET /metrics`; read
  endpoints (`GET /jobs/{id}`, `GET /workflows/{id}`) require at minimum the
  same authentication (not necessarily the same authorization).
- **Minimal authorization**: introduce a principal/caller-identity concept
  and an ownership check — a caller may cancel/inspect only jobs/workflows
  it submitted (or an explicitly privileged principal may act on any). Full
  RBAC (roles, permission graphs) is explicitly deferred.
- **Worker identity**: workers authenticate with a distinct credential from
  API callers; at minimum, document (and where practical enforce) that a
  worker credential cannot also submit jobs as an arbitrary caller.
- **Transport security**: TLS support for the API server (terminate TLS
  natively or document a required TLS-terminating proxy boundary, tested
  either way) and enforce `sslmode=require` (minimum) for the
  API/worker-to-PostgreSQL connection, following Faktory's hard-cutover
  pattern (no silent plaintext fallback once TLS is configured).
- **Audit logging**: thread the authenticated principal into the existing
  structured log schema as an `actor` field for submit/cancel operations,
  closing the "no audit trail" gap in [security-model.md](security-model.md) §5.
- **Secrets handling documentation**: a stated policy that `payload`/
  `result_metadata` are opaque and may contain caller-supplied sensitive
  data TaskForge does not classify or encrypt beyond PostgreSQL's own
  at-rest guarantees — documented, not solved, per
  [security-model.md](security-model.md) §3.

### Explicit non-scope
- No full RBAC/permission-graph system.
- No OIDC/SSO integration.
- No per-job-type or per-tenant authorization policy engine (a simple
  ownership check only).
- No mTLS between workers and the API server (workers talk directly to
  PostgreSQL, not through the API server, per the existing architecture —
  only the worker-to-Postgres and client-to-API-server transport legs are
  in scope).
- No change to `lease_owner`/`lease_generation` fencing semantics (ADR-0002
  stands; this phase adds credential-based worker authentication *alongside*
  existing fencing, it does not replace it).

### Prerequisites
Phase 11 (API versioning) should land first so authentication headers/error
codes are introduced under a stable, versioned contract rather than an
unversioned one that then needs a second breaking change.

### Invariants / proof obligations
- No write endpoint accepts an unauthenticated request (`401`, not silent
  success) — provable by an integration test hitting every write endpoint
  with no/invalid credentials.
- A caller cannot cancel or inspect a job/workflow it did not submit,
  provable by a two-principal integration test.
- Once TLS is enabled for the database connection, a plaintext connection
  attempt fails rather than silently downgrading.
- Existing invariants TF-INV-001–016 are unaffected — this phase adds a
  boundary in front of the existing engine, it does not touch job-state
  transition logic.

### Failure scenarios to guard against
- A credential is leaked (e.g., committed to a public repo) — the design
  must support revocation without a full redeploy (e.g., a credential
  lookup table, not a hardcoded value).
- A worker process is compromised — worst case must be bounded (the worker's
  credential should not also grant arbitrary API-caller submission rights
  for other tenants' job types, if/when Phase 13's tenancy concept exists).
- A replayed cancel request from a captured, valid session — document
  whether this is accepted risk (idempotent effect, low severity per
  [security-model.md](security-model.md) §2) or requires a nonce/freshness
  check; make the decision explicit, not silent.

### Tests / evidence required
- Integration tests: every write endpoint rejects missing/invalid
  credentials with `401`.
- Integration test: principal A cannot cancel/inspect principal B's job.
- A TLS-enabled connection test proving plaintext fallback is refused.
- A log-output test proving the `actor` field is populated on submit/cancel
  log lines and never contains the raw credential itself (extending the
  existing cardinality/sensitivity audit discipline from
  [observability.md](observability.md)).

### Enterprise exit criteria
- [ ] `POST /jobs` without a valid credential returns `401`, not `200`
      (mirrors [enterprise-readiness.md](enterprise-readiness.md)'s own
      exit criterion).
- [ ] A caller cannot act on another caller's job/workflow — proven by test.
- [ ] `POST /jobs` over plaintext HTTP is refused, or the deployment
      documents and tests a required TLS-terminating proxy boundary.
- [ ] The database connection requires TLS in the reference deployment
      configuration, with a test proving plaintext is refused.
- [ ] Structured logs record an `actor` for every submit/cancel action.

---

## Phase 13 — Workload Governance

### Why it matters
Confirmed by direct code inspection: one global claimable pool, `priority`
as a claim-query tiebreak only, no named queues, no per-type/per-tenant
concurrency limits, no rate limiting, no fairness guarantee, no backpressure
signal. [reference-analysis.md](reference-analysis.md) identifies Hatchet's
Postgres-native queue/concurrency/fairness/rate-limit model as the most
directly transferable lesson in this entire review, and explicitly flags
Faktory's strict-priority-draining model as an anti-pattern to avoid.

### Exact scope
- **Named queues**: introduce a `queue_name` concept (schema addition,
  additive migration per [compatibility-policy.md](compatibility-policy.md)'s
  proposed migration rules), routable independently of `job_type`.
- **Per-queue/per-tenant concurrency limits**: a configurable cap on
  concurrently-running jobs per queue (and, using Phase 12's principal
  concept, optionally per-tenant), enforced in the claim query.
- **Fairness**: a group-key-based round-robin claim strategy (Hatchet's
  model) so no single queue/tenant can starve others under sustained load —
  explicitly not Faktory's strict-priority-drain model.
- **Rate limiting**: static, per-queue (and optionally per-tenant) submission
  rate limits at the API layer, staged *after* the above per the Faktory
  precedent (ship governance primitives before rate limiting, not
  simultaneously).
- **Backpressure signal**: `POST /jobs` returns `429`/`503` with a
  `Retry-After` header once a documented threshold (queue depth, rate limit,
  or concurrency cap) is exceeded, replacing today's "accept everything until
  PostgreSQL falls over" behavior.

### Explicit non-scope
- No dynamic/adaptive rate limiting based on real-time load (static,
  configured limits only in this phase).
- No cross-queue global fairness guarantee beyond the group-key round-robin
  mechanism itself — a mathematically strict fairness bound is not claimed
  (consistent with Phase 5's own honest disclaimer).
- No per-job-type backoff configuration (unrelated to queue governance,
  already tracked separately in [retry-semantics.md](retry-semantics.md)'s
  open questions).
- No periodic/cron scheduling (explicitly deferred per
  [reference-analysis.md](reference-analysis.md)).

### Prerequisites
Phase 12 (for the principal concept used in per-tenant limits/fairness) and
Phase 11 (stable, versioned API to add queue/rate-limit fields to).

### Invariants / proof obligations
- A new invariant, tentatively TF-INV-017: "no queue can be starved
  indefinitely while capacity exists for it" — provable under a Phase-5-style
  concurrency stress test with two queues, one flooded and one starved
  under the old model, both making forward progress under the new one.
- Concurrency limits are enforced exactly (never more than N concurrently
  `RUNNING` jobs for a capped queue), provable under the same
  `SKIP LOCKED`-based concurrent-claim stress testing already used in
  Phase 5.
- A caller receiving `429`/`503` and retrying after the documented
  `Retry-After` window succeeds (no permanent lockout from a transient
  backpressure signal).

### Failure scenarios to guard against
- A single tenant/queue submits a sustained flood — other queues must
  continue making measurable forward progress (this is the core "workload
  isolation" failure this phase exists to close).
- A queue's concurrency limit is set to 0 or misconfigured — must fail
  closed with a clear operator-visible error, not silently accept unlimited
  concurrency.
- Rate-limit state itself must survive a worker/API-server restart (durable
  in PostgreSQL, not in-memory only) — consistent with TaskForge's existing
  "PostgreSQL is the source of truth" discipline (ADR-0001).

### Tests / evidence required
- A two-queue fairness stress test (flooded queue + starved queue under the
  old model) proving the starved queue now makes bounded-wait-time progress.
- A concurrency-limit stress test proving the cap is never exceeded under
  concurrent claim attempts.
- An API-level test proving `429`/`503` + `Retry-After` is returned once a
  configured threshold is crossed, and that retrying after that window
  succeeds.

### Enterprise exit criteria
- [ ] A named-queue or per-job-type/tenant concurrency-limit mechanism
      exists with a passing test demonstrating one queue/tenant cannot
      starve another indefinitely (mirrors
      [enterprise-readiness.md](enterprise-readiness.md)'s own exit criterion).
- [ ] Rate limiting exists for at least one dimension (queue or tenant) and
      is durable across a restart.
- [ ] `POST /jobs` returns a documented `429`/`503` + `Retry-After` under
      configured overload, verified by test.

---

## Phase 14 — Upgrade & Compatibility Proof

### Why it matters
Every rule in [compatibility-policy.md](compatibility-policy.md) is marked
`PROPOSED` because none of it has ever actually been tested — TaskForge has
gone through exactly one schema-evolution event of consequence (migration
`0003`) with zero in-flight production traffic to preserve across it, and no
test exercises old-worker/new-server or new-worker/old-server compatibility
in either direction. This phase turns those PROPOSED rules into proven ones,
now that Phases 11–13 have made real, consequential schema/API changes for
the first time since Phase 7 — the ideal moment to prove compatibility
discipline before it is needed under incident pressure.

### Exact scope
- An integration test harness that runs two different binary versions of
  `cmd/api` (or `cmd/worker`) concurrently against the same database and
  asserts correct behavior throughout — closing
  [compatibility-policy.md](compatibility-policy.md)'s single
  largest-identified gap.
- A test proving an old worker binary (pre-Phase-13 schema) continues to
  function correctly against a post-Phase-13 schema with the new queue/
  concurrency columns present but unused by the old worker.
- CI enforcement that every new migration includes and actually exercises
  its down migration (both directions run in CI), closing the gap that
  `.down.sql` files exist today but are never applied by any test.
- Formal adoption of the migration-ordering rule (migrate schema first,
  deploy new worker/server second) as documented, required deployment
  sequence.
- A scoped resolution of workflow-definition versioning: document (per
  [reference-analysis.md](reference-analysis.md)'s Temporal analysis) that
  TaskForge's snapshot-DAG model does not need Temporal-style
  `GetVersion` patching, but *does* need an explicit, tested statement of
  what happens when a claimed job's `job_type` has no registered handler
  (currently undocumented behavior, per
  [compatibility-policy.md](compatibility-policy.md)).
- Formal adoption of the deprecation policy already drafted in
  [compatibility-policy.md](compatibility-policy.md) (minimum one
  minor-version cycle, structured log warning on use of a deprecated
  surface).

### Explicit non-scope
- No workflow-definition-versioning *system* (named/versioned templates) —
  explicitly rejected per [reference-analysis.md](reference-analysis.md)
  absent a demonstrated need.
- No automated schema-migration-generation tooling — migrations remain
  handwritten `.up.sql`/`.down.sql` pairs (existing, working practice).
- No support for skipping more than one minor version in a rolling upgrade
  (single-version-skew compatibility only, matching the exit criterion
  already stated in [enterprise-readiness.md](enterprise-readiness.md)).

### Prerequisites
Phases 11–13, so there is a real, non-trivial schema/API change to prove
compatibility against (testing compatibility against a schema that has
never actually changed would be a hollow proof).

### Invariants / proof obligations
- An old worker binary and a new worker binary can run concurrently against
  one database (mid-rolling-deploy) with zero invariant violations
  (TF-INV-001 through the new TF-INV-017 from Phase 13), checked via the
  same invariant-checker harness Phase 9's `internal/chaos` already
  provides.
- A down migration, when run, actually reverses its up migration's schema
  effect (verified by re-running the up migration's own test suite against
  the down-then-up-again state).
- A claimed job with no registered handler for its `job_type` fails in a
  documented, bounded way (e.g., a specific error classification and
  eventual dead-lettering) rather than an undefined/crashing behavior.

### Failure scenarios to guard against
- A rolling deploy is half-complete (old and new server/worker binaries
  both live) when a job that only the new binary understands is claimed by
  an old worker — must degrade in a documented, safe way (e.g., old worker
  never claims a queue/field it doesn't recognize), not crash or corrupt
  state.
- An operator runs a down migration in production against a still-populated
  table — must not silently drop data without an explicit, loud warning.
- A `job_type` string is repurposed for an incompatible payload shape
  across a deploy — the documented guidance (never repurpose a `job_type`
  string) must be paired with a test proving what actually happens if it's
  violated, so the failure mode is known even though it's not prevented.

### Tests / evidence required
- Two-binary-version concurrent integration test (old worker/new server and
  new worker/old server), passing.
- CI job running every migration's down path, not just up.
- A test claiming a job with an unregistered `job_type`, asserting the
  documented failure/dead-letter behavior.

### Enterprise exit criteria
- [ ] A two-binary-version (old worker/new server, and new worker/old
      server) integration test exists and passes (mirrors
      [enterprise-readiness.md](enterprise-readiness.md)'s own exit criterion).
- [ ] CI runs both migration directions for every migration, not just up.
- [ ] The unregistered-`job_type` failure mode is documented and tested.
- [ ] [compatibility-policy.md](compatibility-policy.md)'s PROPOSED markers
      are updated to reflect what is now actually proven vs. still proposed.

---

## Phase 15 — Tracing & Operator Diagnostics

### Why it matters
[observability.md](observability.md) already states tracing is "an explicit
deferral, not an oversight" — Phase 8 correctly prioritized metrics/logs
first. [slo.md](slo.md) identifies a concrete, actionable gap: TaskForge has
zero HTTP-request-domain instrumentation, meaning "API availability" and
"enqueue availability" SLIs cannot be computed from any metric that exists
today. [enterprise-readiness.md](enterprise-readiness.md) §4 also names
missing health/readiness endpoints and administrative tooling (DLQ replay,
job-history endpoint) as P1 operational blockers.

### Exact scope
- OpenTelemetry Go SDK integration with **span links** (not parent-child
  spans) connecting a job's submission span to its later claim/execution
  span, following OTel's own messaging semantic conventions
  (`create`/`send` producer spans, `process` consumer spans) per
  [reference-analysis.md](reference-analysis.md)'s finding that this is the
  architecturally correct pattern for a queue-wait gap of unbounded length.
- HTTP-request-domain metrics (request count, latency, status code by
  route) added to the existing Prometheus registry, closing the specific
  gap [slo.md](slo.md) identifies.
- Split `taskforge_claim_latency_seconds` and `taskforge_queue_age_seconds`
  into genuinely distinct observations (currently the same sample per
  [slo.md](slo.md)'s finding), enabling the "queue wait vs. claim latency"
  distinction the SLI table already wants to make.
- Health (`/healthz`) and readiness (`/readyz`) endpoints.
- A `GET /jobs/{id}/history` endpoint over the existing `job_attempts`
  ledger (data already exists and is durable; this is a read-API addition).
- A `POST /jobs/{id}/retry` (or equivalent) DLQ-replay endpoint for
  dead-lettered jobs, closing the documented "Deferred Endpoints" gap in
  [worker-protocol.md](worker-protocol.md).
- A documented, operator-facing graceful-drain procedure (SIGTERM handling
  in `cmd/worker`/`cmd/api` that stops accepting new claims/requests and
  waits for in-flight work up to a timeout), formalizing what Phase 5
  already proves at the goroutine level.

### Explicit non-scope
- No vendor dashboards or alerting rules (remains an explicit non-goal,
  correctly, per [observability.md](observability.md)).
- No distributed-tracing-based *replay* or debugging tooling beyond spans/
  traces themselves (no Temporal-style workflow-history UI).
- No admin UI — a `GET /jobs/{id}/history` API and a retry endpoint are in
  scope; a browser-based admin console is not.

### Prerequisites
None strictly required, but Phase 12's principal/actor concept makes the
new audit-relevant endpoints (history, retry) authorizable from day one
rather than needing a second pass.

### Invariants / proof obligations
- A traced job's submission span and execution span(s) are correctly linked
  (not falsely parented) even when the queue wait spans hours (provable
  with a synthetic delayed job and a test-exporter assertion).
- `/healthz` reflects true process liveness; `/readyz` reflects true
  readiness to serve traffic (e.g., false during startup before the DB
  connection pool is established) — both provable by integration test.
- A DLQ-replayed job re-enters the state machine at `QUEUED` with a fresh
  `job_id` and a recorded link (`retried_from`) to the original — the
  `retried_from` field already exists in the schema per the internal audit
  but is currently unexercised by any test; this phase must add that test.

### Failure scenarios to guard against
- The trace exporter itself becomes unavailable — job execution must not
  block or fail due to tracing infrastructure being down (tracing must be
  best-effort, not load-bearing for correctness, consistent with
  TaskForge's existing observability-under-failure discipline from Phase 9).
- `/readyz` reports ready while the database is actually unreachable — must
  be tested against a real disconnected-database scenario using Phase 9's
  existing `internal/chaos` connection-interruption primitives.

### Tests / evidence required
- A span-link assertion test using a test/in-memory OTel exporter.
- HTTP-metrics scrape test proving request count/latency/status appear
  correctly per route.
- `/healthz`/`/readyz` tests under normal and DB-disconnected conditions
  (reusing Phase 9's chaos primitives).
- A DLQ-replay test proving a dead-lettered job can be replayed and the
  `retried_from` linkage is recorded and queryable.

### Enterprise exit criteria
- [ ] Job submission and execution are linked via OpenTelemetry spans
      following OTel messaging semantic conventions.
- [ ] `taskforge_claim_latency_seconds` and `taskforge_queue_age_seconds`
      are distinct, independently meaningful observations.
- [ ] `/healthz` and `/readyz` exist and are proven correct under a
      simulated database outage.
- [ ] A dead-lettered job can be replayed via a documented API, with the
      `retried_from` linkage tested.

---

## Phase 16 — PostgreSQL HA, Backup & Disaster Recovery Evidence

### Why it matters
[failure-model.md](failure-model.md) explicitly and correctly places
PostgreSQL primary failure/failover out of scope for TaskForge's own code —
"if the database is down, TaskForge is down for writes" is a documented,
honest v1 boundary, not an oversight. But per
[reference-analysis.md](reference-analysis.md)'s PostgreSQL research, this
is not a gap TaskForge needs to write code to close — PostgreSQL itself
already provides base backup, WAL archiving, and point-in-time recovery;
what is missing is a documented, *drilled* runbook with a stated RPO/RTO,
which is exactly what an enterprise security/operations review will ask for
first. [enterprise-readiness.md](enterprise-readiness.md) §8 names this
directly as a DR gap.

### Exact scope
- A documented backup procedure using `pg_basebackup` plus continuous WAL
  archiving (`archive_mode`/`archive_command`).
- A documented, **tested at least once**, point-in-time-recovery restore
  procedure using a `recovery_target_time` (or equivalent), with the
  restore timed.
- A stated RPO (how much data loss is acceptable/expected given the chosen
  archiving cadence) and RTO (how long a restore is expected to take at a
  reference data volume).
- A documented HA topology recommendation using vanilla PostgreSQL
  streaming replication (synchronous or asynchronous, with the tradeoff
  stated per PostgreSQL's own documentation), explicitly noting that
  automated failover orchestration (e.g., via a third-party tool) is a
  deployment-time choice TaskForge documents but does not build or bundle.
- A documented read-replica story for `GET /jobs/{id}`/`GET /workflows/{id}`
  reads, if the team chooses to reduce primary load — evaluated, not
  necessarily adopted, in this phase.

### Explicit non-scope
- No TaskForge-built failover orchestration, consensus, or split-brain
  detection — PostgreSQL's own docs confirm this is explicitly not
  PostgreSQL's job either; it is deployment-layer tooling (e.g., Patroni,
  repmgr) chosen by the operator, named or left tool-agnostic per the
  team's later judgment, not built by this project.
- No multi-region/multi-datacenter durability (remains an explicit
  [vision.md](vision.md) non-goal).
- No automatic backup scheduling infrastructure built into TaskForge itself
  — the backup/restore procedure is documented and operator-run (via cron,
  a managed PostgreSQL provider's built-in backup feature, or equivalent),
  not a TaskForge feature.

### Prerequisites
None — independent of Phases 11–15, can run in parallel if a second
work-stream is available.

### Invariants / proof obligations
- A restore from a PITR backup to a stated point in time produces a
  database in which all of TaskForge's existing invariants (TF-INV-001
  through the current set) still hold — i.e., a restored database is not
  just "present" but "correct" (verified by running the existing invariant
  checker against the restored database).
- The measured restore time at a reference data volume (e.g., Phase 9's
  soak-run scale, or larger if available) is recorded and compared against
  the stated RTO target.

### Failure scenarios to guard against
- A restore is attempted from a backup that was taken mid-write (without
  proper `pg_backup_start`/`pg_backup_stop` bracketing) — must be
  demonstrated to either fail cleanly or be prevented by the documented
  procedure, not silently produce a corrupt restore.
- A WAL archive gap (a missing archived segment) — the documented procedure
  must state what happens (recovery fails at that point, with a clear
  error) rather than silently skipping forward.

### Tests / evidence required
- At least one full, timed backup-then-restore drill, with the restored
  database's invariants verified via the existing checker.
- A written runbook a different engineer (not the one who wrote it) can
  follow to reproduce the restore, reviewed for clarity.

### Enterprise exit criteria
- [ ] A documented `pg_basebackup`/WAL-archiving-based backup exists and a
      restore-from-backup drill has been executed and timed at least once
      (mirrors [enterprise-readiness.md](enterprise-readiness.md)'s own
      exit criterion).
- [ ] A stated RPO/RTO exists and the drilled restore time is compared
      against it.
- [ ] The restored database passes the existing invariant checker.
- [ ] An HA topology recommendation is documented, with its tradeoffs
      (sync vs. async replication) stated per PostgreSQL's own guidance.

---

## Phase 17 — Performance, Saturation & Backpressure Proof

### Why it matters
The only load evidence in the entire project is Phase 9's single 4-minute
soak run, explicitly and correctly labeled "illustrative, not benchmarks."
[slo.md](slo.md) lays out exactly what a real capacity-test plan requires
and confirms none of it has been done: no HTTP-domain instrumentation (now
closed by Phase 15), no multi-hour run (the roadmap's own Stable bar for
Phase 9, not yet met), no worker-count/pool-size sweep, and — most
importantly for an enterprise buyer — no deliberate overload run showing
what actually happens when submission rate exceeds sustainable claim
throughput.

### Exact scope
- Run `cmd/chaos -mode=soak` for a multi-hour duration (closing Phase 9's
  own stated gap to its Stable maturity bar), with a Prometheus scraper
  attached throughout, publishing the actually-measured throughput/latency-
  percentile numbers (not fabricated ones).
- A worker-count scaling curve (throughput vs. worker count, fixed job
  volume/pool size) beyond the ~25 workers exercised in Phase 5's stress
  tests.
- A connection-pool-size sweep to find the saturation point.
- A deliberate, sustained overload run (submission rate held above
  sustainable claim throughput) with the resulting behavior documented —
  expected, per [slo.md](slo.md)'s own honest assessment, to show unbounded
  queue growth in PostgreSQL absent Phase 13's backpressure signal; this
  run validates that Phase 13's `429`/`503` + `Retry-After` contract
  actually engages under real sustained overload, not just in a unit test.
- External PostgreSQL-side monitoring (`pg_stat_activity`, `pg_locks`, CPU)
  captured alongside every run above.
- Validate (or revise) the `TARGET (proposed, unvalidated)` SLO thresholds
  in [slo.md](slo.md) against the numbers actually measured here, converting
  them from proposed to evidence-backed where the data supports it.

### Explicit non-scope
- No true multi-process/multi-host chaos (remains a stated Phase 9
  limitation this roadmap does not attempt to close — a genuinely separate,
  larger investment in distributed test infrastructure would be required,
  and no demonstrated need for it has been shown yet).
- No network-partition simulation (iptables/netns) — same reasoning.
- No fabricated or extrapolated numbers under any circumstance — every
  number published must trace to an actual run, per the roadmap's own
  existing discipline (Phase 9's README section).

### Prerequisites
Phase 13 (so there is a real backpressure signal to validate under load) and
Phase 15 (so HTTP-domain and split claim/queue-wait metrics exist to
measure).

### Invariants / proof obligations
- Zero invariant violations across the entire multi-hour run (the existing
  TF-INV-001–016 set, plus TF-INV-017 from Phase 13), checked continuously
  by the existing `internal/chaos` invariant checker exactly as Phase 9
  already does at shorter duration.
- The measured p50/p95/p99 for claim latency, queue wait, and API request
  latency are recorded and compared against [slo.md](slo.md)'s proposed
  targets, with each target either confirmed, revised, or explicitly
  flagged as workload-dependent.

### Failure scenarios to guard against
- A multi-hour run reveals a slow resource leak (connection leak, goroutine
  leak, unbounded memory growth) that a 4-minute run cannot surface — this
  is a primary reason this phase exists, not an incidental finding.
- Sustained overload causes index bloat or degraded claim-query performance
  over time (a query that is fast at low queue depth may degrade at very
  high depth) — must be observed and documented, not assumed away.

### Tests / evidence required
- A published, dated report of one multi-hour `cmd/chaos -mode=soak` run
  with zero invariant violations, committed to the repository.
- A worker-count scaling curve and a connection-pool sweep, both published
  with their actual methodology and raw data.
- A published overload-run report showing the backpressure signal engaging
  and its effect on caller-observed behavior.

### Enterprise exit criteria
- [ ] `cmd/chaos -mode=soak -duration=3h` (or longer) has been run at least
      once with zero invariant violations, and the result is committed to
      the repository as evidence (mirrors
      [enterprise-readiness.md](enterprise-readiness.md)'s own exit
      criterion, and closes Phase 9's own stated gap to Stable).
- [ ] A worker-count scaling curve and connection-pool-size sweep are
      published.
- [ ] A deliberate overload run's results are published, showing the
      Phase 13 backpressure signal actually engaging.
- [ ] [slo.md](slo.md)'s proposed targets are updated to reflect
      actually-measured evidence.

---

## Phase 18 — External Validation & Release Candidate

### Why it matters
Every phase above is TaskForge's own team validating its own work. An
enterprise-readiness claim carries more weight, and is less prone to blind
spots, when it survives review by someone who did not write the code — this
is the standard closing step of any serious infrastructure hardening effort,
and this review does not consider TaskForge "enterprise-ready" merely
because Phases 10–17 are complete; it must also survive scrutiny.

### Exact scope
- An independent security review of the authentication/authorization/
  transport-security work from Phase 12 (this can be TaskForge's own
  `/code-review ultra` multi-agent review, an external contractor, or a
  qualified internal reviewer who did not write the Phase 12 code —
  whichever the team decides is appropriate).
- An independent read of the compatibility proof from Phase 14 and the
  DR drill from Phase 16, checking that the documented runbooks are
  actually followable by someone who did not write them (a real test of
  the runbook, not just its existence).
- A final, consolidated enterprise-readiness scorecard: every exit
  criterion checkbox from Phases 10–17, in one place, each linked to its
  evidence (a committed test, a committed report, a PR).
- A decision on whether to label the resulting state a "Release Candidate"
  and what, if anything, remains explicitly deferred beyond it (e.g.,
  full RBAC, multi-region, periodic/cron scheduling — all already
  identified as legitimate P2/P3 deferrals throughout this review).

### Explicit non-scope
- This phase does not add new capabilities — it validates and consolidates
  what Phases 10–17 built. Any finding from the external review that
  reveals a real gap should be triaged back into the relevant earlier
  phase (or a new, explicitly scoped follow-up), not silently patched here
  without updating that phase's documentation.

### Prerequisites
Phases 10–17 substantially complete. This phase is meaningless run early —
reviewing incomplete work as if it were final defeats its purpose.

### Invariants / proof obligations
- Every checkbox in the consolidated scorecard traces to a specific,
  inspectable artifact (a test name, a CI run, a committed report) — no
  checkbox is marked done on the basis of an unverified claim, consistent
  with this entire review's own discipline of "no unsupported claims."

### Failure scenarios to guard against
- The external review finds a real gap in Phase 12's security work after
  it was believed complete — the response must be to reopen and fix Phase
  12, not to relabel the finding as "future work" to preserve a release
  date.

### Tests / evidence required
- A written external/independent review report, findings, and resolution
  status for each finding.
- The consolidated scorecard itself, committed to the repository.

### Enterprise exit criteria
- [ ] An independent review of Phase 12 (security) has been completed, with
      findings resolved or explicitly and visibly deferred with reasoning.
- [ ] The DR runbook (Phase 16) has been followed successfully by someone
      other than its author.
- [ ] A consolidated scorecard exists, mapping every Phase 10–17 exit
      criterion to committed evidence.
- [ ] A go/no-go decision on a "Release Candidate" label is documented,
      including what remains explicitly and honestly deferred beyond it.

---

## What This Roadmap Deliberately Does Not Include

Per [reference-analysis.md](reference-analysis.md)'s "Explicitly Rejected
Capabilities": no message broker, no Redis, no Kubernetes operators/service
mesh, no deterministic-replay workflow execution, no event sourcing, no
microservices decomposition, no `LISTEN/NOTIFY`-based claim latency
optimization, no strict-priority queue draining, no full RBAC/OIDC system,
and no periodic/cron scheduling. Each is named explicitly, with the
reasoning for rejecting it, rather than silently omitted — consistent with
this review's mandate to avoid feature bloat and prefer a strong,
deliberate architecture over an impressive dependency list.
