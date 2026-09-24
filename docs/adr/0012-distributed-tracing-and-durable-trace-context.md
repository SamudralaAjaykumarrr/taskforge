# ADR-0012: Distributed Tracing and Durable Trace-Context Propagation

Status: Accepted

## Context

[enterprise-roadmap.md](../enterprise-roadmap.md) "Phase 16 — Tracing &
Operator Diagnostics" requires OpenTelemetry span-link-based tracing
connecting a job's submission to its later claim/execution, durably
associated with the job across long queue waits, retries, and lease
reclaims, with telemetry failure remaining non-load-bearing.
[observability.md](../observability.md) named this an "explicit deferral,
not an oversight" back in Phase 8 and already sketched the intended span
boundaries (submission span, one attempt span per `job_attempts` row) —
this ADR is where that sketch becomes a ratified decision.
[reference-analysis.md](../reference-analysis.md) recommends OpenTelemetry
with span **links** (not parent-child) across the queue-wait gap,
following OTel's own messaging semantic conventions, and flags that those
conventions are still marked *Development*, not *Stable*.

[phase-16-plan.md](../phase-16-plan.md) planned this phase in detail
before any code was written, including a direct source-code inventory
that found **zero** existing tracing infrastructure anywhere in this
codebase (no `trace_id`/`span_id`/`correlation_id` field, no OpenTelemetry
dependency), and — a documentation/reality correction this ADR carries
forward — established that the roadmap's own claim that a `retried_from`
column "already exists in the schema" is **false**: no migration, and no
`Job` struct field, defines it. `retried_from` is new work, decided here
alongside the tracing design because both land in the same migration
(§"Durable trace-context storage" below).

