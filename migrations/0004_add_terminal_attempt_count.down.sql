-- taskforge:down-migration-status: data-safe-reversible
ALTER TABLE jobs DROP COLUMN IF EXISTS terminal_attempt_count;
