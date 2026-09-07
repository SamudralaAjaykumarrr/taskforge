-- Phase 2: job_attempts, per docs/data-model.md and docs/roadmap.md's
-- explicit Phase 2 scope ("job_attempts table introduced here if not
-- already in Phase 1"). Populated by the claim/reclaim and completion
-- paths in internal/store (see docs/worker-protocol.md), in the same
-- transaction as the corresponding jobs row write, so the two never
-- diverge (TF-INV-013).
--
-- This migration does NOT alter the jobs table: every column
-- worker-protocol.md's Phase 2 claim/heartbeat/completion queries need
-- (lease_owner, lease_generation, lease_expires_at, heartbeat_at) already
-- exists from migration 0001, per that migration's explicit note ("this
-- avoids a schema migration between Phase 1 and Phase 2").
--
-- Phase 2 only ever writes outcome values SUCCEEDED, FAILED_PERMANENT (the
-- Phase 1/2 "any failure -> DEAD_LETTERED" simplification, per
-- docs/roadmap.md Phase 2 non-goals: "still failure = dead-letter"), and
-- LEASE_EXPIRED (recorded against an attempt that was superseded by
-- reclaim or lazily dead-lettered by the sweep, per data-model.md:
-- "Set ... on lazy detection of lease expiry"). FAILED_RETRYABLE,
-- TIMED_OUT, and CANCELLED are documented outcome values (data-model.md)
-- that Phase 2's code paths do not yet produce (retry backoff, execution
-- timeout classification, and cancellation are all later phases) but the
-- CHECK constraint allows them now so a later phase does not need a
-- constraint migration to start using them.

CREATE TABLE IF NOT EXISTS job_attempts (
    id                  UUID PRIMARY KEY,
    job_id              UUID NOT NULL REFERENCES jobs(id),
    attempt_number      INTEGER NOT NULL,
    lease_generation    BIGINT NOT NULL,
    worker_id           TEXT NOT NULL,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ NULL,
    outcome             TEXT NULL,
    error_class         TEXT NULL,
    error_message       TEXT NULL,

    CONSTRAINT job_attempts_attempt_number_unique
        UNIQUE (job_id, attempt_number),
    CONSTRAINT job_attempts_outcome_check
        CHECK (outcome IS NULL OR outcome IN (
            'SUCCEEDED', 'FAILED_RETRYABLE', 'FAILED_PERMANENT',
            'TIMED_OUT', 'LEASE_EXPIRED', 'CANCELLED'
        ))
);

CREATE INDEX IF NOT EXISTS idx_job_attempts_job_id
    ON job_attempts (job_id, attempt_number);

-- Supports the reclaim path's "finalize the previous attempt" write,
-- which looks up an in-flight (finished_at IS NULL) attempt by
-- (job_id, lease_generation) rather than by attempt_number, since the
-- caller (internal/store.Claim) has the old lease_generation on hand from
-- the claim query's own candidate CTE.
CREATE INDEX IF NOT EXISTS idx_job_attempts_job_id_lease_generation
    ON job_attempts (job_id, lease_generation);
