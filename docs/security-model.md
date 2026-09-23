# Security Threat Model

Status: **review document**, produced during the `enterprise-readiness-review`
review. This is a threat model, not an implementation — it names risks and
classifies them; it does not add any control. Where a control already exists
in the codebase, this document says so and cites it.

This document exists because TaskForge's own [vision.md](vision.md) lists
"Multi-tenant isolation / auth / quota enforcement" as an explicit v1
non-goal. That is a legitimate, honest scoping decision for a correctness
demonstration. It is also, by definition, an unaddressed threat surface the
moment TaskForge is considered for any deployment reachable by more than one
fully-trusted party. This document makes that surface explicit rather than
leaving it implicit.

## Trust Domains

TaskForge's own [architecture.md](architecture.md) draws a "durability
boundary" around API server + PostgreSQL + workers. Security requires a
second, orthogonal boundary — a **trust boundary** — which today does not
exist as a designed concept anywhere in the codebase:

```
 [ any HTTP client ] ---> [ API server ] ---> [ PostgreSQL ] <--- [ any worker process ]
        ^                       ^                    ^                    ^
   no authN/authZ         no authN/authZ      trusted by            no authN/authZ,
   today                  today               construction          no identity
                                               (source of truth)     verification
```

Every arrow above is, today, an implicitly-trusted channel. This document
threat-models each one.

## Enterprise Deployment Profile (Assumed)

The severities below are not evaluated against an unbounded threat model
("assume every component is hostile"). They are evaluated against the
deployment profile this review recommends TaskForge target first —
[enterprise-roadmap.md](enterprise-roadmap.md) Phase 12 is scoped to this
profile, not to a zero-trust worker fleet:

- **Multi-principal API/control plane**: more than one caller/tenant can
  reach the API concurrently, and callers are not assumed to trust each
  other.
- **Trusted first-party worker fleet**: worker processes run code the
  operator controls and deploys; TaskForge does not, in this profile,
  defend against a worker process that is itself malicious or runs
  arbitrary third-party code.
- **PostgreSQL HA cluster**: the database is deployed with replication/
  failover per [enterprise-roadmap.md](enterprise-roadmap.md) Phase 15, not
  a single unreplicated instance. Phase 15 is now implemented and drilled
  ([disaster-recovery.md](disaster-recovery.md)) — this assumption is a
  documented, tool-agnostic deployment-layer topology (streaming
  replication + an operator-chosen orchestration tool), not something
  TaskForge itself builds or verifies at runtime.
- **Single-region initially**: no multi-region durability or cross-region
  read-after-write guarantee is claimed.
- **No hostile third-party worker-code guarantee**: running worker code
  supplied by an untrusted third party is explicitly **not** a supported
  configuration of this profile (see "Worker Trust" below for what would
  need to change if it became one).
- **No multi-region guarantee**: consistent with [vision.md](vision.md)'s
  existing non-goals.

A severity marked P0 below is P0 **within this profile** unless stated
otherwise. Where a threat is only P0 outside this profile (e.g., a fully
untrusted/third-party worker fleet), that condition is stated explicitly —
conflating "not yet built" with "blocks the supported profile" would
misprioritize [enterprise-roadmap.md](enterprise-roadmap.md).

## 1. Application Security (client → API server)

