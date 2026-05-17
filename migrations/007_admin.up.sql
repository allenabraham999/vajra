-- 007_admin.up.sql — operator/admin support for the cluster admin panel.
--
-- Adds the account columns the /v1/admin/* surface reads and writes:
--   * is_admin   — gates the admin panel (see Handlers.requireAdmin). An
--                  account is also treated as admin when its email is in
--                  the VAJRA_ADMIN_EMAIL list, so this column is a cache
--                  the login handler back-fills for env-listed admins.
--   * suspended  — operator-set flag surfaced in the admin Accounts tab.
--   * last_login — stamped by the login handler so operators can spot
--                  dormant accounts.
--
-- The prepaid balance ("credits") is NOT added here: migration 006_billing
-- already owns the accounts.credits_remaining column, and the admin
-- "add credits" action reuses AccountStore.IncrementCredits against it.

ALTER TABLE accounts
    ADD COLUMN is_admin   BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN suspended  BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN last_login TIMESTAMPTZ;

-- Partial index: the admin gate only ever asks "is this account an admin",
-- so we only index the true rows.
CREATE INDEX idx_accounts_is_admin ON accounts (is_admin) WHERE is_admin;
