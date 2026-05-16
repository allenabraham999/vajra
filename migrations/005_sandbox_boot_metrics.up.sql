-- 005_sandbox_boot_metrics.up.sql — record per-sandbox boot performance.
--
-- The dashboard's Metrics page shows a "recent boot times" table so users
-- can see how fast their sandboxes came up and whether the create was
-- served from the pre-warm pool. Two nullable columns carry that:
--
--   time_to_running_ms — wall-clock from create accepted to RUNNING.
--   pool_hit           — true when the agent served the create from a
--                        warm pool member instead of a cold restore.
--
-- Both are nullable: rows created before this migration, and sandboxes
-- that never reached RUNNING, simply leave them NULL.

ALTER TABLE sandboxes
    ADD COLUMN time_to_running_ms BIGINT,
    ADD COLUMN pool_hit           BOOLEAN;
