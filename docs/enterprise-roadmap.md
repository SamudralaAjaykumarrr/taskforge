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

**Canonical phase list** (every cross-reference in this document, and in the
other five enterprise-review documents, must resolve to this list — this is
the single source of truth for phase numbering):

| Phase | Name |
|---|---|
| 10 | Supply-Chain & Release Hardening |
| 11 | Transactional Enqueue & API Contract Hardening |
| 12 | Security & Trust Boundaries |
| 13 | Workload Governance + Retention |
| 14 | Upgrade & Compatibility Proof |
| 15 | PostgreSQL HA / Backup / DR Proof |
| 16 | Tracing & Operator Diagnostics |
| 17 | Performance / Saturation / Multi-Process Failure Proof |
| 18 | External Human Validation & Release Candidate |

## Sequencing Logic

```
Phase 10 (Supply-Chain)          — zero prerequisites, runs first & cheaply
      |
Phase 11 (Enqueue + API Contract) — stabilizes the API surface before
      |                             anything else adds fields to it
      v
Phase 12 (Security & Trust)      — introduces the principal/identity
      |                             concept that governance needs for
      |                             per-tenant fairness, and the
      |                             tenant-scoped idempotency fix it implies
      v
Phase 13 (Workload Governance    — queues/concurrency/rate limits/fairness
      |   + Retention)             and terminal-row retention, built on
      |                             Phase 12's principal concept
      v
Phase 14 (Upgrade & Compat Proof) — proves the API/schema stabilized in
      |                             11–13 can evolve safely going forward,
      |                             via expand/migrate/contract; also where
      |                             graceful worker draining is formalized
      v
Phase 15 (PostgreSQL HA/DR)      — independent, operational; can run in
      |                             parallel with 14/16 if staffing allows
      v
Phase 16 (Tracing & Diagnostics) — independent of 11–15, but benefits from
      |                             Phase 12's actor concept for audit trails
      v
Phase 17 (Performance/Saturation/ — needs 13's backpressure signal and 16's
      |   Multi-Process Failure)    metrics to be measured meaningfully
      v
Phase 18 (External Human          — needs everything above to be a real
          Validation & RC)          release-candidate review, not theater
```

Phase 10 has no prerequisites and should start immediately, in parallel with
whatever else the team is doing — it is CI-only work with no product code
risk. Phases 15 (PostgreSQL HA/DR) and 16 (Tracing) are largely independent
of each other and of 13–14 and may be reordered or run concurrently if two
work-streams are available; they are listed in this order because DR
evidence is more frequently the literal first question in an enterprise
security/ops review than tracing is.

---

## Phase 10 — Supply-Chain & Release Hardening

### Why it matters
[enterprise-readiness.md](enterprise-readiness.md) §3 and [security-model.md](security-model.md)
§6 both name this a P0/P1: zero dependency vulnerability scanning, no SBOM,
no artifact provenance, and no security gate at all in
`.github/workflows/ci.yml` (confirmed: only gofmt/vet/build/test/test-race).
Per [reference-analysis.md](reference-analysis.md), this is also the
cheapest possible gap-closer identified in this entire review — GitHub-native
tooling, no new infrastructure, no new runtime dependency, and it can run
starting today without touching `internal/` at all.

### Exact scope
- Add `govulncheck` as a required CI job (fails the build on a known-exploitable
  vulnerability in a direct or transitive dependency), run on every PR **and**
  on a scheduled (e.g. daily/weekly) cron trigger — so a CVE disclosed after
  the last merged PR is still surfaced, not only one disclosed between two
  PRs.
- Add a Dependabot configuration for Go module updates (security and version
  updates), and enable GitHub's **dependency-review** check as a required PR
  status check (blocks a PR that introduces a newly-vulnerable or
  newly-disallowed dependency).
- Add **CodeQL** analysis for Go as a required CI job, closing the "verified
  by inspection, not by an automated SAST/CodeQL gate" caveat
  [security-model.md](security-model.md) §3 already flags for the
  parameterized-query finding.
- Enable **secret scanning and push protection** for the repository (where
  available for the repository's plan/visibility), so a committed credential
  is caught at push time, not discovered later.
- Set **least-privilege GitHub Actions permissions**: every workflow declares
  an explicit `permissions:` block scoped to only what that job needs
  (default to `contents: read`; grant `id-token: write`/`attestations: write`
  etc. only on the specific job that needs them), rather than relying on the
  repository-wide default.
- **Pin every GitHub Action to a full, immutable commit SHA** (not a
  mutable tag like `@v4`), across every workflow file, so a compromised or
  re-tagged upstream Action cannot silently change what CI runs.
- Generate an SBOM (CycloneDX or SPDX) for each build artifact.
- Add `actions/attest` with `subject-path` (binaries) and `sbom-path` to
  produce a signed GitHub Artifact Attestation for at least one tagged
  release.
- Document the minimal release process this implies (tag → build → attest →
  publish), including **release checksums and version metadata** (a
  `checksums.txt`/`SHASUMS256.txt`-style manifest and the exact version/build
  metadata embedded in the binary), even if no release has happened yet.

### Explicit non-scope
- No container image / Dockerfile is required by this phase (TaskForge
  currently ships no container image at all; if one is added later, this
  phase's attestation step extends to it, but building one is not in scope
  here).
- No code signing of source, no reproducible-builds infrastructure.
- No change to `go.mod` dependencies themselves unless `govulncheck` or
  CodeQL surfaces an actual finding.

### Prerequisites
None. This phase can start immediately.

### Invariants / proof obligations
- CI fails when a direct dependency has a known, exploitable CVE
  (`govulncheck` exit code wired to CI failure, not just a warning log), on
  both a per-PR run and a scheduled run.
- Every workflow file has an explicit, least-privilege `permissions:` block,
  and every third-party Action reference is a full commit SHA, not a tag —
  both provable by a repository-wide grep/lint check.
- Every tagged release has an attached, `gh attestation verify`-passing
  attestation, an SBOM, and a checksums manifest.

### Failure scenarios to guard against
- A new CVE is disclosed in `pgx`, `google/uuid`, or `prometheus/client_golang`
  **after the last merged PR**, with no new PR opened for weeks — the
  scheduled `govulncheck` run (not a PR-triggered one) must still surface it.
  This is why "Dependabot happened to open a PR" cannot be the exit
  criterion for detection: Dependabot's PR cadence is not a deterministic
  signal, and a quiet period with no dependency-bump PR must not be
  mistaken for "no vulnerabilities."
- A release artifact is downloaded from an untrusted mirror or tampered
  build — `gh attestation verify` against the real artifact must fail to
  verify a tampered one, and its checksum must not match a tampered binary.
- A malicious or compromised third-party GitHub Action is re-tagged upstream
  to point at different code — CI must be unaffected, because every Action
  reference is pinned to a specific commit SHA, not a mutable tag.

### Tests / evidence required
- A CI run log showing `govulncheck` executing and passing/failing correctly
  (a deliberately reverted dependency with a known CVE, tested once in a
  branch, is acceptable evidence it actually gates), for both the PR-triggered
  and scheduled invocations.
- A CI run log showing CodeQL executing for Go and the dependency-review
  check blocking a deliberately-introduced vulnerable dependency in a test
  branch.
- A repository-wide audit (committed as evidence) showing every workflow's
  `permissions:` block and every Action reference's commit-SHA pinning.
- At least one tagged release with a verifiable attestation, SBOM, and
  checksums manifest, demonstrated via `gh attestation verify` and a
  checksum comparison.

### Enterprise exit criteria
- [ ] `govulncheck` (or equivalent) runs in CI on every PR **and** on a
      schedule, and fails the build on a known-exploitable vulnerability in
      a direct dependency.
- [ ] Dependabot and GitHub dependency-review are both configured and
      enforced as required PR checks (deterministic configuration evidence —
      not "a Dependabot PR happened to open").
- [ ] CodeQL for Go runs in CI and is a required check.
- [ ] Secret scanning / push protection is enabled for the repository where
      the plan/visibility supports it.
- [ ] Every workflow declares least-privilege `permissions:`, and every
      Action reference is pinned to a full commit SHA (verified by a
      repository-wide audit).
- [ ] An SBOM, a checksums/version-metadata manifest, and a verifiable
      attestation are generated and attached to at least one tagged release.

---

## Phase 11 — Transactional Enqueue & API Contract Hardening

### Why it matters
Two compounding problems, both confirmed by direct code inspection: (1)
there is no way for a caller to enqueue a job atomically with their own
business-logic write when that write lives in the *same* PostgreSQL
instance — `CreateJob` always opens its own connection — and (2) the API has
no version prefix, no documented request-size limit, and no confirmed
behavior for unknown fields, meaning every later phase that touches the API
(security headers, governance fields, tracing headers) would otherwise be
retrofitting an unversioned surface. [reference-analysis.md](reference-analysis.md)
identifies River's `InsertTx` as a high-leverage capability, but River's
design (an embedded library) does not transfer directly to TaskForge's
HTTP/service architecture — see "What this phase is and is not" below.

### What this phase is and is not
This phase deliberately distinguishes two integration scenarios that were
previously conflated:

- **(A) Caller's business data lives in the same PostgreSQL instance as
  TaskForge.** This phase adds a **deliberate, public Go integration
  surface** — a new, exported package (not `internal/store`, which cannot be
  a public API for unrelated applications — `internal/` is Go-enforced
  unimportable outside this module) that accepts a caller-owned `pgx.Tx`
  (an **interface** in `pgx/v5`, not a pointer type — `*pgx.Tx` is a type
  error and must not appear in the design) and enqueues a job as part of
  that transaction. This is additive, narrow, and does not turn TaskForge
  into an embedded library in general — the HTTP API and worker/service
  architecture are preserved unchanged; this is one additional, optional
  Go-native entry point for same-database callers, not a replacement for
  the HTTP path.
- **(B) Caller's business data lives in a different database (or a
  different PostgreSQL instance) than TaskForge's.** TaskForge **cannot**
  provide a distributed atomic transaction across two separate databases —
  no design in this phase claims otherwise. The recommended pattern for
  this case is the **transactional outbox**: the caller writes their
  business row and an "outbox" row in their own database transaction, and a
  separate relay process (the caller's own, not TaskForge's) reads the
  outbox and calls TaskForge's ordinary HTTP `POST /jobs` (idempotently, via
  the existing `Idempotency-Key` mechanism) to enqueue the job. This
  document does not claim atomicity for case (B); it documents outbox as
  the correct pattern and explains why cross-database atomicity is not
  achievable here.

