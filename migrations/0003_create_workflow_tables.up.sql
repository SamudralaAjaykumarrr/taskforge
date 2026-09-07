-- Phase 7: workflow_instances and workflow_nodes, per docs/workflows.md
-- and docs/data-model.md (previously listed under "Tables Deferred to
-- Later Phases (Phase 7)" -- now promoted; see that document's updated
-- Phase 7 entry).
--
-- Workflow nodes reuse the existing jobs table and its full state
-- machine/lease/retry/cancellation/scheduling machinery completely
-- unchanged (TF-INV-012): a workflow_nodes row is a thin structural
-- layer -- which job belongs to which workflow, and its predecessor node
-- IDs -- on top of an ordinary jobs row. No new claiming mechanism, lease
-- mechanism, or execution engine is introduced; see
-- internal/store/workflow.go.
--
-- Dependency-gating mechanism (docs/workflows.md's "eligible_at set far
-- in the future, or a dedicated gating column -- exact mechanism to be
-- finalized during Phase 7 implementation" open question, resolved
-- here): a node with unsatisfied dependencies is inserted with its
-- underlying jobs row's eligible_at set to a fixed, far-future sentinel
-- timestamp (9999-12-31T23:59:59Z -- see internal/store/workflow.go's
-- blockedEligibleAt), not PostgreSQL's special 'infinity' timestamptz
-- value, which is not reliably representable as a Go time.Time through
-- the pgx driver used by this codebase. This choice requires ZERO changes
-- to the existing claim query (docs/worker-protocol.md): a blocked node's
-- job is simply never selected by "eligible_at <= now()" until a
-- predecessor-completion propagation step (see
-- internal/store/workflow.go's propagateWorkflowTransition, invoked
-- inside the SAME transaction as the predecessor's own completion/
-- cancellation/dead-letter transition) advances eligible_at to the
-- node's real intended schedule (COALESCE(scheduled_at, now())). No new
-- column is added to jobs for this -- eligible_at alone is the gating
-- mechanism, exactly as docs/scheduling.md already establishes for
-- ordinary scheduling and retry backoff.

CREATE TABLE IF NOT EXISTS workflow_instances (
    id                   UUID PRIMARY KEY,
    state                TEXT NOT NULL DEFAULT 'RUNNING',
    cancel_requested     BOOLEAN NOT NULL DEFAULT false,
    cancel_requested_at  TIMESTAMPTZ NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at          TIMESTAMPTZ NULL,

    CONSTRAINT workflow_instances_state_check
        CHECK (state IN ('RUNNING','SUCCEEDED','FAILED','CANCELLED'))
);

CREATE INDEX IF NOT EXISTS idx_workflow_instances_state
    ON workflow_instances (state);

-- depends_on is a uuid[] column (not a workflow_edges join table) per
-- docs/workflows.md's "Why an Array Column Instead of an Edge Table" --
-- v1-of-workflows targets DAGs of modest size (tens to low hundreds of
-- nodes per instance), not graphs requiring relational edge queries at
-- scale. workflow_nodes_job_unique enforces that a job row backs at most
-- one workflow node (a job is never shared between two logical workflow
-- positions).
CREATE TABLE IF NOT EXISTS workflow_nodes (
    id                    UUID PRIMARY KEY,
    workflow_instance_id  UUID NOT NULL REFERENCES workflow_instances(id),
    node_key              TEXT NOT NULL,
    job_id                UUID NOT NULL REFERENCES jobs(id),
    depends_on            UUID[] NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workflow_nodes_key_unique UNIQUE (workflow_instance_id, node_key),
    CONSTRAINT workflow_nodes_job_unique UNIQUE (job_id)
);

CREATE INDEX IF NOT EXISTS idx_workflow_nodes_workflow_instance_id
    ON workflow_nodes (workflow_instance_id);

-- Supports the reverse lookup "find every node that depends on node X"
-- (internal/store/workflow.go's dependentsOf, `WHERE depends_on @>
-- ARRAY[$1]`), which runs once per terminal node-state transition on any
-- workflow-backed job.
CREATE INDEX IF NOT EXISTS idx_workflow_nodes_depends_on
    ON workflow_nodes USING GIN (depends_on);
