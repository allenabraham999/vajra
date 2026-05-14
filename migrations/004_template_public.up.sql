-- 004_template_public.up.sql — make system templates discoverable.
--
-- ListByAccount was strictly account-scoped, so the default "ubuntu-noble"
-- template (owned by the bootstrap account) was invisible to every new
-- account that registered through the dashboard. The dropdown rendered
-- empty and users couldn't launch a sandbox.
--
-- A "public" flag is the smallest change that fixes this without
-- introducing a system-account abstraction. Public templates show up in
-- every account's list; the owner still controls writes (Create/Delete).

ALTER TABLE templates
    ADD COLUMN public BOOLEAN NOT NULL DEFAULT FALSE;

-- Mark any existing ubuntu-noble row public so the demo dashboard sees it
-- immediately after the migration runs. Account-scoped duplicates of the
-- same name (created during earlier testing) are all included.
UPDATE templates SET public = TRUE WHERE name = 'ubuntu-noble';

CREATE INDEX idx_templates_public ON templates (public) WHERE public;