### Exact scope
- Add a new, exported Go package providing a `Store`-shaped API that accepts
  an external, caller-owned `pgx.Tx` for job insertion, for case (A) above —
  additive, alongside the existing pool-based path, not a breaking change.
- Document case (B) — the transactional-outbox pattern — in
  [compatibility-policy.md](compatibility-policy.md) or a new integration
  guide, with an explicit statement that TaskForge does not and cannot
  provide cross-database atomicity.
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
- No relay/outbox-processor implementation for case (B) — that is the
  caller's own responsibility; this phase documents the pattern, it does
  not build a relay for callers.

### Prerequisites
None beyond Phase 10 running in parallel (no hard dependency).

### Invariants / proof obligations
- A job inserted via the transactional (case A) API is visible (claimable)
  if and only if the caller's transaction commits — a rollback must leave no
  job row behind. This must be proven under concurrent commit/rollback
  races, in the same spirit as Phase 4's idempotency concurrency tests.
- All existing TF-INV-001 through TF-INV-016 invariants continue to hold
  unchanged (this phase touches only the enqueue entrypoint and HTTP
  routing, not the state machine).
- A request exceeding the size limit is rejected before full-body JSON
  decoding is attempted (protects against unbounded memory use while
  decoding, not just after).

### Failure scenarios to guard against
- Caller starts a transaction, calls the transactional (case A) enqueue
  API, then the caller's process crashes before commit — the job must not
  become claimable (proven via the same crash-injection primitives Phase
  9's `internal/chaos` already provides against real PostgreSQL).
- A 50MB `payload` is submitted — the server must reject it with `413`
  before OOM-risking buffering, not merely slow down.
- An old client (pre-versioning) is pointed at the new `/v1/` routes without
  being updated — behavior must be a clear 404/redirect, not silent
  misbehavior.
- A case-(B) integrator misreads this phase's guidance and assumes
  TaskForge provides cross-database atomicity — the documentation must make
  the outbox requirement unambiguous, precisely to prevent this
  misunderstanding.

### Tests / evidence required
- Concurrent commit/rollback test proving transactional (case A) enqueue
  atomicity (extends the pattern of Phase 4's `TestConcurrent*Idempotency`
  tests).
- A request-size-limit test asserting `413` and bounded memory use for an
  oversized payload.
- A round-trip test proving an old-shape client request (missing new
  optional fields) and a new-shape client request (extra unknown fields)
  both succeed unchanged.

### Enterprise exit criteria
- [ ] A caller on the same PostgreSQL instance can enqueue a job inside
      their own transaction via a documented, exported (non-`internal/`) Go
      API, and a rollback leaves no claimable job.
- [ ] The transactional-outbox pattern is documented as the recommended
      integration for a caller whose business data lives in a different
      database, with an explicit statement that cross-database atomicity is
      not provided.
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
trust, and transport security as uniformly "Absent" **within the Enterprise
Deployment Profile** that document now defines (multi-principal API/control
plane, trusted first-party worker fleet, PostgreSQL HA cluster,
single-region, no hostile-third-party-worker-code guarantee). Per
[reference-analysis.md](reference-analysis.md)'s Faktory finding, the right
scope for a *first* security phase is a shared-credential/API-key model plus
mandatory TLS — not a full RBAC/OIDC system nobody has asked for yet, and
not full isolation against a hostile worker fleet, which this profile does
not claim to support.

### Exact scope
- **Authentication**: API-key-based authentication for all write endpoints
  (`POST /jobs`, `POST /jobs/{id}/cancel`, `POST /workflows`,
  `POST /workflows/{id}/cancel`) and, at minimum, for `GET /metrics`; read
  endpoints (`GET /jobs/{id}`, `GET /workflows/{id}`) require at minimum the
  same authentication (not necessarily the same authorization). API keys
  must satisfy [security-model.md](security-model.md)'s "API Key Design
  Requirements": raw credentials are never stored (hash/HMAC only), a
  non-secret key identifier exists, rotation supports an overlap window,
  revocation takes effect without a redeploy, any raw-secret comparison
  path is constant-time, and credentials never enter metrics/log/trace
  fields.
- **Minimal authorization**: introduce a principal/caller-identity concept
  and an ownership check — a caller may cancel/inspect only jobs/workflows
  it submitted (or an explicitly privileged principal may act on any). Full
  RBAC (roles, permission graphs) is explicitly deferred. Ownership checks
  must avoid unnecessary resource-existence disclosure: "not found" and
  "found but not yours" return the same response (e.g., `404`), not a
  distinguishing `403`.
- **Distinguish API-caller identity from worker identity, explicitly.**
  These are two different principal types with different trust levels: API
  callers are the (potentially multi-tenant, mutually distrusting)
  principals authenticated above; workers are the trusted first-party
  fleet assumed by the Enterprise Deployment Profile. A worker credential
  must not double as a valid API-caller credential (or vice versa).
- **Least-privilege PostgreSQL roles**: provision separate PostgreSQL roles
  for the API server and for worker processes **where practical**, each
  granted only the statements/tables it needs. This bounds a compromised
  worker's blast radius; it does **not**, by itself, make it safe to run
  hostile third-party worker code — a different credential alone provides
  no containment if both roles still hold broad raw-table
  `SELECT`/`UPDATE`/`DELETE` privileges. Fully isolating untrusted worker
  code would require an architectural boundary this phase does not build
  (a worker-facing gateway service, or a PostgreSQL stored-function API
  granting `EXECUTE` but no direct table access) — this is **explicitly
  deferred**, stated honestly rather than silently assumed solved.
- **Transport security**: TLS support for the API server (terminate TLS
  natively or document a required TLS-terminating proxy boundary, tested
  either way, with a tested no-plaintext boundary either way) and, for the
  **enterprise reference deployment**, require **`sslmode=verify-full`**
  (server identity verification against a trusted CA, with hostname
  validation) for the API/worker-to-PostgreSQL connection — not merely
  `sslmode=require`, which encrypts but does not verify server identity.
  Simpler development/local profiles may continue to use `sslmode=require`
  or plaintext against a same-host database, documented as a
  lower-assurance default for that context only. This follows Faktory's
  hard-cutover pattern (no silent plaintext fallback once TLS is
  configured).
- **Audit logging**: thread the authenticated principal into the existing
  structured log schema as an `actor` field for submit/cancel operations,
  closing the "no audit trail" gap in [security-model.md](security-model.md) §5.
- **Tenant-scoped idempotency (if/when principals are introduced as
  tenants)**: the existing `UNIQUE(job_type, idempotency_key)` constraint
  must become principal/tenant-scoped (e.g.,
  `UNIQUE(principal_id, job_type, idempotency_key)`) in the same change that
  introduces principals — not as a follow-up — per
  [security-model.md](security-model.md) §7. Two different tenants choosing
  the same `job_type`/`Idempotency-Key` must not collide.
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
- **No support for hostile/fully-untrusted worker code.** This phase's
  least-privilege roles reduce a compromised worker's blast radius; they do
  not make it safe to run worker code from an untrusted third party. That
  would require the worker-gateway/stored-function architecture named above
  as an explicit, undesigned deferral — not a capability of this phase.

### Prerequisites
Phase 11 (API versioning) should land first so authentication headers/error
codes are introduced under a stable, versioned contract rather than an
unversioned one that then needs a second breaking change.

### Invariants / proof obligations
- No write endpoint accepts an unauthenticated request (`401`, not silent
  success) — provable by an integration test hitting every write endpoint
  with no/invalid credentials.
- A caller cannot cancel or inspect a job/workflow it did not submit,
  provable by a two-principal integration test, and the response for
  "not yours" is identical in shape to "does not exist."
- Once TLS is enabled for the database connection, a plaintext connection
  attempt fails rather than silently downgrading, and (for the enterprise
  reference deployment) a connection presenting a certificate not signed by
  the trusted CA, or a hostname mismatch, is rejected — not merely an
  unencrypted connection.
- If principals are introduced as tenants in this phase, the idempotency
  uniqueness constraint is tenant-scoped from the same migration that adds
  principals — never shipped as two separate changes with a window between
  them.
- Existing invariants TF-INV-001–016 are unaffected — this phase adds a
  boundary in front of the existing engine, it does not touch job-state
  transition logic.

### Failure scenarios to guard against
- A credential is leaked (e.g., committed to a public repo) — the design
  must support revocation without a full redeploy (e.g., a credential
  lookup table, not a hardcoded value).
- A worker process is compromised — its PostgreSQL role's privileges bound
  the damage (least-privilege grants), but this is not a claim that the
  worker fleet is safe against hostile code; that remains explicitly
  unsupported by this phase.
- A replayed cancel request from a captured, valid session — document
  whether this is accepted risk (idempotent effect, low severity per
  [security-model.md](security-model.md) §2) or requires a nonce/freshness
  check; make the decision explicit, not silent.
- Two different tenants submit the same `job_type`/`Idempotency-Key` —
  must be treated as two independent submissions, not a false duplicate.

### Tests / evidence required
- Integration tests: every write endpoint rejects missing/invalid
  credentials with `401`.
- Integration test: principal A cannot cancel/inspect principal B's job,
  and receives the same response shape as for a nonexistent job.
- A TLS-enabled connection test proving plaintext fallback is refused, and
  (for the enterprise reference deployment configuration) a test proving a
  certificate from an untrusted CA or a hostname mismatch is refused under
  `sslmode=verify-full`.
- A log-output test proving the `actor` field is populated on submit/cancel
  log lines and never contains the raw credential itself (extending the
  existing cardinality/sensitivity audit discipline from
  [observability.md](observability.md)).
- If principals are introduced as tenants: a test proving two tenants can
  each use the same `job_type`/`Idempotency-Key` without colliding.
- A privilege-audit test/script confirming the worker's PostgreSQL role
  cannot perform statements outside its documented least-privilege grant
  set.

### Enterprise exit criteria
- [ ] `POST /jobs` without a valid credential returns `401`, not `200`
      (mirrors [enterprise-readiness.md](enterprise-readiness.md)'s own
      exit criterion).
- [ ] A caller cannot act on another caller's job/workflow — proven by test,
      with existence-disclosure avoided.
- [ ] `POST /jobs` over plaintext HTTP is refused, or the deployment
      documents and tests a required TLS-terminating proxy boundary, with a
      tested no-plaintext boundary either way.
- [ ] The enterprise reference deployment's database connection requires
      `sslmode=verify-full` against a trusted CA, with a test proving both
      plaintext and untrusted-certificate connections are refused.
- [ ] Structured logs record an `actor` for every submit/cancel action, and
      never the raw credential.
- [ ] Separate least-privilege PostgreSQL roles exist for API and worker
      processes, with a test/audit confirming each role's grants.
- [ ] If principals are introduced as tenants in this phase, idempotency
      uniqueness is tenant-scoped from the same change, proven by test.

---

## Phase 13 — Workload Governance + Retention

### Why it matters
Confirmed by direct code inspection: one global claimable pool, `priority`
as a claim-query tiebreak only, no named queues, no per-type/per-tenant
concurrency limits, no rate limiting, no fairness guarantee, no backpressure
signal. Separately, and just as confirmed: `jobs`, `job_attempts`, and
workflow rows grow forever — there is no retention/lifecycle policy for
terminal history anywhere in the schema or documentation. Both problems are
"the database keeps everything from every caller, forever, with no
governance over who can claim what" — they are grouped into one phase
because a retention policy that deletes rows without respecting the
governance/tenant model built alongside it would be designed twice.
[reference-analysis.md](reference-analysis.md) identifies Hatchet's
Postgres-native queue/concurrency/fairness/rate-limit model as the most
directly transferable *lesson* in this entire review — a lesson about
Postgres-native governance being achievable, not a commitment to replicate
Hatchet's exact scheduling algorithm.

### Exact scope
**Workload governance:**
- **Named queues**: introduce a `queue_name` concept (schema addition,
  additive migration per the expand/migrate/contract model in
  [compatibility-policy.md](compatibility-policy.md)), routable
  independently of `job_type`. Include explicit **worker queue
  subscription/routing semantics** — which queues a given worker process
  claims from, and how that is configured.
- **Per-queue/per-tenant/principal-scoped concurrency limits**: a
  configurable cap on concurrently-running jobs per queue (and, using Phase
  12's principal concept, per-tenant where supported), enforced in the
  claim query.
- **Fairness**: specify the required fairness/liveness property first —
  e.g., "no queue/tenant with pending capacity-eligible work is starved for
  longer than a documented bound while another queue/tenant is making
  progress" — and require an **ADR** to select the simplest correct
  PostgreSQL-native scheduling algorithm only *after* a concurrency and
  performance analysis. This roadmap does **not** pre-commit to Hatchet's
  (or any other system's) exact algorithm; Hatchet's group-key round-robin
  is one candidate worth evaluating in that ADR, not a foregone conclusion.
  Faktory's strict-priority-drain model is explicitly rejected as a known
  starvation anti-pattern, independent of which algorithm the ADR selects.
- **Rate limiting**: static, per-queue (and optionally per-tenant) submission
  rate limits at the API layer, staged *after* the above per the Faktory
  precedent (ship governance primitives before rate limiting, not
  simultaneously).
- **Backpressure / overload admission signal, with separated HTTP
  semantics**:
  - `429` is reserved for **caller/tenant/queue admission or rate-limit
    policy** — a specific caller, tenant, or queue has been throttled by a
    configured policy.
  - `503` is reserved for **service/system capacity or unavailability** —
    the system as a whole cannot currently accept work (e.g., PostgreSQL
    itself is saturated), independent of which caller is asking.
  - A queue simply reaching its normal, configured concurrency cap is
    **not**, by itself, a rejection condition — jobs still enqueue and wait
    their turn; `429`/`503` apply only once a documented admission/rate/
    capacity threshold is actually crossed. Both responses carry a
    `Retry-After` header.

**Retention (the missing enterprise concern this phase also closes):**
- A retention/lifecycle design for `jobs`, `job_attempts`, and workflow
  rows, covering:
  - **Terminal job retention**: how long a `SUCCEEDED`/`DEAD_LETTERED`/
    `CANCELLED` job row is kept before it becomes eligible for cleanup.
  - **Attempt-history retention**: `job_attempts` rows independently, since
    they can accumulate faster than terminal job rows.
  - **Workflow retention**: `workflow_instances`/`workflow_nodes` rows,
    analogous to jobs.
  - **Safe batch cleanup**: a bounded, batched deletion process (not a
    single unbounded `DELETE`) that does not starve claim-query traffic
    while running, consistent with the "no migration/maintenance operation
    may lock claim-critical tables for an incompatible duration" discipline
    already proposed in [compatibility-policy.md](compatibility-policy.md).
  - **Cleanup observability**: metrics/logs for what was pruned, when, and
    how much — an operator must be able to see retention actually running,
    not infer it silently happened.
  - **Index/vacuum implications**: a large, continuously-churning table has
    autovacuum and index-bloat implications; this must be documented and
    accounted for, not discovered later.
  - **Optional archive/export boundary**: whether pruned rows are simply
    deleted or first exported to cold storage, left as an operator choice
    but the boundary (what gets exported, in what shape, before deletion)
    must be specified if the option is offered.
  - **Idempotency implications of deleting old rows**: pruning historical
    rows must **not** silently destroy the idempotency/dedup guarantee —
    if an `idempotency_key` is only unique because the old row that used it
    still exists, deleting that row while a duplicate-with-the-same-key
    request could still plausibly arrive would silently reopen a dedup
    window that TF-INV-016 currently closes. The retention design must
    state explicitly how long an idempotency key remains dedup-authoritative
    relative to how long its owning job row is retained, and must not
    let cleanup outpace that window.

### Explicit non-scope
- No dynamic/adaptive rate limiting based on real-time load (static,
  configured limits only in this phase).
- No cross-queue global fairness guarantee beyond whatever bounded property
  the required ADR proves — a mathematically strict, unconditional fairness
  bound is not claimed, and a finite stress test never proves "indefinite"
  fairness; only a bounded guarantee under documented assumptions (run
  duration, load shape, queue count) is claimed.
- No per-job-type backoff configuration (unrelated to queue governance,
  already tracked separately in [retry-semantics.md](retry-semantics.md)'s
  open questions).
- No periodic/cron scheduling (explicitly deferred per
  [reference-analysis.md](reference-analysis.md)).
- No automatic archival infrastructure beyond the optional, specified
  export boundary — this phase does not build a data warehouse pipeline.

### Prerequisites
Phase 12 (for the principal concept used in per-tenant limits/fairness, and
because the tenant-scoped idempotency fix must already be in place before
retention can safely prune rows) and Phase 11 (stable, versioned API to add
queue/rate-limit fields to).

### Invariants / proof obligations
- A new invariant, tentatively TF-INV-017: "no queue/tenant with pending,
  capacity-eligible work is starved beyond the documented bound while
  another queue/tenant is making progress" — the *bound* is defined by the
  ADR's chosen algorithm and its stated assumptions, not claimed as
  unconditional. Provable under a Phase-5-style concurrency stress test
  with two queues, one flooded and one starved under the old model, both
  making bounded-wait-time progress under the new one, over the tested
  run's duration (not claimed to extrapolate to "indefinitely").
- Concurrency limits are enforced exactly (never more than N concurrently
  `RUNNING` jobs for a capped queue), provable under the same
  `SKIP LOCKED`-based concurrent-claim stress testing already used in
  Phase 5.
- A caller receiving `429`/`503` and retrying after the documented
  `Retry-After` window succeeds (no permanent lockout from a transient
  backpressure signal), and a queue at its normal concurrency cap (with no
  admission/rate-limit threshold crossed) does not itself trigger either
  code.
- A retention cleanup pass never deletes a row whose `idempotency_key` is
  still inside its documented dedup-authoritative retention window,
  provable by a test that submits a duplicate request against an
  old-but-still-in-window row and confirms the dedup behavior still fires.

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
- A retention batch-cleanup job runs during peak claim-query load and
  measurably degrades claim latency — must be shown not to happen (or
  documented and scheduled to avoid peak windows) rather than discovered in
  production.
- A retention cleanup pass deletes a job row whose idempotency key a
  legitimate late-arriving duplicate request still depends on — must not
  happen; see the invariant above.

### Tests / evidence required
- A two-queue fairness stress test (flooded queue + starved queue under the
  old model) proving the starved queue now makes bounded-wait-time progress
  under the ADR-selected algorithm, over the tested run's duration.
- A concurrency-limit stress test proving the cap is never exceeded under
  concurrent claim attempts.
- An API-level test proving `429`/`503` + `Retry-After` is returned only
  once a configured admission/rate/capacity threshold is crossed (not at
  ordinary concurrency-cap saturation), and that retrying after that window
  succeeds.
- A retention/cleanup test proving batched deletion completes without
  starving concurrent claim-query traffic, and a companion test proving
  idempotency dedup still fires correctly for a row inside its retention
  window.
- The governance-algorithm ADR itself, committed to the repository before
  implementation begins.

### Enterprise exit criteria
- [ ] An ADR selecting the Phase 13 fairness/scheduling algorithm exists,
      committed before implementation, stating the concurrency/performance
      analysis behind the choice.
- [ ] A named-queue or per-job-type/tenant concurrency-limit mechanism
      exists with a passing test demonstrating one queue/tenant cannot
      starve another beyond the documented bound (mirrors
      [enterprise-readiness.md](enterprise-readiness.md)'s own exit
      criterion, restated as a bounded, not indefinite, guarantee).
- [ ] Rate limiting exists for at least one dimension (queue or tenant) and
      is durable across a restart.
- [ ] `POST /jobs` returns a documented `429`/`503` + `Retry-After` only
      under configured admission/rate/capacity overload, verified by test,
      with the two codes' semantics kept distinct.
- [ ] A terminal-record retention/lifecycle policy is documented and
      implemented for `jobs`, `job_attempts`, and workflow rows, with
      batched cleanup, cleanup observability, and a proven
      idempotency-safe retention window.

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
- Formal adoption of **expand / migrate / contract** as the primary
  production schema-compatibility model (see
  [compatibility-policy.md](compatibility-policy.md)): additive expansion
  first, old/new binaries coexisting, backfill/migrate if required, old
  readers/writers stopped and confirmed, and only then a later contract
  release removes the old shape. This **replaces** any universal
  "every migration needs a tested down migration" rule — a down migration
  is required only for the subset of migrations that are genuinely
  data-safe to reverse; a destructive change (e.g., dropping a column with
  data already written under the new shape) is **forward-fixed** by a later
  migration instead, because a down migration cannot honestly reconstruct
  data it never had.
- An integration test harness that runs two different binary versions of
  `cmd/api` (or `cmd/worker`) concurrently against the same database, for
  the **full duration of an expand/migrate/contract window** (not just at
  the two endpoints), and asserts correct behavior throughout — closing
  [compatibility-policy.md](compatibility-policy.md)'s single
  largest-identified gap.
- A test proving an old worker binary (pre-Phase-13 schema) continues to
  function correctly against a post-Phase-13 schema with the new queue/
  concurrency columns present but unused by the old worker.
- CI enforcement that runs the down migration for every migration
  explicitly labeled data-safe-reversible (both directions run in CI for
  that subset); migrations labeled forward-fix-only are not expected to
  have a working down migration, and CI must not require one for them.
- Formal adoption of the migration-ordering rule (migrate schema first,
  deploy new worker/server second) as documented, required deployment
  sequence.
- **Graceful worker/API draining**, formalized here as part of proving a
  rolling deployment is actually safe: a documented, tested,
  operator-facing SIGTERM/drain contract (stop accepting new claims/
  requests, finish in-flight work up to a timeout) for `cmd/worker` and
  `cmd/api`, extending what Phase 5's
  `TestStress_WorkerPoolGracefulShutdown_*` already proves at the goroutine
  level to the OS-process level. A rolling upgrade that kills in-flight
  work mid-deploy is not a safe rolling upgrade, so draining is proven
  together with the mixed-version test above, not as an afterthought.
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
  handwritten `.up.sql`/`.down.sql` pairs (existing, working practice) for
  the data-safe-reversible subset; forward-fix-only migrations may omit a
  meaningful `.down.sql` and must say so explicitly in the migration file.
- No support for skipping more than one minor version in a rolling upgrade
  (single-version-skew compatibility only, matching the exit criterion
  already stated in [enterprise-readiness.md](enterprise-readiness.md)).

### Prerequisites
Phases 11–13, so there is a real, non-trivial schema/API change to prove
compatibility against (testing compatibility against a schema that has
never actually changed would be a hollow proof).

### Invariants / proof obligations
- An old worker binary and a new worker binary can run concurrently against
  one database (mid-rolling-deploy, for the full expand/migrate/contract
  window) with zero invariant violations (TF-INV-001 through the new
  TF-INV-017 from Phase 13), checked via the same invariant-checker harness
  Phase 9's `internal/chaos` already provides.
- A down migration, where one is claimed (data-safe-reversible label),
  actually reverses its up migration's schema effect (verified by
  re-running the up migration's own test suite against the down-then-up-
  again state). A forward-fix-only migration is never asserted to have a
  working down path.
- A claimed job with no registered handler for its `job_type` fails in a
  documented, bounded way (e.g., a specific error classification and
  eventual dead-lettering) rather than an undefined/crashing behavior.
- A SIGTERM'd worker/API process stops accepting new claims/requests
  immediately and exits only after in-flight work completes or a documented
  timeout elapses, whichever comes first.

### Failure scenarios to guard against
- A rolling deploy is half-complete (old and new server/worker binaries
  both live) when a job that only the new binary understands is claimed by
  an old worker — must degrade in a documented, safe way (e.g., old worker
  never claims a queue/field it doesn't recognize), not crash or corrupt
  state.
- An operator runs a down migration in production against a still-populated
  table for a migration that is **not** labeled data-safe-reversible — this
  must be prevented or loudly refused, not silently attempted; the
  forward-fix path must be the documented remediation instead.
- A `job_type` string is repurposed for an incompatible payload shape
  across a deploy — the documented guidance (never repurpose a `job_type`
  string) must be paired with a test proving what actually happens if it's
  violated, so the failure mode is known even though it's not prevented.
- A worker is killed without SIGTERM (e.g., SIGKILL) mid-drain — the drain
  contract does not claim to handle this case; it is a Phase 17 concern
  (lease expiry/reclaim), not a Phase 14 one.

### Tests / evidence required
- Two-binary-version concurrent integration test (old worker/new server and
  new worker/old server), passing across the full expand/migrate/contract
  window.
- CI job running the down path for every migration labeled
  data-safe-reversible; a check confirming forward-fix-only migrations are
  explicitly labeled as such.
- A test claiming a job with an unregistered `job_type`, asserting the
  documented failure/dead-letter behavior.
- A graceful-drain integration test proving in-flight work completes (or
  times out cleanly) after SIGTERM, with no new claims accepted after the
  signal.

### Enterprise exit criteria
- [ ] A two-binary-version (old worker/new server, and new worker/old
      server) integration test exists and passes across a full expand/
      migrate/contract window (mirrors
      [enterprise-readiness.md](enterprise-readiness.md)'s own exit criterion).
- [ ] CI runs the down path for every data-safe-reversible migration; every
      forward-fix-only migration is explicitly labeled and CI does not
      require a down path for it.
- [ ] The unregistered-`job_type` failure mode is documented and tested.
- [ ] A documented, tested graceful-drain (SIGTERM) contract exists for
      `cmd/worker` and `cmd/api`.
- [ ] [compatibility-policy.md](compatibility-policy.md)'s PROPOSED markers
      are updated to reflect what is now actually proven vs. still proposed.

---

## Phase 15 — PostgreSQL HA / Backup / DR Proof

### Why it matters
[failure-model.md](failure-model.md) explicitly and correctly places
PostgreSQL primary failure/failover out of scope for TaskForge's own code —
"if the database is down, TaskForge is down for writes" is a documented,
honest v1 boundary, not an oversight. But per
[reference-analysis.md](reference-analysis.md)'s PostgreSQL research, this
is not a gap TaskForge needs to write code to close — PostgreSQL itself
already provides base backup, WAL archiving, and point-in-time recovery, and
third-party deployment tooling (e.g., Patroni, repmgr) provides failover
orchestration; what is missing is a documented, *drilled* runbook with a
stated RPO/RTO and evidence that TaskForge itself behaves correctly across
a failover, which is exactly what an enterprise security/operations review
will ask for first. TaskForge does **not** build its own replication or
failover machinery in this phase — it uses PostgreSQL-native primitives and
deployment tooling, and proves TaskForge's behavior against them.

### Exact scope
- A documented backup procedure using `pg_basebackup` plus continuous WAL
  archiving (`archive_mode`/`archive_command`). **Correction to a common
  misconception**: `pg_basebackup` automatically manages the low-level
  backup-mode API (`pg_backup_start`/`pg_backup_stop`) internally — an
  operator running ordinary `pg_basebackup` does not need to, and should
  not, wrap it in manual `pg_backup_start`/`pg_backup_stop` calls; that
  low-level API is for custom backup tooling that copies the data directory
  by other means, not for `pg_basebackup` itself. The runbook must not
  present manual bracketing as a required step around `pg_basebackup`.
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
- **At least one controlled standby-promotion/failover drill**: promote a
  standby to primary in a test environment and record what happens,
  including:
  - **Evidence of TaskForge's own reconnect/recovery behavior** — do API
    servers and workers reconnect to the newly-promoted primary
    automatically (connection-string/DNS-based failover, or does an
    operator action/restart intervene), and how long does that take, in
    wall-clock terms, from promotion to TaskForge resuming writes.
  - Whether any in-flight claims/leases are affected by the failover, and
    whether the existing fencing mechanism (ADR-0002) behaves correctly
    across it (it should, by construction, but this must be observed, not
    assumed).
- If **read replicas** are evaluated for `GET /jobs/{id}`/`GET /workflows/{id}`
  reads, explicitly document the consistency/read-after-write implications
  before recommending adoption: a caller that just submitted or cancelled a
  job via the primary could read stale state from a replica lagging behind
  replication, and the runbook/documentation must say so rather than
  silently assume replicas are safe for every read path.

### Explicit non-scope
- No TaskForge-built failover orchestration, consensus, or split-brain
  detection — PostgreSQL's own docs confirm this is explicitly not
  PostgreSQL's job either; it is deployment-layer tooling (e.g., Patroni,
  repmgr) chosen by the operator, named or left tool-agnostic per the
  team's later judgment, not built by this project.
- No multi-region/multi-datacenter durability (remains an explicit
  [vision.md](vision.md) non-goal, and outside the Enterprise Deployment
  Profile's single-region assumption).
- No automatic backup scheduling infrastructure built into TaskForge itself
  — the backup/restore procedure is documented and operator-run (via cron,
  a managed PostgreSQL provider's built-in backup feature, or equivalent),
  not a TaskForge feature.

### Prerequisites
None — independent of Phases 11–14 and 16, can run in parallel if a second
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
- After a controlled standby-promotion drill, TaskForge (API servers and
  workers) resumes correct operation against the new primary within a
  measured, recorded duration, with fencing invariants intact throughout.

### Failure scenarios to guard against
- A restore is attempted from a backup that was taken mid-write — must be
  demonstrated to either fail cleanly or be prevented by the documented
  procedure, not silently produce a corrupt restore. (Per the correction
  above, this is a non-issue for `pg_basebackup` itself, which handles
  backup-mode bracketing internally — this scenario applies to custom
  backup tooling that bypasses `pg_basebackup`, and the runbook should say
  so.)
- A WAL archive gap (a missing archived segment) — the documented procedure
  must state what happens (recovery fails at that point, with a clear
  error) rather than silently skipping forward.
- A standby is promoted while TaskForge workers/API servers are still
  connected to the old primary — the documented reconnect behavior must be
  observed, not assumed, and any window of failed writes must be bounded
  and stated.

### Tests / evidence required
- At least one full, timed backup-then-restore drill, with the restored
  database's invariants verified via the existing checker.
- At least one controlled standby-promotion/failover drill, with TaskForge's
  reconnect/recovery time measured and recorded.
- A written runbook a different engineer (not the one who wrote it) can
  follow to reproduce both the restore and the failover drill, reviewed for
  clarity.

### Enterprise exit criteria
- [ ] A documented `pg_basebackup`/WAL-archiving-based backup exists (with
      the `pg_backup_start`/`pg_backup_stop` misconception corrected in the
      runbook) and a restore-from-backup drill has been executed and timed
      at least once (mirrors [enterprise-readiness.md](enterprise-readiness.md)'s
      own exit criterion).
- [ ] A stated RPO/RTO exists and the drilled restore time is compared
      against it.
- [ ] The restored database passes the existing invariant checker.
- [ ] An HA topology recommendation is documented, with its tradeoffs
      (sync vs. async replication) stated per PostgreSQL's own guidance.
- [ ] At least one controlled standby-promotion/failover drill has been run,
      with TaskForge's reconnect/recovery behavior measured and recorded.
- [ ] If read replicas are recommended for any read path, the
      consistency/read-after-write implications are documented before
      adoption.

---

## Phase 16 — Tracing & Operator Diagnostics

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
  **Caveat**: OTel's messaging semantic conventions are still marked
  *Development* (not Stable) as of this review's access date. This phase
  must **pin and document the exact convention version used**, and must
  **not** treat experimental attribute names as part of TaskForge's
  permanent API/compatibility guarantee — a future convention revision may
  rename attributes, and TaskForge's own compatibility promises
  ([compatibility-policy.md](compatibility-policy.md)) apply to TaskForge's
  API/schema, not to upstream OTel's still-evolving attribute names.
- A documented mechanism for how trace context is **durably associated**
  with a queued job across long waits, retries, and lease reclaims (e.g.,
  storing the trace/span identifiers alongside the job row so a reclaim or
  retry can re-link to the original submission span) — this association is
  for diagnostic/correlation purposes only and must **never** become
  correctness-authoritative: TaskForge's state machine and invariants
  (TF-INV-001 etc.) do not depend on trace context being present, correct,
  or complete.
- **Telemetry failure must remain non-load-bearing**: the trace exporter
  becoming unavailable must not block or fail job execution, consistent
  with TaskForge's existing observability-under-failure discipline from
  Phase 9.
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
- A documented, operator-facing graceful-drain procedure is **not**
  duplicated here — it is formalized in Phase 14, alongside the
  mixed-version compatibility proof it is part of; this phase only consumes
  `/healthz`/`/readyz` as inputs to that drain contract's own testing.
- **All new admin/retry/history endpoints must inherit Phase 12's
  authorization and tenant-ownership checks from day one** — a
  `GET /jobs/{id}/history` or `POST /jobs/{id}/retry` endpoint is exactly
  as sensitive as `GET /jobs/{id}` itself and must not be shipped without
  the same ownership check.

### Explicit non-scope
- No vendor dashboards or alerting rules (remains an explicit non-goal,
  correctly, per [observability.md](observability.md)).
- No distributed-tracing-based *replay* or debugging tooling beyond spans/
  traces themselves (no Temporal-style workflow-history UI).
- No admin UI — a `GET /jobs/{id}/history` API and a retry endpoint are in
  scope; a browser-based admin console is not.
- No claim that trace/span data is authoritative for any correctness
  property — it is diagnostic only.

### Prerequisites
Phase 12 must land first (or concurrently) so the new audit-relevant
endpoints (history, retry) are authorizable from day one rather than
needing a second pass.

### Invariants / proof obligations
- A traced job's submission span and execution span(s) are correctly linked
  (not falsely parented) even when the queue wait spans hours (provable
  with a synthetic delayed job and a test-exporter assertion), and this
  linkage survives at least one retry/reclaim cycle.
- `/healthz` reflects true process liveness; `/readyz` reflects true
  readiness to serve traffic (e.g., false during startup before the DB
  connection pool is established) — both provable by integration test.
- A DLQ-replayed job re-enters the state machine at `QUEUED` with a fresh
  `job_id` and a recorded link (`retried_from`) to the original — the
  `retried_from` field already exists in the schema per the internal audit
  but is currently unexercised by any test; this phase must add that test,
  scoped to the same tenant/principal as the original job (per
  [security-model.md](security-model.md) §7).
- Every new endpoint added by this phase enforces the same authorization/
  ownership check as the existing `GET /jobs/{id}` endpoint, provable by
  the same two-principal test pattern Phase 12 established.

### Failure scenarios to guard against
- The trace exporter itself becomes unavailable — job execution must not
  block or fail due to tracing infrastructure being down.
- `/readyz` reports ready while the database is actually unreachable — must
  be tested against a real disconnected-database scenario using Phase 9's
  existing `internal/chaos` connection-interruption primitives.
- A future OTel semantic-convention revision renames an attribute this
  phase used — must not break TaskForge's own compatibility guarantees,
  because those attributes were never promised as part of TaskForge's API.
- A new admin/history/retry endpoint is added later without going through
  the Phase 12 authorization check — must be caught by a lint/test
  convention (e.g., a route-registration test that fails if a route is
  registered without an auth middleware wrapper), not merely by review
  discipline.

### Tests / evidence required
- A span-link assertion test using a test/in-memory OTel exporter,
  including across a retry/reclaim.
- HTTP-metrics scrape test proving request count/latency/status appear
  correctly per route.
- `/healthz`/`/readyz` tests under normal and DB-disconnected conditions
  (reusing Phase 9's chaos primitives).
- A DLQ-replay test proving a dead-lettered job can be replayed only by its
  owning principal, and the `retried_from` linkage is recorded and
  queryable.

### Enterprise exit criteria
- [ ] Job submission and execution are linked via OpenTelemetry spans
      following a pinned, documented version of OTel's messaging semantic
      conventions, with those conventions excluded from TaskForge's own
      permanent compatibility guarantee.
- [ ] `taskforge_claim_latency_seconds` and `taskforge_queue_age_seconds`
      are distinct, independently meaningful observations.
- [ ] `/healthz` and `/readyz` exist and are proven correct under a
      simulated database outage.
- [ ] A dead-lettered job can be replayed via a documented API, scoped to
      its owning tenant/principal, with the `retried_from` linkage tested.
- [ ] Every endpoint added by this phase enforces Phase 12's authorization/
      ownership checks, verified by test.

---

## Phase 17 — Performance / Saturation / Multi-Process Failure Proof

### Why it matters
The only load evidence in the entire project is Phase 9's single 4-minute
soak run, explicitly and correctly labeled "illustrative, not benchmarks."
[slo.md](slo.md) lays out exactly what a real capacity-test plan requires
and confirms none of it has been done: no HTTP-domain instrumentation (now
closed by Phase 16), no multi-hour run (the roadmap's own Stable bar for
Phase 9, not yet met), no worker-count/pool-size sweep, and — most
importantly for an enterprise buyer — no deliberate overload run showing
what actually happens when submission rate exceeds sustainable claim
throughput. Separately, and just as important: Phase 9's chaos harness has
never crossed an actual OS-process boundary — every simulated crash so far
happens inside one process. An enterprise buyer's real failure mode is a
worker *process* dying (OOM-killed, deployed over, or crashed) while other
worker processes on other hosts keep running against the same database;
this phase adds that missing evidence.

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
- **A real OS-process-boundary failure campaign** (the previously-missing
  multi-process evidence):
  - Run multiple independent worker **processes** (not goroutines within
    one process) against the same PostgreSQL database.
  - Kill selected worker processes with `SIGKILL` (not a graceful signal —
    this specifically tests the case Phase 14's drain contract does *not*
    cover) mid-execution.
  - Restart the killed workers as new processes.
  - Allow the killed workers' leases to expire and be reclaimed by
    surviving/restarted workers.
  - **Verify stale results from the killed processes remain fenced** — if a
    killed worker somehow still completes and reports a result after its
    lease was reclaimed, TF-INV-002/003/014's fencing must reject it, and
    this must be observed under this real multi-process scenario, not only
    under the existing single-process simulated fencing tests.
  - Run the existing invariant checker continuously throughout and after
    the recovery, across all surviving/restarted processes.
- External PostgreSQL-side and host-level monitoring captured alongside
  every run above: OS/container/cgroup CPU measurement (not derivable from
  `pg_stat_*`, per [slo.md](slo.md)'s correction) plus
  `pg_stat_activity`/`pg_locks` for database-side contention.
- Validate (or revise) the `TARGET (proposed, unvalidated)` SLO thresholds
  in [slo.md](slo.md) against the numbers actually measured here, converting
  them from proposed to evidence-backed where the data supports it.

### Explicit non-scope
- **Multi-host/network-partition testing remains explicitly deferred** —
  this phase's process campaign runs multiple processes (potentially on one
  host) against one database; simulating an actual network partition
  (iptables/netns) between hosts is a separate, larger investment in
  distributed test infrastructure with no demonstrated need shown yet, and
  is not attempted here.
- No fabricated or extrapolated numbers under any circumstance, and **no
  universal/general-purpose performance claim is published** — every
  number published traces to one specific, fully-documented run (see the
  benchmark-report requirement below) and is stated as applying to that
  configuration, not as a general TaskForge performance guarantee.

### Prerequisites
Phase 13 (so there is a real backpressure signal to validate under load)
and Phase 16 (so HTTP-domain and split claim/queue-wait metrics exist to
measure).

### Invariants / proof obligations
- Zero invariant violations across the entire multi-hour run (the existing
  TF-INV-001–016 set, plus TF-INV-017 from Phase 13), checked continuously
  by the existing `internal/chaos` invariant checker exactly as Phase 9
  already does at shorter duration.
- Zero invariant violations across the multi-process SIGKILL/restart/
  reclaim campaign specifically — this is a distinct proof obligation from
  the single-process soak run above, because it is the first time
  TF-INV-002/003/014 (fencing) are exercised across a genuine OS-process
  boundary rather than a simulated in-process crash.
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
- A `SIGKILL`ed worker's in-flight job result is somehow still written to
  the database after its lease has been reclaimed and the job re-executed
  by another worker — must be rejected by fencing; if it is not, this is a
  Phase-1–9 invariant regression discovered under real multi-process
  conditions, and must be treated with the same severity as any other
  invariant violation.

### Tests / evidence required
- A published, dated benchmark report of one multi-hour `cmd/chaos
  -mode=soak` run with zero invariant violations, committed to the
  repository.
- A worker-count scaling curve and a connection-pool sweep, both published
  with their actual methodology and raw data.
- A published overload-run report showing the backpressure signal engaging
  and its effect on caller-observed behavior.
- A published report of the multi-process SIGKILL/restart/reclaim campaign,
  including the fencing-under-real-process-death evidence.
- **Every published benchmark report must record**: exact commit SHA,
  random seed used, Go version, PostgreSQL version and relevant
  configuration (e.g., `shared_buffers`, `max_connections`), CPU
  (model/core count), RAM, storage environment (local SSD, network-attached
  volume, cloud instance type), worker count, connection-pool size,
  workload parameters (job count, payload size, job-type mix), and the raw
  results (not only derived percentiles) — so a reader can judge
  applicability to their own environment rather than take a bare number on
  faith.

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
- [ ] A multi-process `SIGKILL`/restart/lease-reclaim campaign has been run
      with the existing invariant checker passing throughout and after
      recovery, and stale post-reclaim results proven fenced.
- [ ] Every published benchmark report includes the full required metadata
      (commit SHA, seed, Go/PostgreSQL versions, hardware, workload
      parameters, raw results), and no universal performance claim is made
      beyond the specific measured configuration.
- [ ] [slo.md](slo.md)'s proposed targets are updated to reflect
      actually-measured evidence.

---

## Phase 18 — External Human Validation & Release Candidate

### Why it matters
Every phase above is TaskForge's own team validating its own work. An
enterprise-readiness claim carries more weight, and is less prone to blind
spots, when it survives review by someone who did not write the code — this
is the standard closing step of any serious infrastructure hardening effort,
and this review does not consider TaskForge "enterprise-ready" merely
because Phases 10–17 are complete; it must also survive scrutiny from a
real, independent human reviewer.

### Exact scope
- An **independent review conducted by at least one real human reviewer**
  who did not implement the relevant Phase 10–17 code, covering at minimum
  the authentication/authorization/transport-security work from Phase 12,
  the compatibility proof from Phase 14, and the DR drill from Phase 15.
  **An AI or multi-agent review (including TaskForge's own
  `/code-review ultra` multi-agent review) may be gathered as supplementary
  evidence, but does not satisfy this requirement on its own** — it is not
  a substitute for independent human judgment, and this phase's exit
  criteria are not met by AI-review evidence alone.
- The independent human reviewer must **attempt to break, not merely read**,
  at least the following, and record what was attempted and found:
  - A **security boundary** from Phase 12 (e.g., attempt to bypass
    authentication, escalate across tenants, or find an existence-
    disclosure leak).
  - The **compatibility/upgrade story** from Phase 14 (e.g., attempt to
    construct a rolling-deploy sequence the documented rules did not
    anticipate).
  - The **recovery/DR procedure** from Phase 15 (e.g., actually follow the
    runbook without help from its author, or attempt a restore/failover
    scenario not explicitly covered).
  - At least one **failure/invariant claim** from Phase 9's original set or
    a phase-specific invariant added since (e.g., attempt to construct a
    fencing violation, a starvation scenario, or a retention/idempotency
    collision).
- A final, consolidated enterprise-readiness scorecard: every exit
  criterion checkbox from Phases 10–17, in one place, each linked to its
  evidence (a committed test, a committed report, a PR).
- **Findings and their resolutions are recorded publicly where safe** (i.e.,
  where doing so does not itself disclose an unpatched, exploitable
  vulnerability) — a security finding that must remain confidential until
  fixed should still have its resolution recorded publicly once the fix
  ships.
- A decision on whether to label the resulting state a "Release Candidate"
  and what, if anything, remains explicitly deferred beyond it (e.g.,
  full RBAC, multi-region, hostile-worker-fleet isolation, periodic/cron
  scheduling — all already identified as legitimate P2/P3 deferrals
  throughout this review).

### Explicit non-scope
- This phase does not add new capabilities — it validates and consolidates
  what Phases 10–17 built. Any finding from the external review that
  reveals a real gap should be triaged back into the relevant earlier
  phase (or a new, explicitly scoped follow-up), not silently patched here
  without updating that phase's documentation.
- An AI/multi-agent review's findings, while useful supplementary evidence,
  do not by themselves close this phase's independent-review exit
  criterion — only a real human reviewer's findings do.

### Prerequisites
Phases 10–17 substantially complete. This phase is meaningless run early —
reviewing incomplete work as if it were final defeats its purpose.

### Invariants / proof obligations
- Every checkbox in the consolidated scorecard traces to a specific,
  inspectable artifact (a test name, a CI run, a committed report) — no
  checkbox is marked done on the basis of an unverified claim, consistent
  with this entire review's own discipline of "no unsupported claims."
- The independent-review exit criterion is satisfied only by evidence of a
  real human reviewer's participation (name/role need not be public, but
  the fact of human review, its scope, and its findings must be recorded).

### Failure scenarios to guard against
- The external review finds a real gap in Phase 12's security work after
  it was believed complete — the response must be to reopen and fix Phase
  12, not to relabel the finding as "future work" to preserve a release
  date.
- A team substitutes an AI/multi-agent review for the required human
  reviewer to save time — this does not satisfy the exit criterion, and a
  scorecard that treats it as if it did must not be considered complete.

### Tests / evidence required
- A written independent human-review report, findings, and resolution
  status for each finding, with an explicit statement of who conducted it
  and confirmation they did not implement the code under review.
- The consolidated scorecard itself, committed to the repository.
- Public record of findings and resolutions where safe to disclose.

### Enterprise exit criteria
- [ ] An independent review of Phase 12 (security) has been completed **by
      a real human reviewer who did not write the Phase 12 code**, with
      findings resolved or explicitly and visibly deferred with reasoning.
      Supplementary AI/multi-agent review evidence may accompany this but
      does not substitute for it.
- [ ] That reviewer (or a comparable independent human reviewer) has
      attempted to break at least one security boundary, the compatibility/
      upgrade story, the recovery/DR procedure, and one failure/invariant
      claim, with findings recorded.
- [ ] The DR runbook (Phase 15) has been followed successfully by someone
      other than its author.
- [ ] A consolidated scorecard exists, mapping every Phase 10–17 exit
      criterion to committed evidence.
- [ ] Findings and resolutions are recorded publicly where safe to disclose.
- [ ] A go/no-go decision on a "Release Candidate" label is documented,
      including what remains explicitly and honestly deferred beyond it.

---

## What This Roadmap Deliberately Does Not Include

Per [reference-analysis.md](reference-analysis.md)'s "Explicitly Rejected
Capabilities": no message broker, no Redis, no Kubernetes operators/service
mesh, no deterministic-replay workflow execution, no event sourcing, no
microservices decomposition, no `LISTEN/NOTIFY`-based claim latency
optimization, no strict-priority queue draining, no full RBAC/OIDC system,
no periodic/cron scheduling, no container image requirement, and no
guarantee of safety against a hostile/fully-untrusted worker fleet (that
would require an undesigned worker-gateway or stored-function architectural
boundary — see Phase 12). Each is named explicitly, with the reasoning for
rejecting or deferring it, rather than silently omitted — consistent with
this review's mandate to avoid feature bloat and prefer a strong,
deliberate, honestly-scoped architecture over an impressive dependency
list or an unsupported claim of universal safety.
