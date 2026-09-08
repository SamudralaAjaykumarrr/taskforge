# Enterprise Readiness Assessment

Status: **review document**, produced during the `enterprise-readiness-review`
review. This document is a snapshot assessment against the codebase as it
exists at the end of Phase 9 (chaos/load/failure testing). It makes no
implementation changes and adds no code. Every claim here is either sourced
from an existing TaskForge document/test, or is explicitly marked as this
review's own judgment.

This document answers one question: **if a large organization asked to run
TaskForge in production today, what would we have to tell them is missing,
and what would we say is already trustworthy?**

## 1. Current TaskForge Maturity

Per [roadmap.md](roadmap.md)'s maturity labels (Foundation → Experimental →
Hardening → Stable) and the README's own per-phase labels:

| Phase | Scope | README maturity label today |
|---|---|---|
| 1 | Single-node durable job engine | Experimental* |
| 2 | Leases, heartbeats, crash recovery | Experimental* |
| 3 | Retry, backoff, DLQ | Experimental* |
| 4 | Idempotency | Experimental* |
| 5 | Concurrency hardening | Experimental* |
| 6 | Scheduling, cancellation, timeouts | Experimental* |
| 7 | Workflow/DAG execution | **Experimental** (by design — does not inherit Stable from the underlying engine) |
| 8 | Observability | **Stable** |
| 9 | Chaos, load, failure testing | **Hardening** (not Stable — the roadmap's Stable gate requires "zero invariant violations across a multi-hour chaos run"; the longest run actually executed is ~4 minutes) |

\* Phases 1–6 each individually satisfy their own roadmap-defined Hardening/
Stable technical bar (deterministic, repeated-locally-green test suites
against real PostgreSQL) but the README deliberately keeps the *project-wide*
label at Experimental because promotion to Hardening is conditioned on
"repeated CI runs" of the project's actual CI pipeline over time on the
merged branch, which has not yet happened (this branch has not been merged).
This is a self-imposed discipline, not a hidden weakness — it is the correct
call and this review does not recommend relaxing it.

**Overall, honest maturity statement**: TaskForge is a **rigorously proven
single-PostgreSQL-instance durable execution core**, with a workflow layer
of Experimental maturity built on top of it, run once through a serious
(but short and single-process) chaos/soak exercise. It is not yet an
enterprise-deployable system, because entire capability categories required
by enterprise operators — authentication, authorization, multi-tenancy,
transport security, workload isolation, HA/DR evidence, upgrade/versioning
guarantees, tracing, supply-chain hardening — do not exist in the codebase
at all today, and are explicitly out of scope in [vision.md](vision.md) and
[failure-model.md](failure-model.md) as written. That is a correct and
honest v1 scope for a distributed-systems correctness demonstration; it is
not yet the scope of enterprise infrastructure.

## 2. Strengths Already Proven by Phase 1–9 (evidence, not assertion)

These are real, load-bearing engineering achievements this review does not
want understated:

- **A precise, closed state machine.** Six job-level states, an exhaustive
  36-pair transition legality table, zero undocumented transitions
  ([execution-semantics.md](execution-semantics.md)).
- **16 numbered invariants (TF-INV-001 through TF-INV-016), each with a
  named violating example, an implementation mechanism, and at least one
  deterministic (non-flaky-by-design) test** ([invariants.md](invariants.md)).
  This is materially more rigorous than almost any open-source job queue's
  public documentation.
- **Fencing correctness under real concurrency, not simulated concurrency.**
  Phase 2 and Phase 5 both exercise fencing (TF-INV-002/003/014) against a
  real PostgreSQL instance with tens of concurrent goroutines and real
  connection pools, not mocks — including arbitrarily-late stale completions
  across 4+ lease generations.
- **Durable retry/backoff with a real property test** —
  `TestProperty_AttemptCountNeverExceedsMaxAttempts` proves TF-INV-006 across
  randomized failure sequences, not just hand-picked cases.
- **Idempotency enforced by a database constraint, not application logic**
  (TF-INV-016) — proven under 50+ simultaneous duplicate submissions
  (Phase 4's quality gate) and explicitly, honestly distinguished from
  execution-side (side-effect) idempotency, which TaskForge deliberately does
  not attempt to solve itself (ADR-0003, ADR-0004).
- **Deterministic, non-probabilistic race resolution for cancellation**
  (TF-INV-010) — both interleavings of the cancel/complete race are forced
  and asserted, not merely observed to "usually" resolve correctly.
- **Workflow/DAG execution reuses the job engine's guarantees by
  construction** rather than introducing a second, parallel correctness
  surface — dependency gating is a claim-query predicate
  (`eligible_at`), not a new execution primitive.
- **Observability with a proven cardinality discipline.** All 13 documented
  metrics exist and are independently tested against real scenarios; a real
  correctness-adjacent bug (conflating `lease_generation > 1` reclaim with
  ordinary retry) was found and fixed during this phase, with a regression
  test. Metric labels are audited to never include `job_id`, `worker_id`, or
  the raw idempotency key.
- **A genuine, real-PostgreSQL-boundary chaos harness.** `internal/chaos`'s
  primitives (`ForceExpireLease`, `PoisonAttemptInsert`,
  `TerminateBackend` via real `pg_terminate_backend()`) manipulate actual
  database state, not mocked failures. The one soak run actually executed
  (254 jobs, 15 workflows, 20 concurrent workers, 4 minutes,
  `-seed=20260908`) produced **zero invariant violations** across 17
  simulated crashes and 56 retryable outcomes. All three defects Phase 9
  found were in the *test harness*, not in `internal/store`/`internal/worker`/
  `internal/api` — a meaningfully different (better) outcome than finding
  product bugs, and worth stating plainly rather than burying.
- **Every honest limitation is written down, not discovered by an
  external reviewer.** ADR-0003's refusal to claim exactly-once execution,
  the explicit "cooperative cancellation only, no forced termination" limits
  in Phase 6, and workflows.md's four explicit deferrals (OR fan-in,
  compensation edges, dynamic modification, workflow submission idempotency)
  are all pre-declared, not findings of this review.

## 3. Enterprise Blockers (P0 — would stop a serious enterprise adoption conversation immediately)

These are evaluated against the **Enterprise Deployment Profile** defined in
[security-model.md](security-model.md) — a multi-principal API/control
plane, a *trusted first-party worker fleet*, a PostgreSQL HA cluster,
single-region initially, with no hostile-third-party-worker-code guarantee
and no multi-region guarantee. A gap that is only a blocker *outside* that
profile (e.g., running untrusted third-party worker code) is named as such
below rather than listed as an unconditional P0.

1. **No authentication or authorization of any kind.** Confirmed by direct
   code search: zero auth middleware, zero API key/token/JWT validation, no
   principal/identity concept anywhere in `internal/` or `cmd/`. `POST /jobs`,
   `POST /jobs/{id}/cancel`, `POST /workflows`, and `GET /metrics` are all
   open to anyone who can reach the port. This is explicit, declared scope
   (vision.md: "Multi-tenant isolation / auth / quota enforcement" is a
   non-goal for v1) — not a bug — but it is an absolute blocker for any
   multi-user or internet-facing deployment.
2. **No transport security.** Zero TLS/mTLS configuration anywhere in the
   codebase (confirmed by code search — the one incidental "tls" grep hit is
   a substring match on "TTLs" in a test comment, not a real reference). All
   traffic (API, worker-to-Postgres, any future admin surface) is assumed to
   run over a trusted network today.
3. **No multi-tenancy of any kind.** No tenant column, no per-tenant
   isolation, no per-tenant quota. A single noisy or malicious job submitter
   can flood the shared `jobs` table for every caller. Note: once a tenant
   concept is introduced, TaskForge's submission-idempotency uniqueness
   constraint must also become tenant-scoped — see
   [security-model.md](security-model.md) §7 — or two tenants choosing the
   same `job_type`/`Idempotency-Key` would collide.
4. **No workload governance: queues, priorities, concurrency limits, or
   rate limiting.** `priority` exists as a schema column
   ([data-model.md](data-model.md)) and is used for claim ordering, but there
   is no concept of a named queue, no per-queue or per-job-type concurrency
   cap, no submission rate limiting, and no backpressure signal returned to
   an over-eager caller beyond ordinary HTTP error codes if the database
   itself falls over. Phase 5 explicitly disclaims proving "strict FIFO
   fairness across priorities/ages under adversarial scheduling."
5. **No PostgreSQL HA, backup, PITR, or disaster-recovery evidence.**
   failure-model.md explicitly places "PostgreSQL primary failure/failover"
   out of scope for v1 ("If the database is down, TaskForge is down for
   writes"). There is no documented backup/restore procedure, no tested PITR
   runbook, and no failover drill anywhere in the repository.
6. **No supply-chain hardening.** No SBOM, no dependency vulnerability
   scanning (no `govulncheck`, Trivy, Snyk, or CodeQL step), no artifact
   signing/provenance/attestation, no Dockerfile (only a `docker-compose.yml`
   for a stock local Postgres). CI (`.github/workflows/ci.yml`) runs
   `gofmt`/`go vet`/`go build`/`go test`/`go test -race` only — five checks,
   no security gate.
7. **No upgrade/version-compatibility guarantees of any kind.** There is no
   documented policy for rolling server upgrades, old-worker/new-server or
   new-worker/old-server compatibility, job payload schema evolution, or
   workflow-definition versioning. Only 3 migrations exist across all 9
   phases, so this has never actually been exercised even once in this
   project's own history.

## 4. Operational Blockers (P1 — would stop a production rollout, once P0s are addressed)

- **No worker identity/trust model, beyond a trusted first-party worker
  fleet.** `lease_owner` is an arbitrary caller-supplied string;
  failure-model.md explicitly declares "malicious workers" and "a worker
  that intentionally forges its lease_owner/lease_generation" as out of
  scope for v1, on the stated assumption that "workers are trusted internal
  processes, not arbitrary untrusted clients." **This is not an unconditional
  P0**: within the Enterprise Deployment Profile this review recommends
  (trusted first-party worker fleet), it is an accepted, documented
  boundary, not a blocker. It becomes a P0 the moment a deployment intends
  to run third-party or otherwise untrusted worker code, and would then
  require an architectural change this review has not designed (a
  worker-facing gateway or a stored-function boundary that avoids granting
  workers direct table access) — see [security-model.md](security-model.md)
  §2 for the honest deferral. Phase 12 does add separate least-privilege
  PostgreSQL roles for API vs. worker processes, which bounds a compromised
  worker's blast radius but does not by itself make untrusted worker code
  safe to run.
- **No distributed tracing.** Explicitly and deliberately deferred in
  [observability.md](observability.md) ("This is an explicit deferral, not
  an oversight"). Structured logs correlate on `job_id`, which is usable but
  materially weaker than a trace for diagnosing a slow multi-hop workflow.
- **No health/readiness endpoints.** Not found in the observability
  documentation or code; an operator has `GET /metrics` and direct database
  queries only.
- **No documented SLOs and no capacity/saturation evidence beyond one
  4-minute soak run.** Phase 9's own numbers are explicitly labeled
  "illustrative, not benchmarks."
- **No administrative/operator tooling.** No DLQ replay endpoint (a
  dead-lettered job's only path back to execution is a brand-new job
  submission — [worker-protocol.md](worker-protocol.md) "Deferred
  Endpoints"), no `GET /jobs/{id}/history` endpoint over `job_attempts`
  (must query the database directly), no admin UI or CLI beyond `cmd/chaos`.
- **No graceful drain/shutdown contract documented for the worker fleet**
  beyond what Phase 5's `TestStress_WorkerPoolGracefulShutdown_*` test
  covers at the goroutine level — there is no operator-facing "drain this
  worker before deploying a new version" procedure.
- **No request/payload size limits documented or enforced** on `payload`
  (jsonb) beyond whatever PostgreSQL's own row-size limits impose.

## 5. Security Blockers

See [security-model.md](security-model.md) for the full threat model. In
summary: application security, worker trust, and transport security are all
presently undefined (P0, above). Database trust is comparatively strong
(PostgreSQL is the single source of truth, and every mutating query is
parameterized through `pgx` — no evidence of string-built SQL was found
during this review's code inspection), but that strength is not yet paired
with any boundary controlling *who* is allowed to submit, inspect, or cancel
jobs.

## 6. Scaling Limitations

- **Single PostgreSQL primary is the durability and throughput ceiling by
  design** (ADR-0001, ADR-0006) — this is a deliberate, defensible v1
  choice, not an accident, but it means TaskForge today cannot scale
  write throughput beyond one primary's capacity, and a management
  conversation about horizontal scale-out (read replicas for `GET`
  endpoints, partitioning `job_attempts`, etc.) has not started.
- **No documented behavior under queue-depth or connection-pool
  saturation** beyond Phase 5's fixed-size stress tests (up to ~300 jobs /
  25 workers) and Phase 9's one soak run (150 jobs / 20 workers / 4
  minutes). There is no data on behavior at, say, 100k queued jobs or 500
  concurrent workers.
- **No fairness/starvation guarantee.** Phase 5 explicitly disclaims a
  "mathematically strict fairness... bound" — a low-priority job type can
  in principle starve indefinitely behind a sustained flood of
  higher-priority submissions, with no documented mitigation.

## 7. Compatibility/Versioning Gaps

See [compatibility-policy.md](compatibility-policy.md) for full detail.
In summary: there is currently **no API version prefix, no documented
deprecation policy, no job-payload schema versioning convention, and no
workflow-definition versioning story** for a workflow definition that
changes shape while instances of the old definition are still in flight.
This has not yet been a problem in practice only because the project has
gone through exactly one schema evolution event of consequence (adding the
workflow tables in migration `0003`) with no in-flight production traffic
to preserve.

## 8. Disaster-Recovery Gaps

No backup procedure, no PITR runbook, no tested restore, no documented RPO/
RTO target, no failover drill. failure-model.md's "Explicitly Out of
Scope" section is honest about this ("PostgreSQL primary failure/failover"
is out of scope), but an honest non-goal is still a gap the moment
enterprise adoption is on the table — every enterprise buyer's security/ops
review will ask for an RPO/RTO number and a tested restore procedure.

## 9. Supply-Chain Gaps

No SBOM, no dependency vulnerability scanning, no artifact signing/
attestation, no container image at all (no Dockerfile), no release process
beyond git tags (not verified to exist). The dependency surface is small and
reputable (`pgx/v5`, `google/uuid`, `prometheus/client_golang`,
`stretchr/testify`, `fergusstrange/embedded-postgres` — the last being a
test-only dependency), which meaningfully limits blast radius, but "small
surface" is not the same claim as "actively scanned surface," and today
neither Dependabot-equivalent scanning nor a documented triage process
exists.

## 10. Observability/Performance Evidence Gaps

Metrics and structured logs are genuinely solid (Phase 8, Stable). What is
missing is *evidence at scale*: no dashboards, no alerting rules (explicit
non-goal, correctly so for this phase), no tracing, and — critically for an
enterprise capacity conversation — no benchmark numbers that were actually
measured and published beyond the one 4-minute, single-process soak run.
Phase 9's own README section is explicit that its numbers are "illustrative,
not benchmarks," and that no true multi-process chaos, no network-partition
simulation, and no HTTP-level fuzz testing have been run.

## 11. Explicit Non-Goals (this review's own position, distinct from TaskForge's existing non-goals)

This review does **not** recommend TaskForge chase feature parity with
Temporal, Hatchet, or any other named system for its own sake. Specifically,
this review recommends TaskForge continue to explicitly reject, absent a
concrete demonstrated requirement:

- A message broker (Kafka/SQS/RabbitMQ) as a delivery layer — the
  single-PostgreSQL-primary throughput ceiling is a real limit, but there is
  no evidence today that TaskForge's target throughput has been reached,
  and ADR-0006 already reasons through this tradeoff correctly.
- Redis for locks/caching — already correctly rejected by
  [architecture.md](architecture.md); nothing in this review's research
  changes that conclusion.
- Kubernetes operators/CRDs, a service mesh, or any deployment-substrate
  dependency — TaskForge's correctness does not depend on the deployment
  substrate today, and should not start to.
- Deterministic-replay workflow execution (Temporal's model) — TaskForge's
  workflow model (a DAG of ordinary jobs with dependency-gated eligibility)
  is a different, and for TaskForge's stated scope (tens to low-hundreds
  of nodes, no dynamic modification) a *simpler and equally defensible*
  architecture. See [reference-analysis.md](reference-analysis.md).
- Event sourcing or a general-purpose workflow DSL — vision.md already and
  correctly rejects both.
- Microservices decomposition of the API/worker/database into more
  services — the current three-component architecture is not the
  bottleneck for any gap identified in this review.

## 12. What "Enterprise-Ready" Will Mean for TaskForge

TaskForge should be considered enterprise-ready when, and only when, all of
the following are true simultaneously — not when any single capability is
added in isolation:

1. Every write path (`POST /jobs`, `POST /jobs/{id}/cancel`,
   `POST /workflows`, `POST /workflows/{id}/cancel`) requires an
   authenticated, authorized caller, and every read path
   (`GET /jobs/{id}`, `GET /workflows/{id}`, `GET /metrics`) is at minimum
   authenticated.
2. All network traffic between clients, API servers, and PostgreSQL can be
   configured to run over TLS, with a documented (even if initially manual)
   certificate rotation story.
3. A documented, tested procedure exists for PostgreSQL backup, PITR
   restore, and (at minimum) planned failover, with a stated RPO/RTO.
4. A documented, tested rolling-upgrade procedure exists covering
   old-worker/new-server and new-worker/old-server compatibility for at
   least one minor version skew.
5. CI enforces dependency vulnerability scanning and produces an SBOM for
   every release artifact.
6. A capacity/saturation test has actually been run and its numbers
   published — not fabricated — at a scale representative of a real
   deployment target (this review deliberately does not invent a number
   here; see [slo.md](slo.md) for how such a test should be designed).
7. Workload governance (named queues and/or per-job-type concurrency
   limits, and some backpressure signal to over-eager submitters) exists
   and is tested.
8. The chaos/soak harness has actually been run for the roadmap's own
   "multi-hour" bar with zero invariant violations, closing Phase 9's own
   stated gap to Stable.

## 13. Measurable Exit Criteria

Each item below is binary (pass/fail), not a vague aspiration, and each
maps to a specific phase in [enterprise-roadmap.md](enterprise-roadmap.md):

- [ ] `POST /jobs` without a valid credential returns `401`, not `200`.
- [ ] `POST /jobs` over plaintext HTTP is refused (or the deployment is
      documented as requiring a TLS-terminating proxy, with that boundary
      tested).
- [ ] A documented `pg_basebackup`/WAL-archiving-based backup exists and a
      restore-from-backup drill has been executed and timed at least once.
- [ ] A two-binary-version (old worker / new server, and new worker / old
      server) integration test exists and passes.
- [ ] `govulncheck` (or equivalent) runs in CI and fails the build on a
      known-exploitable vulnerability in a direct dependency.
- [ ] An SBOM is generated and attached to at least one tagged release.
- [ ] A named-queue or per-job-type concurrency-limit mechanism exists with
      a passing test demonstrating one job type cannot starve another
      indefinitely.
- [ ] `cmd/chaos -mode=soak -duration=3h` (or longer) has been run at least
      once with zero invariant violations, and the result is committed to
      the repository as evidence (not merely claimed).

No unsupported claims are made anywhere above: every strength cited in
Section 2 traces to a named test or document; every gap cited in Sections
3–10 traces to a direct code/doc inspection performed during this review
(see the accompanying [security-model.md](security-model.md),
[compatibility-policy.md](compatibility-policy.md), and
[slo.md](slo.md) for the detailed backing analysis).
