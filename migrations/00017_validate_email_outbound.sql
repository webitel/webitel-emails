-- +goose NO TRANSACTION
-- +goose Up
-- Validation scans under SHARE UPDATE EXCLUSIVE and the indexes are built
-- concurrently, so reads and writes go on. A repeat after a failure is safe: a
-- valid constraint is not checked again, and a leftover invalid index is dropped.
ALTER TABLE email.message VALIDATE CONSTRAINT message_outbound_valid;
ALTER TABLE email.message VALIDATE CONSTRAINT message_delivery_valid;
ALTER TABLE email.thread_participant VALIDATE CONSTRAINT thread_participant_ids_valid;
ALTER TABLE email.thread_participant VALIDATE CONSTRAINT thread_participant_origin_valid;

-- Idempotent Send: one message per intent of an author.
DROP INDEX CONCURRENTLY IF EXISTS email.message_request_uidx;
CREATE UNIQUE INDEX CONCURRENTLY message_request_uidx
    ON email.message (domain_id, author_user_id, request_id)
    WHERE request_id IS NOT NULL;

-- Claims and recovery of the SMTP worker.
DROP INDEX CONCURRENTLY IF EXISTS email.message_delivery_pending_idx;
CREATE INDEX CONCURRENTLY message_delivery_pending_idx
    ON email.message (delivery_phase, delivery_lease_until, id)
    WHERE delivery_status = 'pending';

-- +goose Down
-- The constraints are dropped with their columns by 00016.
DROP INDEX CONCURRENTLY IF EXISTS email.message_delivery_pending_idx;
DROP INDEX CONCURRENTLY IF EXISTS email.message_request_uidx;
