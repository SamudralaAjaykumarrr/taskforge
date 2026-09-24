# Phase 16 Implementation Plan — Tracing & Operator Diagnostics

Status: **PLANNING ONLY. No code written, no migration authored, no ADR
committed.** This document is the pre-implementation plan for Phase 16 —
[docs/enterprise-roadmap.md](enterprise-roadmap.md) "Phase 16 — Tracing &
Operator Diagnostics" — produced by inspecting the repository's
authoritative documents (roadmap, invariants, testing strategy, data
model, security model, compatibility policy, observability, failure
model, disaster-recovery, reference-analysis, every ADR) and the current
implementation on branch `phase-16-planning` (tip `913d33a`, following
merge PR #32 "phase-15-postgres-ha-backup-dr" — Phases 1–15 all merged, no
Phase 16 code exists anywhere in the tree). It does not redefine Phase
16's scope; that scope remains authoritative in
[enterprise-roadmap.md](enterprise-roadmap.md). Where this document and
that roadmap disagree, the roadmap wins.

This plan follows the same discipline
[phase-12-plan.md](phase-12-plan.md), [phase-13-plan.md](phase-13-plan.md),
[phase-14-plan.md](phase-14-plan.md), and [phase-15-plan.md](phase-15-plan.md)
established: every design choice is either **settled** (a concrete
recommendation, ready to implement) or an explicitly labeled **open
decision (OD-N)**. Like Phase 13 (ADR-0009) and Phase 15 (ADR-0011), this
plan concludes a **blocking ADR is required before implementation begins**
— see §29 — but, consistent with this planning pass's own scope (produce
`docs/phase-16-plan.md` only), that ADR is **not** authored here; §29
states exactly what it must decide.

> **Post-planning update (blocking ADR written)**: this plan's own §29
> identified OD-A — a written ADR ratifying the tracing architecture — as
> the single blocking prerequisite before implementation may begin, and
> §30 concluded no separate empirical evidence pass (unlike Phase 15's
> OD-1/OD-2) was additionally required. That ADR now exists:
> [ADR-0012](adr/0012-distributed-tracing-and-durable-trace-context.md)
> ratifies durable trace-context storage (§7 below), the span-link/async
> causality model (§10–§12), the OTLP/HTTP transport choice (OD-B), and
> the noop-by-default posture (§5/§17), and resolves OD-B, OD-C, OD-D, and
> OD-E as this plan recommended. **OD-A is now CLOSED.** OD-F (the exact
> OTel semantic-convention version to pin) remains deliberately **OPEN**,
> as designed — ADR-0012 explicitly declines to resolve it, for the same
> reason §29 states: it is an implementation-time lookup, not a
> pre-decidable architectural choice. §29 and §30 below are retained as
> originally written (the authoritative record of what this plan asked
> the ADR to decide), with OD-A's own entry updated in place to reflect
> this closure.

**A note on sourcing**, following prior plans' own convention: this plan
draws on (1) **roadmap-defined requirements** — quoted or closely
paraphrased from [enterprise-roadmap.md](enterprise-roadmap.md)'s Phase 16
section, never extended in substance; (2) **already-drafted findings** in
[observability.md](observability.md) (which named the tracing deferral and
sketched trace-boundary shapes before the roadmap formalized this phase),
[slo.md](slo.md) (which named the HTTP-metrics and claim/queue-wait
splitting gaps precisely), [security-model.md](security-model.md) §7
(which named the tenant-scoping requirement for DLQ replay), and
[reference-analysis.md](reference-analysis.md) (which recommended
OpenTelemetry span links over parent-child spans); and (3) **design
conclusions this plan itself introduces**, explicitly flagged `(this
plan)`, drawn from a direct source-code inventory of `internal/api`,
`internal/principal`, `internal/worker`, `internal/store`,
`internal/metrics`, `cmd/api`, `cmd/worker`, and `go.mod` performed for
this plan (exact file/line citations throughout).

---

## 1. Exact Phase 16 objective

Per [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 16 — Tracing &
Operator Diagnostics": close TaskForge's last remaining P1 operational
blocker category — the total absence of distributed tracing, HTTP-domain
metrics, health/readiness endpoints, and operator-facing administrative
tooling (job history, DLQ replay) — all four named explicitly in
[enterprise-readiness.md](enterprise-readiness.md) §4 and
[slo.md](slo.md), none touched by Phases 10–15.

**The one-sentence goal**: give an operator (or an on-call engineer during
an incident) the ability to (a) follow one logical job's execution across
an unbounded queue-wait gap and any number of retries/reclaims via
correlated traces, (b) know whether the API server is alive and ready to
serve traffic via standard health/readiness endpoints, (c) inspect a
job's full attempt history and replay a dead-lettered job via a
documented, authorized API — **without weakening a single Phase 1–15
guarantee**, and without inventing a second, parallel observability
system alongside the metrics/logging infrastructure Phase 8 already
proved.

Five concrete deliverables, each with its own roadmap-stated proof
obligation (verbatim scope in §2):

1. OpenTelemetry span-link-based tracing connecting a job's submission
   span to its execution span(s), surviving retries/reclaims, using a
   pinned, documented, non-compatibility-guaranteed version of OTel's
   messaging semantic conventions.
2. HTTP-request-domain metrics (request count/latency/status by route),
   and a genuine split of `taskforge_claim_latency_seconds` /
   `taskforge_queue_age_seconds` into distinguishable observations.
3. `/healthz` and `/readyz` endpoints on `cmd/api`.
4. `GET /jobs/{id}/history` — a read endpoint over the existing
   `job_attempts` ledger.
5. `POST /jobs/{id}/retry` — a DLQ-replay endpoint, with every new
   admin/diagnostic endpoint inheriting Phase 12's authentication and
   ownership checks from day one.

## 2. Authoritative scope (verbatim from the roadmap)

### 2.1 Exact scope

- OpenTelemetry Go SDK integration with **span links** (not parent-child
  spans) connecting a job's submission span to its later claim/execution
  span, following OTel's messaging semantic conventions (`create`/`send`
  producer spans, `process` consumer spans). OTel's messaging semantic
  conventions are still marked *Development*, not *Stable* — this phase
  must **pin and document the exact convention version used** and must
  **not** treat experimental attribute names as part of TaskForge's
  permanent API/compatibility guarantee.
- A documented mechanism for how trace context is **durably associated**
  with a queued job across long waits, retries, and lease reclaims (e.g.,
  storing trace/span identifiers alongside the job row). This association
  is diagnostic-only and must **never** become correctness-authoritative
  — TaskForge's invariants do not depend on trace context being present,
  correct, or complete.
- **Telemetry failure must remain non-load-bearing**: the trace exporter
  becoming unavailable must not block or fail job execution.
- HTTP-request-domain metrics (request count, latency, status code by
  route) added to the existing Prometheus registry.
- Split `taskforge_claim_latency_seconds` and `taskforge_queue_age_seconds`
  into genuinely distinct observations (currently the same sample).
- Health (`/healthz`) and readiness (`/readyz`) endpoints.
- `GET /jobs/{id}/history` over the existing `job_attempts` ledger.
- `POST /jobs/{id}/retry` (or equivalent) DLQ-replay endpoint, closing the
  documented "Deferred Endpoints" gap in [worker-protocol.md](worker-protocol.md).
- Graceful drain is **not** duplicated here (formalized in Phase 14); this
  phase only consumes `/healthz`/`/readyz` as inputs to that existing
  contract's own testing.
- **All new admin/retry/history endpoints must inherit Phase 12's
  authorization and tenant-ownership checks from day one.**

### 2.2 Explicit non-scope

- No vendor dashboards or alerting rules.
- No distributed-tracing-based *replay* or debugging tooling beyond
  spans/traces themselves (no Temporal-style workflow-history UI).
- No admin UI — an API is in scope, a browser-based console is not.
- No claim that trace/span data is authoritative for any correctness
  property — it is diagnostic only.

### 2.3 Prerequisites

Phase 12 must land first (or concurrently) so the new audit-relevant
endpoints (history, retry) are authorizable from day one. **Satisfied**:
Phase 12 is merged (`internal/principal`, `internal/api/auth.go`).

### 2.4 Invariants / proof obligations (roadmap text)

- A traced job's submission span and execution span(s) are correctly
  linked (not falsely parented) even when the queue wait spans hours, and
  this linkage survives at least one retry/reclaim cycle.
- `/healthz` reflects true process liveness; `/readyz` reflects true
  readiness (e.g., false during startup before the DB pool is
  established).
- A DLQ-replayed job re-enters the state machine at `QUEUED` with a fresh
  `job_id` and a recorded link (`retried_from`) to the original, scoped
  to the same tenant/principal as the original job.
- Every new endpoint enforces the same authorization/ownership check as
  the existing `GET /jobs/{id}` endpoint.

### 2.5 Failure scenarios to guard against (roadmap text)

- The trace exporter becomes unavailable — job execution must not block
  or fail.
- `/readyz` reports ready while the database is actually unreachable.
- A future OTel semantic-convention revision renames an attribute this
  phase used — must not break TaskForge's own compatibility guarantees.
- A new admin/history/retry endpoint is added later without going
  through Phase 12 authorization — must be caught by a lint/test
  convention, not merely review discipline.

### 2.6 Tests / evidence required (roadmap text)

- A span-link assertion test using a test/in-memory OTel exporter,
  including across a retry/reclaim.
- HTTP-metrics scrape test proving request count/latency/status appear
  correctly per route.
