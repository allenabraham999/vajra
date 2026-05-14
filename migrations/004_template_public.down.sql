DROP INDEX IF EXISTS idx_templates_public;
ALTER TABLE templates DROP COLUMN IF EXISTS public;
