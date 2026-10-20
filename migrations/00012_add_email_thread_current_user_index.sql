-- +goose NO TRANSACTION
-- +goose Up
-- Current Threads of a user, newest first. Built concurrently; a leftover invalid
-- index of an interrupted run is dropped first.
DROP INDEX CONCURRENTLY IF EXISTS email.thread_domain_current_user_last_message_idx;
CREATE INDEX CONCURRENTLY thread_domain_current_user_last_message_idx
    ON email.thread (domain_id, current_user_id, last_message_at DESC, id DESC)
    WHERE current_user_id IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS email.thread_domain_current_user_last_message_idx;
