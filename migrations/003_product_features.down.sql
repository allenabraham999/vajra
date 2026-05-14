-- 003_product_features.down.sql — reverse of 003_product_features.up.sql.

DROP INDEX IF EXISTS idx_sandboxes_last_activity;
ALTER TABLE sandboxes
    DROP COLUMN IF EXISTS last_activity,
    DROP COLUMN IF EXISTS auto_archive_minutes,
    DROP COLUMN IF EXISTS auto_stop_minutes;

DROP TABLE IF EXISTS webhooks;
DROP TABLE IF EXISTS builds;