| Threat | Description | Status today | Severity if unaddressed |
|---|---|---|---|
| **Malicious/buggy job submitter** | Any client that can reach `POST /jobs` can submit arbitrary `job_type`/`payload` values, including job types the caller has no business submitting. | **Partially addressed (Phase 12).** Every route now requires a valid API key carrying the `jobs` scope (`internal/api/auth.go`), so an *unauthenticated* client can no longer submit anything. An authenticated caller may still submit any `job_type` — per-`job_type` authorization is an explicit non-goal (no policy engine; [enterprise-roadmap.md](enterprise-roadmap.md) Phase 12 "Explicit non-scope"). | P2 after Phase 12 (was P0): the open-to-anyone case is closed; per-`job_type` restriction remains deferred |
| **Oversized payload** | `payload` is an unconstrained `jsonb` column ([data-model.md](data-model.md)); there is no documented request body size cap in `internal/api`. | Not found in code or docs. PostgreSQL's own `jsonb`/row-size limits (~1GB TOAST-able, practically far lower before performance degrades) are the only backstop. | P1 — resource exhaustion / DB bloat vector |
| **Queue flooding** | An unauthenticated or unthrottled caller can submit unbounded volumes of jobs, exhausting the claimable-job index (`idx_jobs_claimable`), connection pool, or disk. | No rate limiting or quota anywhere (confirmed by code search: no matches for "rate.limit"/"ratelimit"/"quota"). | P0 for any internet-facing or multi-caller deployment |
| **Abusive retry workload** | A caller (or a buggy handler) that always fails retryably consumes retry budget and backoff-scheduling churn repeatedly, at up to `max_attempts` per job, with `max_attempts` itself caller-supplied at submission ([data-model.md](data-model.md): `max_attempts` "Caller-configurable at submission"). | **Verified and settled (Phase 11, [enterprise-roadmap.md](enterprise-roadmap.md)).** No arbitrary product/operational cap on caller-supplied `max_attempts` exists in `internal/job.ValidateSubmission` (the single validation path every submission entry point — `POST /jobs`, `POST /workflows`, and the `txenqueue` transactional API — now shares); a storage-representability bound (`job.MaxRepresentableMaxAttempts`, matching the `jobs.max_attempts` `INTEGER` column's range) is enforced instead, closing a separate API-contract hole where a Go-valid `int` could exceed what PostgreSQL can store. This is a deliberate policy decision, not a gap left unverified — but corrected (Phase 11 audit finding): a large `max_attempts` amplifies retry-scheduling churn for that one job and cannot bypass TF-INV-006's ceiling, but **does** consume shared worker/database/claim-query resources other jobs and tenants contend for, so this P2 is accepted as an explicit, deferred risk for Phase 13's workload-governance work, not claimed harmless to other work. See [retry-semantics.md](retry-semantics.md) "Open Questions." | P2, accepted as a deferred risk pending Phase 13 governance — a large `max_attempts` amplifies shared-resource contention (though it does not bypass TF-INV-006's per-job ceiling) |
| **Tenant starvation** | One caller's job volume competes for the same `idx_jobs_claimable` index and worker pool as every other caller's, with only a single `priority smallint` column for differentiation and no fairness guarantee (Phase 5 explicitly disclaims strict fairness). | **Still open, but now expressible (Phase 12).** A principal concept exists (`jobs.principal_id`, NOT NULL), so tenants are now distinguishable — but nothing acts on that distinction: there is still no per-tenant fairness, quota, or rate limit. Phase 12 deliberately builds the identity Phase 13 needs, not the governance itself. | P0 the moment more than one logical tenant shares a deployment — unchanged by Phase 12, and explicitly deferred to Phase 13 |
| **Unauthorized job inspection** | `GET /jobs/{id}` returns full job state, payload, and error detail to any caller who knows or guesses a UUID. | **Addressed (Phase 12).** The read is principal-scoped inside the SQL statement itself (`internal/store.GetByID`'s `WHERE id = $1 AND ($2::boolean OR jobs.principal_id = $3)`), not filtered in the handler. Another principal's job is `ErrNotFound` — indistinguishable from a nonexistent id. The one exception is an `admin`-kind principal. | P3 (residual: an admin principal can read any tenant's payload, by design) |
| **Unauthorized cancellation** | `POST /jobs/{id}/cancel` and `POST /workflows/{id}/cancel` are reachable by any caller who knows the ID, with no ownership check. | **Addressed (Phase 12), in the mutating statement rather than before it.** This endpoint's very first action is a *mutating* `UPDATE`, so a handler-level "check owner, then call the store" design would have let an unauthorized cancel flip `cancel_requested` on another principal's `RUNNING` job before any check ran. The ownership predicate is therefore part of every cascade step's own `WHERE` clause; an unauthorized cancel matches no step and mutates zero rows — proved by direct database reads in `internal/api`'s and `internal/store`'s Phase 12 tests, not by the `404` response alone. | P3 (residual: same admin exception) |

**Avoid unnecessary existence disclosure** — **implemented in Phase 12, by
construction rather than by convention**: `GET /jobs/{id}`,
`POST /jobs/{id}/cancel`, and their workflow equivalents return the
byte-identical `404` body for "does not exist" and "exists but you don't own
it." This is not a matching-strings agreement between two handler branches:
the store layer's scoped `WHERE` clause makes the two cases the same
`ErrNotFound` through the same code path, so there is no branch that could
drift apart. A distinguishing `403` would let a caller enumerate which UUIDs
belong to other tenants' real jobs. The Phase 12 tests compare the actual
response bytes for both cases rather than asserting on a status code.

Scope mismatch is the deliberate exception: a credential that is valid but
lacks the required scope gets `403`, not `404`. That is not an
existence-disclosure risk — there is no per-resource id at stake on a
scope check — and a clear `403` is far more operable for a caller
misconfigured with the wrong key type.

**Authentication failures are uniform**: all six internal rejection reasons
(malformed credential, unknown `key_id`, revoked key, expired key, wrong
secret, revoked principal) produce the identical `401` body, so a caller
cannot use response differences to enumerate valid `key_id`s. The reason
survives only server-side, as a `taskforge_auth_failures_total{reason}`
label and a structured log field — never in the response, and never
containing any part of the credential.

### API Key Design Requirements (for Phase 12)

Whatever authentication mechanism Phase 12 ships must, at minimum:

- Never store a raw credential — store only a hash or HMAC of it.
- Support a non-secret key identifier, so a specific key can be looked up,
  rotated, or revoked without scanning by comparing raw secrets.
- Support rotation with an overlap window (old and new key both valid for a
  bounded period), so rotation does not require a synchronized cutover.
- Support revocation that takes effect without a full redeploy (a lookup
  table, not a hardcoded value).
- Compare secrets in constant time on any path that ever compares a raw
  secret directly (not applicable if only a hash/HMAC digest is compared,
  but load-bearing if any raw-secret comparison path exists).
- Never let a raw credential enter a metric label, log field, or trace
  attribute — the same cardinality/sensitivity discipline
  [observability.md](observability.md) already applies to `job_id` and
  `Idempotency-Key` extends to credentials.

## 2. Worker Trust

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **Forged worker identity** | `lease_owner` is an arbitrary, unauthenticated string supplied by whatever process calls the claim query. Nothing cryptographically binds a `lease_owner` value to a specific process or credential. | Explicitly declared out of scope in [failure-model.md](failure-model.md): "A worker that intentionally forges its `lease_owner`/`lease_generation` to bypass fencing is not defended against in v1; workers are assumed to be trusted internal processes." | P0 the instant TaskForge runs third-party or less-trusted worker code; P3 (accepted, documented) for a fully first-party worker fleet |
| **Compromised worker** | A compromised worker process has direct database credentials and can read/write any job row, including other jobs' payloads, or claim jobs outside its intended `job_type` set (no `job_type`-scoped worker credential exists, and no separate least-privilege PostgreSQL role distinguishes API-server access from worker access). | No worker-scoping mechanism exists. Workers connect with one shared, presumably admin-or-near-admin-level database role. | P0 the moment worker code provenance/trust is not fully controlled by the operator (i.e., outside the Enterprise Deployment Profile above); **within** the assumed trusted-first-party-worker-fleet profile, this is a P2 hardening item (least-privilege roles), not a blocker — see note below |
| **Replayed requests** | A captured, valid API request (e.g., a cancel call) could be replayed by an attacker with network visibility, since there is no request signing, nonce, or freshness check beyond the already-idempotent nature of the underlying operations. | Idempotency (TF-INV-008) actually *helps* here for `POST /jobs` — a replayed identical submission is a safe no-op by design — but `POST /jobs/{id}/cancel` has no such protection and none is needed for its own idempotence, only for authorization. | P1, secondary to the missing authZ layer above |

**Least-privilege roles, and what they do and do not buy**: Phase 12
should distinguish API-caller identity from worker identity, and provision
**separate PostgreSQL roles for the API server and for worker processes**
where practical, each granted only the statements/tables it actually needs
(e.g., workers do not need `DELETE` on `jobs` if Phase 13's retention
cleanup runs under its own, narrower-scoped role). A different password
alone does **not** bound a compromised worker if both roles still hold
broad raw-table `SELECT`/`UPDATE`/`DELETE` privileges — the containment
value comes entirely from the privilege difference, not the credential
difference, and this document does not claim otherwise. Fully isolating
untrusted worker code (so a compromised worker cannot read other tenants'
payloads at all) would require an architectural boundary this review has
not designed — e.g., a worker-facing gateway service, or a PostgreSQL
stored-function API that grants workers `EXECUTE` but no direct table
access — and is explicitly **deferred**, not claimed as solved by Phase
12's least-privilege roles.

## 3. Database Trust

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **SQL injection** | Handwritten or string-concatenated SQL could allow injection through `job_type`, `payload`, or `idempotency_key` fields. | This review's code inspection of `internal/store` found parameterized queries via `pgx` (`$1`, `$2`, ... placeholders) throughout the claim/complete/heartbeat/idempotency/workflow paths; no evidence of string-built SQL incorporating request-controlled values was found. **This is a positive finding**, not a gap — but it was verified by inspection during this review, not by an automated SAST/CodeQL gate in CI, which does not exist. | P3 today (no evidence of the vulnerability); P1 to leave unverified by tooling going forward |
| **Sensitive data in `payload`/`result_metadata`** | `payload` and `result_metadata` are opaque `jsonb` columns TaskForge never inspects or redacts ([data-model.md](data-model.md): "Opaque to TaskForge; interpreted by the job handler"). A caller could store secrets, PII, or credentials in either column with no TaskForge-level warning, masking, or encryption-at-rest beyond whatever PostgreSQL itself provides. | By design — TaskForge deliberately does not interpret payloads. This is architecturally correct (a job engine should not need to understand payload semantics) but means TaskForge currently has **no data-classification or retention policy** for what operators put in it. | P1 — an operational/policy gap, not a code defect |
| **Sensitive logs** | Structured logging fields are documented in [observability.md](observability.md); the raw `Idempotency-Key` value is explicitly never logged (only `had_idempotency_key`), and job `payload` contents are never logged (confirmed: retry-scheduled/dead-letter log lines carry `error_class`/`error_message`, not payload). | This is a genuine, verified positive: the logging discipline documented in observability.md was cross-checked against actual log call sites during this review and matches. | P3 (well-handled) |
| **Database credential handling** | Connection configuration is read via `internal/config` (environment variables, per `.env.example`). No secrets-manager integration (Vault, AWS Secrets Manager, etc.) exists or is expected to at this stage. | Standard practice for this project's current scope; a gap only relative to enterprise secret-rotation expectations. | P2 |
| **Backup/replica artifact confidentiality** (Phase 15) | A `pg_basebackup` output or WAL archive segment contains the **entire** database, including `payload`/`result_metadata` (the opaque, potentially sensitive columns in the row above) and `api_keys.secret_hash` (an HMAC digest, not a raw secret — recoverable only with the out-of-database pepper). | **Extends, does not newly create, the finding above.** A backup artifact or a standby's own data directory requires **at least the same** access-control and encryption-at-rest discipline as the live database itself — this is documentation, not a new control; TaskForge still does not encrypt or classify `payload` at rest, and Phase 15 does not change that. See [disaster-recovery.md](disaster-recovery.md) §10. | P1 — same severity and same reasoning as the row above, now stated explicitly for backups/replicas too |

## 4. Transport Security

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **Plaintext HTTP for the API** | No TLS termination, no HSTS, no HTTPS enforcement exists in `internal/api` or `cmd/api`. | **Decided, not implemented natively (Phase 12, OD-7).** TaskForge deliberately terminates no TLS: termination is an external reverse-proxy/load-balancer responsibility, documented as a **required** deployment boundary for any deployment reachable by an untrusted network. `cmd/api` continues to speak plaintext HTTP behind that boundary and makes no transport-security claim beyond it. Correspondingly, `internal/api` reads **no** proxy-injected header (`X-Forwarded-For`, `X-Forwarded-Proto`, `X-Real-IP`, or any other) for any security decision — asserted by a source-scanning test, so a later change cannot quietly start trusting one. | P1 (was P0): the boundary is now specified and enforced-by-documentation rather than undefined, but TaskForge itself cannot verify the operator actually deployed the proxy |
| **Plaintext PostgreSQL connections** | No `sslmode=require`/`verify-full` configuration was found; `pgx`'s default connection behavior depends entirely on the connection string supplied via `internal/config`, which is not itself enforced to request TLS. | **Reviewed in Phase 12 and deliberately left as a documented deployment requirement, not a code check.** `sslmode=verify-full` is required for the enterprise reference deployment and is stated in `.env.example` and `cmd/api`'s package comment — but TaskForge does **not** inspect or reject a connection string, because local and development profiles are explicitly exempt (this document's own framing, and docs/phase-12-plan.md §9), and a hard check would break them. So this remains unenforced in code, with the same honesty as before: it is a deployment-review item, and there is **no TaskForge-level guardrail or warning**. Phase 12 correspondingly ships no TLS-connection test — a real `verify-full` proof needs a TLS-configured PostgreSQL with a trusted CA, which this project's test harness (embedded PostgreSQL, `sslmode=disable`) does not provide. | P0 for any deployment where the API/worker-to-database network path is not fully trusted — unchanged by Phase 12 |
| **No mTLS between workers and the API/database** | Workers authenticate to PostgreSQL with whatever credential the deployment configures; there is no TaskForge-specific worker certificate or mTLS scheme. | Same root cause as "forged worker identity" above. | P1, coupled to the worker-trust gap |

**Enterprise reference deployment requirement**: `sslmode=require` alone
verifies only that the connection is encrypted — it does **not** verify
server identity, so it does not defend against a network-level
man-in-the-middle presenting a different PostgreSQL server. For the
enterprise reference deployment, [enterprise-roadmap.md](enterprise-roadmap.md)
Phase 12 requires `sslmode=verify-full` (plus a trusted CA and hostname
validation), not merely `sslmode=require`. Simpler development/local
profiles (e.g., `sslmode=require` against a same-host database) may remain
as documented, lower-assurance defaults for that context only.

**Deployment boundary summary after Phase 12** (all four are operator
obligations TaskForge states but cannot verify from inside its own process):

1. Terminate TLS at a reverse proxy or load balancer in front of `cmd/api`
   for any deployment reachable by an untrusted network, and keep the
   proxy-to-TaskForge segment private. TaskForge trusts no forwarded header
   for any security decision, so the proxy cannot be used to assert
   identity either.
2. Use `sslmode=verify-full` on `TASKFORGE_DATABASE_URL` for the enterprise
   reference deployment.
3. Point `cmd/api` at the `taskforge_api` role and `cmd/worker` at
   `taskforge_worker` (`deploy/postgres-roles.sql`). Both processes start
   normally under those roles against an already-migrated database
   (`migrate.Up` applies nothing and returns); applying a *pending*
   migration is a separate, owner-privileged step that must run first,
   because neither least-privilege role may change the schema — a process
   started under one against a not-yet-migrated database fails loudly with
   SQLSTATE 42501 rather than starting against a schema it was not built
   for.
4. Restrict `cmd/worker`'s `TASKFORGE_METRICS_ADDR` listener (default
   `:9090`) to a private, operator-controlled network. It is unauthenticated
   by design (§5) and exposes operational volume to anyone who can reach
   it.

## 5. Operational Security

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **`GET /metrics` exposure** | The Prometheus endpoint is unauthenticated; metric labels are cardinality-safe (audited, per observability.md) but the endpoint itself reveals operational volume (jobs submitted/completed/dead-lettered rates) to anyone who can reach it. | **Addressed for `cmd/api` only (Phase 12).** The API server's `GET /metrics` now requires a credential carrying the `metrics` scope, mounted through the same middleware as every other route; an ordinary job-submission key does **not** grant metrics access, and a metrics-only key does not grant job submission. **`cmd/worker`'s standalone listener (`TASKFORGE_METRICS_ADDR`, default `:9090`) remains completely unauthenticated** and serves the same registry — see the note below. | P2 (was P1): closed on the API surface, open on the worker listener, which is a network-exposure obligation rather than a code gap |
| **No audit log of who cancelled/submitted what** | Because there is no principal/identity concept, structured logs cannot record "who" performed an action, only "what worker"/"what job" — there is no `actor` field anywhere in the documented log vocabulary. | **Addressed (Phase 12).** Every submit and cancel log line (`submission`, `duplicate_submission_hit`, `cancellation_requested`, `workflow_submitted`, `workflow_cancel_requested`) carries an `actor` field holding the authenticated principal's ID — a non-secret identifier, never the credential that proved it. A log-output test audits every success and failure path for the raw secret. | Closed for submit/cancel; worker-side transitions still attribute to `worker_id`, which is an unauthenticated self-reported string (see §2) |
| **No incident-response runbook** | No documented procedure exists for revoking a compromised worker's database credential, rotating it, or auditing what a compromised worker touched. | Not present; not expected at this project stage, but a gap relative to enterprise operational expectations. | P2 |
| **`last_used_at` telemetry is unbounded per authenticated request** | On every successful credential verification, `internal/principal.Store.touchLastUsed` starts a bare goroutine that takes a second connection from the shared pool to write `api_keys.last_used_at`. `cmd/api` sets no `SetMaxOpenConns`, so Phase 12 roughly **doubles** the per-request connection demand of the authenticated path against an unbounded pool; a request burst can exhaust PostgreSQL's `max_connections` and take the request path down with it. Phase 12 ships no rate limiting to bound the burst (deliberate — see §2). | **Known follow-up, NOT fixed in Phase 12.** The write is genuinely best-effort and off the critical path (errors discarded, nothing reads `last_used_at` as a correctness signal), but the *concurrency* of it is unbounded and that is an availability risk, not a security one. Bounding it — a semaphore, a single background writer, or a `SetMaxOpenConns` ceiling on `cmd/api`'s pool — is deferred; it is not claimed to be solved. Related pre-existing gap: no production connection-pool tuning guidance exists (README.md, Phase 5 limitations). | P2 — self-inflicted availability risk under load, no confidentiality/integrity impact |
| **A database outage during credential verification is reported as `401`** | `internal/api`'s middleware maps every `Store.Verify` error to the uniform `401`, including a wrapped database error from the `api_keys` lookup. A caller therefore cannot distinguish "your credential is bad — do not retry" from "the credential store is unreachable — retry later", and well-behaved clients will stop retrying during a recoverable incident. | **Known follow-up, NOT fixed in Phase 12.** It fails *closed*, which is the correct security posture, and the distinction does survive server-side: `principal.FailureReason` classifies an unrecognised error as `internal_error` rather than any credential-specific reason, so `taskforge_auth_failures_total{reason="internal_error"}` is the operator's signal that this is an outage and not credential stuffing. Mapping it to `503` instead is deferred, not done — the uniform-`401` rule does not forbid it, since an infrastructure failure discloses nothing about any credential. | P2 — operability/observability defect, not an authentication bypass |

**The worker metrics listener is not, and cannot be, credential-authenticated
in Phase 12** — stated explicitly rather than left to be inferred from the row
above, which an independent review found overstated as "Closed".

`cmd/worker` exposes the same Prometheus registry on `TASKFORGE_METRICS_ADDR`
(default `:9090`) with no credential check. That is **not** an oversight that
could be fixed by reusing `internal/api`'s middleware: authenticating it with
an API key would require the worker process to read `principals`/`api_keys`,
and OD-3/G4 forbid exactly that — `deploy/postgres-roles.sql` grants
`taskforge_worker` no access to the credential tables at all, and
`TestPostgresRoleScript_AppliesCleanlyAndGrantsTheDocumentedPrivileges`
asserts it. Giving the worker credential-table access to protect a metrics
endpoint would trade a disclosure risk for a trust-boundary violation, which
is a bad trade.

The control is therefore a **deployment obligation, in the same category as
the TLS proxy boundary (§4)**: bind or firewall `TASKFORGE_METRICS_ADDR` to a
private, operator-controlled network reachable only by the Prometheus
scraper, never to an untrusted network. TaskForge cannot verify this from
inside the process and does not claim to. Closing it in code would need a
mechanism this phase does not build — a shared-secret scrape token, mTLS at
the listener, or moving worker metrics to a push gateway — and is not in
Phase 12's scope.

## 6. Supply-Chain Security

**Resolved by docs/enterprise-roadmap.md Phase 10** — see
[docs/supply-chain-security.md](supply-chain-security.md) for the current,
authoritative design (this section's table is retained below as the
original review finding it responds to, for historical/audit traceability;
each row's "Status today" is now out of date and superseded by the note
that follows it).

| Threat | Description | Status at original review | Severity at original review |
|---|---|---|---|
| **Dependency compromise** | A compromised transitive or direct dependency (e.g. a malicious release of `pgx`, `google/uuid`, or `prometheus/client_golang`) would be pulled in on the next `go get`/`go mod tidy` with no automated detection. | No dependency vulnerability scanning in CI (no `govulncheck`, Dependabot config, Snyk, or Trivy step found). `go.sum` exists and provides checksum integrity for what is already pinned, which is a real (if partial) mitigation against silent tampering of already-vetted versions. | P0 — no detection mechanism for a newly-disclosed CVE in an existing pinned dependency, and no automated PR to bump it |
| **Artifact tampering** | No build provenance or artifact signing exists — there is no CI step producing a signed, attestable build artifact (e.g. via GitHub Artifact Attestations/SLSA), and no container image is built at all (only a local-dev `docker-compose.yml`, no `Dockerfile`). | Confirmed absent. | P1 — no current release/distribution mechanism to attest, but a blocker the moment one exists |
| **No SBOM** | No CycloneDX/SPDX file exists anywhere in the repository. | Confirmed absent by file search. | P1 |
| **Small, reputable direct dependency surface** | Direct dependencies are `pgx/v5`, `google/uuid`, `prometheus/client_golang`, `prometheus/client_model`, `stretchr/testify` (test-only), and `fergusstrange/embedded-postgres` (test-only). No web framework, no auth library, no third-party observability SDK. | This is a genuine mitigating factor — a small, well-known dependency surface has less attack surface than a sprawling one, independent of whether scanning exists. | Positive finding, does not offset the missing scanning/SBOM/attestation gaps above |

**Now (post-Phase 10)**: `govulncheck` runs on every PR and weekly
(closing the "Dependency compromise" row's P0); `actions/attest` produces
signed build-provenance and per-binary SBOM attestations for release
binaries (closing "Artifact tampering"); `cyclonedx-gomod` generates one
build-constrained CycloneDX SBOM per released binary at release time
(closing "No SBOM"). The dependency surface itself is unchanged (still
small and reputable) — it is now actively scanned rather than relying on
surface size alone. No release has actually been tagged yet, so no live
attestation/SBOM has been verified end-to-end against a real artifact —
see docs/supply-chain-security.md "Limitations."

## 7. Tenant / Idempotency Interaction

Phase 12 introduced a principal/tenant concept, so TaskForge's
submission-idempotency contract (TF-INV-016) could not be assumed to still
hold as-is. The row below records the finding and how it was resolved; the
constraint is now `UNIQUE(principal_id, job_type, idempotency_key)`:

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **Cross-tenant idempotency-key collision** | Two different tenants that happen to choose the same `job_type` and the same `Idempotency-Key` value would collide on today's global `UNIQUE(job_type, idempotency_key)` constraint — tenant B's submission could be silently treated as a duplicate of tenant A's, or incorrectly rejected/short-circuited. | **Addressed (Phase 12), in the same migration set that introduced principals.** Migration `0009` created `idx_jobs_idempotency_scoped` on `(principal_id, job_type, idempotency_key)` and `0010` retired migration 0001's global `idx_jobs_idempotency_key`, the scoped index being created only *after* `principal_id` had converged to `NOT NULL` in that same migration — so there is no NULL-widening window at any point. The conflict-recovery re-read (`Store.GetByIdempotencyKey`) is principal-scoped too, which is load-bearing: a globally-scoped read there would have handed tenant B's job back to tenant A. | Closed |
| **Workflow/replay ownership scope** | The same reasoning applies to any future workflow-level idempotency key and to DLQ-replay operations (Phase 16): a replay or resubmission keyed only by `job_type`/`idempotency_key`/`job_id`, without a tenant scope, could cross a tenant boundary. | Workflows have no idempotency key today (explicit deferral, [workflows.md](workflows.md)); DLQ replay does not exist yet (Phase 16). | Design requirement for both, before either ships |

**Required contract** — **satisfied by Phase 12**: the uniqueness constraint
is now `(principal_id, job_type, idempotency_key)`, and it landed in the
same migration set as the principal concept itself, not as a follow-up:
shipping principals first and re-scoping idempotency second would have left
a real cross-tenant collision window open in production. There is no release
in which principals exist but idempotency is still globally scoped.
The same ownership scoping must be designed into any future workflow-level
idempotency key and into Phase 16's DLQ-replay `retried_from` linkage,
before either ships.

## Summary by Category

| Category | Verdict |
|---|---|
| Application security | **Authentication and ownership authorization implemented (Phase 12); governance still absent.** Every route (all six job/workflow endpoints on both the `/v1` and legacy surfaces, plus `GET /metrics`) requires a `Bearer <key_id>.<secret>` credential, deny-by-default by router construction. Ownership is enforced in SQL, not in handlers. Request body size limits landed in Phase 11. **Still absent: rate limiting and per-tenant quota — explicitly deferred to Phase 13, with observability (`taskforge_auth_failures_total{reason}`) provided in their place, which is a signal for an operator, not a control.** |
| Worker trust | **Unchanged by Phase 12, and deliberately so (OD-3).** Worker identity remains a PostgreSQL role credential verified by PostgreSQL itself — an infrastructure/database trust boundary, in a different layer, checked by a different system than the application/API boundary Phase 12 secures. There is no application-level `worker` principal kind, and migration `0005`'s `CHECK` constraint forbids one. Phase 12 adds least-privilege role provisioning (`deploy/postgres-roles.sql`), which bounds a compromised worker's blast radius but does **not** contain hostile worker code. Acceptable only for a fully first-party, fully trusted worker fleet. |
| Database trust | **Comparatively strong.** Parameterized queries throughout (verified by inspection, not by automated SAST), payload opacity by design, disciplined non-sensitive logging (verified). |
| Transport security | **Boundary specified, not natively implemented (Phase 12, OD-7).** TaskForge terminates no TLS; an external reverse proxy or load balancer is a documented, required deployment boundary, and no forwarded header is trusted for any security decision. The database leg requires `sslmode=verify-full` for the enterprise reference deployment. TaskForge cannot verify from inside the process that the operator actually deployed the proxy — that remains a deployment-review item, not a code-enforced guarantee. |
| Operational security | **API-server metrics auth and the audit trail implemented (Phase 12); worker metrics listener and runbook still open.** `cmd/api`'s `GET /metrics` requires the `metrics` scope; every submit/cancel log line carries an `actor`. Credential lifecycle is operator tooling (`cmd/taskforge-admin`), deliberately not an HTTP API. **Still open: `cmd/worker`'s `TASKFORGE_METRICS_ADDR` listener is unauthenticated by design** (authenticating it would require worker access to the credential tables, which OD-3/G4 forbid) and must be network-restricted by the operator — see §5's note. **Still absent: an incident-response runbook** (revoking a compromised credential is now mechanically possible — `taskforge-admin revoke-key`/`revoke-principal`, effective on the next request with no redeploy — but the procedure itself is not written down). |
| Supply-chain security | **Implemented (Phase 10).** `govulncheck` (PR + scheduled), CodeQL, Dependabot, dependency-review, SHA-pinned least-privilege CI, and a release process producing an SBOM, checksums, and GitHub Artifact Attestations — see [docs/supply-chain-security.md](supply-chain-security.md). Branch-protection "required check" enforcement and the secret-scanning/push-protection toggle are documented but not verifiable from a non-interactive implementation environment. |

## What This Document Does Not Do

Per this review's scope, this document does not propose or implement any
control; it records which controls exist and which do not. Rows marked
"(Phase 12)" above describe controls that have since been implemented
(see [phase-12-plan.md](phase-12-plan.md)); every other row still describes
a gap. See [enterprise-roadmap.md](enterprise-roadmap.md) for how these
findings translate into a prioritized, sequenced set of phases, and
[enterprise-readiness.md](enterprise-readiness.md) Section 3 for how these
map to P0/P1/P2/P3 severity across the full gap analysis (not just security).
