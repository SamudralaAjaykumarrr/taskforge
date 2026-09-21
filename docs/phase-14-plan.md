# Phase 14 Implementation Plan — Upgrade & Compatibility Proof

Status: **PLANNING ONLY. No code written, no migration authored, no ADR
committed.** This document is the pre-implementation plan for Phase 14 —
[docs/enterprise-roadmap.md](enterprise-roadmap.md) "Phase 14 — Upgrade &
Compatibility Proof" — produced by inspecting the repository's own
authoritative documents (roadmap, invariants, testing strategy, data
model, security model, compatibility policy, observability) and the
current `main`-descended implementation at the commit this plan was
written against (`phase-14-planning`, following merge `c17f89c`, "Merge
pull request #22 ... phase-13-workload-governance-retention"). It does not
redefine Phase 14's scope; that scope remains authoritative in
[enterprise-roadmap.md](enterprise-roadmap.md) and, where cited,
[compatibility-policy.md](compatibility-policy.md). Where this document
and those disagree, they win.

This plan follows the discipline [phase-12-plan.md](phase-12-plan.md) and
[phase-13-plan.md](phase-13-plan.md) established: every design choice is
either **settled** (a concrete recommendation, ready to implement) or an
explicitly labeled **open decision (OD-N)** requiring resolution before or
during implementation. Unlike Phase 13, the roadmap's own Phase 14 text
does **not** mandate a blocking ADR by name — so this plan does not invent
one as a hard gate, but §19 recommends one for the expand/migrate/contract
policy adoption itself, consistent with how [docs/adr/README.md](adr/README.md)
defines what belongs in an ADR ("a decision that was actually weighed
against a real alternative").

**A note on sourcing, following [phase-13-plan.md](phase-13-plan.md)'s own
convention**: this plan draws on (1) **roadmap-defined requirements** —
quoted or closely paraphrased from [enterprise-roadmap.md](enterprise-roadmap.md)'s
Phase 14 section, never extended in substance; (2) **already-drafted
PROPOSED policy**, mostly in [compatibility-policy.md](compatibility-policy.md),
which Phase 14's job is to prove, not invent from scratch; and (3)
**design conclusions this plan itself introduces**, explicitly flagged
`(this plan)`, most of them arising from direct inspection of the current
implementation that turned up either a gap the roadmap did not anticipate
or a piece of scope that, on inspection, turns out to already be done.

> **Post-planning update (decision pass)**: the implementation-blocking
> open decisions this document originally left open — **OD-1** (whether
> an ADR is needed), **OD-3** (worker drain timeout), **OD-4** (`cmd/api`
> shutdown-deadline behavior), **OD-6** (the mixed-binary-version test
> fixture), and **OD-7** ("single minor version" skew) — are now
> **CLOSED**, decided against real repository evidence (git history,
> existing config conventions, existing shutdown code). See §19 for each
> decision's full reasoning; §6.4, §7, §8, §9, and §16 are updated in
> place to reflect the closed decisions rather than restating them as
> open. [ADR-0010](adr/0010-expand-migrate-contract.md) has been written
> and is referenced throughout this document — OD-1's question ("does
> this need an ADR") is answered **yes**, or that ADR would not exist.
> OD-2, OD-5, and OD-9 remain genuinely open but are, as assessed, non-
> blocking — none of the six roadmap deliverables in §1 requires them
> resolved before implementation starts.

> **Post-review correction pass**: an independent final review of this
> document found that OD-3's original closure specified the worker drain
> mechanism as an unconditional change to `internal/worker.Worker.Run(ctx)`'s
> existing cancellation contract — which would have broken
> `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
> (`internal/worker/concurrency_stress_test.go`), a currently-passing
> Phase 5 regression test that relies on that contract exactly as it
> stands today. **This has been corrected, not reopened**: the drain
> grace period is now specified as strictly opt-in, via a new, additive
> `Worker.SetDrainTimeout(d time.Duration)` method whose zero value (the
> state every existing caller is already in, including the test above)
> preserves today's immediate-cancellation behavior byte-for-byte; only
> `cmd/worker/main.go` opts in, to `30s`. See §6.4's "Post-review
> correction" note and "Required regression proof" list, §9, §17, and
> §16's SF-064/SF-064a/SF-064b/SF-069 for the corrected design and its
> mandatory proof obligations. **OD-8 is also now CLOSED** by direct
> verification during the same review: `internal/migrate/phase13_migration_test.go`
> already contains the lock-timing measurements §6.5 had flagged as
> possibly missing — only the `data-model.md` write-up remains, already
> itemized in §17.

---

## 1. Exact Phase 14 objective

Per [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 14 — Upgrade &
Compatibility Proof": turn every rule in
[compatibility-policy.md](compatibility-policy.md) currently marked
`PROPOSED` into a **proven** one — proven the same way every other
`TF-INV-*` guarantee in this repository is proven, against real
PostgreSQL and real compiled binaries, not by argument. Concretely, six
deliverables, each with its own roadmap-stated proof obligation:

1. **Formal adoption of expand/migrate/contract** as the primary
   production schema-compatibility model, replacing any universal
   "every migration needs a tested down migration" rule with the
   data-safe-reversible / forward-fix-only split
   [compatibility-policy.md](compatibility-policy.md) already drafts.
2. **A mixed-binary-version integration test harness** that runs two
   different binary versions of `cmd/api` (or `cmd/worker`) concurrently
   against the same database for the full duration of an expand/migrate/
   contract window, not just at the two endpoints.
3. **Proof that an old worker binary continues to function** against a
   post-Phase-13 schema (new queue/concurrency columns present but unused
   by the old worker).
4. **CI enforcement of down migrations for the data-safe-reversible
   subset only**, with forward-fix-only migrations exempted, not silently
   unverified.
5. **Formal adoption of the migration-ordering rule** (schema first,
   worker/server second) as the required deployment sequence.
6. **Graceful worker/API draining**, formalized and tested at the OS
   process level (SIGTERM), extending what Phase 5's
   `TestStress_WorkerPoolGracefulShutdown_*` already proves at the
   goroutine level.

Plus two smaller, explicitly-named items: a scoped resolution of
workflow-definition versioning (documenting the unregistered-`job_type`
failure mode — see §6.3, largely **already done**), and formal adoption of
the deprecation policy already drafted in
[compatibility-policy.md](compatibility-policy.md).

**The one-sentence goal**: convert [compatibility-policy.md](compatibility-policy.md)
from a document of promises into a document of proofs, using the first
real, consequential schema/API changes this project has ever shipped
(Phases 11–13) as the live subject matter, before any of it is needed
under incident pressure.

## 2. Why Phase 14 follows 11–13, and prerequisite verification

[enterprise-roadmap.md](enterprise-roadmap.md)'s own stated reason:
"testing compatibility against a schema that has never actually changed
would be a hollow proof." This plan verifies, by direct inspection, that
the precondition is now satisfied — Phases 11–13 are not merely claimed
complete in the roadmap's status markers, they are actually present in
the working tree at the commit this plan is written against:

| Prerequisite | Evidence |
|---|---|
| Phase 11 (API versioning, `txenqueue`, size limits, `max_attempts` bound) | `/v1/` routes and legacy deprecation header in `internal/api`; `txenqueue/` package at repo root; `internal/job.ValidateSubmission` / `MaxRepresentableMaxAttempts`. |
| Phase 12 (principals, API keys, TLS boundary docs, least-privilege roles) | `internal/principal/`, `migrations/0005`–`0010`, `deploy/postgres-roles.sql`, `cmd/api/main.go`'s `config.FromEnvRequiringPepper`. |
| Phase 13 (queues, governance, retention) | `internal/governance/`, `internal/retention/`, `cmd/taskforge-admin`, `cmd/taskforge-retention`, `migrations/0011`–`0014` (`queue_name`, claimable-by-queue index, governance tables, old-index drop), [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md). |

Fourteen migrations exist today (`migrations/0001` through `0014`), five
of which (`0011`–`0014`, plus the `0005`–`0010` principal/idempotency
sequence before them) are exactly the kind of real, consequential,
production-shaped schema evolution Phase 14 needs as its proof subject —
this repository no longer has to invent a synthetic schema change to test
compatibility against (see §7's OD-6 for which one this plan recommends
using as the concrete two-binary-version test fixture).

**Both of Phase 14's roadmap-stated non-goal preconditions also hold**:
Phase 10 (supply-chain CI) is implemented per its own roadmap section
(`govulncheck`, CodeQL, Dependabot all live in `.github/workflows/`), and
no git tag has ever been pushed (`git tag -l` returns nothing) — this
matters directly for §19 OD-7 below (what "one minor version of skew"
concretely means when no release has ever been tagged).

## 3. Authoritative source for every major requirement

| Requirement | Authoritative source |
|---|---|
| Overall scope, in-scope/non-scope list, invariant/test/exit-criteria checklist | [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 14 — Upgrade & Compatibility Proof" (quoted extensively below) |
| Expand/migrate/contract model, down-migration data-safety split, rolling-upgrade requirements, old/new worker-server compatibility, payload/workflow schema evolution, deprecation policy | [compatibility-policy.md](compatibility-policy.md) (every `PROPOSED` section is this phase's literal to-do list) |
| TF-INV-001 through TF-INV-019 (full current registry) | [invariants.md](invariants.md) |
| Claim-query shape, reclaim/queue-subscription mechanics | `internal/store/claim.go`; [phase-13-plan.md](phase-13-plan.md) §6b |
| Migration lock-safety discipline and measurement precedent | [data-model.md](data-model.md) "Phase 12 migration lock profile" (the only such profile written to date — see §6.5 for why this is itself a gap Phase 14 should close) |
| SIGTERM/drain current behavior | `cmd/worker/main.go`, `cmd/api/main.go`, `internal/worker/worker.go` — direct inspection, §6.4 |
| Unregistered-`job_type` current behavior | `internal/worker/worker.go` (`ErrNoHandler` path), `internal/handler/handler.go`, `internal/worker/worker_test.go`'s `TestRunOnce_NoHandlerRegistered` — direct inspection, §6.3 |
| CI gate structure | `.github/workflows/ci.yml`, `.github/workflows/scheduled-security.yml` |
| Versioning/release state | `internal/buildinfo/`, `git tag -l` (empty), `.github/workflows/release.yml` |
| Scenario-corpus numbering continuation | [docs/scenario-corpus.md](scenario-corpus.md) (ends at SF-036; [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md) separately defines SF-051–SF-059 for Phase 13's mechanism, not yet folded back into scenario-corpus.md — see §14) |
| No AGENTS/HANDOFF/CLAUDE instruction files exist in this repository | Direct repository search (none found) — this plan follows only the documents above |

## 4. In-scope work

Reproduced from [enterprise-roadmap.md](enterprise-roadmap.md) Phase 14
"Exact scope" (verbatim scope, organized under this plan's six
deliverables from §1):

1. Formal adoption of expand/migrate/contract as the primary
   compatibility model; replacement of any universal down-migration rule
   with the data-safe-reversible/forward-fix-only split.
2. A two-binary-version integration test harness (`cmd/api` and/or
   `cmd/worker`), covering the **full duration** of an expand/migrate/
   contract window.
3. A test proving an old worker binary functions against a post-Phase-13
   schema.
4. CI enforcement of down migrations for the data-safe-reversible subset;
   forward-fix-only migrations explicitly labeled and exempted.
5. Formal adoption of the migrate-first, deploy-second ordering rule.
6. Graceful worker/API draining: documented, tested, operator-facing
   SIGTERM/drain contract for `cmd/worker` and `cmd/api`.
7. Scoped resolution of workflow-definition versioning: no versioning
   *system*, but an explicit, tested statement of unregistered-`job_type`
   behavior.
8. Formal adoption of the deprecation policy already drafted in
   [compatibility-policy.md](compatibility-policy.md).

## 5. Explicit non-goals / deferred work

Reproduced from [enterprise-roadmap.md](enterprise-roadmap.md) Phase 14
"Explicit non-scope", plus this plan's own scoping calls flagged
`(this plan)`:

- No workflow-definition-versioning *system* (named/versioned templates) —
  explicitly rejected absent a demonstrated need.
- No automated schema-migration-generation tooling — migrations remain
  handwritten `.up.sql`/`.down.sql` pairs; forward-fix-only migrations may
  omit a meaningful `.down.sql` if explicitly labeled as such.
- No support for skipping more than one minor version in a rolling
  upgrade (single-version-skew compatibility only) — **but see §19 OD-7**:
  this plan surfaces that "one minor version" has no concrete operational
  referent yet (no git tag has ever been pushed), which this phase's own
  scope does not resolve and the roadmap does not ask it to.
- **`(this plan)`** No change to the actual removal/Sunset date of the
  legacy unprefixed API routes. Phase 11 deliberately left this
  undecided; Phase 14 formalizes the deprecation *policy* (minimum
  window, structured-log-warning requirement) but does not itself decide
  to start that clock for the specific legacy-route deprecation already
  in flight — see §19 OD-5.
- **`(this plan)`** No new workload-governance or retention capability —
  Phase 13's mechanisms are the *subject* of this phase's compatibility
  proofs, not something this phase extends or changes.
- **`(this plan)`** No self-service HTTP API or new authenticated surface
  of any kind — this phase's only new operator-facing surface is CI
  tooling (a migration-label lint/audit) and process-level drain
  behavior, neither of which is a network endpoint.
- **`(this plan)`** No change to `queue_name`, concurrency-limit, or
  retention schema/semantics themselves (Phase 13's own contract, unless
  a Phase 14 proof discovers an actual defect in them — see §13).

## 6. Current-state and gap analysis

This is the section [phase-12-plan.md](phase-12-plan.md)/[phase-13-plan.md](phase-13-plan.md)
did not need in this form, because those phases were building genuinely
new mechanisms. Phase 14 is different in kind: most of its roadmap text
describes **proving** something, and direct inspection shows the
underlying mechanism is, in more than one case, **already correct** —
the gap is the proof and the documentation, not the code. Getting this
distinction right changes the phase's actual implementation cost
significantly, so it is stated explicitly, item by item.

### 6.1 Expand/migrate/contract and down-migration labeling — mechanism exists, CI enforcement does not

Every migration in `migrations/0001`–`0014` already has a `.down.sql`
file, and every down file already carries a **prose** reversibility label
in its leading SQL comment: `0006`, `0008`, `0009`, `0011`–`0013` are
labeled "data-safe reversible" or equivalent; `0010` is labeled "honest,
not blanket-safe" (it can legitimately fail, by design —
[phase-12-plan.md](phase-12-plan.md) §5 verification point 13);
`0007`/`0014` carry their own reasoning without using either fixed
phrase verbatim. **What does not exist**: any machine-checkable form of
this label, and any `internal/migrate.Down` function at all —
`internal/migrate/migrate.go` exports `Up`, `ensureMigrationsTable`,
`appliedVersions`, `applyOne`, `loadMigrations`, `parseVersion`, and
nothing that runs a `.down.sql` file programmatically. The only place a
down migration is currently exercised is two hand-written tests for
`0010` specifically
(`TestMigration0010_Down_FailsLoudlyOnDivergentData`,
`TestMigration0010_Down_SucceedsWhenNoDivergentDataExists` in
`internal/migrate/phase12_migration_test.go`), which apply that one
migration's SQL directly, not through a general `Down` API. **The gap is
real and matches the roadmap's own exit criterion exactly**: "CI runs the
down path for every data-safe-reversible migration" requires (a) an
`internal/migrate.Down` (or equivalent) capable of running one migration
file's down SQL against a given database, (b) a parseable label per
migration (§7), and (c) a CI job that walks every migration, classifies
it via that label, and runs down-then-up-again for the reversible subset
only. None of the three exists today.

### 6.2 Two-binary-version integration test — wholly new infrastructure

Confirmed by repository-wide search: no file anywhere imports `os/exec`.
Every existing integration/chaos/stress test in `internal/store`,
`internal/worker`, `internal/chaos` exercises Go packages directly, in
one process, against a real PostgreSQL instance via `internal/testutil`
— never a second compiled binary. Phase 14's mixed-binary-version test is
not an extension of an existing pattern; it requires new test
infrastructure that builds two separate `cmd/api`/`cmd/worker` binaries
(from two different source states) and runs them as real OS processes
against one shared database, for the duration of a migration window,
while the existing `internal/invariant.Checker` and driving traffic
(ordinary HTTP/claim activity) run concurrently. **§19 OD-6 (CLOSED)**
resolves "two different source states" to two exact, pinned commit SHAs
from this repository's real history — see §7's architecture and §16's
test matrix for the concrete mechanism.

### 6.3 Unregistered-`job_type` behavior — already implemented and tested; only the documentation is PROPOSED

Direct inspection of `internal/worker/worker.go` (`RunOnce`, the
`h, found := w.registry.Lookup(j.JobType)` branch) shows: a claimed job
whose `job_type` has no registered handler is **already** deterministically
dead-lettered via `store.CompleteFailure` with `handler.ErrNoHandler{JobType:
...}` as the permanent error, logged as
`"no handler registered for job_type; dead-lettering"` with
`event="permanent_failure"`. This is already covered by
`internal/worker/worker_test.go`'s `TestRunOnce_NoHandlerRegistered`.
[compatibility-policy.md](compatibility-policy.md)'s "PROPOSED: Long-Running
Scheduled Jobs Across Deployments" section, and
[enterprise-roadmap.md](enterprise-roadmap.md)'s own Phase 14 text
("currently undocumented behavior"), both predate this — the roadmap
text was accurate when written but the behavior was implemented
(apparently as an ordinary part of Phase 3's retry/DLQ machinery,
predating this being called out as a Phase-14-specific requirement) and
never back-documented in the compatibility policy. **This roadmap item's
actual remaining work is therefore small**: (a) update
[compatibility-policy.md](compatibility-policy.md)'s "PROPOSED: Long-Running
Scheduled Jobs Across Deployments" and "PROPOSED: Workflows Surviving
Deployments" sections to state the now-proven behavior and cite the
existing test, flipping their status from PROPOSED to implemented for
this specific sub-claim; (b) confirm (a new, small test, not new
production code) the same behavior holds for a workflow node whose
`job_type` has no handler, since `TestRunOnce_NoHandlerRegistered`
exercises only a plain job today, not a workflow node's underlying job —
worth checking explicitly since workflow completion propagation
(TF-INV-012) is a different code path than a plain job's terminal
transition, even though both ultimately call the same
`store.CompleteFailure`.

### 6.4 Graceful draining — `cmd/api` is largely already correct; `cmd/worker` has a real, specific defect

**`cmd/api`** (`cmd/api/main.go`): `signal.NotifyContext(..., syscall.SIGTERM)`
produces `serveCtx`; on `<-serveCtx.Done()`, the process calls
`srv.Shutdown(shutdownCtx)` with a 10-second timeout. Go's
`http.Server.Shutdown` semantics (stdlib, not TaskForge code) already
implement almost exactly the desired contract: close listeners
immediately (no new connections accepted), then wait for active handlers
to finish, up to the caller's context deadline. **This is already close
to correct** and mostly needs formal documentation + an OS-process-level
test proving it, not a code change — see §8 for the one refinement this
plan recommends (making the 10s timeout configurable) and §13 for the one
adversarial case worth adding (a handler whose own PostgreSQL query
outlives the 10s budget: does `Shutdown` forcibly cut it off, or does the
process exit anyway at the deadline? — Go's `Shutdown` returns
`ctx.Err()` once its deadline passes but does **not** kill in-flight
goroutines; `main`'s `run` returns that error and the process then exits
normally via `os.Exit(1)` from `main`, so an in-flight request's own
goroutine can be abandoned mid-flight at process exit. This is a genuine,
if narrow, gap worth a documented, deliberate answer rather than
inherited stdlib behavior nobody decided on purpose.)

**`cmd/worker`** (`cmd/worker/main.go`, `internal/worker/worker.go`): this
is where Phase 14 has real code to design, not just test. The current
mechanism:

```go
// cmd/worker/main.go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
...
err = w.Run(ctx)
```

```go
// internal/worker/worker.go
func (w *Worker) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
        claimed, err := w.RunOnce(ctx)   // <-- same ctx, all the way down
        ...
    }
}
```

`Run` checks `ctx.Done()` only *before* attempting a claim and *after* an
unclaimed poll wait — never while a claimed job is executing. But
`RunOnce` passes that same top-level `ctx` into `runWithHeartbeat`, which
derives the handler's actual execution context from it directly:
`execCtx, cancelExec := context.WithTimeout(ctx, executionTimeout)`. The
consequence: **the instant SIGTERM fires, `ctx` is cancelled, and
`execCtx` — the context handed to `h.Execute(execCtx, j)` for whatever job
is currently in flight — is cancelled at the same instant**, not after a
documented drain grace period. A cooperative handler sees this exactly
like a lease-loss or execution-timeout signal and stops immediately; an
uncooperative handler that ignores `ctx` runs to whatever completion it
would have reached anyway, with no coordination from the drain sequence
at all. Either way, **this is the opposite of "finish in-flight work up
to a timeout" — it is "cancel in-flight work immediately."** This
directly contradicts [compatibility-policy.md](compatibility-policy.md)'s
own PROPOSED requirement ("SIGTERM stops accepting new claims/requests,
waits for in-flight work up to a timeout") and is a genuine defect
relative to that stated design intent, not merely an untested area.
Phase 5's `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
does not catch this because it asserts no goroutine leak and no orphaned
*authority* (a stale worker never wrongly reports success) — both true
regardless of this defect — not that in-flight work is allowed to
*finish*.

