-- +goose Up
-- Version of what the read API shows of a Thread, raised with every visible change
-- by the writer of the Workspace events. 0 means no change since this release;
-- earlier changes get no version and no event.
-- Both statements change metadata only, so the exclusive lock is short; it still
-- waits for long transactions on email.thread, so run it outside them.
ALTER TABLE email.thread
    ADD COLUMN workspace_version bigint NOT NULL DEFAULT 0,
    -- Validated by 00015 outside this transaction.
    ADD CONSTRAINT thread_workspace_version_valid CHECK (workspace_version >= 0) NOT VALID;

-- +goose Down
ALTER TABLE email.thread
    DROP CONSTRAINT IF EXISTS thread_workspace_version_valid,
    DROP COLUMN IF EXISTS workspace_version;
