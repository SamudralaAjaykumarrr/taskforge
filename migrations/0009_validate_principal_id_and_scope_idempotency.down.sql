-- taskforge:down-migration-status: data-safe-reversible
-- Drops what 0009's up built. Always safe: removing indexes loses no data,
-- and re-running 0009's up rebuilds all three.
--
-- Note what this file does NOT do: it does not un-validate the NOT NULL
-- check constraints. PostgreSQL has no "un-VALIDATE" operation, and none is
-- needed -- 0008's down drops those constraints outright.
--
-- It also does not recreate migration 0001's global idempotency index. That
-- is 0010's down, which is the one that can legitimately fail; keeping the
-- fallible step in its own file means rolling back this one is
-- unconditionally safe.
DROP INDEX IF EXISTS idx_jobs_idempotency_scoped;
DROP INDEX IF EXISTS idx_jobs_principal_id;
DROP INDEX IF EXISTS idx_workflow_instances_principal_id;