**§19 OD-3 (CLOSED) — the fix this plan specifies (implementation is
Phase 14's job, not this plan's, but the design is now decided, not
open)**: separate the two signals that are currently conflated into one
`ctx`, **strictly as an opt-in addition to `Worker`'s existing public
contract — never a change to what an unmodified caller of `Worker.Run`
already observes today.**

> **Post-review correction**: an earlier version of this section
> specified the drain-timeout watcher as observing *whatever* context is
> passed into `Run`, unconditionally. Independent review found this
> would silently change `Worker.Run(ctx)`'s existing, already-relied-on
> cancellation contract for every caller, not just `cmd/worker` — and
> named a concrete, currently-passing regression it would break:
> `internal/worker/concurrency_stress_test.go`'s
> `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`,
> which cancels the context passed to `Run` and asserts every worker
> returns within a **hard 5-second timeout**, using a handler
> (`testdoubles.Gated`) that only stops via context cancellation — a 30s
> unconditional drain window would make that assertion fail. The design
> below is corrected to make the drain grace period **explicitly
> opt-in, at the `Worker` level, with a safe, behavior-preserving zero
> value** — this is the only change relative to the earlier version of
> this section; the `dispositionDraining`/skip-completion/TF-INV-004
> reasoning is unchanged and was independently confirmed correct.

