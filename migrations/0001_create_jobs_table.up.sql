-- Phase 1: the jobs table, per docs/data-model.md.
--
-- job_attempts is intentionally NOT created in this migration. Phase 1's
-- documented state machine (docs/roadmap.md, "Phase 1 — Single-Node
-- Durable Job Engine") is limited to QUEUED -> RUNNING -> SUCCEEDED and
-- RUNNING -> DEAD_LETTERED on any failure; TF-INV-007/TF-INV-009 (the
-- invariants job_attempts exists to satisfy) are not required until
-- Phase 2, and docs/roadmap.md explicitly allows deferring job_attempts:
-- "no job_attempts yet -- deferred to make Phase 1 minimal ... job_attempts
-- table introduced here if not already in Phase 1" (Phase 2 scope).
--
-- Lease columns (lease_owner, lease_generation, lease_expires_at) ARE
-- included now even though Phase 1's single synchronous worker does not
-- stress them, per docs/roadmap.md: "this avoids a schema migration
-- between Phase 1 and Phase 2." The remaining columns mirror
-- docs/data-model.md's jobs table in full, since that document does not
-- mark any of them as deferred (only job_attempts and the workflow tables
-- are marked deferred).

CREATE TABLE IF NOT EXISTS jobs (
    id                          UUID PRIMARY KEY,
    job_type                    TEXT NOT NULL,
    payload                     JSONB NOT NULL,
    state                       TEXT NOT NULL,
    priority                    SMALLINT NOT NULL DEFAULT 0,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    eligible_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    scheduled_at                TIMESTAMPTZ NULL,
    lease_owner                 TEXT NULL,
    lease_generation            BIGINT NOT NULL DEFAULT 0,
    lease_expires_at            TIMESTAMPTZ NULL,
    heartbeat_at                TIMESTAMPTZ NULL,
    attempt_count               INTEGER NOT NULL DEFAULT 0,
    max_attempts                INTEGER NOT NULL DEFAULT 5,
    execution_timeout_seconds   INTEGER NOT NULL,
    cancel_requested            BOOLEAN NOT NULL DEFAULT false,
    cancel_requested_at         TIMESTAMPTZ NULL,
    idempotency_key             TEXT NULL,
    last_error                  TEXT NULL,
    last_error_class            TEXT NULL,
    result_metadata             JSONB NULL,
    terminal_at                 TIMESTAMPTZ NULL,
    version                     BIGINT NOT NULL DEFAULT 0,

    CONSTRAINT jobs_state_check
        CHECK (state IN ('QUEUED','RETRY_WAIT','RUNNING','SUCCEEDED','CANCELLED','DEAD_LETTERED')),
    CONSTRAINT jobs_attempt_count_check
        CHECK (attempt_count <= max_attempts OR state = 'DEAD_LETTERED')
);

-- Enforces TF-INV-008 / TF-INV-016 at the database level. Not exercised by
-- Phase 1's API (no Idempotency-Key support yet, per docs/roadmap.md), but
-- data-model.md establishes it as part of the jobs table now, and the
-- constraint costs nothing to have in place before Phase 4 wires up the
-- request-side support.
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_idempotency_key
    ON jobs (job_type, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_jobs_claimable
    ON jobs (priority DESC, eligible_at ASC)
    WHERE state IN ('QUEUED', 'RETRY_WAIT');

CREATE INDEX IF NOT EXISTS idx_jobs_expired_lease
    ON jobs (lease_expires_at)
    WHERE state = 'RUNNING';

CREATE INDEX IF NOT EXISTS idx_jobs_state
    ON jobs (state);