That plan's own §29 identified this ADR (OD-A) as a **blocking**
prerequisite — following the precedent
[ADR-0009](0009-phase-13-concurrency-and-fairness.md) (Phase 13) and
[ADR-0011](0011-postgresql-native-ha-backup-dr.md) (Phase 15) set: a
phase with genuine architectural alternatives gets a written decision
before implementation, not during it. Unlike ADR-0009, this ADR requires
**no empirical evidence-gathering pass** (`phase-16-plan.md` §30 states
why: the noop-tracer-negligible-overhead property this ADR relies on is a
documented guarantee of the OpenTelemetry SDK itself, not an unknown
about a third-party tool this project would need to independently
discover, unlike Phase 15's `embedded-postgres` capability questions).
This ADR resolves every decision `phase-16-plan.md` §29 marked OD-B
through OD-E, ratifies the design in that plan's §7 and §10–§13, and
deliberately leaves exactly one item open — OD-F, the exact OpenTelemetry
semantic-convention version to pin — for the reason stated in
"Consequences" below.

## Decision

TaskForge adopts the **OpenTelemetry Go SDK**, with spans **durably
anchored to job/attempt rows** via new, additive, nullable schema columns,
using **span links** (never parent-child) across every persistence
boundary, with a **noop `TracerProvider` by default** so tracing is
strictly opt-in and never load-bearing.

### 1. Durable trace-context storage

**Schema** (migration `0015_add_trace_context_and_retry_link`, additive
and nullable throughout — see [phase-16-plan.md](../phase-16-plan.md) §7
for the full column table and measured-lock-profile expectation):

| Table | Column | Type |
|---|---|---|
| `jobs` | `trace_id` | `text NULL` |
| `jobs` | `span_id` | `text NULL` |
| `jobs` | `retried_from` | `uuid NULL REFERENCES jobs(id)` |
| `job_attempts` | `trace_id` | `text NULL` |
| `job_attempts` | `span_id` | `text NULL` |

`trace_id`/`span_id` are stored as **W3C Trace Context hex strings**
(32-hex-char trace ID, 16-hex-char span ID) — the canonical `String()`
form OTel's Go SDK already produces — never a vendor-specific or
serialized-carrier format (see "Alternatives Considered").

**What must survive, and how**:

- **Asynchronous enqueue**: `jobs.trace_id`/`span_id` are written exactly
  once, in the same `INSERT` transaction that creates the job row (the
  producer span's identifiers, captured before commit, discarded if the
  transaction rolls back — no orphaned trace context for a job that was
  never durably created, mirroring TF-INV-001's own "nothing exists until
  commit" discipline).
- **Retry** (`RETRY_WAIT` → `RUNNING`): the claim query already returns
  the full row (`RETURNING jobs.*`, unchanged); `jobs.trace_id`/`span_id`
  are **read, never rewritten**, by any claim, fresh or retried. Every
  attempt of the same job links back to the identical submission span.
- **Lease reclaim**: identical treatment to an ordinary retry — a
  reclaim is a claim of an expired-lease `RUNNING` row through the same
  query, and this ADR draws no distinction between the two for trace
  purposes (see "Async Causality Model" below for why lease/generation
  state and trace-context state are kept fully independent, mirroring how
  [ADR-0009](0009-phase-13-concurrency-and-fairness.md) already keeps
  lease ownership and capacity-slot state independent).
- **Scheduling**: `scheduled_at`/`eligible_at` govern *when* a job may be
  claimed, never *whether* it has trace context — a job scheduled a year
  out gets its submission span at the moment of `POST /jobs`, exactly
  like an immediately-eligible job, and the eventual claim's attempt span
  links back to it across however long that gap turns out to be. This is
  the literal scenario span links exist to handle correctly.
- **Workflow transitions**: **no new column, no workflow-instance-level
  span.** Each workflow node's underlying job is an ordinary job for
  every tracing purpose, exactly as it already is for metrics/logs
  ([observability.md](../observability.md) "Workflow observability") —
  it gets its own submission span and its own linked attempt span(s),
  using the schema above unchanged. `propagateWorkflowTransition`'s
  internal cascade (predecessor completion → dependent's `eligible_at`
  advance) is store-internal bookkeeping, not a user-facing execution
  boundary, and is not separately traced, mirroring that same document's
  existing choice not to log per-node cascade events individually.
- **DLQ**: a `DEAD_LETTERED` job's attempt spans simply end with an error
  status on the terminal attempt; no separate "DLQ span" exists.
- **DLQ replay**: see "Async Causality Model" below — `retried_from` is
  the durable link; it does **not** imply trace continuity by itself.

**What must NOT be persisted**:

- `jobs.payload` / `jobs.result_metadata` — never in any column this
  migration adds, and never later copied into a span attribute (§
  "Security").
- The raw `Idempotency-Key` value, any API credential, `key_id`, or the
  pepper.
- The **complete serialized W3C carrier** (a full `traceparent`/
  `tracestate` header string) — only the two fixed-width identifiers
  needed to reconstruct a link. See "Alternatives Considered" for why.
- OTel **Baggage** of any kind (§"Security").

**Compatibility for pre-Phase-16 rows**: every row that exists before
this migration, and every row created while tracing is disabled (the
noop-by-default posture, §4), has `trace_id`/`span_id` = `NULL`. The
worker's span-start logic treats `NULL` as "no context to link to" and
either starts an unlinked span (if tracing happens to be enabled for this
process but the job predates it) or does nothing at all (if tracing is
disabled) — a plain `nil`/empty-string check, never an error path, never
a special migration or backfill. `retried_from` is `NULL` for every
ordinarily-submitted job, pre- or post-Phase-16, with identical
"no special case" treatment.

**Security/privacy treatment**: `trace_id`/`span_id` are treated as
exactly the same sensitivity class as `job_id` itself — non-secret,
unguessable identifiers, safe to return in an API response (§"Propagation
Boundaries") and safe to appear in logs, but never used as an
authentication or authorization credential of any kind. `retried_from` is
a `jobs.id` foreign key and inherits `job_id`'s own existing
principal-scoping discipline: reading a job's `retried_from` value is
already governed by that job's own ownership check ([phase-16-plan.md](../phase-16-plan.md)
§6.3/§6.4), no separate authorization rule is introduced for the column
itself.

### 2. Async causality model

**The governing rule**: TaskForge uses a **parent-child** span
relationship only within one **bounded, in-process, synchronous** call
graph — a single attempt's own execution, where a job handler's further
OTel-instrumented calls (e.g., an outbound HTTP request) become ordinary
child spans of that attempt span, using OTel's ordinary propagation,
exactly like any other instrumented Go program. TaskForge uses a **span
link** at every boundary that a parent-child relationship would
misrepresent as having a synchronous, bounded lifetime — concretely, any
boundary that crosses a database commit followed by an independently
scheduled resumption:

- **Submission → first attempt**: linked, per the roadmap's own explicit
  requirement — a `RETURNING`-committed job may sit `QUEUED` for an
  unbounded, arbitrary duration before any worker claims it; a
  parent-child relationship would force the submission span to either
  stay open for that entire wait (misleadingly implying the API request
  is still "in progress" long after `POST /jobs` has already returned,
  contradicting TF-INV-001's own "the response is the durable commitment,
  full stop" framing) or be closed and then falsely re-opened as a parent
  later, which OTel's model does not support cleanly and this ADR does
  not attempt to approximate.
- **Attempt → next attempt (retry or reclaim)**: **linked, and sharing
  the same `trace_id` as the submission span** — not each attempt getting
  its own independent trace. This is a deliberate refinement of the
  general "new persistence boundary ⇒ new trace" instinct, justified
  specifically here: a job's full lifecycle (submission plus every retry)
  is one logical unit of work an operator wants to see as one trace
  ([observability.md](../observability.md)'s own pre-existing framing:
  "a job's full lifecycle... is naturally represented as multiple linked
  attempt spans sharing a common `job_id` attribute"), and — critically —
  this cannot grow unboundedly, because `attempt_count` is already
  bounded by `max_attempts` (TF-INV-006, enforced independently of and
  unmodified by this ADR). A trace with a handful to a few dozen linked
  attempt spans is well within what any OTel-compatible backend renders
  correctly; an *unbounded* number would not be, which is exactly why the
  next case below is decided differently.
- **Original job → DLQ replay**: **NOT linked into the same trace. A
  replayed job gets its own, fresh `trace_id`/`span_id`, generated at the
  moment `POST /jobs/{id}/retry` inserts the new row** — a new submission
  span, exactly like any other `POST /jobs`-shaped insert. The new
  submission span additionally carries a **span Link** back to the
  *original* job's submission span, reconstructed from the original row's
  own (possibly `NULL`) `jobs.trace_id`/`span_id` — read during the same
  operation that already reads the original row to authorize the replay
  ([phase-16-plan.md](../phase-16-plan.md) §6.4). **Why a new trace, not
  a continuation**: unlike a retry (system-driven, bounded by
  `max_attempts`, the *same* logical submission continuing), a replay is,
  by this project's own state-machine design (TF-INV-005, ADR-0008), a
  **new job row** — a distinct logical submission an operator or their
  automation triggers, potentially hours, days, or weeks after the
  original dead-lettered, with no bound on how many times a job could in
  principle be replayed. Chaining every replay onto one ever-growing
  trace would reintroduce exactly the unbounded-growth risk the
  attempt-linking decision above avoids only because `max_attempts`
  bounds it — no equivalent bound exists on replay count. The durable,
  tracing-independent correlation path for "what was this job replayed
  from" is `jobs.retried_from` itself (queryable via `GET /jobs/{id}` or
  `GET /jobs/{id}/history` regardless of whether tracing was ever
  enabled for either job) — the span Link is a **convenience** for an
  operator already working in a trace backend, not the sole path, and
  correctness of the replay linkage never depends on tracing being
  enabled, configured, or successful.
- **Transactional enqueue (`txenqueue.EnqueueTx`) and its own rollback**:
  a producer span is started for this path identically to `POST /jobs`
  (§"Propagation Boundaries"). If the caller's transaction rolls back, no
  job row exists — the span must record that outcome (an OTel span status
  of `Error`, or an attribute noting no row was created) and must never
  be interpreted by a downstream consumer as implying a job exists,
  mirroring [phase-16-plan.md](../phase-16-plan.md) §13's explicit
  requirement.

### 3. OTLP transport

**Decision: OTLP over HTTP (`otlptracehttp`), not OTLP over gRPC.**
Reasons: this project's build has no gRPC codegen toolchain today and
this ADR does not want to introduce one for a diagnostic-only feature;
OTLP/HTTP traverses ordinary corporate HTTP proxies without special
handling; and the transport choice has no bearing on the correctness or
non-load-bearing properties this ADR cares about (batched, asynchronous
export either way). This ratifies [phase-16-plan.md](../phase-16-plan.md)
OD-B.

**Configuration boundary** (extending `internal/config`'s existing
`FromEnv` pattern, no new configuration surface beyond what is listed):

| Variable | Default | Meaning |
|---|---|---|
| `TASKFORGE_OTEL_EXPORTER_ENDPOINT` | unset | OTLP/HTTP collector endpoint. **Unset is the trigger for the noop posture (§4)** — this is the single on/off switch for tracing, deliberately not split into a separate "enabled" boolean, so there is exactly one way to turn tracing on and no way to have it half-configured. |
| `TASKFORGE_OTEL_SERVICE_NAME` | `taskforge-api` / `taskforge-worker` | The `service.name` resource attribute. |

No `TASKFORGE_OTEL_EXPORTER_PROTOCOL` or transport-selection variable is
introduced — the transport is fixed to OTLP/HTTP by this ADR, not
operator-configurable, per this project's own "no speculative
configuration surface" discipline
([data-model.md](../data-model.md) "Why Not Over-Design v1",
[phase-16-plan.md](../phase-16-plan.md) OD-G). A future ADR may add gRPC
support if a real, demonstrated need arises; this one does not
pre-provision for it.

**TaskForge's own startup and runtime must never require a collector to
be reachable.** With `TASKFORGE_OTEL_EXPORTER_ENDPOINT` unset,
`cmd/api`/`cmd/worker` construct the noop provider (§4) and make **zero**
network calls related to tracing, ever. With it set, the exporter is
constructed but export happens asynchronously off a background batch
processor — a collector that is down, slow, or never reachable at all
delays or drops spans, and must never delay or block process startup,
job claiming, or job completion (§4, §"Failure Implications").

### 4. Noop-by-default posture

**Exact behavior when unconfigured**: `internal/tracing.New(cfg)` (a new,
small package, mirroring `internal/metrics.New()`'s own initialization
shape) returns a `trace.TracerProvider` backed by OpenTelemetry's own
`noop` implementation whenever `TASKFORGE_OTEL_EXPORTER_ENDPOINT` is
empty. Every `Tracer.Start()` call against a noop provider returns a
non-recording span at negligible, SDK-guaranteed cost — no background
goroutine, no batching queue, no exporter client is constructed at all.

**No change to existing API/worker behavior**: every code path this
phase adds (submission-span start/end, attempt-span start/end, link
construction) executes identically whether the provider is noop or real
— the calling code never branches on "is tracing enabled," it simply
always calls the same `Tracer`/`Span` API, and the SDK's own noop
implementation is what makes the disabled case free. This is what proves
"zero behavior change unless opted in" by construction rather than by an
if-statement a future change could accidentally invert.

**No loss of jobs because an exporter/collector is unavailable**:
PostgreSQL remains the sole source of truth (ADR-0001, unchanged); no
tracing code path is ever positioned between a job's durable state
transition and its caller-visible outcome. Span export is queued into an
in-memory batch processor and flushed asynchronously, entirely decoupled
from the `Complete*`/`Claim` transaction it describes.

**Telemetry export failures must never become job-processing failures —
stated as a code-level rule, not merely an intention**: no function in
`internal/store` or `internal/worker` may check, propagate, wrap, or
return a tracing/export error from any exported function signature. A
span-export failure is, at most, logged at a low severity from within
the tracing plumbing itself (or silently dropped, per the OTel SDK's own
default behavior) — it is never visible to `RunOnce`, `Claim`,
`CompleteSuccess`, or any HTTP handler as an error requiring a decision.

**Phase 1–15 correctness guarantees preserved**: no `TF-INV-*` invariant
depends on trace-context presence, correctness, or completeness — the
roadmap states this explicitly and this ADR does not weaken it; a job
whose trace context is `NULL`, garbled, or entirely absent (tracing
disabled) executes through the identical state machine, with identical
fencing, identical retry/backoff, and identical dead-lettering as it
always has. See [phase-16-plan.md](../phase-16-plan.md) §20's invariant
mapping — this ADR adds no new `TF-INV-*` ID.

### 5. Propagation boundaries

| Boundary | Span behavior |
|---|---|
| `POST /jobs` / `POST /workflows` (API request → enqueue) | Producer span (`create`), started on handler entry, ends only after the `INSERT` transaction commits; `trace_id`/`span_id` persisted onto the new row(s) in that same transaction. A workflow's every node job gets its own producer span. |
| `txenqueue.EnqueueTx` (transactional enqueue) | Identical producer-span treatment; on caller rollback, span records no-row-created, never implies a durable job (§2). |
| Worker claim → execution | Consumer span (`process`), started immediately after a successful claim, **linked** to the submission span reconstructed from the claimed row's `trace_id`/`span_id` (or no link if `NULL`). Ends when the attempt reaches a terminal disposition; its own `trace_id`/`span_id` persisted onto the `job_attempts` row in the same call that already writes `finished_at`/`outcome`. |
| Retries (`RETRY_WAIT` → `RUNNING`) | New attempt span, same `trace_id` as the submission span, linked (not parented) to it — see §2. |
| Lease reclaim | Identical to an ordinary retry — no distinct treatment; a reclaim is simply another claim of the same eligibility branch. |
| Scheduled jobs | No special treatment — the submission span exists from the moment of `POST /jobs` regardless of how far in the future `scheduled_at`/`eligible_at` is; the eventual attempt links back across however long that gap is. |
| Workflows | Each node's underlying job is traced exactly like an ordinary job (§1); no workflow-instance-level span. |
| DLQ (`DEAD_LETTERED`) | The terminal attempt's span ends with an error status; no separate DLQ-specific span. |
| DLQ replay (`POST /jobs/{id}/retry`) | **New** trace for the new job's own submission span (§2), plus a span Link back to the original job's submission span, plus the always-available, tracing-independent `retried_from` durable FK. |
| Handler-internal instrumented calls | Ordinary OTel parent-child, as a child of the attempt span — bounded, synchronous, in-process work, outside this ADR's "link across a persistence boundary" rule. |

### 6. Observability relationship

**Traces are additive; they do not replace logs or metrics.** Restating
[phase-16-plan.md](../phase-16-plan.md) §12, ratified here: structured
logs (Phase 8, unchanged) remain the always-on, no-opt-in-required
correlation mechanism keyed on `job_id`; Prometheus metrics (Phase 8,
extended by this phase per §6.5/§6.6 of the plan) remain the
alerting/dashboarding/SLI layer; traces are the new, **opt-in** layer for
following one job's complete queue-wait-then-retries timeline.

**Correlation strategy**: every span this phase emits carries a
namespaced `taskforge.job_id` attribute (never a bare `job_id`, to avoid
colliding with any current or future OTel semantic-convention key of that
bare name) — the same identifier every log line already carries. `GET
/jobs/{id}` and `GET /jobs/{id}/history` additionally return the job's
`trace_id` in their response body (an additive field, per
[compatibility-policy.md](../compatibility-policy.md)'s unknown-field-
tolerance guarantee), so an operator can pivot from a `job_id` they
already have to a trace, or from a trace's `taskforge.job_id` attribute
back to logs/database rows, in either direction.

**Metric-name preservation**: no existing metric name is repurposed.
This phase adds two wholly new names
(`taskforge_http_requests_total`, `taskforge_http_request_duration_seconds`)
and one additive label on an existing metric — see next paragraph — per
[compatibility-policy.md](../compatibility-policy.md) rule 4 ("only adds
new metrics or new labels on existing ones").

**The `taskforge_claim_latency_seconds`/`taskforge_queue_age_seconds`
ambiguity, resolved per [phase-16-plan.md](../phase-16-plan.md) §6.5 and
ratified here**: `taskforge_claim_latency_seconds` gains a new `origin`
label (`queued` / `retry_wait` / `reclaimed`), computed from the claim
query's already-available `oldState` signal;
`taskforge_queue_age_seconds` remains unlabeled and unchanged, continuing
to answer the single, generic "how long did this claim wait past
eligibility" question. This ADR does not revisit that resolution — it is
a metrics-only design decision independent of the tracing architecture
this ADR otherwise governs, included here only because the task that
produced this ADR asked for it to be explicitly addressed.

### 7. Security

**No baggage, no arbitrary user-controlled metadata.** TaskForge does
**not** use OpenTelemetry's Baggage API in this phase, and does not
persist or propagate any span data whose shape or content is not
explicitly enumerated by the allowlist below. Baggage is designed
precisely for arbitrary, cross-cutting, caller-supplied key-value data —
the opposite of the redaction posture this ADR requires, and adopting it
would reopen exactly the "unbounded, third-party/caller-controlled
content in a persisted or exported field" risk the rest of this section
exists to close. If a future phase demonstrates a real need for baggage,
it requires its own ADR revisiting this decision, not a silent addition.

**Attribute allowlist** — the only span attributes this phase's own code
may emit:

- `taskforge.job_id`, `taskforge.job_type`, `taskforge.queue_name`,
  `taskforge.attempt_number`, `taskforge.principal_id`,
  `taskforge.outcome`, `taskforge.error_class`,
  `taskforge.had_idempotency_key` (boolean — never the raw key value,
  mirroring [observability.md](../observability.md)'s existing identical
  log-field discipline), `taskforge.retried_from` (a job ID, when
  present).

**Explicitly forbidden, never a span attribute**: `jobs.payload`,
`jobs.result_metadata`, the raw `Idempotency-Key` value, any API
credential/`key_id`/secret/pepper, and — treated with the same caution
[observability.md](../observability.md) already applies to it in
logs — `job_attempts.error_message` free text is **excluded by default**;
including it requires an explicit, separately reviewed decision at
implementation time, not a default inclusion in this ADR's own allowlist.

**Enforcement mechanism, not merely a stated policy**: exactly one,
centralized span-attribute-building helper function is the sole
sanctioned way any TaskForge code attaches an attribute to a span this
project emits — mirroring how `mount()` is already the sole sanctioned
way a route is attached to the router
([phase-16-plan.md](../phase-16-plan.md) §4.3). A future change that
tries to attach `payload` to a span directly, bypassing that helper, is
caught by the sensitive-data span-attribute audit test
([phase-16-plan.md](../phase-16-plan.md) SF-092), not by review
discipline alone.

**Authentication/authorization semantics are unchanged.** No new scope
value, no new principal kind, and no change to `internal/principal`'s
existing `Verify`/`AccessContext`/`HasScope` mechanism. Every new
endpoint this phase adds (`/jobs/{id}/history`, `/jobs/{id}/retry`)
reuses the existing `jobs` scope and the existing
ownership-predicate-in-SQL pattern exactly, per
[phase-16-plan.md](../phase-16-plan.md) §6.3/§6.4 — tracing is a
diagnostic overlay on top of that unchanged boundary, never a
replacement or bypass of it. `/healthz`/`/readyz` are deliberately
unauthenticated (a separate, already-resolved decision,
[phase-16-plan.md](../phase-16-plan.md) OD-D) and carry no trace-context
concern since they touch no job data.

## Alternatives Considered

- **Trace context held only in memory (no durable columns).** Rejected:
  cannot survive a worker crash-and-reclaim by a *different* process,
  which has no shared memory with the one that submitted or last
  attempted the job — this is precisely the failure mode the roadmap's
  "durably associated... across long waits, retries, and lease reclaims"
  requirement exists to prevent. An in-memory-only design also cannot
  bridge the unbounded queue-wait gap at all, since nothing is running to
  hold the context during that gap.
- **Storing the complete serialized OTel carrier** (a full `traceparent`/
  `tracestate` header string, or a serialized `SpanContext` blob) instead
  of the two decomposed hex identifiers. Rejected: `tracestate` is
  explicitly designed to carry vendor-specific, potentially large,
  third-party-controlled entries whose contents TaskForge cannot audit —
  storing it verbatim would be exactly the kind of "arbitrary
  user-controlled metadata" §"Security" rejects for Baggage, for no
  benefit, since reconstructing a link requires only the trace ID and
  span ID, not the full carrier. The decomposed, fixed-width `text`
  columns (§"Durable trace-context storage") are simpler, bounded, and
  sufficient.
- **Parent-child spans everywhere, no links.** Rejected outright by the
  roadmap itself (not this ADR's call to make), and independently
  correct to reject: a parent span representing a `QUEUED` job would have
  to remain "open" for an unbounded, arbitrary duration, which
  misrepresents the job's actual state (already durably committed and
  done, per TF-INV-001) as "still in progress," and most trace backends'
  own UIs and sampling/retention behavior assume a parent span's
  lifetime is bounded and short.
- **Links only, everywhere — even within one synchronous, in-process
  call chain.** Rejected: would discard the natural, readable nesting a
  trace backend renders for genuinely bounded, synchronous work (a job
  handler's own downstream instrumented call), for no benefit — the
  rationale for links is specifically the unbounded-persistence-boundary
  problem (§2), which does not exist within one execution.
- **A vendor-specific tracing SDK** (e.g., a proprietary APM agent).
  Rejected: [observability.md](../observability.md) already frames this
  project's intended posture as "instrumentable via standard interfaces
  ... without hard-coding to a specific SaaS," and
  [reference-analysis.md](../reference-analysis.md) independently
  recommends OpenTelemetry specifically. A vendor SDK would lock every
  future trace-backend integration to one vendor for no offsetting
  correctness or performance benefit.
- **Mandatory tracing / a required exporter** (refuse to start without a
  configured collector, or block job completion on successful export).
  Rejected: directly violates the roadmap's own "telemetry failure must
  remain non-load-bearing" requirement and this project's own
  PostgreSQL-is-the-sole-source-of-truth precedent
  ([ADR-0001](0001-postgresql-as-source-of-truth.md)) — no other
  component's availability may gate job processing.
- **Building custom, TaskForge-specific tracing infrastructure instead of
  OpenTelemetry.** Rejected, for the same class of reason
  [ADR-0011](0011-postgresql-native-ha-backup-dr.md) rejected building
  TaskForge-native replication machinery: this is a large, mature,
  already-solved problem with wide backend/UI interoperability,
  duplicating it from scratch would forfeit that interoperability for a
  diagnostic-only feature with no correctness-critical semantics that
  would justify a bespoke format, and
  [reference-analysis.md](../reference-analysis.md) already evaluated
  this space and reached the same conclusion.

## Consequences

**Positive**:

- TaskForge gains a genuine, standards-based answer to "follow this one
  job across an unbounded queue-wait gap and any number of
  retries/reclaims," closing the last of the four P1 operational
  blockers [enterprise-readiness.md](../enterprise-readiness.md) §4
  named that Phases 10–15 had not yet touched.
- The noop-by-default architecture makes "zero behavior change unless
  opted in" a property of the design (an SDK-guaranteed no-op code path
  every process always executes) rather than a claim resting on careful
  conditional logic a future change could invert.
- The schema addition is minimal, additive, and reuses this project's
  already-proven, already-measured migration-safety discipline
  ([data-model.md](../data-model.md) "Phase 12/13 migration lock
  profile" precedent) — no new table, no index, no validation scan.
- `retried_from` finally exists, correcting a real
  documentation/implementation mismatch this ADR's own drafting process
  found ([phase-16-plan.md](../phase-16-plan.md) §4.2), and is designed
  to work correctly whether or not tracing is ever enabled for either the
  original or the replayed job — the durable FK is the load-bearing
  correlation path; tracing is a convenience layered on top of it, never
  a dependency of it.

**Negative, accepted**:

- **A genuinely new production dependency** (`go.opentelemetry.io/otel`
  and its SDK/exporter packages) — the first non-PostgreSQL,
  non-Prometheus, non-test-only dependency in this project's history.
  Accepted because Phase 10's existing, repository-wide CI gates
  (`govulncheck`, CodeQL, Dependabot, dependency-review) already cover
  any dependency the moment it appears in `go.sum`, with no new workflow
  file required — this is a direct, concrete payoff of Phase 10's own
  design, not a new risk this ADR introduces unmitigated.
- **Same-trace-across-attempts, bounded only by `max_attempts`**: an
  operator who configures an unusually large `max_attempts` will see a
  correspondingly large number of linked attempt spans in one trace.
  This is accepted because `max_attempts`' own storage-representability
  ceiling (`job.MaxRepresentableMaxAttempts`, Phase 11) already exists
  independently of tracing, and a deployment choosing a very large value
  already accepts the "retry-scheduling churn" cost
  [compatibility-policy.md](../compatibility-policy.md) already documents
  for that choice — this ADR does not add a new, tracing-specific cap on
  top of it.
- **`error_message` is excluded from spans by default**, meaning an
  operator using traces alone (without also consulting
  `GET /jobs/{id}/history` or logs) will not see full failure detail in
  their trace backend. Accepted deliberately, per §"Security" — free-text
  error messages are exactly the kind of caller-influenced content this
  ADR's redaction posture is cautious about, and the existing
  `GET /jobs/{id}/history` endpoint (this same phase) already provides
  full detail through the properly-scoped, already-audited path.
- **The replay-gets-a-new-trace decision** (§2) means an operator cannot
  see an original job and its replay as one continuous trace timeline
  without following the `retried_from`/link cross-reference manually —
  accepted because the alternative (chaining replays into one trace) has
  no bound on growth, unlike retries, and this ADR prioritizes a bounded,
  well-behaved trace shape over single-trace convenience for an
  operator-triggered, already-rare operation.

**Operational implications**: tracing remains an operator opt-in
requiring a deployed OTLP/HTTP-compatible collector; the four other new
endpoints/metrics this phase adds (`/healthz`, `/readyz`,
`/jobs/{id}/history`, `/jobs/{id}/retry`, HTTP-domain metrics) function
identically regardless of whether tracing is ever configured.

**Compatibility implications**: fully additive per
[compatibility-policy.md](../compatibility-policy.md) — no version-prefix
bump, no existing route/metric-name/response-shape change; an old binary
tolerates the new nullable columns' presence unchanged, exactly like
every prior additive migration in this project's history.

**Expected implementation/test obligations**: this ADR does not restate
[phase-16-plan.md](../phase-16-plan.md) §21's full proof matrix (SF-076
through SF-095) — that matrix, including the span-link tests, the
telemetry-non-load-bearing failure-injection test, the sensitive-attribute
audit, and the migration timing/reversibility tests, is the binding
implementation-time obligation this ADR's decisions must be proven
against, unchanged by this ADR.

**Deferred, deliberately, not decided here**: **OD-F — the exact
OpenTelemetry semantic-convention version/module to pin — is explicitly
NOT resolved by this ADR.** [phase-16-plan.md](../phase-16-plan.md) §29
OD-F already explains why: OTel's messaging semantic conventions are
under active development (*Development*, not *Stable*, status), and
naming a specific version today risks citing a value already stale or
superseded by actual implementation time. The implementer must, at
implementation time, check the then-current published state of
`go.opentelemetry.io/otel/semconv/...`'s messaging conventions, pin that
exact version in `go.mod`, and cite it by version string in both the
span-emitting code's doc comment and this ADR's own future amendment (or
a superseding ADR, per this directory's "never renumbered, a superseding
ADR references the one it replaces" convention) — not invented,
guessed, or hardcoded as part of this planning/decision pass.

## Failure Implications

- If a future change attaches `payload`, `result_metadata`, a raw
  `Idempotency-Key`, or a raw credential to a span attribute by bypassing
  the centralized allowlist helper (§"Security"), sensitive data leaks
  into whatever OTLP collector/backend the operator has configured — the
  sensitive-attribute audit test
  ([phase-16-plan.md](../phase-16-plan.md) SF-092) is the safety net;
  this ADR's centralized-helper convention is the design-level
  defense-in-depth that makes the leak require deliberately bypassing a
  single, reviewable choke point rather than being one careless call away
  in any of several places.
- If a future exporter addition (whether a different OTLP transport or a
  different backend integration entirely) is wired synchronously into the
  `Complete*`/`Claim` critical path instead of through the same
  noop-by-default, asynchronously-batched `TracerProvider` this ADR
  establishes, "telemetry failure must remain non-load-bearing" would be
  silently violated even though the disabled case still works correctly
  — the telemetry-exporter-unavailable failure-injection test
  ([phase-16-plan.md](../phase-16-plan.md) SF-080) must be re-run against
  any future exporter change, not treated as a one-time proof.
- If the same-trace-across-attempts design (§2) is later extended to also
  chain DLQ replays into the original trace, the unbounded-growth
  argument this ADR relies on for the replay decision no longer holds —
  an implementer proposing that change must independently establish a
  bound (e.g., a maximum replay-chain depth) before doing so, not assume
  the `max_attempts`-based justification for retries transfers to
  replays, which have no analogous ceiling today.
- If `internal/tracing`'s noop-provider construction is ever made
  conditional on anything other than "is
  `TASKFORGE_OTEL_EXPORTER_ENDPOINT` set" (e.g., silently defaulting to a
  hardcoded collector address), the "TaskForge startup/runtime must not
  require a collector when tracing is disabled" guarantee this ADR states
  would be broken for any deployment that does not expect one — this must
  be caught by SF-079 ([phase-16-plan.md](../phase-16-plan.md)), which
  this ADR's decision depends on remaining a permanent regression check,
  not a one-time acceptance test.
- Unchanged from every prior ADR in this project: this ADR adds no new
  `TF-INV-*` invariant and does not alter fencing (ADR-0002), retry
  semantics, or dead-lettering — if a future implementation of this ADR's
  design is found to alter any of those, that is a regression against
  this project's existing guarantees, not a consequence this ADR accepts.
