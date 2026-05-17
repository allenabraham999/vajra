-- 006_billing.up.sql — real billing: prepaid credits, Stripe purchases,
-- and a per-day usage rollup the billing meter writes every tick.
--
-- IDs stay TEXT (caller-generated) to match the rest of the schema; we
-- deliberately do not lean on gen_random_uuid() — see 001_init.up.sql.

-- credits_remaining is the prepaid balance the billing meter decrements.
-- Existing accounts inherit the $200 demo grant via the column DEFAULT.
ALTER TABLE accounts
    ADD COLUMN credits_remaining DECIMAL(10,4) NOT NULL DEFAULT 200.0;

-- transactions is the ledger of credit purchases. A row is created in
-- 'pending' when a Stripe Checkout session opens and flipped to
-- 'completed' by the webhook once payment clears (or 'failed').
CREATE TABLE transactions (
    id                TEXT PRIMARY KEY,
    account_id        TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    amount_usd        DECIMAL(10,2) NOT NULL,
    stripe_session_id TEXT UNIQUE,
    status            TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_transactions_account_id ON transactions (account_id);
CREATE INDEX idx_transactions_created_at ON transactions (created_at);

-- usage_daily is the billing meter's per-account, per-day rollup. The
-- meter upserts a slice of usage into the (account_id, day) row on every
-- tick; the dashboard reads it for the spend chart and 30-day totals.
CREATE TABLE usage_daily (
    account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    day             DATE NOT NULL,
    vcpu_hours      DOUBLE PRECISION NOT NULL DEFAULT 0,
    memory_gb_hours DOUBLE PRECISION NOT NULL DEFAULT 0,
    cost_usd        DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (account_id, day)
);
CREATE INDEX idx_usage_daily_day ON usage_daily (day);
