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

## 1. Application Security (client → API server)

| Threat | Description | Status today | Severity if unaddressed |
|---|---|---|---|
| **Malicious/buggy job submitter** | Any client that can reach `POST /jobs` can submit arbitrary `job_type`/`payload` values, including job types the caller has no business submitting. | No authorization check exists — confirmed by code inspection (no auth code in `internal/api`). | P0 for any shared/multi-caller deployment |
| **Oversized payload** | `payload` is an unconstrained `jsonb` column ([data-model.md](data-model.md)); there is no documented request body size cap in `internal/api`. | Not found in code or docs. PostgreSQL's own `jsonb`/row-size limits (~1GB TOAST-able, practically far lower before performance degrades) are the only backstop. | P1 — resource exhaustion / DB bloat vector |
| **Queue flooding** | An unauthenticated or unthrottled caller can submit unbounded volumes of jobs, exhausting the claimable-job index (`idx_jobs_claimable`), connection pool, or disk. | No rate limiting or quota anywhere (confirmed by code search: no matches for "rate.limit"/"ratelimit"/"quota"). | P0 for any internet-facing or multi-caller deployment |
| **Abusive retry workload** | A caller (or a buggy handler) that always fails retryably consumes retry budget and backoff-scheduling churn repeatedly, at up to `max_attempts` per job, with `max_attempts` itself caller-supplied at submission ([data-model.md](data-model.md): `max_attempts` "Caller-configurable at submission"). | No upper bound on caller-supplied `max_attempts` was found in the API validation path during this review's code inspection; this should be verified against `internal/api/handlers.go` before being treated as settled. | P2 — a large `max_attempts` amplifies, but does not bypass, TF-INV-006's ceiling |
| **Tenant starvation** | One caller's job volume competes for the same `idx_jobs_claimable` index and worker pool as every other caller's, with only a single `priority smallint` column for differentiation and no fairness guarantee (Phase 5 explicitly disclaims strict fairness). | No tenancy concept exists at all — there is nothing to starve *between*, because there is nothing to distinguish callers. | P0 the moment more than one logical tenant shares a deployment |
| **Unauthorized job inspection** | `GET /jobs/{id}` returns full job state, payload, and error detail to any caller who knows or guesses a UUID. | No authorization check exists. UUIDv4-style IDs make guessing impractical, but "impractical to guess" is not an access control. | P0 if payloads or error messages ever contain sensitive data |
| **Unauthorized cancellation** | `POST /jobs/{id}/cancel` and `POST /workflows/{id}/cancel` are reachable by any caller who knows the ID, with no ownership check. | No authorization check exists. | P0 — a caller can cancel another caller's work |

## 2. Worker Trust

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **Forged worker identity** | `lease_owner` is an arbitrary, unauthenticated string supplied by whatever process calls the claim query. Nothing cryptographically binds a `lease_owner` value to a specific process or credential. | Explicitly declared out of scope in [failure-model.md](failure-model.md): "A worker that intentionally forges its `lease_owner`/`lease_generation` to bypass fencing is not defended against in v1; workers are assumed to be trusted internal processes." | P0 the instant TaskForge runs third-party or less-trusted worker code; P3 (accepted, documented) for a fully first-party worker fleet |
| **Compromised worker** | A compromised worker process has direct database credentials and can read/write any job row, including other jobs' payloads, or claim jobs outside its intended `job_type` set (no `job_type`-scoped worker credential exists). | No worker-scoping mechanism exists. Workers connect with one shared, presumably admin-or-near-admin-level database role. | P0 for any environment where worker code provenance/trust is not fully controlled |
| **Replayed requests** | A captured, valid API request (e.g., a cancel call) could be replayed by an attacker with network visibility, since there is no request signing, nonce, or freshness check beyond the already-idempotent nature of the underlying operations. | Idempotency (TF-INV-008) actually *helps* here for `POST /jobs` — a replayed identical submission is a safe no-op by design — but `POST /jobs/{id}/cancel` has no such protection and none is needed for its own idempotence, only for authorization. | P1, secondary to the missing authZ layer above |

## 3. Database Trust

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **SQL injection** | Handwritten or string-concatenated SQL could allow injection through `job_type`, `payload`, or `idempotency_key` fields. | This review's code inspection of `internal/store` found parameterized queries via `pgx` (`$1`, `$2`, ... placeholders) throughout the claim/complete/heartbeat/idempotency/workflow paths; no evidence of string-built SQL incorporating request-controlled values was found. **This is a positive finding**, not a gap — but it was verified by inspection during this review, not by an automated SAST/CodeQL gate in CI, which does not exist. | P3 today (no evidence of the vulnerability); P1 to leave unverified by tooling going forward |
| **Sensitive data in `payload`/`result_metadata`** | `payload` and `result_metadata` are opaque `jsonb` columns TaskForge never inspects or redacts ([data-model.md](data-model.md): "Opaque to TaskForge; interpreted by the job handler"). A caller could store secrets, PII, or credentials in either column with no TaskForge-level warning, masking, or encryption-at-rest beyond whatever PostgreSQL itself provides. | By design — TaskForge deliberately does not interpret payloads. This is architecturally correct (a job engine should not need to understand payload semantics) but means TaskForge currently has **no data-classification or retention policy** for what operators put in it. | P1 — an operational/policy gap, not a code defect |
| **Sensitive logs** | Structured logging fields are documented in [observability.md](observability.md); the raw `Idempotency-Key` value is explicitly never logged (only `had_idempotency_key`), and job `payload` contents are never logged (confirmed: retry-scheduled/dead-letter log lines carry `error_class`/`error_message`, not payload). | This is a genuine, verified positive: the logging discipline documented in observability.md was cross-checked against actual log call sites during this review and matches. | P3 (well-handled) |
| **Database credential handling** | Connection configuration is read via `internal/config` (environment variables, per `.env.example`). No secrets-manager integration (Vault, AWS Secrets Manager, etc.) exists or is expected to at this stage. | Standard practice for this project's current scope; a gap only relative to enterprise secret-rotation expectations. | P2 |

