-- taskforge:down-migration-status: data-safe-reversible
-- Data-safe-reversible for the same reason as every other purely-additive
-- Phase 13 table: nothing outside these four tables references them (jobs
-- and workflow_instances gained only a queue_name TEXT column, never a
-- foreign key into queue_state/queue_limits/queue_slots), so dropping them
-- loses only governance/runtime-rate-limit state, never job data.

DROP TABLE IF EXISTS rate_limit_buckets;
DROP TABLE IF EXISTS queue_slots;
DROP TABLE IF EXISTS queue_limits;
DROP TABLE IF EXISTS queue_state;
