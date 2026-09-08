# Reference Analysis: TaskForge vs. Production Job/Workflow Systems

Status: **review document**, produced during the `enterprise-readiness-review`
review. Comparisons focus on architecture and guarantees, not marketing
claims. Every externally-sourced fact below is cited with the exact URL used
and the access date. TaskForge facts are cited to internal docs/code
inspected during this review. No performance numbers are asserted for any
system unless that system's own documentation states them, and no TaskForge
performance number is stated beyond what [slo.md](slo.md) already
disclaims (Phase 9's single 4-minute soak run).

**Purpose**: this document exists to answer, capability by capability,
"does TaskForge need this, and if so, is there a Postgres-native way to get
it" — not "does system X have a feature TaskForge lacks." A system having a
capability is not by itself a reason for TaskForge to adopt it; see the
"TaskForge decision" and "Reason" columns for the actual judgment in each
row, and [enterprise-roadmap.md](enterprise-roadmap.md) for how the "adopt"
decisions are sequenced.

## Sources (access date: 2026-09-08)

- Temporal: https://docs.temporal.io/workflows, https://docs.temporal.io/develop/go/versioning
- Hatchet: https://docs.hatchet.run/home/architecture, https://docs.hatchet.run/home/concurrency, https://hatchet.run/blog/multi-tenant-queues
- River: https://riverqueue.com/docs, https://riverqueue.com/docs/unique-jobs
- Graphile Worker: https://github.com/graphile/worker, https://worker.graphile.org/
- Faktory: https://github.com/contribsys/faktory/blob/main/docs/protocol-specification.md, https://github.com/contribsys/faktory/wiki/Security, https://www.mikeperham.com/2017/10/24/introducing-faktory/
- PostgreSQL: https://www.postgresql.org/docs/current/high-availability.html, https://www.postgresql.org/docs/current/continuous-archiving.html
- OpenTelemetry Go: https://opentelemetry.io/docs/languages/go/, https://opentelemetry.io/docs/specs/semconv/messaging/messaging-spans/
- GitHub: https://docs.github.com/en/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds

TaskForge facts: [architecture.md](architecture.md), [data-model.md](data-model.md),
[execution-semantics.md](execution-semantics.md), [worker-protocol.md](worker-protocol.md),
[retry-semantics.md](retry-semantics.md), [idempotency.md](idempotency.md),
[scheduling.md](scheduling.md), [workflows.md](workflows.md),
[observability.md](observability.md), [failure-model.md](failure-model.md),
ADR-0001 through ADR-0008, and direct code inspection performed during this
review (`internal/store`, `internal/worker`, `internal/api`, `internal/chaos`,
`migrations/`, `.github/workflows/ci.yml`, `go.mod`).

## Comparison Matrix

| Capability | TaskForge | Temporal | Hatchet | River | Graphile Worker | Faktory | TaskForge decision | Reason |
|---|---|---|---|---|---|---|---|---|
| Source of truth / durability model | PostgreSQL rows are the state directly (ADR-0001) | Append-only Event History in a dedicated History Service; workers replay code against it | PostgreSQL, transactional state transitions; optional RabbitMQ for high-throughput inter-service messaging at scale | PostgreSQL rows, `river_job` table | PostgreSQL rows (`graphile_worker.jobs`) | Custom durable store (not Postgres) behind a TCP protocol | **Keep PostgreSQL-as-truth** | Matches River/Hatchet/Graphile Worker precedent for Postgres-backed queues; Temporal's history-service tier is a different architecture solving a different problem (deterministic replay), not a gap in TaskForge |
| Execution guarantee | At-least-once; execution-side idempotency is the handler author's responsibility (ADR-0003/0004) | At-least-once for Activities; Workflow code is replay-deterministic | At-least-once; task code must be idempotent/retry-safe | At-least-once; docs explicitly state unique-insert dedup "does not" make execution exactly-once | At-least-once | At-least-once | **No change** | River's own docs state the identical split (submission dedup ≠ execution exactly-once) as an intentional design, independently confirming ADR-0003/0004 rather than exposing a gap |
| Transactional enqueue (enqueue inside caller's own DB transaction) | **Not implemented** — `CreateJob` always opens its own connection; no `Store` API accepts an external `*sql.Tx`/`pgx.Tx` | N/A (workflow start is a distinct RPC, not a DB transaction primitive from the caller's perspective) | Not confirmed in this pass as a first-class API | **Yes — `Client.InsertTx`**, the library's signature feature | Not confirmed as a first-class API in this pass | N/A (custom protocol, no shared transaction with caller's own DB) | **Adopt (A)** — see [enterprise-roadmap.md](enterprise-roadmap.md) Phase 11 | Same Postgres, same `jobs` table — this is a pure API surface addition (accept a `Tx` alongside the pool), not a new architectural concept. Closes a real, confirmed gap: today a caller has no TaskForge-provided way to guarantee a job is only enqueued if its triggering business write commits |
| Named queues | **None** — one global claimable pool, `job_type` is a label, not a routing/isolation mechanism | Task Queues (first-class routing + worker-pool binding) | Queues + `GROUP_ROUND_ROBIN` concurrency slots keyed by a group function | Named queues, per-queue worker concurrency | `queue_name` column (serializes jobs within a queue) | Named queues, worker specifies fetch order | **Adopt (A)** — Phase 12 | Every researched system treats named queues as foundational for workload isolation; TaskForge's single-pool model is the direct cause of the "one noisy job type/tenant floods everyone" gap identified in [enterprise-readiness.md](enterprise-readiness.md) §3 |
| Priorities / fairness | `priority smallint`, used only as an `ORDER BY priority DESC` claim-query tiebreak; no fairness guarantee (Phase 5 explicitly disclaims strict fairness) | Task-queue-level priority (worker/SDK dependent) | FIFO / LIFO / Round Robin / **Priority**, plus **per-tenant fair queuing via group keys** so tenants are mixed fairly regardless of relative volume | Priority field on jobs | Priority column, processed in priority order | **Strict priority**: one queue drains to exhaustion before the next is touched | **Adopt Hatchet's fairness-aware model (A); explicitly reject Faktory's strict-priority model (D)** — Phase 12 | Strict per-queue draining is a documented starvation anti-pattern (a sustained high-priority stream starves everything below it indefinitely); Hatchet's group-key round-robin achieves priority *and* fairness simultaneously and is Postgres-compatible in spirit |
| Per-type/per-tenant concurrency limits | **None** — any worker can claim any job type, no cap on concurrent jobs of one type/tenant | Configurable per-Task-Queue worker slots | `GROUP_ROUND_ROBIN` concurrency slots per group key | Per-queue goroutine concurrency limits | Not confirmed in this pass | Per-queue worker allocation via fetch-order list | **Adopt (A)** — Phase 12 | Directly closes the "no workload isolation" P0 gap; a Postgres-native implementation (a concurrency-slot counter scoped by queue/tenant, checked in the claim query) requires no new infrastructure |
| Rate limiting / quotas | **None** | Not a core primitive (handled via task-queue sizing/worker count) | Static and **dynamic per-user rate limits** | Not a first-class primitive in the reviewed docs | Not confirmed in this pass | **Enterprise (paid) add-on**, not core | **Adopt, but stage after queues/concurrency exist (A, sequenced)** — Phase 12 | Even Faktory — a decade-old production system — ships rate limiting as a later/premium layer rather than a day-one primitive; this validates sequencing it after named queues/concurrency limits rather than building it first |
| Backpressure / overload signal to caller | **None** — `POST /jobs` succeeds until PostgreSQL itself fails; no documented behavior at saturation | Server-side flow control internal to Temporal; not a caller-facing contract in the docs reviewed | Rate limits imply caller-facing rejection/delay | Not surfaced as a distinct caller contract in the docs reviewed | Not confirmed in this pass | Not confirmed in this pass | **Adopt: add an explicit 429/503 + `Retry-After` contract once queue depth/rate limits exist (A)** — Phase 12 | Today TaskForge's honest answer to "what happens when you send more than you can handle" is "the queue grows in the database until disk/index performance degrades" ([slo.md](slo.md)) — an enterprise buyer's ops review will ask this question directly; a documented, tested answer is required before quotas make sense |
| Worker claim mechanism | `SELECT...FOR UPDATE SKIP LOCKED` + fenced conditional `UPDATE` in one statement | Long-poll gRPC against Task Queues (dedicated service tier) | Bidirectional gRPC, low-latency push dispatch | Poll + `LISTEN/NOTIFY`-assisted wake, same SQL claim pattern as TaskForge | `LISTEN/NOTIFY` for near-real-time pickup layered on polling | `FETCH` command over custom TCP protocol, server reserves job | **Keep `SKIP LOCKED` polling as-is (no change, D on LISTEN/NOTIFY for now)** | River validates TaskForge's exact claim pattern as an industry-normal Postgres approach; Graphile Worker's `LISTEN/NOTIFY` buys lower latency at real operational cost (held listener connections, missed-notification-on-reconnect handling) for a latency problem TaskForge has not measured or been asked to solve — revisit only if [slo.md](slo.md)'s claim-latency SLO is measured and found wanting |
| Worker lease / fencing | `(lease_owner, lease_generation, lease_expires_at)`, every write fenced by generation (ADR-0002) | Sticky worker assignment + task-queue visibility timeout | gRPC session-based liveness | Lease + fencing, same shape as TaskForge | Row lock during claim | Timeout-based reservation, explicit `ACK`/`FAIL` | **Keep as-is** | River's independent adoption of the identical lease-fencing shape validates ADR-0002 as an industry-normal pattern, not a bespoke risk |
| Graceful shutdown / draining | Proven at the goroutine level (Phase 5); **no documented OS-process-level SIGTERM/drain contract** | Worker SDK drain support | Not confirmed in this pass | **Documented client-level contract**: stop accepting new work, finish in-flight within a timeout | Not confirmed in this pass | Not confirmed in this pass | **Adopt: formalize the existing goroutine-level guarantee into an operator-facing process contract (A)** — Phase 12 | TaskForge already has the correctness property (Phase 5's tests); River shows the missing piece is purely the operator-facing documentation/CLI contract, not new engineering |
| Workflow/DAG model | DAG of already-resolved `job_type`/`payload` pairs, fixed at submission time; no dynamic modification (explicit deferral) | Workflow **code**, replayed deterministically against Event History | DAG-of-tasks (DSL similar in spirit to TaskForge's) | N/A (job queue, not a workflow engine) | N/A | **Batches** (workflow-like grouping, paid tier) | **Keep DAG-at-submission model (no change)** | For TaskForge's stated scope (tens to low-hundreds of nodes, no dynamic modification), a submission-time DAG is simpler and equally defensible; adopting Temporal's replay model would mean adopting an entirely different execution architecture for a capability (dynamic workflow modification) TaskForge has not been asked to support |
| Workflow-definition versioning | **None** — no named/versioned workflow template concept; every submission is a self-contained, already-concrete DAG | `workflow.GetVersion`/patching against replayed history; newer Worker Versioning for build-ID-based routing | Not confirmed in this pass | N/A | N/A | N/A | **Partial adopt, TaskForge's own way (A, scoped down)** — Phase 14 | `GetVersion`-style patching is inseparable from Temporal's replay model and does not transfer. The real, transferable need — "an in-flight workflow instance shouldn't be affected by a later change to how that workflow is defined" — is already true by construction for TaskForge's snapshot-DAG model *if* job handlers themselves stay compatible; the actual gap is documenting handler-compatibility expectations (see [compatibility-policy.md](compatibility-policy.md)), not building a version-patching system |
| Scheduling: one-shot delayed jobs | `eligible_at` column, durable (TF-INV-011) | Timers (replay-safe) | Supported | Supported | Supported | Via enterprise cron add-on | **No change** | Already proven and adequate for TaskForge's stated scope |
| Scheduling: periodic/cron jobs | **None** — explicit deferral | Cron workflows | Supported | Native periodic job scheduling | Native cron (minute granularity) | Enterprise (paid) add-on | **Defer (C)** — not in this roadmap | No demonstrated TaskForge requirement today; every comparable system treats it as a bounded, well-understood addition once actually needed, not a prerequisite for enterprise readiness |
| Retry/backoff | Exponential backoff + equal jitter, global config only, `max_attempts` enforced by app logic and a DB `CHECK` constraint (defense in depth) | Configurable retry policies per Activity | Configurable per-task retry policies | Configurable retry policies | Exponential backoff | Configurable | **No change to mechanism; per-job-type config is a P2, not this roadmap** | The double-enforced ceiling (app logic + `CHECK` constraint) is already stronger than typical; per-job-type backoff tuning is a real but non-blocking enhancement ([retry-semantics.md](retry-semantics.md) "Open Questions") |
| Submission idempotency / dedup | `UNIQUE(job_type, idempotency_key)`, INSERT-first, proven under 60-goroutine concurrent duplicates | Workflow ID uniqueness | Not confirmed in this pass | `UniqueOpts`: dedup by args/period/queue/state combinations, richer than TaskForge's single-key model | `job_key` for dedup/replace-in-place semantics | Not confirmed in this pass | **Keep current mechanism; richer dedup dimensions are a P2 enhancement, not this roadmap** | River's own docs state unique-insert dedup is *not* equivalent to exactly-once execution — independent validation that TaskForge's current, simpler mechanism is not under-engineered relative to a more feature-rich implementation of the same underlying guarantee |
| Authentication | **None** | Client-side mTLS/API-key options at the gRPC layer (deployment-dependent) | Not conclusively researched in this pass | N/A (embedded library, not a network service) | N/A (embedded library) | Single shared global password + challenge/response (`pwdhash`) | **Adopt a minimal shared-credential/API-key model first (A)** — Phase 11 | Faktory — a mature, decade-old production job server — defaults to one shared secret, not per-caller RBAC, and explicitly targets trusted-internal deployments first. This calibrates TaskForge's first security phase correctly: API keys + TLS is a legitimate, proven starting point, not an under-scoped one; RBAC/OIDC is a later phase, not a prerequisite for the first one |
| Authorization / RBAC | **None** — no ownership check on cancel/inspect | Namespace-level access control (deployment-dependent) | Per-tenant framing via group keys (fairness/rate-limit context, not confirmed as full RBAC) | N/A | N/A | None (shared-secret model, no per-caller RBAC) | **Adopt minimal ownership/ACL check (A); defer full RBAC (C)** — Phase 11 | Matches the industry pattern observed above: ship "who submitted this may cancel/inspect this" before a general role/permission system nobody has asked for yet |
| Multi-tenancy | **None** (explicit v1 non-goal, [vision.md](vision.md)) | Namespaces | **Per-tenant fair queuing as a first-class concept** | N/A | N/A | N/A | **Adopt tenant-scoped fairness/limits as part of Phase 12, not a separate multi-tenancy subsystem** | Hatchet's approach — tenancy expressed as a group key threaded through existing queue/concurrency/rate-limit mechanisms — is the right-sized version of this for TaskForge; a dedicated multi-tenancy subsystem (schemas-per-tenant, row-level security, etc.) is not evidenced as needed yet |
| Transport security (TLS) | **None anywhere in the codebase** | TLS/mTLS supported at the gRPC layer | Not conclusively researched in this pass | Deployment-dependent (Postgres connection string) | Deployment-dependent | **Native TLS termination (v1.9.0+)**; plaintext connections reset once enabled | **Adopt (A)** — Phase 11 | Faktory's native-TLS-with-hard-cutover model (no silent plaintext fallback once enabled) is a clean, provable pattern worth mirroring for both the API and the Postgres connection string |
| Observability: metrics | 13 Prometheus metrics, job-domain only, cardinality-audited | Rich SDK + Cloud metrics | Metrics available | Metrics available | Not confirmed in this pass | Metrics available | **Add HTTP-request-domain metrics (A)** — Phase 14 | TaskForge's job-domain metrics are genuinely solid (Phase 8, Stable); the gap is the complete absence of API/HTTP-layer instrumentation needed for an "API availability" SLO |
| Observability: tracing | **None** (explicit deferral) | Deep replay-aware tracing integration | Not confirmed in this pass | Not confirmed in this pass | Not confirmed in this pass | Not confirmed in this pass | **Adopt OpenTelemetry with span *links* (not parent-child) across the queue-wait gap (A)** — Phase 14 | OTel's own messaging semantic conventions specify link-based correlation between producer (`create`/`send`) and consumer (`process`) spans precisely because a queue wait can be arbitrarily long — this is the architecturally correct pattern for TaskForge's submit-then-later-claim shape, not a bespoke design |
| PostgreSQL HA / failover | **Explicitly out of scope** (ADR-0001, [failure-model.md](failure-model.md)): "if the database is down, TaskForge is down for writes"; no replication, no automated failover | N/A (Temporal's own persistence layer, not TaskForge's concern) | N/A | N/A | N/A | N/A (custom store) | **Document a deployment-time HA topology using vanilla Postgres replication; do not build TaskForge-specific failover code (A, operational)** — Phase 15 | PostgreSQL's own docs confirm automated failover orchestration is explicitly *not* provided by PostgreSQL itself — that is the job of third-party tooling (e.g., Patroni, repmgr) layered underneath, chosen at deployment time. This is an operational/deployment decision, not a TaskForge code gap |
| Backup / PITR | **None documented** | N/A (out of TaskForge's scope) | N/A | N/A | N/A | N/A | **Write and drill a runbook using existing Postgres primitives (`pg_basebackup`, WAL archiving, `recovery_target_time`) (A, operational)** — Phase 15 | PostgreSQL already provides everything needed (base backup + continuous WAL archiving + point-in-time recovery targets); the gap is a documented, tested runbook with a stated RPO/RTO, not new code |
| Supply-chain: dependency scanning | **None** — no `govulncheck`, no Dependabot | N/A (not applicable to a downstream consumer's roadmap) | N/A | N/A | N/A | N/A | **Adopt (A)** — Phase 10 | Zero-cost, GitHub-native, closes a real P0 (no detection mechanism for a newly-disclosed CVE in an already-pinned dependency) |
| Supply-chain: SBOM + provenance | **None** | N/A | N/A | N/A | N/A | N/A | **Adopt GitHub Artifact Attestations + SBOM (A)** — Phase 10 | GitHub's `actions/attest` (with `sbom-path`) is a single bounded CI addition on top of the CI pipeline TaskForge already runs — no new infrastructure or service dependency required |

## Narrative Notes by System

**Temporal** — architecturally the furthest from TaskForge (dedicated
History Service, replay-based determinism). Its versioning machinery is the
most sophisticated researched, but it is inseparable from the replay model;
adopting it piecemeal would mean adopting replay. The transferable lesson is
narrower than "add GetVersion": TaskForge's actual, smaller need is
documenting how job-handler compatibility must be preserved across
deployments for in-flight jobs/workflows, which [compatibility-policy.md](compatibility-policy.md)
already begins to specify.

**Hatchet** — the closest architectural cousin to TaskForge (Postgres as
source of truth, transactional state transitions, an optional-not-required
broker for scale). Its queue/concurrency/fairness/rate-limit model is the
single most directly transferable set of lessons in this research pass,
because it is Postgres-native governance, not a different architecture.

**River** — the most directly applicable system, because it targets the
identical niche (Go + Postgres job queue) that TaskForge occupies. Its
transactional-enqueue API (`InsertTx`) is the cheapest, highest-leverage gap
closer identified in this entire review — pure API surface, zero new
architectural concept. Its unique-jobs and lease-fencing design independently
validate two of TaskForge's own core decisions (ADR-0002, ADR-0003/0004)
rather than exposing gaps.

**Graphile Worker** — validated TaskForge's polling-based claim design is a
legitimate choice, not an oversight; its `LISTEN/NOTIFY` latency optimization
is a real technique but solves a problem TaskForge has not measured itself
to have. Public documentation depth available for this review was lower
than the other systems (see the external-research report's confidence note);
capability rows sourced from it should be treated as lower-confidence than
Temporal/Hatchet/River/Faktory rows.

**Faktory** — the most useful calibration point for security scoping and
feature staging. A decade-old, widely-deployed production system defaults to
a single shared secret plus TLS (not per-caller RBAC) and ships rate
limiting/cron/batching as later or premium capabilities, not core primitives.
This directly informs this roadmap's sequencing: minimal authN before RBAC,
governance primitives before rate limiting, and rejection of its
strict-priority queue model as a known fairness anti-pattern.

## Explicitly Rejected Capabilities (with reasoning)

- **Message broker (Kafka/SQS/RabbitMQ) as the primary delivery layer** —
  Hatchet's own architecture treats a broker as optional scale-out
  infrastructure layered *under* a Postgres-backed control plane, not a
  replacement for it. No TaskForge throughput ceiling has been reached or
  even measured (see [slo.md](slo.md)); ADR-0006 already reasons through
  this tradeoff correctly.
- **Redis for locks/caching** — no researched system's core correctness
  model depends on Redis; already correctly rejected in
  [architecture.md](architecture.md).
- **Deterministic-replay workflow execution (Temporal's model)** — a
  different, more complex architecture solving a problem (dynamic workflow
  modification, arbitrary code-as-workflow) TaskForge has not been asked to
  solve; the DAG-at-submission model remains simpler and equally defensible
  for TaskForge's stated scope.
- **Strict per-queue priority draining (Faktory's model)** — a known
  starvation anti-pattern; explicitly reject in favor of Hatchet's
  fairness-aware group-key round robin.
- **`LISTEN/NOTIFY`-based low-latency claim (Graphile Worker's model)** —
  real complexity (held listener connections, missed-notification-on-reconnect
  handling) for a latency improvement that is unmeasured and unrequested;
  revisit only if a measured claim-latency SLO is found wanting.
- **Kubernetes operators/CRDs, service mesh, Elasticsearch, event sourcing,
  microservices decomposition** — no system researched demonstrates a
  concrete TaskForge requirement for any of these; consistent with
  [vision.md](vision.md)'s existing non-goals.

See [enterprise-roadmap.md](enterprise-roadmap.md) for how the "adopt" (A)
decisions above are sequenced into phases.