- **`Worker.Run(ctx)`'s existing default semantics are unchanged and
  are the contract every current caller keeps getting for free.** A
  freshly-constructed `*Worker` (via `worker.New`, exactly as today) has
  a **zero-value drain timeout**, and a zero-value drain timeout means:
  the instant `ctx` (the context passed into `Run`) is cancelled,
  `execCtx` — the context handed to the currently-executing handler — is
  cancelled **immediately**, with no grace period at all. This is
  byte-for-byte today's behavior. No existing caller of `Worker.Run`
  (production or test — including
  `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
  and every other test in `internal/worker`/`internal/store`/`internal/chaos`)
  needs to change, because none of them opt into anything new.
- **A new, additive `Worker` method, `SetDrainTimeout(d time.Duration)`**
  (mirroring the existing `SetRetryConfig`/`SetRandSource`/`SetQueues`
  configuration-setter shape already on `*Worker` — the smallest,
  most idiom-consistent addition available, not a new construction
  parameter or a second `New`-like constructor). Passing a positive `d`
  is what opts a `*Worker` instance into the Phase 14 drain contract
  described below; passing `d <= 0` (including never calling it at all,
  the default) restores/keeps the zero-value "cancel immediately"
  behavior above. This mirrors `SetQueues(nil)`'s own existing
  documented convention of "the default is itself a meaningful, explicit
  choice, not merely the absence of a call."
- **Only when `SetDrainTimeout` has been called with a positive value**:
  a **poll-gating context** — `ctx`, the parameter passed into `Run`,
  unchanged — is cancelled immediately on outer cancellation (SIGTERM, in
  `cmd/worker`'s case) and stops `Run`'s loop from attempting any *new*
  claim, exactly as today (this half of the contract was already correct
  and needed no change). An **independent drain context** is used for
  whatever job is already claimed and executing at that moment: `execCtx`
  continues to be `context.WithTimeout(context.Background(), executionTimeout)`
  — bounded only by the job's own `execution_timeout_seconds`, exactly as
  if no shutdown were in progress, so ordinary (non-drain-configured)
  execution-timeout behavior is completely unaffected either way. A new
  goroutine (matching the existing `runWithHeartbeat` heartbeat
  goroutine's own `select`-loop shape, for consistency with the file's
  existing idiom) is started **only in the drain-configured case** and
  watches the poll-gating context: the instant it observes that context's
  `Done()` firing, it starts a fresh `time.AfterFunc(drainTimeout,
  cancelExec)` — so the drain clock starts counting **from the moment the
  outer context is cancelled**, not from claim time, giving the in-flight
  job the full configured window to finish normally from whenever
  cancellation actually arrives. The same goroutine also selects on
  `execCtx.Done()` so it exits cleanly (no leak) if the job finishes, by
  any means, before the outer context is cancelled or before the drain
  timeout elapses. **In the non-drain-configured (default) case, this
  goroutine is never started at all** — `execCtx` is cancelled by the
  existing, unmodified code path the instant `ctx` is cancelled, exactly
  as `TestStress_WorkerPoolGracefulShutdown_*` already requires and
  already tests today.
- **A new, fourth `attemptDisposition` value, `dispositionDraining`**,
  set only when the drain-timeout goroutine above (itself only active
  when opted in) is what called `cancelExec()` (distinguished from
  `dispositionLeaseLost` and `dispositionCancelled`, which are set by the
  existing heartbeat goroutine for unrelated reasons, and from
  `dispositionTimedOut`, which is the job's own execution-timeout budget
  elapsing — none of those three are what happened here). `RunOnce`
  handles `dispositionDraining` **exactly like `dispositionLeaseLost`
  today**: log and return without calling *any* `Complete*` method. This
  is the load-bearing safety choice: a drain-timeout is never reported to
  the store as a cancellation, a timeout, or a failure — the job's row is
  left exactly as it was (`RUNNING`, under this worker's
  `lease_owner`/`lease_generation`), and it becomes reclaimable the
  moment its existing `lease_expires_at` passes, through the
  **already-proven** TF-INV-004 mechanism — the identical path an
  ordinary worker crash takes today. This is what makes "lease/retry/
  reclaim safety remains intact" true by construction rather than by new,
  unproven code: drain-timeout expiry is deliberately made
  indistinguishable, from the job's and the store's perspective, from the
  process being killed at that exact instant — a scenario TF-INV-004
  already handles and Phase 2/5/9 already prove. No `job_attempts` row is
  written recording a misleading "failure" or "timeout" caused by
  infrastructure shutdown rather than the handler's own behavior. This
  disposition, and the goroutine that produces it, exist in the code
  regardless of opt-in status (it is ordinary Go source, always compiled
  in), but are only ever *reachable* for a `*Worker` that called
  `SetDrainTimeout` with a positive value — an un-opted-in `*Worker` can
  never produce `dispositionDraining`, by construction (the goroutine
  that sets it is never started).
- `Run` must not return, and the process must not exit, until the
  in-flight job's `RunOnce` call has actually returned (whichever
  disposition applies) — a `sync.WaitGroup` (size 0 or 1, since Phase
  1–13's `Worker` runs one job at a time per `Worker` instance) makes
  this ordering explicit rather than implicit in the existing loop
  structure. This applies uniformly to both the default and
  drain-configured cases — it is not itself new opt-in behavior, only
  made explicit as part of this change.
- **`cmd/worker/main.go` is the sole caller that opts in**: it
  constructs `*Worker` exactly as today, then calls
  `w.SetDrainTimeout(cfg.WorkerDrainTimeout)` where `cfg.WorkerDrainTimeout`
  comes from the new `TASKFORGE_WORKER_DRAIN_TIMEOUT` config value
  (§9), **default `30s` for the real worker process** — this default
  applies only because `cmd/worker` explicitly wires it in, never because
  `Worker`'s own zero value changed. No other in-repository caller of
  `worker.New` (any existing or future test, `cmd/chaos`, or any other
  direct `internal/worker` caller that does not itself call
  `SetDrainTimeout`) is affected by this default at all.

**The concrete SIGTERM sequence in `cmd/worker`, once wired as above**:

1. SIGTERM arrives → the poll-gating context (`ctx`, from
   `signal.NotifyContext`) is cancelled immediately → `Run`'s loop stops
   attempting any *new* claim.
2. Whatever job is already claimed and executing continues normally,
   under its own independent drain context, for up to
   `TASKFORGE_WORKER_DRAIN_TIMEOUT` (30s default) from the moment SIGTERM
   was received.
3. If the job finishes (any outcome) before the drain timeout elapses,
   it is reported to the store normally (`CompleteSuccess`/
   `CompleteFailure`/etc., exactly as if no shutdown were in progress) —
   the drain-timeout goroutine observes `execCtx.Done()` and exits
   without ever setting `dispositionDraining`.
4. If the drain timeout elapses first, `execCtx` is cancelled,
   `dispositionDraining` is set, and `RunOnce` returns **without calling
   any `Complete*` method** — no terminal or retry transition is written.
5. The job's lease is left to expire and is reclaimed through the
   existing, unmodified TF-INV-004 path, exactly like an ordinary worker
   crash.
6. Only after the in-flight `RunOnce` call has returned (step 3 or step
   4) does `Run` itself return and the process exit.

**Required regression proof (mandatory before this item is considered
done — not optional hardening)**: the opt-in redesign above is only an
acceptable fix if the following four properties are each demonstrated by
a passing test, not merely argued for in prose:

1. **`TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
   (`internal/worker/concurrency_stress_test.go`) continues passing
   unmodified** — same assertions, same 5-second hard timeout, no change
   to the test file itself. This is the direct regression check for the
   defect independent review found; it is not sufficient to reason that
   the zero-value default preserves behavior — the existing test must
   actually be re-run and shown green against the changed code.
2. **Default `Worker.Run` cancellation remains prompt for any `*Worker`
   that never calls `SetDrainTimeout`** — a new, dedicated test
   (`TestRunOnce`/`TestRun`-level, not merely relying on item 1) asserting
   that for an un-opted-in `*Worker`, cancelling the context passed to
   `Run` cancels the executing handler's context immediately (bounded by
   a tight, sub-second assertion window, not merely "eventually"),
   confirming the zero-value case is exercised directly, not only as an
   incidental side effect of item 1's larger stress test.
3. **`cmd/worker`'s opt-in drain allows an in-flight job to finish before
   the configured deadline** — SF-064 (§16): a real, separately-run
   `cmd/worker` binary, sent a real SIGTERM while executing a job whose
   handler finishes within `TASKFORGE_WORKER_DRAIN_TIMEOUT`, completes
   that job normally (`CompleteSuccess`/whatever the handler's real
   outcome is) before the process exits, and attempts no new claim after
   the signal.
4. **Drain-timeout cancellation leaves the job reclaimable and writes no
   invalid terminal transition** — SF-069 (§16): a real `cmd/worker`
   binary, sent SIGTERM while executing a job whose handler is made to
   run *longer* than `TASKFORGE_WORKER_DRAIN_TIMEOUT`, exits without
   calling any `Complete*` method (directly observable: the job's row is
   still `RUNNING`, under the draining worker's own
   `lease_owner`/`lease_generation`, immediately after that worker's
   process has exited); a second, freshly-started worker then reclaims
   and completes the job once `lease_expires_at` passes, through the
   ordinary, unmodified TF-INV-004 path — proving no `job_attempts` row
   was written recording a false failure/timeout, and no state
   transition occurred that TF-INV-005/TF-INV-013 would flag as invalid.

**Symmetric redesign for `cmd/api` (§19 OD-4, CLOSED)**: the same
"bounded, then explicitly cancelled — never silently abandoned" principle
is applied to `cmd/api`. Today, `http.Server.Shutdown` waits for
in-flight handlers up to its context deadline and then returns, but does
**not** cancel any still-running handler's own request context — a
goroutine serving a request that outlives the shutdown deadline is
silently abandoned at process exit, an inherited stdlib default nobody in
this codebase decided on purpose. This phase changes that: `cmd/api`
supplies an explicit, cancellable `http.Server.BaseContext` (a single
process-lifetime context, not per-request), and after
`srv.Shutdown(shutdownCtx)` returns — whether because every handler
finished or because `shutdownCtx`'s deadline (`TASKFORGE_API_SHUTDOWN_TIMEOUT`,
default 10s — see §9) passed — the process explicitly cancels that base
context too. Any handler still running at that point (and therefore
already known to be taking longer than the configured shutdown budget)
observes cancellation through `r.Context()` immediately afterward, rather
than being left to run to whatever completion it eventually reaches on
its own, unobserved. This makes `cmd/api`'s shutdown behavior
deterministic and testable end-to-end (SF-070, §16), matching the same
contract `cmd/worker`'s redesign above provides, instead of leaving one
process's shutdown behavior decided and the other's inherited by
accident.

This is a **behavior change** for `cmd/api` (unconditionally — every
`cmd/api` process now gets `BaseContext` cancellation at its shutdown
deadline) and an **additive, opt-in-only** change for `internal/worker`
(no existing `Worker.Run` caller's behavior changes at all; only
`cmd/worker`, which explicitly opts in, observes new behavior) — not a
pure test addition either way. Flagged clearly here because an
implementer reading only the roadmap's "formalize what Phase 5 already
proves at the goroutine level" language could reasonably (and
incorrectly) conclude no code change is needed. It is, and the exact
shape of that change — including, on the worker side, exactly which
callers are and are not affected — is now fully specified above, not
left to be improvised at implementation time.

### 6.5 Migration lock-safety documentation lags Phase 13 (OD-8, CLOSED — write-up only, measurement already exists)

