-- Data-safe-reversible: dropping an index loses no data. Re-running the
-- up migration after this is safe (IF NOT EXISTS).

DROP INDEX IF EXISTS idx_jobs_reclaimable_by_queue;
DROP INDEX IF EXISTS idx_jobs_claimable_by_queue;
