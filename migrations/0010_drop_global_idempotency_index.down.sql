-- taskforge:down-migration-status: data-safe-reversible
-- Honest, not blanket-safe (docs/phase-12-plan.md §5, verification point
-- 13). This is the one Phase 12 down migration that can legitimately fail,
-- which is why it lives alone: every other down in this phase is
-- unconditionally safe, and rolling one back must not be gated on this one's
-- outcome.
--
-- Recreating idx_jobs_idempotency_key in its original GLOBAL
-- (job_type, idempotency_key) shape is impossible once two different
-- principals share one (job_type, idempotency_key) pair -- precisely the
-- case Phase 12 exists to permit. When that data exists this CREATE UNIQUE
-- INDEX fails with a PostgreSQL uniqueness violation and the migration
-- aborts having changed nothing (internal/migrate runs each file in one
-- transaction).
--
-- That failure is correct behaviour, not a bug to work around: TaskForge
-- cannot silently pick which tenant's job to discard. This rollback is
-- documented as forward-fix-preferred once real divergent data exists.
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_idempotency_key
    ON jobs (job_type, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
