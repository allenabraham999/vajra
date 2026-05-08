-- 001_init.up.sql — initial Vajra schema.
--
-- Conventions:
--   * IDs are TEXT (caller-generated UUIDs/ulids). Letting the DB pick
--     uuid_generate_v4() pins us to a specific extension; keeping IDs
--     application-side keeps the store driver-agnostic.
--   * State enums are TEXT. The Go models own validation via Valid().
--     Storing as TEXT avoids the painful Postgres ENUM ALTER dance when
--     a new state is added.
--   * JSONB for blob-like config columns matches the (Scan/Value) pairs in
--     internal/models/jsonb.go.
--   * TIMESTAMPTZ everywhere; everything else is a guaranteed bug.

CREATE TABLE accounts (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_accounts_created_at ON accounts (created_at);

CREATE TABLE api_keys (
    id          TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    key_hash    TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    permissions JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ
);
CREATE INDEX idx_api_keys_account_id ON api_keys (account_id);
CREATE INDEX idx_api_keys_created_at ON api_keys (created_at);

CREATE TABLE clusters (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    region     TEXT NOT NULL,
    state      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_clusters_state      ON clusters (state);
CREATE INDEX idx_clusters_created_at ON clusters (created_at);

CREATE TABLE nodes (
    id             TEXT PRIMARY KEY,
    cluster_id     TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    hostname       TEXT NOT NULL,
    ip             TEXT NOT NULL,
    state          TEXT NOT NULL,
    capacity       JSONB NOT NULL DEFAULT '{}'::jsonb,
    used_resources JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_nodes_cluster_id     ON nodes (cluster_id);
CREATE INDEX idx_nodes_state          ON nodes (state);
CREATE INDEX idx_nodes_last_heartbeat ON nodes (last_heartbeat);

CREATE TABLE templates (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    version       TEXT NOT NULL,
    hash          TEXT NOT NULL,
    rootfs_path   TEXT NOT NULL,
    kernel_path   TEXT NOT NULL,
    snapshot_path TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (account_id, name, version)
);
CREATE INDEX idx_templates_account_id ON templates (account_id);
CREATE INDEX idx_templates_hash       ON templates (hash);
CREATE INDEX idx_templates_created_at ON templates (created_at);

CREATE TABLE sandboxes (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    node_id     TEXT REFERENCES nodes(id)            ON DELETE SET NULL,
    cluster_id  TEXT REFERENCES clusters(id)         ON DELETE SET NULL,
    template_id TEXT NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
    state       TEXT NOT NULL,
    config      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_sandboxes_account_id ON sandboxes (account_id);
CREATE INDEX idx_sandboxes_node_id    ON sandboxes (node_id);
CREATE INDEX idx_sandboxes_cluster_id ON sandboxes (cluster_id);
CREATE INDEX idx_sandboxes_state      ON sandboxes (state);
CREATE INDEX idx_sandboxes_created_at ON sandboxes (created_at);

CREATE TABLE snapshots (
    id           TEXT PRIMARY KEY,
    sandbox_id   TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    account_id   TEXT NOT NULL REFERENCES accounts(id)  ON DELETE CASCADE,
    node_id      TEXT NOT NULL REFERENCES nodes(id)     ON DELETE RESTRICT,
    storage_path TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_snapshots_account_id ON snapshots (account_id);
CREATE INDEX idx_snapshots_sandbox_id ON snapshots (sandbox_id);
CREATE INDEX idx_snapshots_node_id    ON snapshots (node_id);
CREATE INDEX idx_snapshots_created_at ON snapshots (created_at);

CREATE TABLE operations (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id)  ON DELETE CASCADE,
    sandbox_id   TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    type         TEXT NOT NULL,
    status       TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    error        TEXT
);
CREATE INDEX idx_operations_account_id ON operations (account_id);
CREATE INDEX idx_operations_sandbox_id ON operations (sandbox_id);
CREATE INDEX idx_operations_status     ON operations (status);
CREATE INDEX idx_operations_started_at ON operations (started_at);

CREATE TABLE sandbox_usage (
    id                BIGSERIAL PRIMARY KEY,
    sandbox_id        TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    account_id        TEXT NOT NULL REFERENCES accounts(id)  ON DELETE CASCADE,
    period_start      TIMESTAMPTZ NOT NULL,
    period_end        TIMESTAMPTZ NOT NULL,
    vcpu_seconds      BIGINT NOT NULL DEFAULT 0,
    memory_mb_seconds BIGINT NOT NULL DEFAULT 0,
    disk_gb_seconds   BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_sandbox_usage_account_id   ON sandbox_usage (account_id);
CREATE INDEX idx_sandbox_usage_sandbox_id   ON sandbox_usage (sandbox_id);
CREATE INDEX idx_sandbox_usage_period_start ON sandbox_usage (period_start);