- `/healthz`/`/readyz` tests under normal and DB-disconnected conditions
  (reusing Phase 9's chaos primitives).
- A DLQ-replay test proving replay only by the owning principal, and the
  `retried_from` linkage is recorded and queryable.

### 2.7 Enterprise exit criteria (roadmap text, reproduced for §28)

- [ ] Job submission and execution are linked via OpenTelemetry spans
      following a pinned, documented version of OTel's messaging semantic
      conventions, excluded from TaskForge's own compatibility guarantee.
- [ ] `taskforge_claim_latency_seconds` and `taskforge_queue_age_seconds`
      are distinct, independently meaningful observations.
- [ ] `/healthz` and `/readyz` exist and are proven correct under a
      simulated database outage.
- [ ] A dead-lettered job can be replayed via a documented API, scoped to
      its owning tenant/principal, with the `retried_from` linkage
      tested.
- [ ] Every endpoint added by this phase enforces Phase 12's
      authorization/ownership checks, verified by test.

## 3. Explicit non-goals

Restated for implementation clarity, and to prevent Phase 17+ scope from
leaking in:

- **No performance/capacity claims.** HTTP-metrics and the claim/queue-age
  split exist so Phase 17 can *measure* meaningfully — this phase adds the
  instrumentation, it does not run the multi-hour soak or publish a
  benchmark. That is exclusively [enterprise-roadmap.md](enterprise-roadmap.md)
  Phase 17.
- **No multi-process/SIGKILL failure campaign.** That is Phase 17's "real
  OS-process-boundary failure campaign." Phase 16's proof obligations are
  provable with ordinary in-process integration tests plus a test-exporter
  (roadmap §2.6 says exactly this) — see §23.
- **No workload-governance change.** `queue_name`/concurrency/rate-limit
  machinery (Phase 13, already merged) is read-only input to the new
  metrics/diagnostics surfaces; this phase does not alter governance
  behavior.
- **No new authentication/authorization mechanism.** Every new endpoint
  reuses Phase 12's existing `principal`/`api_keys`/scope machinery
  unchanged — see §6.2. No new scope value beyond the existing `jobs`,
  `metrics`, `admin` is introduced (an admin/history/retry endpoint
  requires the existing `jobs` scope, exactly like `GET /jobs/{id}`).
- **No admin UI, no alerting rules, no vendor dashboard** (roadmap §2.2,
  restated).
- **No cron/periodic scheduling, no message broker, no Redis** — unrelated
  to this phase and already rejected project-wide
  ([enterprise-roadmap.md](enterprise-roadmap.md) "What This Roadmap
  Deliberately Does Not Include").
- **No workflow-level tracing redesign.** A workflow node's underlying job
  is an ordinary job for every tracing purpose in this phase, exactly as
  it already is for metrics/logs ([observability.md](observability.md)
  "Workflow observability"). Per-node cascade/activation events are not
  separately traced, mirroring that same document's existing choice not
  to log them individually.

## 4. Current-state and gap analysis

Verified by direct source inspection (not documentation) against the
current tree.

### 4.1 Tracing: zero infrastructure exists

A repository-wide, case-insensitive search for `trace_id`, `span_id`,
`correlation_id`, `TraceID`, `SpanID`, and `opentelemetry` across every
`.go`/`.sql`/`.md` file returns **zero** hits related to distributed
tracing (the only "trace" hits are two unrelated uses of the English word
in a benchmarking comment, `tools/phase13bench/sweep.go:223,437`). `go.mod`
carries no `go.opentelemetry.io/...` dependency of any kind. This phase's
tracing work is a **greenfield addition**, not an extension of a partial
mechanism — confirming [observability.md](observability.md)'s own
"explicit deferral, not an oversight" framing is accurate as of today.

The closest existing analog is the correlation-via-structured-logging
convention already proven in Phase 8: every worker lifecycle log line is
built via `w.logger.With("job_id", ..., "job_type", ..., "worker_id", ...,
"attempt", ..., "lease_generation", ...)` (`internal/worker/worker.go`,
`RunOnce`, around line 219). Phase 16's tracing work should be understood
as *adding a second correlation axis alongside this one*, not replacing
it — see §13.

### 4.2 `retried_from`: a documentation/reality mismatch this plan corrects

[enterprise-roadmap.md](enterprise-roadmap.md) line 1198 states "the
`retried_from` field already exists in the schema per the internal
audit." **This is false.** Verified against every migration file in
`migrations/` (`0001` through `0014`) and the `Job` struct
(`internal/job/job.go:17-53`): no `retried_from` column, and no
`RetriedFrom` Go field, exists anywhere. Every other reference to
`retried_from` — [worker-protocol.md](worker-protocol.md) "Deferred
Endpoints", [retry-semantics.md](retry-semantics.md), and
[ADR-0008](adr/0008-explicit-terminal-states.md) — correctly describes it
as a **documented future design**, not an implemented column. This plan
treats `retried_from` as new work: a new migration (§7), a new `Job`
field, and a new store method (§6.3). **The roadmap's line 1198 should be
corrected during Phase 16's own implementation PR** (a one-line doc fix,
not a Phase 16 deliverable in itself, but flagged here so it is not
silently perpetuated into Phase 17+ planning).

### 4.3 `internal/api`: routing has exactly one sanctioned attachment point, and it hard-requires auth

`NewRouter` (`internal/api/server.go:199-217`) builds an `http.ServeMux`
and calls `registerJobRoutes(mux, h, "")` / `registerJobRoutes(mux, h,
"/v1")` for the dual legacy/versioned surface established in Phase 11.
Every route this project has ever added is attached through exactly one
function, `mount(mux, h, pattern, requiredScope, handler)`
(`server.go:244-246`), which unconditionally wraps the handler in
`h.requireAuth(requiredScope, handler)`. This is not incidental: a
structural test, `TestRouter_AllHandlersMountedThroughAuth`
(`internal/api/structure_phase12_test.go`), scans the package source and
fails if any `mux.Handle`/`HandleFunc` call exists outside `mount`. This
is exactly the "lint/test convention" the roadmap's §2.5 asks for a *new*
admin endpoint to be caught by — it already exists, generically, for
every route in the router.

**Gap this creates for Phase 16**: `/healthz`/`/readyz` are conventionally
unauthenticated (a load balancer or orchestrator health probe does not
carry an API key). `mount()` as it exists today cannot express an
unauthenticated route at all — see OD-D (§29) for the resolved design.

`GET /jobs/{id}/history` and `POST /jobs/{id}/retry`, by contrast, fit the
*existing* pattern exactly: both are ordinary `mount(..., principal.ScopeJobs,
...)` calls added to `registerJobRoutes`, registered under both the
legacy and `/v1` prefixes exactly like the six existing routes.

### 4.4 `internal/principal`: ownership pattern is "authz threaded as an explicit argument into the SQL WHERE clause," never a handler-level check

`Store.Verify(ctx, credential) (AccessContext, error)` (`internal/principal/store.go:297`)
is the sole authentication entry point; `AccessContext{PrincipalID,
IsAdmin, Scopes}` is attached to the request context by `requireAuth`
(`internal/api/auth.go:90-118`) and retrieved by a handler via
`h.accessContext(w, r)` (`auth.go:169-179`) — the **only** sanctioned way
a handler learns identity. From there, `AccessContext` is passed as an
**explicit function parameter** into every store call (e.g.
`GetByID(ctx, id, authz)`), which embeds the ownership predicate directly
in the SQL statement (`... AND ($n::boolean OR principal_id = $n+1)`) —
never a "read the row, then check `if row.PrincipalID != authz.PrincipalID`"
pattern, which would reopen exactly the TOCTOU race Phase 12's own
design note (`server.go:25-53`'s `JobStore` interface doc comment)
rejected. **Every new store method Phase 16 adds (job-attempt history
read, DLQ replay) must follow this identical pattern** — this is a "reuse
X," not new design.

### 4.5 `internal/worker`: no existing hook/middleware surface for span attachment

`RunOnce` (`internal/worker/worker.go:194-316`) claims one job
(`w.store.Claim`/`ClaimFromQueues`), dispatches to `runWithHeartbeat`
(`worker.go:532-653`), and reports the outcome via one of
`CompleteSuccess`/`reportFailure`/`reportCancelled`/`reportTimeout`.
`runWithHeartbeat` constructs `execCtx` (`worker.go:542`,
`context.WithTimeout(workCtx, executionTimeout)`) — the exact
`context.Context` passed to `h.Execute(execCtx, j)` (`worker.go:632`).
There is **no** existing `Interceptor`/`Before`/`After` hook interface in
this package; the only existing "enrichment" precedent is the
`*slog.Logger`-with-fields convention (§4.1). **Attaching a span requires
new plumbing** at exactly two points: (a) `RunOnce`, immediately after a
successful claim (span start, linked to the stored submission span), and
(b) the point where `runWithHeartbeat` returns a terminal disposition
(span end) — both are new code, not a reuse of an existing extension
point, and this plan states that explicitly rather than implying
otherwise.

### 4.6 `internal/metrics`: the claim-latency/queue-age identity is a single, well-documented line pair

`internal/store/claim.go`'s `recordClaim(j *job.Job, oldState
jobstate.State, workerID string, ...)` computes `claimLatency :=
j.UpdatedAt.Sub(j.EligibleAt)` once and calls
`s.metrics.ClaimLatencySeconds.Observe(claimLatency.Seconds())` followed
immediately by `s.metrics.QueueAgeSeconds.Observe(claimLatency.Seconds())`
(`claim.go:631-637`) — the exact redundancy
[observability.md](observability.md)'s own doc comment (`metrics.go:96-109`)
already acknowledges as deliberate-but-undifferentiated. Critically,
`recordClaim` **already receives `oldState`** — the exact signal
(`QUEUED`/`RETRY_WAIT`/expired-lease-`RUNNING`) needed to distinguish "a
job waiting for a free worker" from "a job waiting through retry backoff"
from "a job reclaimed after a crash," which is precisely the gap
[slo.md](slo.md) names. See §6.5 for the resolved design (an additive
label, not a second histogram).

Full current metric-name inventory (for the "never repurpose a metric
name" compatibility check, [compatibility-policy.md](compatibility-policy.md)
rule 4), from `internal/metrics/metrics.go`'s `New()` (lines 249-344) plus
the two collector-based families:
`taskforge_jobs_submitted_total`, `taskforge_jobs_completed_total`,
`taskforge_jobs_dead_lettered_total`, `taskforge_claim_latency_seconds`,
`taskforge_queue_age_seconds`, `taskforge_execution_duration_seconds`,
`taskforge_lease_expirations_total`,
`taskforge_stale_completion_rejections_total`, `taskforge_retry_count`,
`taskforge_heartbeats_total`, `taskforge_idempotent_submission_hits_total`,
`taskforge_auth_failures_total`, `taskforge_admission_rejections_total`,
`taskforge_retention_rows_deleted_total`,
`taskforge_retention_sweep_duration_seconds`,
`taskforge_retention_sweep_errors_total`,
`taskforge_worker_drain_duration_seconds`,
`taskforge_worker_drain_timed_out_total`, `taskforge_jobs_by_state`,
`taskforge_active_workers`, `taskforge_queue_depth`,
`taskforge_queue_running`, `taskforge_queue_concurrency_limit`. None of
Phase 16's new names below collides with this list.

### 4.7 `cmd/api`: zero health/readiness surface today; `WithMetricsEndpoint` is the exact precedent to copy

`run()` (`cmd/api/main.go:72-214`) opens the DB pool, runs `migrate.Up`,
constructs `metrics.New()`/`store.New()`/`principal.NewStore()`, builds
`api.NewHandlers(...)`, and calls `api.NewRouter(handlers,
api.WithMetricsEndpoint(promhttp.HandlerFor(...)))` (`main.go:138-140`).
`WithMetricsEndpoint` is a `RouterOption` that causes `NewRouter` to
`mount(mux, h, "GET /metrics", principal.ScopeMetrics, ...)`
(`server.go:208-215`, `219-232`). **This is the exact, already-proven
pattern to copy** for wiring `/healthz`/`/readyz` — a new
`api.WithHealthEndpoint(...)`-shaped `RouterOption`, called from
`main.go` at the same point `WithMetricsEndpoint` is called today. There
is currently **no health check of any kind** anywhere in `cmd/api`.

### 4.8 `cmd/worker`: has an HTTP listener already, but it is out of this phase's literal roadmap scope

`cmd/worker/main.go` starts a second, standalone, deliberately
**unauthenticated** `http.Server` on `TASKFORGE_METRICS_ADDR` (default
`:9090`, `main.go:199-216`) serving only `/metrics` — unauthenticated
because the worker's least-privilege PostgreSQL role has no access to
`principals`/`api_keys` (OD-3, Phase 12). No `/healthz`/`/readyz` exists
here. The roadmap's exact scope (§2.1) names health/readiness endpoints
without specifying `cmd/api` vs. `cmd/worker`, but every "why it matters"
citation ([slo.md](slo.md)'s "API availability"/"enqueue availability"
SLIs, [enterprise-readiness.md](enterprise-readiness.md) §4's "no
health/readiness endpoints") is framed around the API server specifically.
See OD-E (§29) for the resolved scope decision.

### 4.9 `go.mod`: Go 1.25.0 (toolchain 1.26.8), no OTel dependency present

Direct dependencies today: `fergusstrange/embedded-postgres`,
`google/uuid`, `jackc/pgx/v5`, `prometheus/client_golang`,
`prometheus/client_model`, `stretchr/testify`. Adding an OpenTelemetry SDK
is a **genuinely new dependency** for this project — the first
non-Postgres, non-Prometheus, non-test dependency since Phase 1. It is
covered automatically by Phase 10's repo-wide CI gates (`govulncheck`,
CodeQL, Dependabot, dependency-review) with no new workflow file needed
(those gates already scan every dependency in `go.sum`, per
[supply-chain-security.md](supply-chain-security.md)).

### 4.10 `docs/idempotency.md`: the existing idempotency-key mechanism cannot be silently reused for DLQ-replay dedup

`InsertIdempotent`'s documented behavior (`idempotency.md:247-252`):
resubmitting the same `(job_type, idempotency_key)` after the mapped job
reaches a terminal state returns that job's **existing terminal**
representation — it never inserts a new row and never reopens the old
one. This means a DLQ-replay design that tried to key the **new** replay
job off the **original** job's idempotency key would not work: the
constraint would resolve straight back to the old, terminal,
`DEAD_LETTERED` row. §6.4 states the resolved design (the replay endpoint
accepts its own, independent, optional `Idempotency-Key` for the *new*
submission, exactly like `POST /jobs` already does — no new dedup
mechanism required).

### 4.11 `docs/scenario-corpus.md`: highest existing scenario is SF-075

Phase 16 scenario numbering starts at **SF-076**.

## 5. Architecture and design overview

Phase 16 adds three independent, loosely-coupled capability groups to the
existing three-component architecture ([architecture.md](architecture.md):
API server, PostgreSQL, worker pool) without changing that architecture's
shape:

```
                    +-------------------+
   HTTP clients --->|     API Server    |----+
   (+ LB/orchestr.  |  + /healthz       |    |
    health probes)  |  + /readyz        |    |
                    |  + HTTP metrics   |    |
                    |    middleware     |    v
                    |  + /jobs/{id}/    |  +------------------+
                    |    history        |  |   PostgreSQL      |
                    |  + /jobs/{id}/    |  |  jobs (+trace_id,  |
                    |    retry          |  |   span_id,         |
                    +-------------------+  |   retried_from),   |
                              |            |  job_attempts       |
                              | submission |  (+trace_id,        |
                              | span       |   span_id)          |
                              v            +------------------+
                     [OTel TracerProvider]           ^
                     (noop by default;               |
                      OTLP exporter if      +-------------------+
                      configured — non-     |   Worker Pool     |
                      load-bearing)         |  + consumer span, |
                              ^             |    linked to      |
                              +-------------|    submission     |
                                execution   |    span via       |
                                span        |    stored         |
                                            |    trace/span id   |
                                            +-------------------+
```

1. **Diagnostic HTTP surfaces on `cmd/api`** (§6.1–6.4): `/healthz`,
   `/readyz`, `GET /jobs/{id}/history`, `POST /jobs/{id}/retry`, plus an
   HTTP-request-metrics middleware applied uniformly at the `mount()`
   choke point (§6.6). These are ordinary additive HTTP work, following
   existing patterns exactly (§4.3, §4.4, §4.7).
2. **A durable trace-context carrier** (§7): two new nullable columns on
   `jobs` and two on `job_attempts`, populated once at submission and once
   per attempt respectively, read-but-never-overwritten thereafter — the
   mechanism the roadmap's §2.1 explicitly asks for ("storing the
   trace/span identifiers alongside the job row").
3. **OpenTelemetry SDK integration** (§11–13): a new, small
   `internal/tracing` package providing a `TracerProvider` constructed
   once per process (mirroring `internal/metrics.New()`'s existing
   initialization shape), threaded into `internal/api`, `txenqueue`, and
   `internal/worker` via functional options (mirroring how `metrics.M`
   is already threaded through `store.New(store.WithMetrics(m))`).
   **Noop by default** (no exporter configured ⇒ zero overhead, zero new
   failure mode) — this is what makes "telemetry failure must remain
   non-load-bearing" true by construction rather than by careful error
   handling.

None of this introduces a new durability boundary, a new trust boundary,
or a new consensus/coordination mechanism. PostgreSQL remains the sole
source of truth (ADR-0001, unchanged); tracing data is diagnostic
metadata riding alongside existing rows, never read by any correctness
path.

## 6. API surface changes

### 6.1 `GET /healthz` — liveness

**Semantics**: returns `200 OK` with a trivial body (e.g. `{"status":"ok"}`)
as long as the HTTP server is accepting connections. No dependency check
(no DB ping) — this is deliberate: liveness answers "is the process
itself alive and able to respond," not "is it fully functional." A
liveness probe that depends on the database would cause an orchestrator
to kill and restart a perfectly healthy process during a transient
database outage, which is exactly the wrong response (the correct
response during a DB outage is *no new pods*, not *pod churn* —
`/readyz`'s job, not `/healthz`'s). Continues answering `200` throughout
`cmd/api`'s existing graceful-drain window (Phase 14) up until
`srv.Shutdown` closes the listener — no new code is needed for this,
since it is registered on the same `http.Server`/mux as every other
route.

**Wiring**: unauthenticated (see OD-D, §29) — mounted via a new,
explicitly-named exception path alongside `mount()`, not through
`requireAuth`.

### 6.2 `GET /readyz` — readiness

**Semantics**: returns `200 OK` only if the most recent database
liveness check succeeded; `503 Service Unavailable` otherwise (during
startup before `db.PingContext` first succeeds, or after the pool loses
connectivity). Implementation: `cmd/api`'s `run()` already holds the
`*sql.DB` pool (`main.go:89`); a `RouterOption` (`api.WithReadinessCheck(func(ctx
context.Context) error)`) is passed a closure wrapping
`db.PingContext(ctx)` with a short (e.g. 2s) timeout — `internal/api`
itself never imports `database/sql` directly (it depends on the `JobStore`
interface, `server.go:25-53`), so the check is injected as a function,
not a concrete DB handle, keeping the existing layering intact.

**Wiring**: unauthenticated, same exception path as `/healthz`.

**Proof obligation** (roadmap §2.4/§2.6): `/readyz` must report `503`
under a real, simulated database outage — proven by reusing
`internal/chaos`'s existing `TerminateBackend`/connection-interruption
primitives (Phase 9) against the pool `/readyz` checks, not a fake/mocked
DB error.

### 6.3 `GET /jobs/{id}/history`

**Semantics**: returns the full, ordered `job_attempts` ledger for one
job (`attempt_number`, `lease_generation`, `worker_id`, `started_at`,
`finished_at`, `outcome`, `error_class`, `error_message` — the existing
columns per [data-model.md](data-model.md); no new column needed for this
endpoint itself). A pure read over already-durable, already-append-only
(TF-INV-007) data.

**New store method**: `Store.GetJobAttempts(ctx, jobID uuid.UUID, authz
principal.AccessContext) ([]job.Attempt, error)`, following §4.4's
pattern exactly: a single SQL statement joining `job_attempts` to `jobs`
on `job_id`, with the ownership predicate (`jobs.principal_id = $n OR
$admin`) in the same statement — not a `GetByID` call followed by a
separate `job_attempts` query, which would reopen a TOCTOU-shaped gap
between the ownership check and the data read (the same reasoning
`server.go:25-53`'s doc comment already states for every other scoped
read).

**Wiring**: `mount(mux, h, "GET "+prefix+"/jobs/{id}/history",
principal.ScopeJobs, ...)`, added inside `registerJobRoutes`, registered
under both the legacy and `/v1` prefixes exactly like the six existing
routes (including the same `deprecationWarning` wrapping on the legacy
prefix, for consistency — this is a **new** route, but the legacy/`/v1`
dual-registration convention applies uniformly to every job/workflow
route this router serves, and there is no reason to special-case a new
one out of it).

**Existence-disclosure discipline** (roadmap §2.4, "exactly as sensitive
as `GET /jobs/{id}` itself"): a request for a nonexistent job and a
request for another principal's job must return the byte-identical `404`
body, exactly as `GetByID` already proves (`internal/api/handlers_phase12_test.go`'s
`TestGetJob_CrossPrincipal_IndistinguishableFromNonexistent` pattern,
extended to this new endpoint).

### 6.4 `POST /jobs/{id}/retry` — DLQ replay

**Semantics**: given a `DEAD_LETTERED` job's ID, creates a **new** job row
— fresh `id`, `state = QUEUED`, `attempt_count = 0`, `eligible_at =
now()` — copying `job_type`, `payload`, `priority`, `max_attempts`,
`execution_timeout_seconds`, and `queue_name` from the original, and
recording `retried_from = <original job id>` on the new row.
`principal_id` on the new row is set to the **original job's**
`principal_id` (not necessarily the replaying actor's — see below), so
the replayed job remains attributed to, and discoverable/cancellable by,
the same tenant the original belonged to. **The original `DEAD_LETTERED`
row is never modified** (TF-INV-005 — see §21).

**Who may call it**: the owning principal, or an admin principal — the
identical `AccessContext`-in-SQL-WHERE pattern as every other scoped
operation (§4.4). Reading the original job to authorize the replay and
copy its fields uses the *existing* `GetByID`-shaped ownership predicate;
no new authorization primitive is introduced.

**Preconditions, explicit and tested** (new acceptance criteria, not a
new invariant — see §21): the target job must currently be
`DEAD_LETTERED`. Calling this endpoint against a job in any other state
returns `409 Conflict` (a well-formed, documented rejection, not a
silent no-op and not a 500) — a replay of a still-active or
already-succeeded/cancelled job is a caller error, not an idempotent
retry-request pattern like `POST /jobs/{id}/cancel`'s.

**Idempotency of the replay request itself** (resolves §4.10's open
question): `POST /jobs/{id}/retry` accepts an **optional**
`Idempotency-Key` request header, exactly like `POST /jobs` already does
— scoped to `(principal_id, job_type, idempotency_key)` on the **new**
row via the existing `InsertIdempotent` mechanism, completely unchanged.
This deliberately does **not** invent a new dedup primitive keyed off
`retried_from`: a caller who wants "calling retry twice creates at most
one replay job" already has the tool (supply the same
`Idempotency-Key` on both calls); a caller who does not supply one gets
today's ordinary at-most-once-per-explicit-call semantics (calling retry
twice creates two independent replay jobs — no worse, and no different in
kind, than calling `POST /jobs` twice without a key). This is the
**closed** resolution to the design question the exploration pass for
this plan flagged as open — see OD-C (§29).

**New store method**: `Store.RetryDeadLettered(ctx, originalJobID uuid.UUID,
authz principal.AccessContext, idempotencyKey *string) (*job.Job, error)`
— internally: (1) read-and-authorize the original row via the existing
ownership predicate, rejecting with `404` (not `403`) if not found or not
owned, exactly like every other scoped read; (2) reject with `409` if
`state != 'DEAD_LETTERED'`; (3) construct a new `job.Job` copying the
named fields, setting `RetriedFrom = &originalJobID`; (4) call the
existing `InsertIdempotent` with that constructed job — the same INSERT
path `POST /jobs` already uses, reused verbatim, not a parallel insert
mechanism.

**Wiring**: `mount(mux, h, "POST "+prefix+"/jobs/{id}/retry",
principal.ScopeJobs, ...)`, same dual-prefix pattern as §6.3.

### 6.5 Metric split: `taskforge_claim_latency_seconds` gains an `origin` label

**Resolved design** (closes the gap identified in §4.6): add a new label
`origin` ∈ `{"queued", "retry_wait", "reclaimed"}` to
`taskforge_claim_latency_seconds` only, computed from the `oldState`
parameter `recordClaim` already receives — `queued` for
`jobstate.Queued`, `retry_wait` for `jobstate.RetryWait`, `reclaimed` for
an expired-lease `RUNNING` source row. `taskforge_queue_age_seconds`
remains unlabeled and unchanged, continuing to answer "how long did this
specific claim wait past eligibility, period" — the single number an
operator wants for a generic queue-depth/backlog dashboard.

**Why a label, not a second histogram**: this is additive under
[compatibility-policy.md](compatibility-policy.md) rule 4 ("only adds new
metrics or new labels on existing ones") — no existing series' meaning
changes, no existing alert/dashboard built against the unlabeled
aggregate breaks (Prometheus's own aggregation still answers the
old question via `sum(rate(taskforge_claim_latency_seconds_bucket[...]))`
across all `origin` values). Three low-cardinality enum values match this
project's existing cardinality discipline (`outcome`, `state` labels
already work this way) — no new audit exemption is needed in
`TestCardinality_NoMetricLabelIsIdempotencyKeyOrJobID`'s sibling tests, but
that test suite gains one new assertion confirming `origin` is bounded to
exactly three values.

**What this closes concretely**: [slo.md](slo.md)'s "Queue wait latency"
row states the gap as "TaskForge cannot currently distinguish 'waiting
because no worker is free' from 'waiting because of `RETRY_WAIT` backoff'
... without also joining on `taskforge_retry_count`" (which cannot be
joined at all — it carries no per-job key). After this change,
`taskforge_claim_latency_seconds{origin="queued"}` answers "worker fleet
capacity relative to fresh submission load" directly, with no join
required.

### 6.6 HTTP-request-domain metrics

**New metrics**: `taskforge_http_requests_total{route, method, status}`
(counter) and `taskforge_http_request_duration_seconds{route, method}`
(histogram). **`route` is the registered pattern, not the raw path** —
Go's `net/http.ServeMux` (in use since Phase 1, confirmed unchanged) has
exposed a matched pattern via `http.Request.Pattern` since Go 1.22; this
project's `go.mod` already pins Go 1.25.0/toolchain 1.26.8, so no new Go
version requirement is introduced. Using the pattern (e.g. `"GET
/v1/jobs/{id}"`) rather than the interpolated path keeps cardinality
bounded to the fixed set of registered routes, regardless of how many
distinct job UUIDs are ever requested — the same cardinality discipline
[observability.md](observability.md) already applies to every other
label in this codebase.

**Wiring**: applied **once**, at the `mount()` choke point
(`server.go:244-246`) itself, wrapping every handler passed through it in
a small metrics-recording `http.HandlerFunc` before it reaches
`requireAuth`. Because `mount()` is the *sole* attachment point for every
route in this router (§4.3, mechanically enforced), this automatically
and uniformly instruments every existing route (all six job/workflow
routes, `/metrics`) and every route this phase adds (`/jobs/{id}/history`,
`/jobs/{id}/retry`) with zero per-route wiring, and — because it wraps at
the mux-attachment level rather than inside `requireAuth` — it also
records requests that fail authentication (status `401`/`403`), which is
exactly the data an "API availability" SLI (per [slo.md](slo.md)) needs:
a flood of unauthenticated requests is still API traffic, and its
rejection rate is part of "API availability," not invisible to it.

`/healthz`/`/readyz` are deliberately **excluded** from this
instrumentation's route label space by virtue of being mounted through
the new, separate unauthenticated path (§6.1/§6.2, OD-D) rather than
`mount()` — an orchestrator's health-probe traffic (potentially every few
seconds, from every replica) should not pollute an "API availability"
SLI computed from business-endpoint traffic. If operators want
probe-traffic visibility, `/healthz`/`/readyz` can be added to the same
metrics wrapper in a later phase; this plan does not do so by default,
to keep the SLI's denominator meaningful from day one.

