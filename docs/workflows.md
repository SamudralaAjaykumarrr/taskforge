# Workflows / DAG Execution

Status: **staged for Phase 7** (see [roadmap.md](roadmap.md)). This document
records the target design now so that v1's schema and job primitives do not
foreclose it, but no workflow code or table exists yet. Treat this as a
design contract for a future phase, not a current capability.

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

Mechanically: a node's underlying job is inserted in a non-eligible holding
state (`eligible_at` set far in the future, or a dedicated gating column —
exact mechanism to be finalized during Phase 7 implementation) at workflow
submission time. When a predecessor's job reaches a terminal state, a
trigger (application-level, evaluated in the same transaction as the
predecessor's completion, or a polling check — to be decided in Phase 7)
re-evaluates each dependent node's readiness and, if all its dependencies
are now satisfied, sets its `eligible_at = now()`, making it a normal
claimable job through the existing claim query. **No new claiming mechanism
is introduced for workflow nodes** — they become ordinary `QUEUED` jobs
once eligible, reusing every guarantee already built for standalone jobs.

This directly implements TF-INV-012 (documented now, enforced starting
Phase 7).

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
node.

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

## Cross-References

- Invariant: TF-INV-012 in [invariants.md](invariants.md)
- Deferred schema: [data-model.md](data-model.md)
- Roadmap staging: [roadmap.md](roadmap.md) Phase 7

## Open Questions

- OR/any-of fan-in semantics: deferred, not designed.
- Compensation/rollback edges (run B if A fails): deferred, not designed.
- Dynamic workflow modification (adding nodes to a running workflow):
  deferred, not designed. v1-of-workflows assumes the full DAG is known at
  submission time.
- Exact mechanism for "re-evaluate dependents when a predecessor
  completes" (trigger inside the completion transaction vs. a separate
  polling sweep) is unresolved and will be decided during Phase 7
  implementation, weighing transactional complexity against the
  "everything is a query predicate, no service" philosophy established in
  [scheduling.md](scheduling.md).
