-- 006_billing.down.sql — reverse the billing schema.

DROP TABLE IF EXISTS usage_daily;
DROP TABLE IF EXISTS transactions;
ALTER TABLE accounts DROP COLUMN IF EXISTS credits_remaining;