[data-model.md](data-model.md)'s "Phase 12 migration lock profile" section
measures and tabulates lock behavior for migrations `0005`–`0010` only.
Migrations `0011`–`0014` (queue_name, the new claimable index, the
governance tables, the old index drop) have no equivalent measured
profile written up anywhere in the docs, even though
[phase-13-plan.md](phase-13-plan.md) §11 explicitly promised one ("this
phase's index build deserves the same
`TestPhase1X_...BlocksWritesAndTheClaimQuery`-style test, not a repeated
optimistic claim"). **§19 OD-8 (CLOSED) resolves this by direct
verification**: `internal/migrate/phase13_migration_test.go` already
contains exactly this measurement
(`TestPhase13Migration0011_AddColumnIsFastRegardlessOfTableSize`,
`TestPhase13Migration0012_BlocksWritesAndReadsAreUnaffected`,
`TestPhase13Migration0013_BlocksWritesViaForeignKeyLock_ReadsUnaffected`,
`TestPhase13Migration0014_BlocksOnAccessExclusive_ButIsCatalogOnly`) — the
promise was kept; only the `data-model.md` write-up summarizing these
already-passing tests' results (mirroring the "Phase 12 migration lock
profile" section's format) remains outstanding, already itemized in §17.

## 7. Architecture

```
                    +-----------------------------------------+
                    |  Migration label registry (NEW)          |
                    |  - one parseable token per migration      |
                    |  - "data-safe-reversible" |                |
                    |    "forward-fix-only"                      |
                    +-----------------------------------------+
                                |                  |
                                v                  v
              +------------------------+   +--------------------------+
              | internal/migrate.Down   |   | CI job (NEW): for every   |
              | (NEW) -- runs one       |   | migration labeled         |
              | migration's .down.sql   |   | data-safe-reversible, run |
              | against a live DB       |   | up -> down -> up again    |
              +------------------------+   +--------------------------+

   +----------------------------------------------------------------+
   |  Two-binary-version test harness (NEW), internal/compattest or  |
   |  similar -- §19 OD-6 CLOSED, exact fixture below                |
   |                                                                  |
   |   OLD binary: git worktree --detach @ 794abbb57a7e2a965d556700 -+--> shared,
   |     468930a1ebed23e4 ("Merge pull request #21 ...                |   real
   |     phase-13-concurrency-analysis" -- last commit before         |   Postgres
   |     Phase 13's schema changes; migrations 0001-0010 only)        |   instance,
   |   NEW binary: git worktree --detach @ c17f89c592c803a8f7d2fbd6 -+   both run
   |     1cde566467ebe062 ("Merge pull request #22 ...                |   as real
   |     phase-13-workload-governance-retention" -- current main/     |   OS
   |     phase-14-planning tip; migrations 0001-0014, full Phase 13)  |   processes,
   |                                                                  |   for the
   |   both binaries built via `go build ./cmd/worker` (and           |   full
   |   `./cmd/api`) FROM WITHIN each pinned worktree -- immutable      |   0011-0014
   |   commit SHAs only, never a branch name                          |   window
   |                                                                  |
   |   driving traffic: ordinary HTTP submissions + worker claims      |
   |   from BOTH binaries concurrently, plus internal/invariant.Checker|
   |   re-run continuously (mirrors internal/chaos's existing pattern) |
   +----------------------------------------------------------------+

   +----------------------------------------------------------------+
   |  cmd/worker drain redesign (§6.4, §19 OD-3 CLOSED) -- OPT-IN      |
   |  at the Worker level; default (SetDrainTimeout never called,     |
   |  every EXISTING caller incl. TestStress_WorkerPoolGraceful       |
   |  Shutdown_*) is UNCHANGED: outer ctx cancelled -> execCtx         |
   |  cancelled immediately, no grace period, exactly as today         |
   |                                                                  |
   |  cmd/worker ALONE calls w.SetDrainTimeout(cfg.WorkerDrainTimeout) |
   |  (TASKFORGE_WORKER_DRAIN_TIMEOUT, 30s default) -- only then:      |
   |                                                                  |
   |   SIGTERM --> [poll-gating ctx: cancelled now] --> Run() stops   |
   |                 attempting new claims immediately                |
   |            --> [independent drain ctx for in-flight job: NOT     |
   |                 derived from the poll-gating ctx; bounded by the |
   |                 job's own execution_timeout_seconds as always,   |
   |                 PLUS a side goroutine (only started because      |
   |                 SetDrainTimeout was called) that starts a 30s    |
   |                 timer the instant SIGTERM is observed]           |
   |            --> drain-timeout fires: dispositionDraining -- NO    |
   |                 Complete* call is made; lease is left to expire  |
   |                 and be reclaimed via the existing TF-INV-004      |
   |                 path, exactly like an ordinary crash              |
   |            --> process exit only after in-flight RunOnce returns |
   +----------------------------------------------------------------+

   +----------------------------------------------------------------+
   |  cmd/api drain redesign (§6.4, §19 OD-4 CLOSED)                  |
   |                                                                  |
   |   SIGTERM --> srv.Shutdown(10s, TASKFORGE_API_SHUTDOWN_TIMEOUT)  |
   |            --> on return (finished OR deadline passed): the      |
   |                 process explicitly cancels its BaseContext, so   |
   |                 any still-running handler observes cancellation  |
   |                 promptly instead of being silently abandoned     |
   +----------------------------------------------------------------+
```

**No new network-facing component.** Every new piece is test/CI
infrastructure or a change to `cmd/worker`'s existing shutdown sequence —
consistent with [architecture.md](architecture.md)'s "smallest
architecture" philosophy and every prior phase's own precedent (push
correctness into existing processes/tests, not a new service).

## 8. Migration labeling and the down-migration CI mechanism

**Settled recommendation**: add a single-line, machine-parseable marker
as the *first* line of every `.down.sql` file (existing prose comments
stay, unchanged, below it — this is additive to the existing convention,
not a replacement):

```sql
-- taskforge:down-migration-status: data-safe-reversible
```
or
```sql
-- taskforge:down-migration-status: forward-fix-only
```

`0010`'s "honest, not blanket-safe" case is still `data-safe-reversible`
under this scheme — CI still runs its down migration, but the migration's
own SQL, not CI, is what decides whether that run succeeds (per
[phase-12-plan.md](phase-12-plan.md) §5's original design for exactly
that file: it is a deliberate, expected-sometimes-to-fail down migration,
not an exemption from being run at all). A `forward-fix-only` migration's
`.down.sql` file, where one exists at all, is exempted from CI's
up→down→up cycle but is not deleted — some forward-fix-only migrations
may still usefully contain a `-- see migration NNNN for the forward fix`
comment for a future reader, which is documentation, not a tested
guarantee.

**`internal/migrate.Down(ctx, db, version int64) error`** (new,
additive): loads and runs exactly one migration's `.down.sql` inside a
single transaction, mirroring `applyOne`'s existing shape for `.up.sql`
files, and removing that version's row from the `schema_migrations`
tracking table on success — symmetric with what `applyOne` does today.

