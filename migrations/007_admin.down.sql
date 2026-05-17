DROP INDEX IF EXISTS idx_accounts_is_admin;
ALTER TABLE accounts DROP COLUMN IF EXISTS last_login;
ALTER TABLE accounts DROP COLUMN IF EXISTS suspended;
ALTER TABLE accounts DROP COLUMN IF EXISTS is_admin;
