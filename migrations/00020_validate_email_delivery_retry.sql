-- +goose NO TRANSACTION
-- +goose Up
-- Validation scans under SHARE UPDATE EXCLUSIVE and the indexes are built
-- concurrently, so reads and writes go on. A repeat after a failure is safe.
ALTER TABLE email.message VALIDATE CONSTRAINT message_delivery_valid;

-- Claim merges separate indexes for initial sends and due retries.
-- The pending index from 00017 remains available for recovery.
DROP INDEX CONCURRENTLY IF EXISTS email.message_delivery_queued_idx;
CREATE INDEX CONCURRENTLY message_delivery_queued_idx
    ON email.message (received_at, id)
    WHERE delivery_status = 'pending' AND delivery_phase = 'queued' AND delivery_retry_at IS NULL;

DROP INDEX CONCURRENTLY IF EXISTS email.message_delivery_retry_idx;
CREATE INDEX CONCURRENTLY message_delivery_retry_idx
    ON email.message (delivery_retry_at, id)
    WHERE delivery_status = 'pending' AND delivery_phase = 'queued' AND delivery_retry_at IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS email.message_delivery_retry_idx;
DROP INDEX CONCURRENTLY IF EXISTS email.message_delivery_queued_idx;
