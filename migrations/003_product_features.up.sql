-- 003_product_features.up.sql — Daytona-parity product features.
--
-- Adds:
--   * builds            — async Dockerfile→Template build jobs.
--   * webhooks          — per-account outbound HTTP notification targets.
--   * sandboxes.auto_stop_minutes / auto_archive_minutes — idle lifecycle.
--   * sandboxes.last_activity — drives auto-stop / auto-archive.

CREATE TABLE builds (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    template_name TEXT NOT NULL,
    template_ver  TEXT NOT NULL,
    status        TEXT NOT NULL,
    dockerfile    TEXT NOT NULL,
    template_id   TEXT REFERENCES templates(id) ON DELETE SET NULL,
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at  TIMESTAMPTZ
);
CREATE INDEX idx_builds_account_id ON builds (account_id);
CREATE INDEX idx_builds_status     ON builds (status);
CREATE INDEX idx_builds_created_at ON builds (created_at);

CREATE TABLE webhooks (
    id          TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    events      JSONB NOT NULL DEFAULT '[]'::jsonb,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_webhooks_account_id ON webhooks (account_id);
CREATE INDEX idx_webhooks_active     ON webhooks (active);

ALTER TABLE sandboxes
    ADD COLUMN auto_stop_minutes    INTEGER NOT NULL DEFAULT 15,
    ADD COLUMN auto_archive_minutes INTEGER NOT NULL DEFAULT 1440,
    ADD COLUMN last_activity        TIMESTAMPTZ NOT NULL DEFAULT NOW();
CREATE INDEX idx_sandboxes_last_activity ON sandboxes (last_activity);