## 7. Data model / schema changes

One new migration, `0015_add_trace_context_and_retry_link` (the next
sequential number after `0014`), entirely additive:

| Table | New column | Type | Notes |
|---|---|---|---|
| `jobs` | `trace_id` | `text NULL` | W3C Trace Context hex-encoded trace ID (32 hex chars), captured once at submission (the producer/`create` span, §12). Never overwritten by any subsequent claim/retry/reclaim — this is the stable anchor every attempt's consumer span links back to. |
| `jobs` | `span_id` | `text NULL` | W3C hex-encoded span ID (16 hex chars) of the submission span. Paired with `trace_id` above. |
| `jobs` | `retried_from` | `uuid NULL REFERENCES jobs(id)` | The original `DEAD_LETTERED` job this row was replayed from, if any (§6.4). `NULL` for every ordinarily-submitted job. |
| `job_attempts` | `trace_id` | `text NULL` | The trace ID of *this attempt's* consumer/`process` span. Ordinarily identical to the owning job's `jobs.trace_id` (same trace, linked not parented — §12), stored per-attempt because [observability.md](observability.md)'s existing "Trace Boundaries" design already specifies one span per `job_attempts` row. |
| `job_attempts` | `span_id` | `text NULL` | This attempt's own consumer span ID — distinct per attempt, even though `trace_id` is shared with the parent job and across sibling attempts. |