## 4. Transport Security

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **Plaintext HTTP for the API** | No TLS termination, no HSTS, no HTTPS enforcement exists in `internal/api` or `cmd/api`. | Confirmed by code search: zero TLS configuration anywhere in `internal/` or `cmd/`. | P0 for any deployment where clients and the API server are not on a fully trusted private network |
| **Plaintext PostgreSQL connections** | No `sslmode=require`/`verify-full` configuration was found; `pgx`'s default connection behavior depends entirely on the connection string supplied via `internal/config`, which is not itself enforced to request TLS. | Not enforced in code; entirely a deployment-time/operator responsibility today, with no TaskForge-level guardrail or warning. | P0 for any deployment where the API/worker-to-database network path is not fully trusted |
| **No mTLS between workers and the API/database** | Workers authenticate to PostgreSQL with whatever credential the deployment configures; there is no TaskForge-specific worker certificate or mTLS scheme. | Same root cause as "forged worker identity" above. | P1, coupled to the worker-trust gap |

## 5. Operational Security

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **`GET /metrics` exposure** | The Prometheus endpoint is unauthenticated; metric labels are cardinality-safe (audited, per observability.md) but the endpoint itself reveals operational volume (jobs submitted/completed/dead-lettered rates) to anyone who can reach it. | No authentication on `/metrics`. | P1 — information disclosure, not data disclosure (no `job_id`/payload ever appears in a metric label, verified) |
| **No audit log of who cancelled/submitted what** | Because there is no principal/identity concept, structured logs cannot record "who" performed an action, only "what worker"/"what job" — there is no `actor` field anywhere in the documented log vocabulary. | Absent by construction, since there is no authentication to derive an actor from. | P1, resolves automatically once authentication is added, provided the actor is then threaded into the log schema |
| **No incident-response runbook** | No documented procedure exists for revoking a compromised worker's database credential, rotating it, or auditing what a compromised worker touched. | Not present; not expected at this project stage, but a gap relative to enterprise operational expectations. | P2 |

## 6. Supply-Chain Security

| Threat | Description | Status today | Severity |
|---|---|---|---|
| **Dependency compromise** | A compromised transitive or direct dependency (e.g. a malicious release of `pgx`, `google/uuid`, or `prometheus/client_golang`) would be pulled in on the next `go get`/`go mod tidy` with no automated detection. | No dependency vulnerability scanning in CI (no `govulncheck`, Dependabot config, Snyk, or Trivy step found). `go.sum` exists and provides checksum integrity for what is already pinned, which is a real (if partial) mitigation against silent tampering of already-vetted versions. | P0 — no detection mechanism for a newly-disclosed CVE in an existing pinned dependency, and no automated PR to bump it |
| **Artifact tampering** | No build provenance or artifact signing exists — there is no CI step producing a signed, attestable build artifact (e.g. via GitHub Artifact Attestations/SLSA), and no container image is built at all (only a local-dev `docker-compose.yml`, no `Dockerfile`). | Confirmed absent. | P1 — no current release/distribution mechanism to attest, but a blocker the moment one exists |
| **No SBOM** | No CycloneDX/SPDX file exists anywhere in the repository. | Confirmed absent by file search. | P1 |
| **Small, reputable direct dependency surface** | Direct dependencies are `pgx/v5`, `google/uuid`, `prometheus/client_golang`, `prometheus/client_model`, `stretchr/testify` (test-only), and `fergusstrange/embedded-postgres` (test-only). No web framework, no auth library, no third-party observability SDK. | This is a genuine mitigating factor — a small, well-known dependency surface has less attack surface than a sprawling one, independent of whether scanning exists. | Positive finding, does not offset the missing scanning/SBOM/attestation gaps above |

## Summary by Category

| Category | Verdict |
|---|---|
| Application security | **Absent.** No authN/authZ, no rate limiting, no quota, no request size limits confirmed. |
| Worker trust | **Absent, and explicitly declared out of scope in failure-model.md.** Acceptable only for a fully first-party, fully trusted worker fleet. |
| Database trust | **Comparatively strong.** Parameterized queries throughout (verified by inspection, not by automated SAST), payload opacity by design, disciplined non-sensitive logging (verified). |
| Transport security | **Absent.** No TLS/mTLS configuration anywhere. |
| Operational security | **Absent.** No metrics auth, no actor-attributed audit trail (blocked on missing authN), no incident runbook. |
| Supply-chain security | **Absent.** No scanning, no SBOM, no attestation — partially offset by a small, reputable dependency surface and `go.sum` integrity checksums. |

## What This Document Does Not Do

Per this review's scope, this document does not propose or implement any
control. See [enterprise-roadmap.md](enterprise-roadmap.md) for how these
findings translate into a prioritized, sequenced set of phases, and
[enterprise-readiness.md](enterprise-readiness.md) Section 3 for how these
map to P0/P1/P2/P3 severity across the full gap analysis (not just security).
