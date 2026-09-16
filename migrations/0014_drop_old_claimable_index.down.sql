-- Recreates the retired index unconditionally -- see the up migration's
-- comment for why this, unlike migration 0010's down, needs no
-- honest-failure caveat.

CREATE INDEX IF NOT EXISTS idx_jobs_claimable
    ON jobs (priority DESC, eligible_at ASC)
    WHERE state IN ('QUEUED', 'RETRY_WAIT');