**CI job (new, in `.github/workflows/ci.yml` or a dedicated
`migration-reversibility.yml`)**: for every migration file in
`migrations/`, parse the marker; for every `data-safe-reversible` one, in
ascending version order against a freshly-migrated-to-that-point
database, run `Up` through that version, then `Down` for that version,
then `Up` again for that version, and assert the resulting schema state
(via the same schema-introspection queries `internal/migrate_test.go`
already uses for its upgrade tests) is identical to running `Up` straight
through without the down/up detour. A migration file with **no** marker
at all is a **CI failure**, not a silent skip — this is the mechanism
that makes "every forward-fix-only migration is explicitly labeled"
(the roadmap's own exit criterion) actually enforced rather than merely
documented.

**Retrofit requirement**: all fourteen existing `.down.sql` files need
the new marker line added, classified per their existing prose comments
(§6.1 already identifies which is which) — a mechanical, low-risk change
that is nonetheless *implementation*, not part of this planning pass.

**Additive capability needed for §16's mixed-binary-version harness**:
proving the two-binary test holds "for the full duration of the
expand/migrate/contract window, not just at the two endpoints" (the
roadmap's own explicit language) requires applying migrations `0011`
(expand: `queue_name` column), `0012` (expand: new claimable-by-queue
index), `0013` (expand: governance tables), and `0014` (contract: drop
the old index) **incrementally**, with both the OLD and NEW binaries
(§7, §19 OD-6) running and exercised against the database at each
intermediate stage — not applied as one atomic batch via the existing
`migrate.Up`, which applies every pending migration in one call. This
requires a small, additive `internal/migrate` capability (e.g. `UpTo(ctx,
db, targetVersion int64) error`, applying only pending migrations up to
and including `targetVersion`) that does not exist today
(`internal/migrate/migrate.go` exports only `Up`, which has no version
ceiling parameter). This is implementation scope for §16's harness, not a
new production entry point — `cmd/api`/`cmd/worker` continue to call
`migrate.Up` unchanged; only the compat-test harness calls `UpTo`.

## 9. API / CLI / config changes

**No new HTTP endpoint, no new request/response field.** This phase's
only config-shaped additions:

- **`TASKFORGE_WORKER_DRAIN_TIMEOUT`** (new, `cmd/worker`,
  `time.Duration`-parsed via the existing `FromEnv` convention, mirroring
  `TASKFORGE_ACTIVE_WORKER_WINDOW`'s/`TASKFORGE_WORKER_POLL_INTERVAL`'s
  own `time.ParseDuration`-with-default shape in `internal/config`): the
  upper bound on how long `cmd/worker` waits for an in-flight job to
  finish after SIGTERM, independent of that job's own
  `execution_timeout_seconds`. **§19 OD-3, CLOSED: default `30s` for the
  real `cmd/worker` process.** This is a `cmd/worker`/`internal/config`
  process-level default only — it is not, and must not be read as,
  `internal/worker.Worker`'s own zero value (§6.4's post-review
  correction): `Worker.New` continues to construct a `*Worker` with no
  drain grace period configured at all (the zero value, meaning
  "cancel `execCtx` immediately," today's exact behavior, preserved for
  every caller that does not explicitly opt in). `cmd/worker/main.go` is
  the **only** place `30s` is ever wired in, via
  `w.SetDrainTimeout(cfg.WorkerDrainTimeout)` after constructing `w`
  exactly as today — see §6.4's "Required regression proof" for the
  tests that must confirm this separation actually holds. Rationale for
  the `30s` value itself, since `internal/config` has no existing
  convention for this specific kind of timeout to inherit from
  (`TASKFORGE_WORKER_POLL_INTERVAL` defaults to 500ms and
  `TASKFORGE_ACTIVE_WORKER_WINDOW` to 30s, but neither governs shutdown
  behavior): 30s is long enough for an ordinary heartbeat-and-complete
  round trip to finish cleanly (heartbeat interval is
  `executionTimeout / heartbeatIntervalFraction`, i.e. at most a third of
  whatever the job's own budget is — for any job whose
  `execution_timeout_seconds` is itself under roughly 90s, at least one
  full heartbeat cycle fits inside the drain window), and it matches
  Kubernetes' own default `terminationGracePeriodSeconds` (30s) exactly,
  so an operator deploying under default Kubernetes settings does not
  silently race their orchestrator's own SIGKILL against this drain
  window — the two defaults were chosen to agree, not merely coexist.
  This value is decided, not left open; an operator with longer-running
  jobs is expected to raise it explicitly (and, per §18, to raise their
  orchestrator's own grace period to match).
- **`TASKFORGE_API_SHUTDOWN_TIMEOUT`** (new, `cmd/api`): makes the
  currently-hardcoded 10-second `srv.Shutdown` budget
  (`cmd/api/main.go`) operator-configurable. **§19 OD-4, CLOSED: default
  `10s`**, i.e. the config default exactly reproduces today's hardcoded
  value — this is not a behavior change to the *duration*, only to (a)
  making it configurable and (b) the explicit-cancellation-at-deadline
  change described in §6.4 (a behavior change to what happens *once* the
  deadline is reached, not to the deadline's length).

**CLI**: no new `cmd/taskforge-admin` subcommand. The migration-label
audit (§8) is a CI-only concern, not an operator-facing tool, consistent
with this project's existing precedent of keeping operator tooling
minimal (`phase-13-plan.md` §5's "no self-service HTTP API" non-goal,
extended here by analogy: no new operator surface where a CI check
suffices).

**Deprecation policy config**: none needed — the deprecation *policy*
(minimum one minor-version cycle, structured log warning) is a documented
convention this phase formally adopts in
[compatibility-policy.md](compatibility-policy.md), not a runtime
toggle. The existing `Deprecation: @1788998400` header and
`deprecated_route_used` log line (Phase 11) already implement the
mechanism the policy describes; this phase's job is to state, in writing,
that the mechanism is now the *adopted* policy for any future
deprecation, not merely what Phase 11 happened to build once.

## 10. Security / trust boundary implications

- **No new principal type, no change to authentication or authorization.**
  This phase touches process lifecycle (drain) and CI/test tooling
  (migration labels, mixed-binary harness), not the request-authorization
  path Phase 12 built.
- **The two-binary-version test harness runs two real binaries against a
  real database and must not weaken any existing credential/role
  boundary to do so.** Both the "old" and "new" test binaries connect
  using the same least-privilege roles (`taskforge_api`/`taskforge_worker`)
  a production deployment would use — this is in fact one of the things
  the harness incidentally re-proves (an old binary's queries still
  succeed under the roles Phase 12 defined), not a reason to grant it a
  superuser/test-only connection string. `TASKFORGE_TEST_DATABASE_URL`
  (the existing CI convention) is reused; no new credential material is
  introduced.
- **The drain-timeout config (§9) is not a security boundary** — it
  bounds how long a shutting-down process keeps running someone else's
  in-flight work, not who is allowed to submit work. No authorization
  implication.
- **CI's migration-reversibility job must not run against a database
  containing real credential material** — it runs against the same
  ephemeral, disposable `TASKFORGE_TEST_DATABASE_URL` Postgres service
  container every other CI job already uses (`.github/workflows/ci.yml`),
  never a persistent or production-adjacent instance. Stated explicitly
  because a down-migration test is, by construction, a destructive
  operation (`DROP COLUMN`/`DROP TABLE`) and must never be pointed at
  anything but disposable, per-run infrastructure.

## 11. Invariant / proof obligations

Phase 14 adds **no new numbered invariant**. Its proof obligation is
explicitly **the existing set, TF-INV-001 through TF-INV-019, holding
throughout a mixed-binary-version window** — a breadth requirement across
an operational scenario (concurrent old/new binaries), not a new
state-machine property. This mirrors how [invariants.md](invariants.md)'s
own Cross-Phase Governance Additions section classifies non-`TF-INV`
requirements: a deployment/compatibility contract is not, by that
document's own stated method, "a durable state-machine safety property"
in its own right — it is a proof obligation *about* the existing
properties, run under a new adversarial condition.

- **Every `TF-INV-*` invariant holds for the full duration of the
  two-binary-version test** (§8's harness runs `internal/invariant.Checker`
  continuously against the shared database throughout the window, the
  same discipline `internal/chaos`'s combined campaigns already use).
- **TF-INV-004's queue-subscription interaction (established in
  [phase-13-plan.md](phase-13-plan.md) §6b/§10)** is the specific
  invariant-adjacent property this phase's "old worker against
  post-Phase-13 schema" test (§4 item 3) must re-confirm under a real,
  separately-compiled old binary, not merely the existing in-process
  `SF-046`/`SF-049`-style tests — those prove the *query* behaves
  correctly; this phase's test proves an actual old *binary* (which
  cannot know about `queue_name` at the Go type level at all) behaves
  correctly, a strictly stronger claim.
- **No invariant is weakened, narrowed, or reinterpreted by this phase.**
  Every existing test in `internal/store`, `internal/worker`,
  `internal/chaos` must remain green, unmodified in assertion strength,
  throughout Phase 14's implementation — the same "full regression pass
  required" discipline [phase-13-plan.md](phase-13-plan.md) §10 states
  for itself.

## 12. Migration requirements and rollback concerns

- **No new schema migration is strictly required by this phase's own
  scope** — Phase 14 proves compatibility of migrations that already
  exist (`0001`–`0014`); it does not need a new column or table to do so,
  *unless* §19 OD-6 resolves toward authoring a small, deliberately
  synthetic migration as the two-binary-version test's fixture rather
  than reusing an existing historical boundary (§19 OD-6 states both
  options and this plan's recommendation).
- **The migration-label retrofit (§8)** touches only `.down.sql` files'
  leading comment line — an additive, zero-risk, non-schema change (a
  comment, not SQL that PostgreSQL executes differently).
- **Rollback of Phase 14's own work, if needed**: since this phase adds
  no schema, "rollback" here means reverting the `cmd/worker` drain
  redesign (§6.4) and the CI migration-reversibility job (§8) via
  ordinary code revert — no data-migration rollback concern exists for
  this phase specifically.

## 13. Observability requirements

New metrics/logs (cardinality-audited per
[observability.md](observability.md)'s existing discipline — none of the
below introduces a new high-cardinality label; all are either unlabeled
or labeled by a small, bounded, operator-controlled set of values):

| Signal | Type | Purpose |
|---|---|---|
| `taskforge_worker_drain_duration_seconds` | histogram | How long `cmd/worker` actually waited between SIGTERM and process exit — validates the drain redesign (§6.4) is doing something, not just present in code. |
| `taskforge_worker_drain_timed_out_total` | counter | Count of drains that hit `TASKFORGE_WORKER_DRAIN_TIMEOUT` before the in-flight job finished on its own — an operator-visible signal that the configured timeout may be too short relative to real job durations. |
| structured log `worker_drain_started` / `worker_drain_completed` | log event | `event="worker_drain_started"` on SIGTERM receipt (before the poll-gating context is cancelled), `event="worker_drain_completed"` with `outcome` (`"in_flight_job_finished"` / `"drain_timeout_exceeded"` / `"no_job_in_flight"`) on actual exit — mirrors the existing `retention_sweep_started`/`retention_sweep_completed` pairing precedent from Phase 13. |
| structured log `api_shutdown_started` / `api_shutdown_completed` | log event | Same pairing for `cmd/api`'s existing `srv.Shutdown` call — today this path has no structured log event at all beyond the plain `"shutting down api server"` info line; this phase adds the event-tagged pair for consistency with the worker side and with every other phase's `_started`/`_completed` convention. |

**What this phase does not change**: no existing Phase 8/12/13 metric or
log event's name or meaning changes
([compatibility-policy.md](compatibility-policy.md)'s own "never
repurposes an existing metric name" rule, which this phase is itself
formally adopting — applying it to its own additions is the first test of
whether the policy is actually followed once adopted).

## 14. Failure modes and adversarial cases

| Failure scenario | Mechanism this plan provides |
|---|---|
| A rolling deploy is half-complete when a job only the new binary understands is claimed by an old worker | Covered by TF-INV-004's queue-subscription rule (already correct, §11) plus this phase's mixed-binary-version test proving it holds for real compiled binaries, not just in-process query behavior. |
| An operator runs a down migration in production against a still-populated table for a migration **not** labeled data-safe-reversible | The new marker (§8) makes this a CI-enforced distinction; production enforcement is an operator-tooling question this plan does not solve (no `internal/migrate` production down-migration CLI is proposed — down migrations remain a manual, deliberate operator action against raw `.down.sql` files, exactly as `Up` migrations are today via `migrate.Up`, never auto-run against production). |
| A `job_type` string is repurposed for an incompatible payload shape across a deploy | [compatibility-policy.md](compatibility-policy.md) already documents the guidance (never repurpose); this phase's own scope does not add a test proving what happens if violated — **flagged as a real gap the roadmap's own "Failure scenarios to guard against" text explicitly names** ("this must be paired with a test proving what actually happens if it's violated, so the failure mode is known even though it's not prevented"). This plan adds it to §16's test matrix (SF-064) rather than silently dropping it. |
| A worker is SIGKILLed mid-drain | Explicitly out of scope, per the roadmap's own text — this is TF-INV-004's lease-expiry/reclaim mechanism (already proven), not a Phase 14 drain-contract concern. The drain redesign (§6.4) makes no claim about surviving SIGKILL. |
| **`(this plan)`** The mixed-binary-version harness's "old" binary is built from a git ref that later drifts (branch moves, force-push) so the test becomes non-reproducible | **§19 OD-6, CLOSED**: pinned to immutable commit SHAs `794abbb57a7e2a965d556700468930a1ebed23e4` (old) and `c17f89c592c803a8f7d2fbd61cde566467ebe062` (new), recorded directly in the harness's own source, never a branch name. |
| `cmd/api`'s `srv.Shutdown` deadline passes while a handler's own PostgreSQL query is still running | **§19 OD-4, CLOSED**: no longer an unexamined stdlib default — the process explicitly cancels its `BaseContext` once `srv.Shutdown` returns (whether from completion or deadline), so any still-running handler observes cancellation promptly instead of being silently abandoned (§6.4, §9). See §16 SF-070. |
| **`(this plan)`** A worker's drain-timeout goroutine (§6.4) fires while a job is executing; the process must not report a misleading job outcome | §19 OD-3's `dispositionDraining` design: no `Complete*` call is made at all; the lease is left to expire and is reclaimed through the existing, already-proven TF-INV-004 path — see §16 SF-069. |
| **`(this plan)`** A workflow node's `job_type` has no registered handler | §6.3: almost certainly already correct via the same `store.CompleteFailure`/`ErrNoHandler` path a plain job uses, but not yet confirmed by a dedicated test — §16 SF-062. |

## 15. Compatibility / backward-compatibility requirements

- **This phase changes `cmd/worker`'s SIGTERM behavior** (§6.4) — this
  is itself a behavior change an operator upgrading across this phase
  must be aware of: pre-Phase-14 workers cancel in-flight work
  immediately on SIGTERM; post-Phase-14 workers drain it, up to a
  configurable timeout. This is a **desired** compatibility improvement
  (it makes the actually-shipped behavior match what
  [compatibility-policy.md](compatibility-policy.md) always said it
  should be), not a regression, but it must be called out in
  [roadmap.md](roadmap.md)/a changelog per this phase's own adopted
  deprecation-policy discipline (§9) — a behavior change to an
  operational contract is exactly the kind of thing that policy exists
  to announce.
- **No change to any wire format, schema shape, or metric/log name**
  beyond the additive observability signals in §13.
- **The migration-label marker (§8) is additive to existing `.down.sql`
  files** — no existing migration's `.up.sql` (the only file
  `internal/migrate.Up` actually runs in production) changes at all.
- **`internal/migrate.Down` is new, additive, unexported-by-default-risk**
  surface — it is not wired into `cmd/api`'s or `cmd/worker`'s own
  startup path (both call only `migrate.Up`, unchanged), so its
  existence cannot accidentally cause a running server to roll back its
  own schema.

## 16. Deterministic test / proof matrix

Continuing the scenario-corpus numbering. [docs/scenario-corpus.md](scenario-corpus.md)
itself currently ends at **SF-036**; [ADR-0009](adr/0009-phase-13-concurrency-and-fairness.md)
separately defines SF-051 through SF-059 for Phase 13's concurrency
mechanism, not yet folded back into `scenario-corpus.md`. This plan
proposes **SF-060 onward**, explicitly flagging the same reconciliation
risk [phase-13-plan.md](phase-13-plan.md) noted for its own ADR (final
numbering to be confirmed against whichever of `scenario-corpus.md` or
the ADR's own numbering is authoritative by implementation time — this
plan recommends folding SF-051–059 back into `scenario-corpus.md` as
part of Phase 14's own documentation-hygiene work, precisely because
Phase 14 is the phase auditing cross-document consistency anyway, per
§6.5's similar finding).

| # | Scenario | Proves |
|---|---|---|
| SF-060 (proposed) | Down-then-up-again round trip for every migration labeled `data-safe-reversible`, resulting schema identical to a straight-through `Up` | Roadmap's own required test; §8's CI mechanism. |
| SF-061 (proposed) | Every migration file carries a `taskforge:down-migration-status` marker; a file with none fails the audit | Roadmap's "every forward-fix-only migration is explicitly labeled" exit criterion. |
| SF-062 (proposed) | A workflow node's `job_type` has no registered handler | §6.3/§14 — extends the existing plain-job test (`TestRunOnce_NoHandlerRegistered`) to the workflow-node completion path, confirming TF-INV-012's cascade behaves identically. |
| SF-063 (proposed) | `cmd/api` under SIGTERM: new connections refused immediately; an in-flight request completes if it finishes within `TASKFORGE_API_SHUTDOWN_TIMEOUT` (default 10s, §9) | §6.4/§9's documented, now-decided answer to ordinary (within-budget) shutdown, at the real OS-process level (an actual `cmd/api` binary sent a real `SIGTERM`, not an in-process `context.Context` substitute). |
| SF-064 (proposed) | A real, separately-run `cmd/worker` binary (which alone calls `SetDrainTimeout`) executing a job of `job_type="X"` receives SIGTERM; the in-flight job completes normally before the process exits, because it finishes within `TASKFORGE_WORKER_DRAIN_TIMEOUT` (default 30s, §9); no new claim is attempted after SIGTERM is received | §6.4's opt-in drain redesign, "Required regression proof" item 3 — the roadmap's own required graceful-drain test, at the real OS-process level, exercising `cmd/worker`'s specific opt-in wiring. |
| SF-064a (proposed, new — §6.4 "Required regression proof" item 2) | An un-opted-in `*Worker` (constructed via `worker.New`, `SetDrainTimeout` never called) has its `Run`-context cancelled while a job is executing; the handler's context is observed cancelled immediately (sub-second assertion window), not after any grace period | Directly proves the zero-value default preserves today's prompt-cancellation behavior — the specific property independent review found missing, tested in isolation rather than only incidentally via SF-064b's larger stress test. |
| SF-064b (regression re-run, not new — §6.4 "Required regression proof" item 1) | `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority` (`internal/worker/concurrency_stress_test.go`), re-run **unmodified** against the changed code | The direct regression check for the defect independent review found and this correction pass fixed — must remain green, same 5-second hard timeout, same assertions, no edits to the test file itself. |
| SF-065 (proposed) | A worker forcibly killed via SIGKILL (not SIGTERM) mid-execution — job reclaimed by lease expiry exactly as pre-Phase-14, drain contract makes no claim | §14's explicit out-of-scope confirmation — a regression guard, not new behavior, proving the drain redesign did not accidentally change the SIGKILL/lease-expiry path. |
| SF-066 (proposed) | Two real, separately-compiled binaries — OLD pinned to `794abbb57a7e2a965d556700468930a1ebed23e4`, NEW pinned to `c17f89c592c803a8f7d2fbd61cde566467ebe062` (§7, §19 OD-6, CLOSED) — run concurrently against one database while migrations `0011`→`0014` are applied incrementally via `internal/migrate.UpTo` (§8); `internal/invariant.Checker` re-run continuously throughout finds zero violations at every intermediate stage, not just before `0011` and after `0014` | The roadmap's flagship required test — old worker/new server AND new worker/old server, both directions, across the full expand/migrate/contract window, in the same harness run. |
| SF-067 (proposed) | Within SF-066's harness: the OLD binary's claim query (compiled before `0011` existed — no knowledge of `queue_name` at the Go type level at all, not merely at the query-predicate level) continues claiming jobs submitted with a default `queue_name` throughout the window, with no error and no stall, at every intermediate migration stage | Roadmap's explicit "old worker binary continues to function correctly against a post-Phase-13 schema" requirement — a real-binary strengthening of the existing SF-046 in-process test, using the actual pre-Phase-13 binary rather than an in-process analog of one. |
| SF-068 (proposed) | A `job_type` string is repurposed for an incompatible payload shape mid-deploy (a queued job submitted under the old shape, claimed by a worker running a handler that now expects the new shape) | Roadmap's own named gap: the failure mode (whatever the handler itself does — likely a handler-level unmarshal error surfaced as a `Permanent`/`Retryable` failure per ordinary `internal/handler` classification, not a TaskForge-level guard) is *documented and demonstrated*, not silently assumed safe or silently prevented. |
| SF-069 (proposed) | A real, separately-run `cmd/worker` binary's `TASKFORGE_WORKER_DRAIN_TIMEOUT` elapses while a job is still executing (handler deliberately made to run longer than the drain window): `dispositionDraining` fires, no `Complete*` call is made, the job row is left `RUNNING` under the draining worker's now-abandoned lease (directly verified by querying the row immediately after that process exits), and — once `lease_expires_at` passes — a second, freshly-started worker reclaims and completes it through the ordinary, unmodified TF-INV-004 path | §19 OD-3's core safety claim ("lease/retry/reclaim safety remains intact") and §6.4 "Required regression proof" item 4 — proves the drain-timeout path is indistinguishable, from the job's and the store's perspective, from an ordinary worker crash, and that no state transition TF-INV-005/TF-INV-013 would flag as invalid occurs. |
| SF-070 (proposed) | `cmd/api` under SIGTERM with a deliberately slow handler (a fake handler whose own PostgreSQL query is held open past `TASKFORGE_API_SHUTDOWN_TIMEOUT`): the handler's `r.Context()` is observed as cancelled promptly once the shutdown deadline passes, rather than the goroutine running unobserved to its own eventual completion | §19 OD-4's `BaseContext`-cancellation redesign — proves `cmd/api`'s shutdown-deadline behavior is now deterministic and testable, not an unexamined stdlib default. |
| — | Full Phase 1–13 regression suite, `internal/chaos` campaigns, unchanged | §11/§15's no-regression requirement. |

All new tests run against real PostgreSQL, and SF-063/SF-064/SF-065/SF-066/SF-067/SF-069/SF-070
additionally run against **real, separately-compiled OS-process binaries**
— a new test category for this project, not covered by any existing row
in [testing-strategy.md](testing-strategy.md)'s Test Categories table
(§17 adds one). **SF-064a and SF-064b are deliberately *not* OS-process
tests** — both exercise `internal/worker.Worker` directly, in-process,
exactly like every existing `internal/worker` test (`worker.New`, a
directly-cancelled `context.Context`, a real PostgreSQL-backed `*store.Store`)
— because their entire purpose is proving the *default, un-opted-in*
`Worker` contract is unchanged, which is a claim about the Go package's
own API, not about `cmd/worker`'s process-level behavior; SF-064b in
particular is not a new test at all but a mandatory unmodified re-run of
an existing one.

## 17. Files / packages expected to change

- **New**: `internal/migrate/down.go` (or similar) — `Down(ctx, db,
  version)`, mirroring `applyOne`'s shape; plus `UpTo(ctx, db,
  targetVersion)` (§8), needed by SF-066/067's incremental-migration
  harness.
- **New**: a migration-label parser/audit, likely `internal/migrate/label.go`
  plus a small `cmd`-level or `make`-level CI entry point (`make
  migration-audit` or similar, following the existing `make
  vulncheck`/`make sbom` precedent from Phase 10).
- **New**: `.github/workflows/migration-reversibility.yml` (or a new job
  inside `ci.yml`) running the down/up-again cycle.
- **New**: a mixed-binary-version test package — likely
  `internal/compattest/` or `test/compat/` (outside `internal/`, since it
  needs to `go build` two separate binaries and orchestrate OS processes,
  a different shape of test than every existing `internal/*_test.go`
  file) — exact package location and whether it lives under `internal/`
  or a new top-level `test/` directory is itself an open question (§19
  OD-9, since this project has no existing precedent for a
  process-orchestrating test and `internal/` visibility rules are
  irrelevant to a test that shells out to `go build`/`exec.Command`
  rather than importing anything).
- **`internal/worker/worker.go`**: the opt-in drain redesign (§6.4, §19
  OD-3) — a new, additive `SetDrainTimeout(d time.Duration)` method on
  `*Worker` (zero value, and never calling it, both mean "unchanged,
  today's immediate-cancellation behavior"), the new
  `dispositionDraining` `attemptDisposition` value and its
  skip-completion handling in `RunOnce` (reachable only for a `*Worker`
  that has been given a positive drain timeout), the drain-timeout
  watcher goroutine (started only when opted in), and a
  `sync.WaitGroup`-based exit-ordering guarantee (applies uniformly,
  not itself opt-in). **No existing method's behavior changes** — this
  is a pure addition to `*Worker`'s public surface.
- **`cmd/worker/main.go`**: new `TASKFORGE_WORKER_DRAIN_TIMEOUT`
  (default `30s`) config plumbing, and the one call site —
  `w.SetDrainTimeout(cfg.WorkerDrainTimeout)`, immediately after
  constructing `w` exactly as today — that is the **only** place in the
  repository this opt-in is exercised for the real worker process. New
  `worker_drain_started`/`worker_drain_completed` log events, new
  `taskforge_worker_drain_duration_seconds`/`taskforge_worker_drain_timed_out_total`
  metrics.
- **`internal/worker/worker_test.go`** (or a new
  `internal/worker/drain_test.go`): the four regression-proof tests
  §6.4 now requires — an unmodified re-run of
  `TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`
  (`internal/worker/concurrency_stress_test.go`, itself **not** edited),
  plus a new, dedicated test asserting prompt default cancellation for
  an un-opted-in `*Worker`.
- **`cmd/api/main.go`**: `TASKFORGE_API_SHUTDOWN_TIMEOUT` (default
  `10s`) config, an explicit cancellable `http.Server.BaseContext` plus
  the post-`Shutdown` explicit-cancel call (§6.4, §19 OD-4),
  `api_shutdown_started`/`api_shutdown_completed` log events.
- **`internal/config/config.go`**: the two new `FromEnv` fields above.
- **`internal/worker/worker_test.go`**: SF-062's workflow-node
  unregistered-handler test.
- **All 14 existing `migrations/*.down.sql` files**: retrofit the new
  marker line (§8).
- **`docs/compatibility-policy.md`**: flip every section this phase
  proves from `PROPOSED` to `implemented`, citing the specific new tests
  — mirroring exactly how Phase 11's sections in this same document were
  updated in place, not rewritten.
- **`docs/data-model.md`**: close the §6.5 documentation-lag gap with a
  "Phase 13 migration lock profile" section, if SF-066's harness produces
  the measurement and it was not already captured elsewhere.
- **`docs/testing-strategy.md`**: a new Test Categories row for
  "mixed-binary-version tests" (real, separately-compiled OS processes),
  and the Phase 14 section documenting what's now proven, following the
  exact per-phase-append convention every prior phase used in this file.
- **`docs/scenario-corpus.md`**: SF-060–SF-068, plus (recommended, §16)
  folding ADR-0009's SF-051–059 back in.
- **`docs/invariants.md`**: no new `TF-INV-*` entry (§11), but the
  Cross-Phase Governance Additions section's own convention suggests a
  short note confirming Phase 14 was reviewed and produced no new ID —
  the same honesty discipline that section already models for phases
  that *do* add one.

## 18. Deployment / operational considerations

- **New operator-facing config**: `TASKFORGE_WORKER_DRAIN_TIMEOUT`,
  `TASKFORGE_API_SHUTDOWN_TIMEOUT` (§9) — both optional, both with
  sensible defaults, so an operator who does nothing gets a safe default
  drain window, not a behavior change requiring action.
- **Orchestrator alignment obligation** (documented, not enforced by
  TaskForge — the same category as Phase 12/13's TLS/role/scheduling
  obligations): whatever platform runs `cmd/worker` (Kubernetes,
  systemd, etc.) must give the process at least
  `TASKFORGE_WORKER_DRAIN_TIMEOUT` (plus a safety margin) between
  SIGTERM and SIGKILL, or the drain redesign's grace period is moot —
  e.g. Kubernetes' `terminationGracePeriodSeconds` must exceed the
  configured drain timeout. This plan states the obligation; it cannot
  verify a deployment's orchestrator configuration from inside the
  process.
- **Migration-ordering rule becomes a required, documented deployment
  sequence** (not new tooling): migrate schema first (confirmed
  complete/applied), then deploy new worker/server binaries — already
  informally true (`migrate.Up` runs automatically at process startup in
  both `cmd/api` and `cmd/worker` today), but this phase's job is to
  state it as the *required* sequence in
  [compatibility-policy.md](compatibility-policy.md), given the
  mixed-binary-version proof (§16 SF-066) now demonstrates why deviating
  from it is unsafe rather than merely assuming so.
- **The CI migration-reversibility job (§8) adds CI runtime** — every
  data-safe-reversible migration now runs an extra up/down/up cycle on
  every PR touching `migrations/`; this is bounded by the number of
  labeled migrations (14 today) and is not expected to meaningfully slow
  CI, but is noted since it is a new, recurring CI cost this phase
  introduces.

## 19. Risks, decisions, and their resolutions (OD-N)

Six of the nine open decisions this plan originally raised are now
**CLOSED**: five were implementation-blocking — either the roadmap's own
required deliverables could not be built without them (OD-6, OD-3), or
they followed directly and necessarily from resolving those (OD-4, OD-7),
or they governed whether a foundational ADR needed to exist before any of
the rest could be considered adopted policy (OD-1) — decided against real
repository evidence (exact `git log` output, the existing
`internal/config` conventions, and direct reading of `cmd/worker`/`cmd/api`'s
current shutdown code). The sixth, OD-8, was never blocking but is closed
by direct verification rather than left as a to-do (§6.5's flagged
"possibly missing" measurement was checked and found to already exist).
**OD-3's original closure was itself corrected during an independent
review pass** — see its entry below and §6.4's "Post-review correction"
— after that review found the original design would have broken an
existing, currently-passing test; the semantic contract (30s default,
`dispositionDraining`, TF-INV-004 reclaim) is unchanged, only the
mechanism's attachment point (opt-in at the `Worker` level, not an
unconditional change to `Worker.Run`'s contract) was corrected. The
remaining three (OD-2, OD-5, OD-9) are confirmed, on review, to be
genuinely non-blocking — none of §1's six roadmap deliverables requires
them resolved before implementation starts — and remain open by design,
not by omission.

### OD-1 — CLOSED: ADR-0010 is required, and has been written

**Decision**: yes, this decision needs an ADR, and
[ADR-0010](adr/0010-expand-migrate-contract.md) now exists, Accepted.
Reasoning: [docs/adr/README.md](adr/README.md)'s own stated criterion
("a decision that had a genuine alternative... if there was no genuine
alternative, it belongs in architecture.md as a description, not here as
a decision") is squarely met — the real, rejected alternative is a
universal "every migration needs a tested down migration" rule, and
migration `0010`'s own already-shipped, honest "can legitimately fail"
down migration is concrete, in-repository proof that the universal rule
would have been a false promise if adopted. This is exactly the shape of
decision every other structurally-consequential choice in this project
(ADR-0001 through ADR-0009) already gets an ADR for, and it governs every
migration this project will ever write from this point forward, not only
Phase 14's own scope — the highest-leverage, most durable decision this
phase makes. See [ADR-0010](adr/0010-expand-migrate-contract.md) for the
full Context/Decision/Alternatives/Consequences/Failure-Implications
record; §8 above is its direct implementation.

### OD-3 — CLOSED: worker graceful-drain timeout, default 30s, opt-in contract, full design specified

**Decision**: `TASKFORGE_WORKER_DRAIN_TIMEOUT` defaults to **30 seconds**
— for `cmd/worker` specifically, wired in by `cmd/worker/main.go` alone.
The full semantic contract (poll-gating context cancelled immediately on
SIGTERM; an independent drain context for in-flight work, decoupled from
the poll-gating context, bounded by the job's own
`execution_timeout_seconds` as always plus a drain-timeout side goroutine;
a new `dispositionDraining` value that makes drain-timeout expiry skip
every `Complete*` call and leave the job's lease to expire and be
reclaimed through the existing, unmodified TF-INV-004 path) is specified
in full in §6.4 and diagrammed in §7.

**Post-review correction (superseding this OD's original closure)**:
independent review found the original closure specified the drain-timeout
mechanism as an unconditional change to `Worker.Run(ctx)`'s existing
cancellation contract, which would have broken a currently-passing
regression test
(`TestStress_WorkerPoolGracefulShutdown_NoGoroutineLeak_NoOrphanedAuthority`,
`internal/worker/concurrency_stress_test.go`) — that test cancels the
context passed to `Run` and requires every worker to return within a hard
5-second window, using a handler that only stops via context
cancellation. **The decision is corrected, not reopened**: the drain
grace period is **explicitly opt-in**, via a new, additive
`Worker.SetDrainTimeout(d time.Duration)` method whose zero value (and
never calling it — the state every existing caller is already in) means
"cancel immediately," byte-for-byte preserving today's behavior for every
caller that does not explicitly opt in. `cmd/worker/main.go` is the sole
caller in the repository that opts in, via `w.SetDrainTimeout(cfg.WorkerDrainTimeout)`.
The `30s` default, the `dispositionDraining` mechanism, and the
TF-INV-004 reclaim argument are all unchanged by this correction — only
*how* the mechanism attaches to `Worker`'s public surface changed. See
§6.4 for the full corrected design and its now-mandatory four-part
regression-proof requirement.

`TASKFORGE_API_SHUTDOWN_TIMEOUT` is closed alongside this at **10s**,
matching `cmd/api`'s existing hardcoded value exactly (§9) — no
behavior-duration change on the API side, only configurability plus the
OD-4 cancellation change below. **Proof obligations**: SF-064 (ordinary,
within-budget `cmd/worker` drain), SF-064a (default, un-opted-in `Worker`
cancellation remains prompt), SF-064b (the existing Phase 5 stress test
re-run unmodified), SF-069 (drain-timeout expiry does not corrupt
job/lease state), SF-063 (ordinary API shutdown) — all in §16.

### OD-4 — CLOSED: `cmd/api` adopts explicit deadline-linked cancellation, symmetric with the worker redesign

**Decision**: changed, not merely documented. Per the task's own
instruction to resolve OD-4 "if its answer follows directly from the
[OD-3] contracts" — it does: OD-3's contract requires that "when the
[drain] deadline expires, execution is cancelled," stated as a
requirement for deterministic, testable shutdown behavior. Leaving
`cmd/api`'s side of the same problem as "silently abandon the goroutine,
because that happens to be what the Go standard library does by default"
would be an asymmetric, undecided answer to the identical question Phase
14 is already deciding for the other process. `cmd/api` therefore adopts
an explicit, cancellable `http.Server.BaseContext`, cancelled by this
process immediately after `srv.Shutdown` returns (§6.4) — so a handler
still running once the shutdown deadline passes observes cancellation
promptly, exactly as an in-flight job observes its own drain-timeout
cancellation on the worker side, rather than running to whatever
completion it happens to reach on its own, unobserved. **Proof
obligation**: SF-070 (§16).

### OD-6 — CLOSED: exact commit hashes identified from repository history; harness design specified

**Decision**: reuse the real Phase 13 `queue_name`/governance migration
boundary, per the task's own directed evaluation, confirmed correct by
direct `git log` inspection (no repository evidence contradicts it):

| Binary | Commit | Identified via |
|---|---|---|
| **OLD** | **`794abbb57a7e2a965d556700468930a1ebed23e4`** — "Merge pull request #21 from SamudralaAjaykumarrr/phase-13-concurrency-analysis" | `git log --oneline --all`; confirmed to be the immediate parent of the Phase 13 implementation commit via `git rev-parse 16de01a^` and `git merge-base --is-ancestor`. Its `migrations/` tree (confirmed via `git ls-tree -r 794abbb -- migrations/`) contains exactly `0001`–`0010`, no `queue_name`, no governance tables — the last commit reachable from `main` with the pre-Phase-13 schema. |
| **NEW** | **`c17f89c592c803a8f7d2fbd61cde566467ebe062`** — "Merge pull request #22 from SamudralaAjaykumarrr/phase-13-workload-governance-retention" | `git rev-parse HEAD` / `git rev-parse main` / `git rev-parse phase-14-planning` — all three identical at the time of this decision pass. Contains `migrations/0001`–`0014` in full. |

Both migrations `0011`–`0014` and the entirety of Phase 13's
implementation code (`internal/governance/`, `internal/retention/`,
`cmd/taskforge-admin`, `cmd/taskforge-retention`, the queue-aware
`claimQuery`) were introduced in a single commit,
`16de01a` ("feat: implement Phase 13 workload governance and
retention"), whose sole parent is the pinned OLD commit above — confirmed
via `git log --oneline -- migrations/0011_add_queue_name.up.sql
migrations/0012_create_claimable_by_queue_index.up.sql
migrations/0013_create_governance_tables.up.sql
migrations/0014_drop_old_claimable_index.up.sql`, which returns only
`16de01a` for all four files (no later commit touched any of them). This
means the four migrations' own **expand → migrate → contract** staging
(`0011` add column, `0012` add new index, `0013` add governance tables,
`0014` drop the old index — exactly the sequence
[phase-13-plan.md](phase-13-plan.md) §11 designed) is available to be
replayed incrementally by the harness even though it landed in one
commit, via `internal/migrate.UpTo` (§8) stepping through versions
`0011`→`0014` one at a time while both pinned binaries run continuously
against the database throughout.

**Harness build/run mechanism (reproducible, no mutable branch
dependency)**:

1. `git worktree add --detach <tmp-old-dir> 794abbb57a7e2a965d556700468930a1ebed23e4`
   and `git worktree add --detach <tmp-new-dir> c17f89c592c803a8f7d2fbd61cde566467ebe062`
   — `--detach` guarantees a detached-HEAD checkout at the exact commit,
   immune to the source branch later moving; using the commit SHA
   directly (never `main`/`phase-14-planning`) means the harness's
   behavior cannot change under it even if those branches are later
   force-pushed or fast-forwarded.
2. `go build -o <bin> ./cmd/worker` (and `./cmd/api`, where the test
   needs it) run **from within each pinned worktree**, so each binary is
   built against that commit's own `go.mod`/`go.sum` and vendored
   migration set (`migrations/embed.go`'s `//go:embed`, per commit) —
   the OLD binary's embedded migration set genuinely contains only
   `0001`–`0010`, which is what makes SF-067's claim ("no knowledge of
   `queue_name` at the Go type level at all") literally true rather than
   simulated.
3. Both binaries run as real OS processes (`os/exec`, §6.2 — new for
   this repository), pointed at one shared, ephemeral
   `TASKFORGE_TEST_DATABASE_URL` Postgres instance (the same CI
   convention every other integration test already uses).
4. The harness drives ordinary traffic (HTTP submissions against
   whichever binary is running `cmd/api`, claims from both `cmd/worker`
   binaries) and re-runs `internal/invariant.Checker` on a timer
   throughout, mirroring `internal/chaos`'s existing combined-campaign
   pattern (§6.2).
5. Both worktrees are removed (`git worktree remove`) on test
   completion/failure — cleanup is unconditional (`defer`/`t.Cleanup`),
   not just on the success path, so a failed run does not leave a stray
   worktree behind.

**Proof obligations**: SF-066, SF-067 (§16).

### OD-7 — CLOSED: "single minor version of skew" is defined operationally, without inventing a release system

**Decision**: per the task's own instruction not to invent a
release/versioning system to satisfy this, and because the answer
follows directly from OD-6's resolution: this project's actual,
evidenced unit of deployable change is **one phase-boundary commit
range**, not a semver tag — confirmed by `git log --oneline --all`
(§2, reproduced in OD-6's table above), where every phase from Phase 1
through Phase 13 landed to `main` as exactly one feature commit (or one
squash-merged PR), never as an incremental trickle of smaller
independent releases. No git tag has ever been pushed
(`git tag -l` returns nothing), so "N.x vs. N.(x-1).y" has no concrete
referent to test against today, and this plan does not manufacture one.
**For Phase 14's own purposes**, "single-version-skew" is therefore
defined as: the schema/code delta between two adjacent phase-boundary
commits — precisely the OLD/NEW pairing OD-6 already pins
(`794abbb...` → `c17f89c...`, the entire Phase 13 delta in one step).
SF-066/067 (§16) are, under this operational definition, definitionally
a single-version-skew compatibility proof, which satisfies the roadmap's
non-goal boundary ("no support for skipping more than one minor version")
using evidence already in hand rather than leaving the term undefined.
This operational definition is scoped to Phase 14's own proof
obligations; it does not retroactively require Phase 10's eventual
tagged-release process to adopt "one phase per version," and a future
phase is free to define version skew in terms of actual semver tags once
Phase 10 produces a first one — that supersession is out of Phase 14's
scope and is not required for this phase's exit criteria (§21).

### Remaining open decisions (non-blocking — confirmed on review, not merely carried over)

- **OD-2** (open): does the migration-label marker (§8) belong in
  `.down.sql` only, or should `.up.sql` also carry a marker? This plan
  recommends `.down.sql`-only (the file whose behavior the label
  actually governs), but does not close the ergonomic question of also
  prompting at `.up.sql`-authoring time. Non-blocking: §8's CI mechanism
  is fully specified and enforceable either way.
- **OD-5** (open): does "formal adoption of the deprecation policy"
  (§4 item 8) also mean *setting* the actual Sunset date/version for the
  legacy unprefixed API routes, or only adopting the *policy* for future
  deprecations in general? This plan reads the roadmap's text as the
  latter and proceeds on that reading (§5), but does not foreclose the
  alternative. Non-blocking: neither reading changes any other section
  of this plan or any test in §16.
- **OD-9** (open): the mixed-binary-version test package's exact location
  (`internal/compattest/` vs. a new top-level `test/` directory). A
  structural, low-stakes choice §17 already flags; does not affect the
  harness's design (§8, OD-6) or any proof obligation in §16.

### OD-8 — CLOSED: the Phase 13 lock-timing measurement already exists

**Decision**: closed by direct verification, not by design choice.
`internal/migrate/phase13_migration_test.go` **already contains** the
lock-timing measurements §6.5 flagged as possibly missing:
`TestPhase13Migration0011_AddColumnIsFastRegardlessOfTableSize`,
`TestPhase13Migration0012_BlocksWritesAndReadsAreUnaffected`,
`TestPhase13Migration0013_BlocksWritesViaForeignKeyLock_ReadsUnaffected`,
`TestPhase13Migration0014_BlocksOnAccessExclusive_ButIsCatalogOnly`, and
`TestPhase13Migrations0011To0014_AreDataSafeReversible` (the last of
which also confirms, independent of OD-6/§8, that a generic
`applyDown`-style pattern for running any named `.down.sql` file already
exists in this codebase as test infrastructure, at
`internal/migrate/phase12_migration_test.go`'s `applyDown` helper,
reused by the Phase 13 test file — useful, directly reusable precedent
for §8's `internal/migrate.Down`, not something to build from zero).
**§6.5's concern is resolved: the measurement was done.** The only
remaining work is the documentation write-up — a "Phase 13 migration
lock profile" section in [data-model.md](data-model.md), mirroring the
existing "Phase 12 migration lock profile" section's table/prose format,
summarizing results these already-passing tests establish. This was
already itemized as implementation scope in §17 and needs no further
design decision.

## 20. Staged implementation order

No item below is blocked on an open decision — §19 closes every decision
that gated ordering. This order follows the same "cheapest,
least-coupled, least-novel first" logic
[enterprise-roadmap.md](enterprise-roadmap.md)'s own phase sequencing
uses, deferring the largest, most novel piece of infrastructure (item 6)
to last so it lands against a codebase already hardened by everything
before it:

1. **ADR-0010** ([already written](adr/0010-expand-migrate-contract.md),
   §19 OD-1) — done as part of this planning pass, not deferred; every
   later item that touches migration policy cites it directly rather
   than a still-open recommendation.
2. **Migration-label retrofit (all 14 `.down.sql` files) +
   `internal/migrate.Down`/`UpTo` + CI job** (§8) — zero dependency on
   anything else in this phase, and item 6's harness (below) directly
   depends on `UpTo` existing.
3. **Unregistered-`job_type` documentation + workflow-node test** (§6.3,
   SF-062) — smallest remaining item; mostly documentation, one new test,
   no dependency on anything else.
4. **`internal/worker` opt-in drain support + `cmd/worker` wiring**:
   the additive `Worker.SetDrainTimeout` method, poll-gating/drain-context
   split (only active when opted in), `dispositionDraining`,
   `cmd/worker`'s sole `SetDrainTimeout(cfg.WorkerDrainTimeout)` call, and
   `TASKFORGE_WORKER_DRAIN_TIMEOUT` (default 30s) (§6.4, §9, §19 OD-3) —
   proven by SF-064, SF-064a, SF-064b (the unmodified existing Phase 5
   stress test, re-run as a mandatory regression check), SF-065, SF-069 —
   must land and be green, with SF-064b confirmed unbroken, before item 7,
   since that harness depends on worker shutdown behaving correctly under
   mixed-version load.
5. **`cmd/api` `BaseContext` cancellation redesign +
   `TASKFORGE_API_SHUTDOWN_TIMEOUT`** (default 10s) (§6.4, §9, §19 OD-4)
   — proven by SF-063, SF-070; independent of item 4, can run in
   parallel with it.
6. **`compatibility-policy.md` formal adoption** of expand/migrate/
   contract (citing ADR-0010), the migration-ordering rule, and the
   deprecation policy (§4 items 1, 5, 8) — sequenced after items 1–2 so
   it can cite the now-real ADR and CI mechanism, not describe an
   aspiration.
7. **The mixed-binary-version harness** (§7, §8, §19 OD-6: pinned to
   `794abbb57a7e2a965d556700468930a1ebed23e4` /
   `c17f89c592c803a8f7d2fbd61cde566467ebe062`; SF-066, SF-067, SF-068) —
   depends on item 2 (`UpTo`) and benefits from items 4–5 already being
   correct (a mixed-version worker fleet that drains incorrectly would
   otherwise produce confusing, hard-to-attribute harness failures).
   Sequenced last deliberately, matching Phase 13's own experience
   (cited throughout [phase-13-plan.md](phase-13-plan.md)) that the
   largest, most novel piece of a phase is where independent review is
   most likely to surface additional gaps — building it last means those
   findings land against an otherwise-complete phase, not one still
   mid-change.
8. **Final `compatibility-policy.md`/`testing-strategy.md`/
   `scenario-corpus.md` status-flip pass** (§21's last exit criterion) —
   depends on every prior item having actually landed; this is the
   summary update, not independent work.

## 21. Exit criteria

Reproduced from [enterprise-roadmap.md](enterprise-roadmap.md) Phase 14
"Enterprise exit criteria." **Post-implementation update**: every
criterion below is now implemented and proven; this section originally
tracked pre-implementation status (see the struck-through per-item notes
below, kept for the historical record of what this plan estimated before
implementation started) and is superseded by
[enterprise-roadmap.md](enterprise-roadmap.md)'s own exit-criteria section,
now checked off with exact evidence.

- [x] A two-binary-version (old worker/new server, and new worker/old
      server) integration test exists and passes across a full
      expand/migrate/contract window. Implemented:
      `test/compat/two_binary_test.go`,
      `TestCompat_SF066_SF067_TwoBinaryVersionAcrossExpandMigrateContractWindow`
      — real `cmd/worker` binaries pinned to `794abbb...`/`c17f89c...`
      (§19 OD-6), built via detached `git worktree`s, run concurrently
      while migrations `0011`→`0014` apply one at a time via
      `internal/migrate.UpTo`, with `internal/invariant.Checker` clean at
      every intermediate stage. ~~Not started~~ (was: genuinely new
      infrastructure, §6.2, §8, OD-6, OD-9).
- [x] CI runs the down path for every data-safe-reversible migration;
      every forward-fix-only migration is explicitly labeled and CI does
      not require a down path for it. Implemented: the
      `taskforge:down-migration-status` marker (§8) on all fourteen
      `.down.sql` files, `internal/migrate.Down`/`UpTo` (new, additive),
      and `internal/migrate/reversibility_test.go`'s SF-060/061 —
      enforced by the existing `go test -p 1 ./...` CI step (no separate
      workflow needed) and, for a fast standalone check, `make
      migration-audit`. ~~Partially present~~ (was: files existed and
      were prose-labeled; the marker, `Down`, and CI enforcement did not).
- [x] The unregistered-`job_type` failure mode is documented and tested.
      Implemented: `TestRunOnce_SF062_WorkflowNodeWithNoRegisteredHandler`
      (`internal/worker/worker_test.go`) extends the pre-existing plain-job
      proof to a workflow node, and
      [compatibility-policy.md](compatibility-policy.md)'s status flipped
      accordingly. ~~Substantially done for plain jobs~~ (was: workflow-node
      case, SF-062, and the doc status flip remained).
- [x] A documented, tested graceful-drain (SIGTERM) contract exists for
      `cmd/worker` and `cmd/api`. Implemented: `Worker.SetDrainTimeout`
      (opt-in, additive; `cmd/worker`'s sole use,
      `TASKFORGE_WORKER_DRAIN_TIMEOUT` default 30s) and `cmd/api`'s
      `BaseContext`/`srv.Close` redesign
      (`TASKFORGE_API_SHUTDOWN_TIMEOUT` default 10s), proven at the real
      OS-process level by `test/procs` (SF-063/064/065/069/070) and
      in-process by `internal/worker/drain_test.go` (SF-064a) plus an
      unmodified re-run of Phase 5's `TestStress_WorkerPoolGracefulShutdown_*`
      (SF-064b). ~~`cmd/worker` has a genuine defect~~ (was: the pre-Phase-14
      immediate-cancellation-on-SIGTERM behavior contradicted the
      documented drain intent; now corrected, opt-in, and regression-proven).
- [x] [compatibility-policy.md](compatibility-policy.md)'s PROPOSED
      markers are updated to reflect what is now actually proven vs.
      still proposed. Implemented: every section this phase proves is
      flipped to "Status: implemented", citing the specific tests above;
      only the job-payload `schema_version` guidance and the legacy
      routes' actual removal date remain PROPOSED, by design (§5, §19
      OD-5).

## 22. Cross-references

- Invariants: [invariants.md](invariants.md)
- Scenarios: [scenario-corpus.md](scenario-corpus.md)
- Compatibility policy (this phase's primary subject matter):
  [compatibility-policy.md](compatibility-policy.md)
- Data model / migration lock precedent: [data-model.md](data-model.md)
- Testing strategy: [testing-strategy.md](testing-strategy.md)
- Prior phase plans this plan mirrors in structure:
  [phase-12-plan.md](phase-12-plan.md), [phase-13-plan.md](phase-13-plan.md)
- ADR: [ADR-0010](adr/0010-expand-migrate-contract.md) — expand/migrate/
  contract as the primary schema-compatibility model (§19 OD-1)
- Roadmap: [enterprise-roadmap.md](enterprise-roadmap.md) "Phase 14 —
  Upgrade & Compatibility Proof"
