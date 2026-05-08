-- 002_share_links.up.sql — adds the share_links table for
-- shareable sandbox URLs. Tokens are stored as their SHA256 hex
-- digest (matches the api_keys.key_hash pattern); the cleartext
-- token is shown to the user exactly once at creation time.

CREATE TABLE share_links (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    sandbox_id   TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    port         INTEGER,                 -- nullable: NULL = any port
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ,             -- nullable: NULL = never
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX idx_share_links_account_id ON share_links (account_id);
CREATE INDEX idx_share_links_sandbox_id ON share_links (sandbox_id);
CREATE INDEX idx_share_links_token_hash ON share_links (token_hash);
