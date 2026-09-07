# Workflows / DAG Execution

Status: **implemented (Phase 7)**, per [roadmap.md](roadmap.md). This
document originally recorded the target design ahead of implementation;
it now also records the exact mechanism Phase 7 built, including the two
points the original draft explicitly left open ("exact mechanism to be
finalized during Phase 7 implementation"). See README.md's "Phase 7: What's
Implemented" section for the full implementation summary, test list, and
current maturity label.

## Why Workflows Are Not Part of v1 Core

Workflow/DAG execution is built *on top of* the single-job engine's
primitives (leasing, fencing, retries, idempotency), not alongside or
underneath them. Attempting to design DAG semantics before the underlying
job engine's reliability properties are proven and tested would mean
building on an unvalidated foundation — every workflow-level guarantee
("node B doesn't run before node A succeeds") ultimately reduces to
job-level guarantees ("node B's job doesn't become eligible before node A's
job reaches `SUCCEEDED`"). See [roadmap.md](roadmap.md) Phase 7 entry
criteria.

## Model

A workflow is a directed acyclic graph of nodes, each node wrapping exactly
one `jobs` row:

```
A -> B
A -> C
B, C -> D
```

- `workflow_instances`: one row per workflow execution. Carries its own
  workflow-level `state` (`RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED`) —
  see [data-model.md](data-model.md) deferred tables. This is a **separate
  state space from the job-level state enum** in
  [execution-semantics.md](execution-semantics.md): a workflow's `FAILED`
  means "at least one required node dead-lettered or was cancelled,
  causing the workflow to not complete successfully," a workflow-level
  summary concept with no `RETRY_WAIT`-equivalent, not a reintroduction of
  the job-level `FAILED` state rejected in
  [ADR-0005](adr/0005-durable-job-state-machine.md). Individual nodes
  within the workflow still use the ordinary job state machine.
- `workflow_nodes`: one row per node, referencing a `jobs.id` and a list of
  predecessor node IDs (`depends_on`).

## Dependency Satisfaction Semantics

**Default condition: predecessor must reach `SUCCEEDED`.** A node with
multiple predecessors (fan-in, e.g. node D depends on B and C) becomes
eligible only when **all** required predecessors satisfy this condition —
AND semantics by default. OR/any-of fan-in semantics are deferred past the
initial workflow implementation (tracked as an open question below).

Mechanically (as implemented — this resolves the open question below):
a node's underlying job is inserted with `eligible_at` set to a fixed,
concrete far-future sentinel timestamp (`9999-12-31T23:59:59Z`; see
`internal/store/workflow.go`'s `blockedEligibleAt` and migration
`0003`'s comment) if it has one or more dependencies, or immediately
eligible (`eligible_at = COALESCE(scheduled_at, now())`, exactly like an
ordinary job) if it is a root node with none. PostgreSQL's special
`infinity` timestamptz value was considered and rejected — it is not
reliably representable as a Go `time.Time` through the pgx driver this
codebase uses, so a concrete sentinel was chosen instead; the effect for
correctness purposes is identical (the row is simply never selected by
the claim query's `eligible_at <= now()` predicate).

Propagation is application-level, evaluated inside the SAME transaction
as the predecessor's own completion/cancellation/dead-letter transition
(`internal/store/workflow.go`'s `propagateWorkflowTransition`, called from
`CompleteSuccess`, `CompleteFailure`, the retry-exhaustion branch of
`completeRetryableOutcome`, `CompleteCancelled`,
`CancelQueuedOrRetryWait`, and the Lazy Dead-Letter Sweep) — not a
separate polling sweep, resolving this document's other open question in
favor of transactional correctness over the "everything is a query
predicate, no service" simplicity a polling approach would have offered.
A predecessor reaching `SUCCEEDED` re-evaluates each direct dependent's
readiness and, if every one of its dependencies is now satisfied, advances
its `eligible_at` off the sentinel to `COALESCE(scheduled_at, now())`,
making it a normal claimable job through the existing, completely
unmodified claim query. **No new claiming mechanism is introduced for
workflow nodes** — they become ordinary `QUEUED` jobs once eligible,
reusing every guarantee already built for standalone jobs (leasing,
fencing, retries, DLQ, cancellation, scheduling).

Concurrent fan-in (two predecessors of the same dependent completing at
the same instant) is resolved by row-locking the dependent's job
(`SELECT ... FOR UPDATE`) before re-evaluating its predecessors: whichever
of the two completing transactions acquires that lock second is
guaranteed to see the other's already-committed terminal state, so
exactly one of the two performs the activation. See
`internal/store/workflow.go`'s `resolveDependent` doc comment for the
precise argument, and `internal/store/workflow_test.go`'s
`TestConcurrentFanIn_BothPredecessorsCompleteSimultaneously` for the test
that exercises it with two real, concurrently-committing goroutines.

This directly implements TF-INV-012.

## Failure Propagation

| Predecessor outcome | Effect on dependents |
|---|---|
| `SUCCEEDED` | Dependency satisfied; dependent may become eligible (if all its other dependencies are also satisfied). |
| `DEAD_LETTERED` | Default: all dependents are transitioned to `CANCELLED` (skipped) — a failed prerequisite means downstream work cannot meaningfully proceed unless explicitly configured otherwise. |
| `CANCELLED` | Same as `DEAD_LETTERED` — dependents are cancelled by default. |
| Still `RETRY_WAIT`/`RUNNING` (retrying) | Dependents remain non-eligible; no propagation occurs until the predecessor reaches a terminal state. A retrying predecessor is not treated as failed. |

An explicit "run regardless of predecessor outcome" or "run only if
predecessor failed" (compensation/rollback edge) configuration is a
**deferred feature**, not part of the initial workflow implementation — the
default AND/`SUCCEEDED`-only semantics above are the entire v1-of-workflows
contract.

## Workflow-Level Cancellation

Cancelling a `workflow_instance` cancels every non-terminal node's
underlying job via the same `POST /jobs/{id}/cancel` semantics
([worker-protocol.md](worker-protocol.md)) — a workflow cancellation is
implemented as "cancel all of my currently-non-terminal jobs," not as a new
primitive. The same TF-INV-010 race rule applies independently to each
node. `POST /workflows/{id}/cancel` (`internal/store.CancelWorkflow`) is
the concrete implementation: it durably records
`workflow_instances.cancel_requested = true` (idempotent, no-op if the
workflow is already terminal), then calls `CancelQueuedOrRetryWait` for
every node currently `QUEUED`/`RETRY_WAIT` (this includes a
dependency-blocked node — it is `QUEUED` by construction, gated only by
`eligible_at`) and `RequestCancellation` for every node currently
`RUNNING`. A `RUNNING` node's cancellation is requested, not yet
confirmed, exactly like a standalone job — the caller polls
`GET /workflows/{id}` to observe the eventual outcome.

`workflow_instances.cancel_requested` exists specifically to distinguish
an explicit workflow-level cancellation from a workflow that reaches
`FAILED` organically via failure propagation (a dead-lettered node
cascade-cancelling its dependents also drives every affected node through
the same job-level `CANCELLED` state) — the workflow's own terminal state
is `CANCELLED` only when `cancel_requested` was set, `FAILED` otherwise,
once every node has reached a terminal job state
(`internal/store.finalizeWorkflowIfComplete`).

## Fan-Out / Fan-In

Fan-out (one node with multiple dependents, e.g. A → B and A → C) requires
no special handling beyond the dependency-satisfaction check above — B and
C simply both list A as a (their only) predecessor and both become eligible
independently once A succeeds. Fan-in (D depends on B and C) is the AND
case already described. Diamond dependencies (A → B, A → C, B+C → D) are
the composition of both and require no additional primitive.

## Why an Array Column Instead of an Edge Table

`workflow_nodes.depends_on` is proposed as a `uuid[]` column rather than a
separate `workflow_edges` join table, because v1-of-workflows targets DAGs
of modest size (tens to low hundreds of nodes per workflow instance, not
graphs requiring relational edge queries like "find all descendants of
node X efficiently at scale"). This is recorded as an explicit,
revisitable simplification — if workflow use cases grow to need efficient
graph traversal queries in SQL, an edge table is the natural evolution.

## API Contract

Per [worker-protocol.md](worker-protocol.md)'s "Deferred Endpoints"
section naming this Phase 7's scope:

- `POST /workflows` — submit a full `GraphSpec` (every node and its
  `depends_on` list, keyed by a caller-chosen `node_key` string rather
  than a server-generated ID, since dependencies must reference sibling
  nodes before any database identifiers exist). Per-node fields
  (`job_type`, `payload`, `max_attempts`, `execution_timeout_seconds`,
  `scheduled_at`) mirror `POST /jobs`'s own fields and defaults exactly.
  A 201 response is returned if and only if the entire workflow (instance
  + every node's job + every node's dependency edges) has committed
  atomically; an invalid graph (see "DAG Validation" below) is rejected
  with 400 before any database write. There is no `Idempotency-Key`
  equivalent for workflow submission — see "Open Questions."
- `GET /workflows/{id}` — the current durable workflow-level state plus
  every node's current job state, freshly joined on every call (never
  cached). 404 if the workflow does not exist.
- `POST /workflows/{id}/cancel` — see "Workflow-Level Cancellation" above.
  200 with the workflow's current representation whether or not every
  node's cancellation has been confirmed yet; 404 if the workflow does not
  exist.

Every ordinary job endpoint (`POST /jobs`, `GET /jobs/{id}`,
`POST /jobs/{id}/cancel`) is unchanged and fully usable against a workflow
node's underlying job directly — a workflow node is an ordinary job row,
not a distinct kind of resource with restricted visibility.

## DAG Validation

`internal/workflow.ValidateGraph` rejects, entirely in memory and before
any SQL is issued (so an invalid submission is never partially persisted
and never acknowledged): an empty node list; a missing or duplicate
`node_key`; a missing `job_type`; a `depends_on` entry referencing an
unknown `node_key`, the node's own key (self-dependency), or the same
key twice; and any directed cycle (detected via a deterministic
three-color depth-first search in submission order, so the same invalid
graph always reports the same cycle). `internal/store.CreateWorkflow`
calls this before opening its transaction — see "Atomic Workflow
Creation" below.

## Atomic Workflow Creation

`internal/store.CreateWorkflow` creates the `workflow_instances` row,
every node's underlying `jobs` row, and every `workflow_nodes` row inside
one PostgreSQL transaction. A failure partway through (a constraint
violation, a cancelled context, a dropped connection) rolls back the
entire transaction, leaving no partial workflow, no orphaned job rows,
and no dangling dependency edges — see
`internal/store/workflow_test.go`'s
`TestCreateWorkflow_RollbackLeavesNoPartialState` for the fault-injection
test proving this directly.

## Cross-References

- Invariant: TF-INV-012 in [invariants.md](invariants.md)
- Schema: [data-model.md](data-model.md)'s `workflow_instances` /
  `workflow_nodes` tables
- Roadmap staging: [roadmap.md](roadmap.md) Phase 7
- Scenario corpus: [scenario-corpus.md](scenario-corpus.md)'s Phase 7
  section (SF-019 through SF-030)

## Open Questions

Resolved by the Phase 7 implementation (see "Dependency Satisfaction
Semantics" and "Why an Array Column Instead of an Edge Table" above):

- ~~Exact mechanism for "re-evaluate dependents when a predecessor
  completes"~~ — resolved in favor of an in-transaction propagation step
  (`internal/store/workflow.go`'s `propagateWorkflowTransition`), not a
  polling sweep, so a predecessor's completion and its dependents'
  activation/cascade are part of one atomic commit.
- ~~`eligible_at` sentinel vs. a dedicated gating column~~ — resolved in
  favor of a concrete far-future `eligible_at` sentinel value
  (`blockedEligibleAt`), not PostgreSQL's `infinity` timestamptz (not
  reliably representable as a Go `time.Time` via this codebase's pgx
  driver) and not a new column (which would have required a claim-query
  change this design specifically avoids).

Still deferred, not designed — explicit Phase 8+ non-goals per
[roadmap.md](roadmap.md)'s Phase 7 entry:

- OR/any-of fan-in semantics: deferred, not designed.
- Compensation/rollback edges (run B if A fails): deferred, not designed.
- Dynamic workflow modification (adding nodes to a running workflow):
  deferred, not designed. v1-of-workflows assumes the full DAG is known at
  submission time, and `internal/store.CreateWorkflow` provides no
  mechanism to add nodes to an already-created workflow_instance.
- Workflow submission idempotency (an `Idempotency-Key`-equivalent for
  `POST /workflows`): not designed or implemented — this document never
  established a workflow-level idempotency contract, and Phase 7
  deliberately does not invent one; a caller that needs safe retry of
  workflow submission must handle deduplication at the caller's own
  layer for now.
