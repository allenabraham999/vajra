-- Git auto-clone: a sandbox create may carry a git_url, in which case
-- master clones that repository into /workspace after the sandbox reaches
-- RUNNING. git_url / git_branch echo the request; git_clone_status tracks
-- the post-create hook ('' | pending | cloning | done | failed) and
-- git_clone_error carries the failure reason when it failed.
--
-- The access token used for private repos is deliberately NOT stored — it
-- lives only in master's memory for the duration of the clone.
ALTER TABLE sandboxes
    ADD COLUMN git_url          TEXT NOT NULL DEFAULT '',
    ADD COLUMN git_branch       TEXT NOT NULL DEFAULT '',
    ADD COLUMN git_clone_status TEXT NOT NULL DEFAULT '',
    ADD COLUMN git_clone_error  TEXT NOT NULL DEFAULT '';
