ALTER TABLE sandboxes
    DROP COLUMN git_url,
    DROP COLUMN git_branch,
    DROP COLUMN git_clone_status,
    DROP COLUMN git_clone_error;