**Why text, not a dedicated UUID/bytea type**: OTel's Go SDK represents
`TraceID`/`SpanID` as fixed-length byte arrays with a canonical
lowercase-hex `String()` form; storing the hex string directly avoids a
TaskForge-side encode/decode step and is directly pasteable into any
trace-backend UI/query without transformation. `text` also costs nothing
extra when `NULL` (the common case for any job submitted with tracing
disabled — the noop-tracer default, §5 point 3).

**Migration lock profile** (following the discipline
[data-model.md](data-model.md)'s "Phase 12/13 migration lock profile"
sections established): every new column is nullable with no default,
which — per the same PostgreSQL 11+ behavior already measured and cited
for migration `0011`'s `NOT NULL DEFAULT` column add — requires no table
rewrite. A nullable `ADD COLUMN` with **no default at all** is even
cheaper than `0011`'s case: it is catalog-only regardless of default
volatility, since there is no default to backfill or validate. The
`retried_from` foreign key does **not** require a `VALIDATE CONSTRAINT`
step against existing data the way Phase 12's `principal_id NOT NULL`
constraint did (`data-model.md` §"Phase 12 migration lock profile") —
because the column is nullable, every existing row already satisfies the
constraint trivially (`NULL` requires no referential check), so a plain
`ADD COLUMN ... REFERENCES jobs(id)` needs no `NOT VALID`/`VALIDATE`
two-step. **Expected lock profile**: `ACCESS EXCLUSIVE`, catalog-only,
O(1) regardless of table size — the same class as migrations `0006`,
`0008`, `0010`, `0014`. This must still be **measured**, not merely
asserted, per this project's own "measure before documenting" discipline
(`data-model.md`'s explicit self-correction after two retracted claims) —
a `TestMigration0015_AddColumnsAreFastRegardlessOfTableSize`-shaped test
is required in the proof matrix (§22), mirroring
`TestPhase13Migration0011_AddColumnIsFastRegardlessOfTableSize`.

**Down-migration classification**: `data-safe-reversible`. Dropping five
nullable, diagnostic-only columns destroys no information any other
invariant or feature depends on (unlike, say, dropping `principal_id`
would) — a down-then-up-again round trip is expected to reproduce an
identical schema, extending `TestMigrations_SF060_DataSafeReversibleSubsetRoundTripsCleanly`
(`internal/migrate/reversibility_test.go`) to cover migration `0015`.

**No index is added on `retried_from`** in this plan's initial design: an
operator query "find the replay chain for job X" is expected to be rare,
manual, diagnostic traffic (an incident investigation, §16), not a
claim-path or hot-path query — see §20's performance discussion for why
an unindexed foreign key is an acceptable default here, distinct from
`idx_jobs_principal_id`'s justification (a column touched by every scoped
read).

## 8. Operator workflows

Concrete, named workflows this phase must make possible — each is a
proof-matrix entry (§22), not merely a design aspiration:

1. **"Is the API up?"** — an orchestrator/load-balancer polls
   `GET /healthz` on an interval; a non-`200` (connection refused, once
   the listener closes) triggers routing traffic away from this replica.
2. **"Is the API ready to serve traffic?"** — the same orchestrator uses
   `GET /readyz` to gate whether a freshly-started replica receives
   traffic at all, and to detect an already-running replica that has lost
   its database connection (should be pulled from rotation, not killed —
   `/healthz` staying green during a DB outage is correct; `/readyz`
   going red is what should drive the routing decision).
