-- 001_init.down.sql — drop everything 001_init.up.sql created.
-- Order is reverse of creation so dependent tables vanish before their
-- referents. IF EXISTS guards make the file idempotent under partial-state
-- recovery.
DROP TABLE IF EXISTS sandbox_usage;
DROP TABLE IF EXISTS operations;
DROP TABLE IF EXISTS snapshots;
DROP TABLE IF EXISTS sandboxes;
DROP TABLE IF EXISTS templates;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS clusters;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS accounts;