3. **"Why did job X fail, and how many times did we try?"** — an operator
   calls `GET /jobs/{id}/history`, sees the full `job_attempts` ledger
   (every `outcome`/`error_class`/`error_message`/`worker_id`/timestamps),
   without needing direct database access — closing the P1 gap
   [enterprise-readiness.md](enterprise-readiness.md) §4 names explicitly
   ("no `GET /jobs/{id}/history` endpoint... must query the database
   directly").
4. **"This job is dead-lettered but the underlying issue is fixed — run it
   again"** — an operator (or their own automation) calls `POST
   /jobs/{id}/retry`, gets back a new job ID, and can track the new job
   exactly like any other submission (`GET /jobs/{newID}`), with
   `retried_from` visible on the new row's representation for audit
   purposes.
5. **"This job took an unusually long time between submission and
   execution — was it a busy queue, a stuck worker, or something else?"**
   — an operator scrapes `taskforge_claim_latency_seconds{origin=...}` to
   distinguish "genuinely waiting for capacity" from "backoff after
   repeated failure," and/or follows the job's trace (if tracing is
   configured) from its submission span through every linked attempt
   span in their trace backend of choice, seeing exactly how long the
   queue-wait gap was and what happened in each attempt — see §16 for the
   full worked incident-investigation example.
6. **"Are we seeing elevated API error rates right now?"** — an operator
   builds a dashboard/alert directly from
   `taskforge_http_requests_total{status=~"5.."}` for the first time,
   closing the "API availability" SLI gap [slo.md](slo.md) names as
   currently uncomputable from any existing metric.

## 9. Diagnostic surfaces (summary)

| Surface | New in Phase 16? | Mechanism |
|---|---|---|
| Structured logs (`job_id`-correlated) | No — Phase 8, unchanged | `internal/worker`/`internal/store`/`internal/api` `*slog.Logger` fields |
| Prometheus metrics (job-domain) | No — Phase 8, unchanged | `internal/metrics` |
| Prometheus metrics (HTTP-domain) | **Yes** | §6.6 |
| Prometheus metrics (claim/queue-wait distinguishability) | **Yes** (label add) | §6.5 |
| `GET /metrics` | No — Phase 8/12, unchanged | Existing |
| `GET /healthz` | **Yes** | §6.1 |
| `GET /readyz` | **Yes** | §6.2 |
| `GET /jobs/{id}` | No — Phase 1/12, unchanged | Existing |
| `GET /jobs/{id}/history` | **Yes** | §6.3 |
| `POST /jobs/{id}/retry` | **Yes** | §6.4 |
| Distributed traces (span links, submission↔execution) | **Yes** | §11–§13 |
| Durable trace-context columns | **Yes** | §7 |

## 10. Tracing requirements (roadmap-mandated)

Per §2.1, tracing is **required** by this phase, not optional. The exact
requirements, restated with this plan's resolved design:

- **SDK**: OpenTelemetry Go SDK (`go.opentelemetry.io/otel`,
  `.../sdk/trace`, `.../trace`), a new direct dependency (§4.9).
- **Span shape**: **links, not parent-child**, exactly per the roadmap and
  [reference-analysis.md](reference-analysis.md)'s citation of OTel's own
  messaging semantic conventions (`create`/`send` producer, `process`
  consumer) — this is the architecturally correct pattern for a queue-wait
  gap of unbounded length, and this plan does not reconsider it; it is
  settled by the roadmap itself.
- **Two span kinds**:
  1. **Submission span** (producer, `create`): started in
     `internal/api`'s `CreateJob`/`CreateWorkflow` handlers and in
     `txenqueue.EnqueueTx` (§11) — ends at the same point TF-INV-001's
     "commit before response" boundary already exists (§13), never
     before.
  2. **Execution/attempt span** (consumer, `process`): one per
     `job_attempts` row (matching [observability.md](observability.md)'s
     pre-existing "Trace Boundaries" design, written before this phase
     existed), started in `internal/worker.RunOnce` immediately after a
     successful claim, linked (via `trace.Link`, not
     `trace.ContextWithSpanContext`-as-parent) to the submission span
     reconstructed from `jobs.trace_id`/`jobs.span_id`.
- **Semantic-convention version pinning**: this plan does **not** hardcode
  a specific OTel semantic-conventions module version or commit — see
  §29 OD-F for why that decision is deliberately deferred to
  implementation time, not invented here.
- **Attribute compatibility boundary**: whatever attribute names/values
  OTel's messaging semconv specifies are **not** part of
  [compatibility-policy.md](compatibility-policy.md)'s guarantees — a
  future semconv revision renaming an attribute is not a TaskForge
  breaking change. This must be stated in the span-emitting code's own
  doc comment and in [observability.md](observability.md)'s update (§27),
  not left implicit.

## 11. Correlation / context propagation

**The durable-association mechanism** (roadmap's explicit ask): `jobs.trace_id`/`jobs.span_id`
are written exactly once, in the same transaction that inserts the job
row (`InsertIdempotent`, extended to accept an optional
`TraceContext{TraceID, SpanID}` on the `job.Job` it inserts) — this
mirrors how `principal_id` is written exactly once, from the authenticated
credential, never from a request body (§ data-model.md's own framing for
`principal_id`). They are **read, never rewritten**, by every subsequent
claim: `Claim`/`ClaimFromQueues`'s `RETURNING jobs.*` already returns the
full row (`worker-protocol.md`'s claim query, unchanged), so the claiming
worker receives `TraceID`/`SpanID` for free, with no extra query.

**Propagation across a retry/reclaim** (the roadmap's explicit proof
obligation: "this linkage survives at least one retry/reclaim cycle"):
because `jobs.trace_id`/`span_id` are never overwritten by `Claim`,
**every** attempt — the first, and every subsequent `RETRY_WAIT`→`RUNNING`
reclaim — links back to the **same** submission span. This is the direct
payoff of choosing "durable columns on the job row" over an
in-memory-only or per-attempt-only carrier: an in-memory carrier cannot
survive a worker crash-and-reclaim by a *different* worker process (which
has no shared memory with the crashed one), and a per-attempt-only
carrier (storing trace context only on `job_attempts`, never on `jobs`)
would lose the stable anchor a second/third attempt needs to link back to
the *original* submission rather than to the *previous attempt* (which
the roadmap does not ask for — the messaging semconv pattern links every
consumer span back to the producer span, not attempt-to-attempt).

**Propagation into the handler**: the worker's `execCtx`
(`worker.go:542`) — the exact context passed to `h.Execute(execCtx, j)` —
carries the attempt span via `trace.ContextWithSpan(execCtx, attemptSpan)`,
so a job handler that itself makes further OTel-instrumented calls
(an HTTP request to another service, say) automatically produces
*child* spans of the attempt span, using OTel's ordinary parent-child
propagation for that *bounded*, in-process portion of the work — the
"links, not parent-child" rule applies specifically to the
submission-to-attempt boundary (the unbounded queue-wait gap), not to
ordinary in-process call chains within one attempt's execution, which
behave like any other OTel-instrumented Go program.

**What is explicitly NOT propagated, and why**: `payload` and
`result_metadata` are never placed in a span attribute — see §17.
`Idempotency-Key`'s raw value is never placed in a span attribute — only
a boolean equivalent to `had_idempotency_key`, mirroring the exact
existing log-field discipline (§17).

## 12. Logs / metrics / traces relationship

This phase adds a third observability axis; it does not replace or
weaken the first two. Restated explicitly, since
[observability.md](observability.md) itself will need this update:

- **Logs remain the primary, always-on correlation mechanism**, keyed on
  `job_id` — every lifecycle log line already carries it
  ([observability.md](observability.md), unchanged). This works even when
  tracing is disabled (the default, noop-tracer posture, §5) and is what
  an operator without a trace backend configured still has.
- **Metrics remain the alerting/dashboarding/SLI layer** — aggregate,
  low-cardinality, always-on (no opt-in required), cheap to scrape.
  Nothing in this phase changes that role; HTTP-domain metrics and the
  claim-latency label extend it, they do not compete with it.
- **Traces are the new, opt-in, high-fidelity correlation layer**,
  specifically for the one problem logs and metrics answer poorly: "what
  is the complete timeline of one specific job's queue-wait-then-retries
  shape, across an unbounded and highly variable duration." A trace
  backend can render this as a literal timeline in a way that grepping
  structured logs for a `job_id` cannot (logs give you the events; a
  trace gives you the shape). Traces require an operator to have deployed
  and pointed TaskForge at an OTLP collector — they are the only one of
  the three axes that is off by default.
- **The bridge between the three**: every span (submission and every
  attempt) carries a `taskforge.job_id` attribute (namespaced, per OTel's
  own guidance for non-semconv-reserved keys — not a bare `job_id`
  attribute, to avoid an unintentional future collision with an OTel
  semantic-convention key of the same bare name). This lets an operator
  go from "I found this span in my trace backend" to "here is the exact
  `job_id` to grep in my log aggregator or query in `job_attempts`" in one
  step, and vice versa — the three axes are cross-referenced by a shared
  identifier, never merged into one system.

## 13. API and worker diagnostic requirements

**API server** (`internal/api`, `txenqueue`):
- Every one of the three submission entry points — `POST /jobs`, `POST
  /workflows`, and `txenqueue.EnqueueTx` (Phase 11's caller-owned-transaction
  path) — must start a producer span and persist its `trace_id`/`span_id`
  onto the inserted row(s). A workflow's every node job gets its own
  submission span (each node is an ordinary job for tracing purposes,
  exactly as it already is for metrics — §3's non-goals), not one shared
  span for the whole workflow.
- The submission span must end **only after** the insert transaction
  commits — mirroring TF-INV-001's own "response only after commit" rule.
  A rolled-back submission (Phase 11's transactional-enqueue rollback
  case, `txenqueue`) must record its span as ended-without-a-successful-job
  (an OTel span status of `Error`, or simply an attribute noting no row
  was created), never fabricate a span implying a job exists when
  TF-INV-001 says it does not.
- `GET /jobs/{id}` and the new `GET /jobs/{id}/history` should, on a
  successful response, include the job's `trace_id` (if non-null) in the
  response body — so a caller who has a `job_id` can find the
  corresponding trace without a separate lookup. This is an additive
  response field (compatible per [compatibility-policy.md](compatibility-policy.md)'s
  unknown-field-tolerance proof, Phase 11).

**Worker** (`internal/worker`):
- `RunOnce`, immediately after a successful claim, must reconstruct a
  `trace.SpanContext` from the claimed job's `TraceID`/`SpanID` (if
  present — a job submitted before tracing was enabled, or with tracing
  disabled, has `NULL` values and gets no span, not an error) and start a
  consumer span linked to it.
- `runWithHeartbeat`'s terminal disposition (success/retryable-
  failure/permanent-failure/timeout/cancelled/lease-lost) must end the
  attempt span with a status reflecting that outcome, and must persist
  the attempt span's own `trace_id`/`span_id` onto the corresponding
  `job_attempts` row in the same call that already writes
  `finished_at`/`outcome` (no extra query — this data is already in hand
  at that point).
- Span end must **never** be on the critical path of the fenced
  `Complete*` store call — the span is ended (queued into the SDK's own
  batch exporter) either just before or just after the store call
  commits, but the store call's own success/failure is never gated on
  span-export success. This is what "telemetry failure must remain
  non-load-bearing" means concretely for the worker (§14).

## 14. Failure / incident investigation workflow (worked example)

A concrete walkthrough, because "operator diagnostics" is easiest to
validate against a real scenario:

> **Symptom**: A customer reports "my job `a1b2c3d4-...` seems stuck; I
> submitted it 20 minutes ago and it still isn't done."
>
> 1. Operator calls `GET /jobs/a1b2c3d4-.../history` (§6.3). Sees three
>    `job_attempts` rows: attempt 1 `FAILED_RETRYABLE` (`error_class:
>    "upstream_timeout"`), attempt 2 `LEASE_EXPIRED` (worker crashed
>    mid-execution), attempt 3 currently in flight (`finished_at` is
>    `NULL`).
> 2. Operator checks `taskforge_claim_latency_seconds{origin="retry_wait"}`
>    (§6.5) — p99 is elevated, confirming this job's long apparent wait is
>    consistent with fleet-wide backoff pressure, not a fluke specific to
>    this job.
> 3. If tracing is configured, the operator opens the job's `trace_id`
>    (returned on the `GET /jobs/{id}` response, §13) in their trace
>    backend and sees the full timeline: submission span, then three
>    linked attempt spans with real wall-clock gaps between them — visibly
>    distinguishing "waiting for backoff" from "actively executing" in a
>    way the `job_attempts` table's timestamps alone require manual
>    arithmetic to reconstruct.
> 4. Root cause identified (an upstream dependency timing out). Once
>    fixed, if the job had instead reached `DEAD_LETTERED`, the operator
>    would call `POST /jobs/{id}/retry` (§6.4) to resubmit it with a fresh
>    `job_id`, and could track `retried_from` to link the new attempt back
>    to this investigation for audit purposes.
> 5. Throughout, `GET /readyz` and `taskforge_http_requests_total` confirm
>    the API server itself was never the bottleneck — ruling out an API
>    server-side incident and keeping the investigation focused on the
>    worker/claim path, exactly the kind of fast elimination
>    HTTP-domain metrics exist to enable.

## 15. Security / privacy implications

- **No new trust boundary.** Tracing data flows from TaskForge processes
  to an operator-configured OTLP endpoint — the same category of
  operator-controlled, TaskForge-cannot-verify-from-inside-the-process
  deployment obligation already established for the TLS-proxy boundary
  and the worker metrics listener ([security-model.md](security-model.md)
  §4/§5). This plan documents that obligation; it does not build
  authentication for the exporter's own transport (OTLP's own TLS/mTLS
  support, if the operator's collector requires it, is a standard OTel
  SDK exporter configuration option, not new TaskForge code).
- **`/healthz`/`/readyz` are intentionally unauthenticated** (§6.1/§6.2,
  OD-D) — this is a deliberate, narrow exception to Phase 12's
  deny-by-default posture, justified because (a) they reveal no
  job/tenant data whatsoever (a boolean liveness/readiness signal only),
  and (b) load balancers and orchestrators universally expect
  unauthenticated health probes. This exception must be enumerated by
  name in the route-registration test (§22), not left as an undetected
  gap in `TestRouter_AllHandlersMountedThroughAuth`'s coverage.
- **`GET /jobs/{id}/history` and `POST /jobs/{id}/retry` carry the same
  sensitivity as `GET /jobs/{id}`** (roadmap §2.1, restated) — both can
  reveal `error_message` (potentially containing details about a caller's
  own payload-processing failure) and enable resource mutation
  (creating a new job) respectively. Both inherit the existing
  ownership-scoping and existence-disclosure discipline unchanged (§6.3,
  §6.4).
- **DLQ replay must not become a tenant-boundary-crossing primitive**
  ([security-model.md](security-model.md) §7's explicit forward-looking
  note: "the same reasoning applies to... DLQ-replay operations (Phase
  16): a replay or resubmission keyed only by `job_type`/`idempotency_key`/
  `job_id`, without a tenant scope, could cross a tenant boundary...
  Design requirement... before it ships"). This is satisfied by §6.4's
  design: the new job's `principal_id` is copied from the **original**
  job's `principal_id` (read under the existing ownership check), never
  from the replaying admin's own identity — an admin replaying tenant A's
  job produces a job still owned by tenant A, not by the admin.

## 16. Sensitive-data / redaction requirements

Extending [observability.md](observability.md)'s existing, tested
discipline ("no arbitrary payload body is ever logged"; raw
`Idempotency-Key` never logged) to the new tracing surface, since a span
attribute is exactly as durable and exactly as exposed to a third-party
backend as a log line, arguably more so (an OTLP exporter ships data
off-host by design):

- **Never** place `jobs.payload` or `jobs.result_metadata` (opaque,
  potentially caller-sensitive per [security-model.md](security-model.md)
  §3) in any span attribute, event, or name.
- **Never** place the raw `Idempotency-Key` value in a span attribute —
  only a `taskforge.had_idempotency_key` boolean, mirroring the exact
  existing log-field name/semantics.
- **Never** place a raw API credential, `key_id`, or the pepper in any
  span attribute — principals are represented only by `taskforge.principal_id`
  (a non-secret identifier), exactly mirroring the existing `actor` log
  field's discipline ([observability.md](observability.md) "actor
  (Phase 12)").
- **Never** place `error_message` free text from a `job_attempts` row
  verbatim into a span attribute without the same consideration already
  given to logs — `error_class` (a bounded, structured category) is safe;
  `error_message` may itself embed caller-supplied context and should be
  treated with the same caution `observability.md` already applies to it
  in logs (this plan recommends including `error_class` on attempt spans
  and treating `error_message` inclusion as an explicit, reviewed
  decision at implementation time, not a default).
- **New audit test category, mirroring existing ones**: a
  `TestSpanAttributes_NeverContainPayloadIdempotencyKeyOrCredential`-shaped
  test, analogous to `TestCardinality_NoMetricLabelIsIdempotencyKeyOrJobID`
  (metrics) and the existing raw-secret log-output audit tests
  (`internal/api/observability_test.go`), using a test-exporter to
  capture every span attribute ever recorded across the existing
  scenario-corpus test suite and asserting none contains a forbidden
  value.

## 17. Configuration implications

New environment variables, following `internal/config`'s existing
`FromEnv`-style pattern (`internal/config/config.go`):

| Variable | Default | Meaning |
|---|---|---|
| `TASKFORGE_OTEL_EXPORTER_ENDPOINT` | unset (empty) | OTLP endpoint to export spans to. **Unset ⇒ noop `TracerProvider`** — tracing fully disabled, zero overhead, zero new failure mode. This is the safe-by-default posture (§5 point 3). |
| `TASKFORGE_OTEL_SERVICE_NAME` | `"taskforge-api"` / `"taskforge-worker"` (per binary) | The `service.name` resource attribute OTel's own conventions expect. |
| `TASKFORGE_API_READINESS_TIMEOUT` | e.g. `2s` | Timeout for `/readyz`'s `db.PingContext` call — bounded so a hung database connection cannot hang the readiness probe itself. |

No existing environment variable's meaning changes. No existing default
behavior changes for a deployment that sets none of the above — this is
the concrete mechanism by which "Phase 1–15 guarantees preserved" and
"telemetry non-load-bearing" are simultaneously true: an operator who
does nothing gets identical behavior to today, plus three new endpoints
and two new metric families that cost a few extra `mount()`-wrapped
handlers and a handful of `Observe()`/`Inc()` calls already proven cheap
by Phase 8's own "recording a metric... cannot fail, block on a network
call, or hold a database transaction open" design
([observability.md](observability.md) "Telemetry failure behavior").

`cmd/worker`'s existing `TASKFORGE_METRICS_ADDR`/drain-timeout
configuration is unchanged; if OD-E (§29) is accepted, one additional
route is added to that existing listener, not a new listener or new
environment variable.

## 18. Compatibility implications

- **All five new HTTP surfaces are additive** — no existing route's
  request/response shape, status-code semantics, or behavior changes.
  `/healthz`/`/readyz` are new, unversioned, infrastructure-only paths
  (deliberately **not** placed under `/v1/` — health/readiness probes are
  platform contracts, not business-API surface, and every production
  convention this project could plausibly follow — Kubernetes, most load
  balancers — expects a fixed, well-known path rather than a versioned
  one). `GET /jobs/{id}/history` and `POST /jobs/{id}/retry` **are**
  placed under both the legacy and `/v1/` prefixes, exactly like every
  other job/workflow route, for the same dual-prefix consistency reason
  §6.3 states.
- **The new response field** (`trace_id` on `GET /jobs/{id}` /
  `/jobs/{id}/history`, §13) is additive per Phase 11's own proven
  unknown-field-tolerance guarantee
  (`TestCreateJob_UnknownFieldsAreIgnored_RoundTrip`'s sibling discipline
  for responses) — an old client that does not know about this field
  ignores it, per [compatibility-policy.md](compatibility-policy.md)'s
  existing "documented clients are expected to ignore unknown response
  fields" rule.
- **The new metric label (`origin`) and new metric names** are additive
  per [compatibility-policy.md](compatibility-policy.md) rule 4, verified
  in §6.5/§6.6 above — no existing metric name is repurposed.
- **The new migration is additive/nullable** (§7) — an old `cmd/api`/`cmd/worker`
  binary that has not been rebuilt to know about `trace_id`/`span_id`/
  `retried_from` continues to function unchanged against the new schema,
  exactly like every prior additive migration in this project's history
  (Phase 12/13's own two-binary-version proof already established this
  pattern generally; this migration follows it, it does not need its own
  new two-binary-version test — see §23 for why).
- **No breaking change of any kind.** No version-prefix bump is required
  (this phase adds capability under the existing `/v1/` contract, per
  [compatibility-policy.md](compatibility-policy.md)'s "additive changes
  do not require a version bump" rule).

## 19. Performance / overhead considerations

- **Noop-tracer default (§5, §17) is the primary overhead mitigation**:
  OpenTelemetry's own SDK guarantees a noop `Tracer`'s `Start()` call is a
  cheap no-op (allocates a no-op span, does not touch the exporter
  pipeline at all) — this is a documented property of the SDK itself, not
  something this project needs to independently re-verify before
  deciding the architecture (distinguishing this from Phase 15's OD-1/
  OD-2, which *did* require empirical verification of an unknown, `embedded-postgres`-specific
  behavior — see §29 for why Phase 16 has no equivalent empirical
  prerequisite).
- **When tracing is enabled**, the added cost per attempt is: one
  `Tracer.Start()` call, one `trace.Link` construction from stored hex
  strings, and one span-end/export enqueue — all in-memory,
  batched-and-asynchronous by the SDK's own `BatchSpanProcessor` design
  (spans are queued and exported off a background goroutine on a timer/
  batch-size trigger, never synchronously per-span). This must still be
  **measured**, not assumed, as part of this phase's own proof matrix
  (§22): a `go test -bench` comparing `RunOnce` throughput with tracing
  disabled vs. enabled-against-a-local-collector, committed as evidence,
  following this project's "measure before documenting" discipline. This
  benchmark is implementation-time proof, not a blocking prerequisite
  before implementation begins (§29 explains this distinction).
- **HTTP-metrics middleware overhead** (§6.6): two `Observe`/`Inc` calls
  per request, on the same in-memory `prometheus.Registry` every other
  metric already uses — the same "cannot fail, cannot block" cost profile
  Phase 8 already established, extended to the HTTP layer.
- **New nullable columns** (§7) add negligible per-row storage (two
  `NULL`-by-default `text` columns and one `NULL`-by-default `uuid` on
  `jobs`; two `NULL`-by-default `text` columns on `job_attempts`) and are
  not part of the claim query's `WHERE`/`ORDER BY` predicate
  (`idx_jobs_claimable` is unaffected) — no claim-path performance
  regression is expected or introduced.
- **Migration duration**: per §7, expected O(1) regardless of table
  size (catalog-only `ADD COLUMN`, no default, no validation scan) —
  unlike Phase 12's `0009`/Phase 13's `0012`, this migration is expected
  to **not** block the claim query, and this expectation is itself a
  measured proof-matrix item (§22), not an assumption.

## 20. Invariant mapping

Following the exact method [invariants.md](invariants.md)'s "Cross-Phase
Governance Additions" section established (and the "Phase 14 reviewed" /
"Phase 15 reviewed" precedent it set): **Phase 16 adds no new numbered
`TF-INV-*` invariant.** Every property this phase must prove is either a
proof obligation against an *existing* invariant (a breadth requirement
under a new condition) or a request-time/operational concern that — per
the same method already applied to Phase 12's G1/G3/G4/G5/G6/G8 — does
not qualify as a durable state-machine safety property.

| Candidate | Disposition | Why |
|---|---|---|
| Trace-context association is present/correct/complete | **Not an invariant, by explicit roadmap statement.** | The roadmap's own §2.1 states trace context "must never become correctness-authoritative" — this is the opposite of an invariant by definition; TaskForge's state machine must behave identically whether tracing is enabled, disabled, or produces garbage. |
| A replayed job is a **new** row, never a reopened terminal row | **Proof obligation against TF-INV-005.** | This is exactly the same category [invariants.md](invariants.md) already used for Phase 14's "graceful-drain grace period" entry: `RetryDeadLettered` (§6.4) creates a fresh row via the existing `InsertIdempotent` path; it contains no code path that could `UPDATE` the original row's `state`. TF-INV-005's existing durable checks (`internal/invariant`) already detect any reopening, from any code path, with no Phase-16-specific extension needed. |
| A replayed job's idempotency key (if supplied) is tenant-scoped | **Proof obligation against TF-INV-008/016/018.** | The replay endpoint reuses `InsertIdempotent` unchanged (§6.4) — the existing `UNIQUE(principal_id, job_type, idempotency_key)` constraint applies to the new row exactly as it does to any other submission; no new uniqueness rule is introduced. |
| `retried_from` correctly, durably links the new row to the original | **Not an invariant.** Ordinary foreign-key referential integrity, enforced by PostgreSQL's own `REFERENCES` constraint, not a TaskForge-specific state-machine property. | Analogous to `workflow_nodes.job_id REFERENCES jobs(id)` — a schema-level constraint, never elevated to a `TF-INV-*` ID in this project's own precedent. |
| Every new endpoint enforces Phase 12's ownership check | **Proof obligation against TF-INV-017**, extended in breadth. | Identical category to how Phase 14/15 extended breadth across every existing invariant under a new *condition* (mixed binaries, a restored database) — here the new condition is "a new endpoint," and TF-INV-017's existing test *pattern* (zero-row-mutation / indistinguishable-from-nonexistent) is reused, not a new invariant. |
| `/healthz`/`/readyz` correctness | **Not an invariant.** Operational/process-liveness contract, the same category as Phase 14's drain-timeout and shutdown-deadline entries. | A boolean liveness/readiness signal is not a durable row-level state property. |
| Telemetry failure does not affect job execution | **Not an invariant.** Restated operational requirement, same category as Phase 8's "telemetry failure behavior" (already an operational guarantee, never a `TF-INV-*`). | Proven by the SDK's own architecture (noop/batched, §19) plus a dedicated failure-injection test (§24), not a state-machine safety property. |

This section itself becomes a new "Phase 16 reviewed: no new invariant
added" entry in [invariants.md](invariants.md) as part of this phase's
implementation PR (mirroring the existing Phase 14/15 entries verbatim in
structure) — not part of this planning document's own file changes,
consistent with this plan's single-file deliverable scope (see header).

## 21. Test / proof matrix

New scenario IDs starting at **SF-076** (§4.11), extending
[scenario-corpus.md](scenario-corpus.md) and
[testing-strategy.md](testing-strategy.md)'s invariant-to-test matrix:

| Scenario | Proves | Test location (planned) |
|---|---|---|
| SF-076 — Submission span and single-attempt execution span are linked, not parented | Roadmap §2.4 (basic link correctness) | `internal/worker` or a new `internal/tracing` integration test, using an in-memory `sdktrace.SpanRecorder` |
| SF-077 — Span link survives one `RETRY_WAIT`→`RUNNING` reclaim | Roadmap §2.4 ("survives at least one retry/reclaim cycle") | Same harness, extended: two attempts, both linked to the same submission span |
| SF-078 — Span link survives a lease-expiry reclaim (crash simulation) | Roadmap §2.4, extended to the crash path specifically | Reuses `internal/chaos.ForceExpireLease` alongside the span-recorder harness |
| SF-079 — A job submitted with tracing disabled (noop tracer) produces no span and no error | §5/§17's noop-default requirement | Unit/integration test asserting zero exporter activity and unchanged job-completion behavior |
| SF-080 — Telemetry exporter unavailable does not block or fail job execution | Roadmap §2.5 | Failure-injection test (§24) pointing the exporter at an unreachable/blackholed endpoint |
| SF-081 — `/healthz` returns 200 under normal operation and throughout graceful drain | Roadmap §2.4 | `internal/api` integration test + extension of existing Phase 14 drain tests |
| SF-082 — `/readyz` returns 200 when DB reachable, 503 when not | Roadmap §2.4/§2.6 | `internal/chaos.TerminateBackend`-driven integration test |
| SF-083 — `/readyz` returns 503 during startup before the pool is confirmed | Roadmap §2.4 ("false during startup") | Integration test constructing the readiness check before first successful ping |
| SF-084 — `GET /jobs/{id}/history` returns the full, ordered attempt ledger | §6.3 | `internal/api` integration test |
| SF-085 — `GET /jobs/{id}/history` cross-principal request is indistinguishable from nonexistent | Roadmap §2.4 ("exactly as sensitive as `GET /jobs/{id}`"); TF-INV-017 breadth | Mirrors `TestGetJob_CrossPrincipal_IndistinguishableFromNonexistent` |
| SF-086 — `POST /jobs/{id}/retry` creates a new `QUEUED` job with `retried_from` set, original row untouched | Roadmap §2.4; TF-INV-005 breadth | `internal/store` + `internal/api` integration test |
| SF-087 — `POST /jobs/{id}/retry` against a non-`DEAD_LETTERED` job returns 409 | §6.4 acceptance criterion | `internal/api` integration test |
| SF-088 — `POST /jobs/{id}/retry` by a non-owning, non-admin principal is rejected, indistinguishable from nonexistent | Roadmap §2.4; TF-INV-017 breadth | Mirrors existing cross-principal cancel test pattern |
| SF-089 — Two replay calls with the same `Idempotency-Key` produce exactly one new job; without a key, produce two | §6.4's resolved idempotency design; TF-INV-008/016/018 breadth | Extends `TestConcurrentTwoPrincipalIdempotency_ExactlyOneRowPerPrincipal`'s pattern |
| SF-090 — `taskforge_claim_latency_seconds{origin=...}` records the correct origin for a fresh claim, a retry-wait claim, and a reclaimed claim | §6.5 | Extends `internal/store/observability_test.go`'s existing `TestMetrics_SF00*` pattern |
| SF-091 — `taskforge_http_requests_total`/`_duration_seconds` record correctly per route, including for a `401` rejection | §6.6; roadmap §2.6 | `internal/api` integration test scraping `/metrics` after driving requests through the router |
| SF-092 — No span attribute anywhere in the existing scenario-corpus suite contains `payload`, the raw `Idempotency-Key`, or a raw credential | §16 | New audit test, test-exporter capturing every span across a full suite run |
| SF-093 — A route added without going through `mount()`'s auth wrapping (or the named unauthenticated exception list) fails the route-registration test | Roadmap §2.5 ("caught by a lint/test convention") | Extension of `TestRouter_AllHandlersMountedThroughAuth` to enumerate the exact unauthenticated exception set |
| SF-094 — Migration `0015`'s `ADD COLUMN` statements are O(1) regardless of `jobs`/`job_attempts` table size | §7, §19 | Mirrors `TestPhase13Migration0011_AddColumnIsFastRegardlessOfTableSize` |
| SF-095 — Migration `0015` is data-safe-reversible (up→down→up round trip) | §7 | Extends `TestMigrations_SF060_DataSafeReversibleSubsetRoundTripsCleanly` |

Every scenario above must appear in
[testing-strategy.md](testing-strategy.md)'s invariant-to-test matrix
(most rows map to "not an invariant" per §20, and should be recorded
there using the same convention Phase 14/15's non-invariant rows already
use) as part of the implementation PR, not this planning document.

## 22. Real-process proof requirements

**None beyond what already exists.** Per the roadmap's own §2.6 ("A
span-link assertion test using a test/in-memory OTel exporter... HTTP-metrics
scrape test... `/healthz`/`/readyz` tests... reusing Phase 9's chaos
primitives... A DLQ-replay test"), every proof obligation this phase
states is satisfiable by ordinary in-process Go integration tests against
real PostgreSQL (this project's existing, dominant test category) plus an
in-memory OTel span recorder. This phase does **not** require:

- `test/procs`-style real, separately-compiled-binary/real-OS-signal
  testing (Phase 14's category) — tracing/health/history/retry are not
  process-lifecycle concerns.
- `test/dr`-style multi-instance-PostgreSQL testing (Phase 15's category)
  — this phase touches no replication/failover behavior.

The one exception worth naming explicitly: the performance/overhead
benchmark (§19) should run as a real `go test -bench` against a real,
locally-run OTLP collector (e.g., the OTel Collector's own reference
binary, or a minimal test-double gRPC/HTTP server) to produce a genuine
measurement rather than a mocked-exporter number — but this is an
ordinary benchmark, not a new "real-process" test *category* in this
project's `testing-strategy.md` sense (it does not require signals,
multiple binary versions, or multiple PostgreSQL instances).

## 23. Failure-injection requirements

- **`/readyz` under a real database outage**: reuse
  `internal/chaos.TerminateBackend`/the existing constrained-connection-pool
  primitives (Phase 9) — no new chaos primitive is required, since these
  already produce a real, PostgreSQL-boundary connection failure
  `db.PingContext` will observe.
- **Telemetry exporter unavailability**: this is **not** a PostgreSQL-boundary
  fault, so it does not belong in `internal/chaos` (which is scoped to
  PostgreSQL-boundary faults per its own package design, §
  `internal/chaos`'s existing primitives are all Postgres-specific). A
  small, new, OTel-SDK-level fault double is sufficient: an
  `sdktrace.SpanExporter` implementation whose `ExportSpans` always
  returns an error (or a real OTLP exporter configured against a closed
  port), used only by SF-080's test. This does not require a new package;
  a test-local fake in whichever package hosts the tracing integration
  tests is sufficient, following this project's own precedent of
  test-local fakes (e.g., `internal/api`'s poisoned `JobStore` fake in
  `TestCreateJob_StoreFailure_Returns500WithoutLeakingInternalDetails`).
- **No new invariant-checker (`internal/invariant`) extension is
  required** — this phase adds no new durable state-machine property for
  `internal/invariant.Checker` to check (§20); the existing checker
  continues to run, unmodified, across every scenario above, and finding
  zero violations remains part of the acceptance bar for SF-086 through
  SF-089 (the DLQ-replay scenarios) specifically, since those are the only
  new scenarios that mutate durable job-table state.

## 24. CI proof requirements

- **No new CI workflow file is needed.** Phase 10's existing gates
  (`govulncheck`, CodeQL, Dependabot, dependency-review — all repository-
  and-dependency-wide, not per-package) automatically cover the new
  OpenTelemetry dependency the moment it appears in `go.mod`/`go.sum`,
  exactly as they already cover every other dependency
  ([supply-chain-security.md](supply-chain-security.md)). This mirrors
  how Phase 11/13/14 each added dependencies/test categories without a
  new workflow file.
  - Actually: this project's dependency set has not changed since Phase
    1 except for existing ones — Phase 16 is the second time (after none
    previously) a genuinely new **production** dependency is added.
    `govulncheck`'s scheduled weekly run (Phase 10) is exactly the
    mechanism that will surface a future CVE in the new OTel SDK, so this
    is a direct, concrete beneficiary of Phase 10's own design, not a
    hypothetical one.
- **Ordinary `go test -p 1 ./...` extension**: every new test in §21
  runs as part of the existing, single CI test step — no separate job.
- **The route-registration lint test (SF-093)** is itself a CI-enforced
  proof, exactly like `TestRouter_AllHandlersMountedThroughAuth` already
  is today — no new tooling, an extension of an existing test.
- **The migration-lock-timing tests (SF-094)** and **reversibility test
  (SF-095)** run as ordinary `go test`s, exactly like every migration
  proof since Phase 12 (`data-model.md`'s "measure before documenting"
  discipline, enforced by tests, not a separate script).

## 25. Rollout / rollback strategy

- **Migration `0015`** is additive/nullable/data-safe-reversible (§7) —
  deployed first, per the existing migrate-first-deploy-second ordering
  rule ([compatibility-policy.md](compatibility-policy.md), formally
  adopted Phase 14). An old `cmd/api`/`cmd/worker` binary tolerates the
  new columns' presence unchanged (they are simply never read by code
  that doesn't know about them) — no two-binary-version proof specific to
  this migration is required beyond the general pattern Phase 12/13/14
  already established and proved generically.
- **New endpoints** are purely additive — rolling out a new `cmd/api`
  binary that serves them alongside old binaries (mid-rolling-deploy) is
  safe by construction: an old binary simply does not serve the new
  routes yet, which is not a correctness hazard (a client calling
  `/jobs/{id}/history` against an old-binary replica gets an ordinary
  404-from-the-mux, not a partial/corrupt response).
- **Tracing is opt-in by default** (§17) — rollout carries **zero** risk
  for an operator who does not set `TASKFORGE_OTEL_EXPORTER_ENDPOINT`;
  the noop `TracerProvider` makes the new code paths present-but-inert.
  An operator who *does* opt in should roll out to a single canary
  instance first and confirm the benchmark's overhead expectation (§19)
  holds against their own real collector before fleet-wide rollout — a
  documented operator recommendation, not a TaskForge-enforced gate.
- **Rollback** is an ordinary redeploy of the previous binary version;
  rolling back migration `0015` itself is optional and low-risk (the
  down migration drops five diagnostic-only nullable columns nothing
  else durably depends on) but is not required merely to roll back the
  binaries, since old binaries already tolerate the new columns' presence
  (the same asymmetry every prior additive migration in this project's
  history already establishes as the norm).
- **No feature flag beyond the existing environment-variable-presence
  gate for tracing** is introduced — per this project's own stated
  practice of not using feature flags/backwards-compatibility shims where
  a direct, honest configuration switch suffices.

## 26. Implementation order

Staged by risk and dependency, not by the order the roadmap's bullet list
happens to state them in (following Phase 14/15's own precedent of
re-ordering for implementation sanity):

1. **Migration `0015`** (§7) — lowest risk, no behavior change, unlocks
   every later step that needs the new columns to exist.
2. **HTTP-request-domain metrics middleware at `mount()`** (§6.6) — no
   new dependency, no schema dependency, immediately useful, and
   validates the "wrap at the choke point" design before anything else
   builds on top of the router.
3. **`taskforge_claim_latency_seconds` `origin` label** (§6.5) — isolated
   to `internal/store/claim.go`'s `recordClaim`, independent of every
   other work item.
4. **`/healthz` / `/readyz` + the new unauthenticated-mount exception
   path** (§6.1/§6.2, OD-D) — resolves the one genuine router-design gap
   (§4.3) before any further route work, and extends the
   route-registration lint test (SF-093) immediately so every subsequent
   new route in this phase is checked by it from the moment it is added.
5. **`GET /jobs/{id}/history`** (§6.3) — pure read, reuses existing
   ownership-scoping pattern exactly, no new store-write path, lowest-risk
   of the two new business endpoints.
6. **`POST /jobs/{id}/retry`** (§6.4) — depends on migration `0015`'s
   `retried_from` column; reuses `InsertIdempotent` unchanged.
7. **`internal/tracing` package + OTel SDK dependency + noop-by-default
   wiring in `cmd/api`/`cmd/worker`** (§5, §17) — introduces the new
   dependency; done after every schema/endpoint prerequisite exists, and
   before any span-emitting code, so the plumbing (TracerProvider
   construction, functional-option threading) is proven inert-by-default
   first.
8. **Submission-span capture** in `internal/api` (`CreateJob`,
   `CreateWorkflow`) and `txenqueue.EnqueueTx` (§13) — depends on step 7.
9. **Execution-span capture** in `internal/worker` (§13) — depends on
   steps 1, 7, 8 (needs a submission span to link back to).
10. **Sensitive-data span-attribute audit suite** (§16, SF-092) — written
    last among the tracing work, once every span-emitting code path
    exists, so the audit has something to actually scan.
11. **Performance/overhead benchmark** (§19) — run once tracing is fully
    wired, against a real local collector.
12. **Documentation updates** (§27) — [observability.md](observability.md)
    (tracing is no longer deferred; document the actual shape shipped),
    [invariants.md](invariants.md) ("Phase 16 reviewed" entry),
    [testing-strategy.md](testing-strategy.md) (new scenario rows),
    [security-model.md](security-model.md) (close the §7 DLQ-replay
    forward-reference), [worker-protocol.md](worker-protocol.md) (move
    `/jobs/{id}/retry` and `/jobs/{id}/history` out of "Deferred
    Endpoints"), [enterprise-roadmap.md](enterprise-roadmap.md) (correct
    the line-1198 `retried_from` claim per §4.2, and update Phase 16's
    exit-criteria checkboxes).

Steps 1–6 have no dependency on OpenTelemetry at all and could ship as an
independently valuable, lower-risk sub-release ahead of steps 7–11 if the
team wants to de-risk the new-dependency work separately — this plan
notes that option but does not mandate splitting the phase into two PRs;
that is an implementation-time sequencing choice, not an architectural
one.

## 27. Documentation updates required (implementation-time, not this file)

Listed here for completeness against "implementation-ready," but these
are implementation-PR deliverables, not part of this planning pass's own
file-change scope (per this document's header: the only file this
planning pass creates is `docs/phase-16-plan.md` itself):

- [observability.md](observability.md): replace the "Tracing: not
  implemented in Phase 8" note with the actual Phase 16 design; update
  "Trace Boundaries" from aspirational ("if distributed tracing is
  introduced") to descriptive.
- [invariants.md](invariants.md): add a "Phase 16 reviewed: no new
  invariant added" section, mirroring §20 of this plan.
- [testing-strategy.md](testing-strategy.md): add a "Phase 16" section
  mirroring the existing per-phase sections, and populate the new
  SF-076–SF-095 rows into the invariant-to-test matrix.
- [scenario-corpus.md](scenario-corpus.md): add SF-076 through SF-095 in
  full scenario-corpus format (initial state / actions / fault / expected
  behavior / invariants proved / test), mirroring §21's table.
- [security-model.md](security-model.md) §7: close the forward-reference
  ("Design requirement for both, before either ships") now that the DLQ-
  replay tenant-scoping design is settled (§6.4, §15).
- [worker-protocol.md](worker-protocol.md): move `POST /jobs/{id}/retry`
  and `GET /jobs/{id}/history` from "Deferred Endpoints" to the
  documented, implemented API contract section.
- [data-model.md](data-model.md): add the "Phase 16 migration lock
  profile" section (mirroring the existing Phase 12/13 sections) once
  migration `0015`'s lock behavior is actually measured (§7, §22).
- [enterprise-roadmap.md](enterprise-roadmap.md): correct the false
  `retried_from`-already-exists claim (§4.2) and update Phase 16's
  checkboxes to reflect what is now proven, per this project's existing
  per-phase convention (Phases 10/11/14/15's own sections already do
  this).
- [compatibility-policy.md](compatibility-policy.md): no rule changes
  required (§18 shows every change here is already covered by existing
  adopted rules); a short note may be added observing that Phase 16 is
  the first real exercise of rule 4's "new label on an existing metric"
  clause, mirroring how Phase 14's drain metrics were noted as "the first
  real test" of that same rule.

## 28. Enterprise exit criteria

Reproduced verbatim from the roadmap (§2.7), the authoritative bar this
phase's implementation PR must clear:

- [ ] Job submission and execution are linked via OpenTelemetry spans
      following a pinned, documented version of OTel's messaging semantic
      conventions, with those conventions excluded from TaskForge's own
      permanent compatibility guarantee.
- [ ] `taskforge_claim_latency_seconds` and `taskforge_queue_age_seconds`
      are distinct, independently meaningful observations.
- [ ] `/healthz` and `/readyz` exist and are proven correct under a
      simulated database outage.
- [ ] A dead-lettered job can be replayed via a documented API, scoped to
      its owning tenant/principal, with the `retried_from` linkage
      tested.
- [ ] Every endpoint added by this phase enforces Phase 12's
      authorization/ownership checks, verified by test.

**Additional proof this plan requires, beyond the roadmap's own literal
minimum**, to meet this project's existing `testing-strategy.md`
discipline (not new roadmap scope, just this project's ordinary bar for
"proven," applied consistently):

- [ ] HTTP-request-domain metrics (`taskforge_http_requests_total`/
      `_duration_seconds`) exist and are proven correct per route,
      including for a rejected (`401`) request (SF-091) — implied but not
      separately checkbox-listed by the roadmap's own §2.1/§2.6.
- [ ] `GET /jobs/{id}/history` exists, is principal-scoped, and its
      cross-principal response is indistinguishable from a nonexistent
      job (SF-084/SF-085).
- [ ] A route added without going through the router's auth wrapping (or
      the named unauthenticated exception list) is caught by an extended
      `TestRouter_AllHandlersMountedThroughAuth`-style test (SF-093),
      satisfying roadmap §2.5's explicit "must be caught by a lint/test
      convention" requirement.
- [ ] No span attribute anywhere in the test suite contains `payload`,
      the raw `Idempotency-Key`, or a raw credential (SF-092).
- [ ] A telemetry-exporter-unavailable test proves job execution is
      unaffected (SF-080), satisfying roadmap §2.1/§2.5's "non-load-bearing"
      requirement with an actual test, not only a design argument.
- [ ] Migration `0015` is measured (not merely asserted) to be O(1)
      regardless of table size, and is proven data-safe-reversible
      (SF-094/SF-095), consistent with this project's "measure before
      documenting" discipline.

## 29. Open architectural decisions

### OD-A — ADR required before implementation (blocking)

**Status: CLOSED.** [ADR-0012](adr/0012-distributed-tracing-and-durable-trace-context.md)
is written and Accepted. Following the precedent Phase 13 (ADR-0009) and
Phase 15 (ADR-0011) established — a phase with a genuine, real
alternative to weigh gets an ADR before implementation — Phase 16's
tracing design involved at least four decisions with real alternatives:
(a) durable trace-context storage on the job/attempt rows vs. an external
correlation store; (b) span links vs. parent-child (already decided by
the roadmap itself; ADR-0012's role was to record and detail that
decision, including the further refinement of exactly which boundaries
get a link vs. an ordinary parent-child relationship, not re-litigate the
roadmap's own choice); (c) OTLP transport choice (resolved: HTTP, not
gRPC — OD-B below); (d) noop-by-default vs. mandatory-exporter posture
(resolved: noop-by-default). ADR-0012 is
[docs/adr/0012-distributed-tracing-and-durable-trace-context.md](adr/0012-distributed-tracing-and-durable-trace-context.md),
the next sequential number per [docs/adr/README.md](adr/README.md)'s
"numbered sequentially" convention, and [docs/adr/README.md](adr/README.md)'s
index has been updated to list it. It additionally ratifies a decision
this plan's own §10–§13 had not fully specified: **DLQ replay gets its
own, fresh trace (linked back to the original submission span), while an
ordinary retry/reclaim shares the submission span's `trace_id`** — see
ADR-0012 §"Async causality model" for the full reasoning (bounded by
`max_attempts` for retries; no equivalent bound exists for replay count,
so replays are not chained into one ever-growing trace).

### OD-B — OTLP transport: HTTP, not gRPC (resolved, non-blocking)

**Status: CLOSED (this plan).** Recommend `otlptracehttp` over
`otlptracegrpc` as the default/documented exporter: OTLP/HTTP requires no
gRPC codegen toolchain addition to this project's build (which today has
none), works through simple corporate HTTP proxies without special
handling, and is functionally equivalent for this phase's purposes (a
batch of spans, sent periodically, non-load-bearing). This is recorded
here as a recommendation for ADR-0012 to ratify or override — it does not
block this plan's other conclusions.

### OD-C — DLQ-replay idempotency-key strategy (resolved)

**Status: CLOSED (this plan).** §6.4 resolves this: the replay endpoint
accepts its own independent, optional `Idempotency-Key`, reusing
`InsertIdempotent` unchanged for the new row. No new dedup mechanism
keyed off `retried_from` is introduced. This was flagged as a genuinely
open question during this plan's codebase-inventory pass (§4.10); this
document closes it rather than leaving it open, since no empirical
evidence is needed to decide it — it is a pure design choice already
consistent with existing, proven machinery.

### OD-D — Unauthenticated route-mounting mechanism (resolved)

**Status: CLOSED (this plan).** §4.3 identifies that `mount()`
unconditionally requires auth today. Resolution: add a second, explicitly
named function (e.g. `mountPublic(mux, pattern, handler)`) used **only**
for `/healthz`/`/readyz`, and extend
`TestRouter_AllHandlersMountedThroughAuth` (or add a sibling test) to
assert that every route is reached through exactly one of `mount()` or
`mountPublic()`, with `mountPublic()`'s call sites enumerated by an exact,
hardcoded allowlist of patterns (`GET /healthz`, `GET /readyz`) — so a
future accidental unauthenticated route addition still fails the test
(SF-093), rather than the carve-out silently widening over time.

### OD-E — Extend `/healthz`/`/readyz` to `cmd/worker`'s metrics listener (resolved, beyond roadmap's literal minimum)

**Status: CLOSED (this plan), recommended but not part of the formal exit
criteria.** §4.8 notes the roadmap's exact scope does not specify
`cmd/api` vs. `cmd/worker`, and every "why it matters" citation is framed
around the API server. This plan recommends **also** adding `/healthz`
(liveness only — the worker has no request-serving readiness concept
analogous to the API's DB-pool check, so `/readyz` is not proposed for
the worker) to `cmd/worker`'s existing, already-unauthenticated
`TASKFORGE_METRICS_ADDR` listener, since it is a near-zero-cost addition
to a listener that already exists, on the same trust boundary, and
directly closes [enterprise-readiness.md](enterprise-readiness.md) §4's
P1 finding ("no health/readiness endpoints") for the worker fleet too,
not just the API. This is listed as a recommended addition in §26 step 4
but is **not** added to §28's formal exit criteria, since the roadmap
does not require it — an implementer may defer it without failing Phase
16's exit bar.

### OD-F — Exact OTel semantic-convention version to pin (deliberately NOT resolved now)

**Status: OPEN — by design, resolved at implementation time.** The
roadmap explicitly requires pinning "the exact convention version used"
(§2.1) precisely because OTel's messaging semantic conventions are under
active development (marked *Development*, not *Stable*, as of the
roadmap's own writing). This plan does **not** name a specific module
path/version/commit here: doing so today risks citing a value that is
already stale, superseded, or renamed by the time Phase 16 is actually
implemented, which would itself violate the spirit of "pin the exact
version **used**" (used at implementation time, not decided months
earlier in a planning document). **Resolution procedure for the
implementer**: at the start of implementation, check the current
published state of `go.opentelemetry.io/otel/semconv/...`'s messaging
conventions, pin that exact version in `go.mod` and cite it by version
string in the span-emitting code's doc comment and in ADR-0012, and note
its *Development* status explicitly per §10's requirement. This is the
one item in this plan that is intentionally left for implementation time
rather than decided now, and it is not a blocker in the sense OD-A is —
it requires a five-minute lookup at implementation time, not a design
decision or empirical drill.

### OD-G — Span-exporter batching/timeout defaults (minor, non-blocking)

**Status: CLOSED (this plan) at "use the SDK's own defaults."** The OTel
Go SDK's `BatchSpanProcessor` ships with documented, sane defaults
(batch size, queue size, export timeout, scheduled delay). This plan does
not propose overriding any of them absent a demonstrated need — consistent
with this project's own "no speculative configuration surface" discipline
(cf. [data-model.md](data-model.md)'s "Why Not Over-Design v1"). If the
performance benchmark (§19, §26 step 11) reveals a need to tune one of
these, that is an implementation-time finding to document, not a
pre-decided plan parameter.

## 30. Prerequisite ADR / evidence determination

Unlike Phase 15 (§29 of that plan identified OD-1/OD-2 as requiring a
genuine **empirical** pre-implementation drill — an unknown about
`embedded-postgres`'s actual capabilities that no amount of reading code
could resolve), **Phase 16 requires no empirical evidence-gathering pass
before implementation begins.** The reasons:

- The noop-tracer-overhead claim (§19) rests on OpenTelemetry SDK's own
  documented, widely-relied-upon design guarantee, not an
  unknown/undocumented third-party behavior this project would need to
  independently discover (contrast: Phase 15's OD-2 needed to discover
  whether `embedded-postgres`'s `StartParameters` surface could even
  enable WAL archiving at all — a genuine unknown; nothing analogous
  exists here).
- Every other design question this plan identified (§29 OD-B through
  OD-E, OD-G) is a pure architectural choice resolvable by reasoning
  about this codebase's own existing patterns, which this plan has
  already done.
- The one item requiring a lookup rather than a decision (OD-F, the
  semconv version) is a matter of checking a currently-published value
  at implementation time, not running an experiment or a drill.

**What Phase 16 *does* require before implementation, per this project's
own precedent (Phase 13/15): a written, committed ADR (OD-A, ADR-0012)**
recording the ratified decisions from §10–§13 and §29's OD-B/OD-D. This
is a **design-documentation** prerequisite, not an **empirical-evidence**
prerequisite — the distinction this section exists to draw precisely,
mirroring how Phase 15's own §29 distinguished its two blocking,
empirical ODs from its several non-blocking, purely-design ones.

**Update: this prerequisite is now satisfied.**
[ADR-0012](adr/0012-distributed-tracing-and-durable-trace-context.md) is
written and Accepted, closing OD-A. No empirical evidence pass was run or
is required, consistent with the determination above — ADR-0012's own
"Context" section restates the same reasoning. OD-F (§29) remains the
only open item in this plan, and it remains open by design, not because
this determination was wrong: it is a version-string lookup to perform at
implementation time, not a decision this planning-and-ADR pass could
correctly make in advance.

## Cross-references

- Roadmap: [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 16 —
  Tracing & Operator Diagnostics"
- Observability foundation: [observability.md](observability.md)
  (metrics, structured logs, existing "Trace Boundaries" sketch)
- Capacity/SLI gap analysis: [slo.md](slo.md) (HTTP-metrics gap,
  claim-latency/queue-age gap)
- Security/tenant-scoping requirement for DLQ replay:
  [security-model.md](security-model.md) §7
- Enterprise gap citations: [enterprise-readiness.md](enterprise-readiness.md)
  §4
- Reference research: [reference-analysis.md](reference-analysis.md)
  (OTel span-link recommendation)
- Prior authorization/ownership design this phase reuses:
  [phase-12-plan.md](phase-12-plan.md), [invariants.md](invariants.md)
  TF-INV-017/018
- Prior compatibility/migration discipline this phase follows:
  [compatibility-policy.md](compatibility-policy.md),
  [data-model.md](data-model.md) ("Phase 12/13 migration lock profile"),
  [phase-14-plan.md](phase-14-plan.md)
- Prior ADR precedent for a phase-specific blocking ADR:
  [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md),
  [ADR-0011](adr/0011-postgresql-native-ha-backup-dr.md)
- This phase's own blocking ADR (OD-A, now CLOSED):
  [ADR-0012](adr/0012-distributed-tracing-and-durable-trace-context.md)
- Deferred endpoints this phase implements:
  [worker-protocol.md](worker-protocol.md) "Deferred Endpoints"
- Retry/DLQ semantics this phase's replay endpoint builds on:
  [retry-semantics.md](retry-semantics.md),
  [ADR-0008](adr/0008-explicit-terminal-states.md)
- Idempotency mechanism reused unchanged: [idempotency.md](idempotency.md)
- Next phase this work feeds: [enterprise-roadmap.md](enterprise-roadmap.md)
  Phase 17 ("needs 13's backpressure signal and 16's metrics to be
  measured meaningfully")
