-- +goose Up
-- This migration changes metadata. 00020 validates the new constraint.

-- Attempt number in a series is delivery_attempts minus delivery_series_start_attempt.
-- delivery_retry_at remains visible until the next public change, which increments delivery_version.
ALTER TABLE email.message
    ADD COLUMN delivery_version              bigint NOT NULL DEFAULT 0,
    ADD COLUMN delivery_retry_at             timestamptz NULL,
    ADD COLUMN delivery_manual_retry_count   smallint NOT NULL DEFAULT 0,
    ADD COLUMN delivery_series_start_attempt integer NOT NULL DEFAULT 0,
    DROP CONSTRAINT message_delivery_valid,
    -- Every term is NOT NULL: a NULL term would let the row pass.
    ADD CONSTRAINT message_delivery_valid CHECK (
        delivery_attempts >= 0
        AND (delivery_status IS NULL OR delivery_status IN ('pending', 'sent', 'failed', 'cancelled'))
        AND (delivery_phase IS NULL OR delivery_phase IN ('queued', 'sending', 'committed'))
        AND (delivery_phase IS NOT NULL) = coalesce(delivery_status = 'pending', false)
        AND (delivery_lease_until IS NOT NULL) = coalesce(delivery_phase IN ('sending', 'committed'), false)
        AND (delivered_at IS NOT NULL) = coalesce(delivery_status = 'sent', false)
        AND (delivery_error_code IS NOT NULL) = coalesce(delivery_status = 'failed', false)
        AND (delivery_error_code IS NULL OR delivery_error_code IN (
            'auth', 'recipient_rejected', 'message_too_large', 'connection', 'profile_unavailable',
            'rejected', 'temporary_failure', 'interrupted', 'unknown_result'))
        AND (delivery_retry_at IS NULL OR coalesce(delivery_status = 'pending', false))
        AND delivery_version >= 0
        AND delivery_manual_retry_count >= 0
        AND delivery_series_start_attempt BETWEEN 0 AND delivery_attempts
        AND (delivery_status IS NOT NULL
            OR (delivery_version = 0 AND delivery_manual_retry_count = 0 AND delivery_series_start_attempt = 0))
    ) NOT VALID;

-- +goose Down
-- Stop workers and settle active attempts before downgrading.
-- Stage 15 cannot keep cancelled mail and would send deferred retries early.
-- +goose StatementBegin
DO $$
BEGIN
    LOCK TABLE email.message IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM email.message WHERE delivery_status = 'cancelled') THEN
        RAISE EXCEPTION 'cancelled outbound emails exist: stage 15 cannot keep them';
    END IF;
    IF EXISTS (SELECT 1 FROM email.message WHERE delivery_phase IN ('sending', 'committed')) THEN
        RAISE EXCEPTION 'SMTP attempts are running: stop the workers and let them finish first';
    END IF;
    IF EXISTS (SELECT 1 FROM email.message
               WHERE delivery_phase = 'queued' AND delivery_retry_at > clock_timestamp()) THEN
        RAISE EXCEPTION 'scheduled retries exist: stage 15 would send them before their time';
    END IF;
END $$;
-- +goose StatementEnd

-- The constraint of 00016, added valid: rows are checked under the lock above.
ALTER TABLE email.message
    DROP CONSTRAINT message_delivery_valid,
    DROP COLUMN delivery_series_start_attempt,
    DROP COLUMN delivery_manual_retry_count,
    DROP COLUMN delivery_retry_at,
    DROP COLUMN delivery_version,
    ADD CONSTRAINT message_delivery_valid CHECK (
        delivery_attempts >= 0
        AND (delivery_status IS NULL OR delivery_status IN ('pending', 'sent', 'failed'))
        AND (delivery_phase IS NULL OR delivery_phase IN ('queued', 'sending', 'committed'))
        AND (delivery_phase IS NOT NULL) = coalesce(delivery_status = 'pending', false)
        AND (delivery_lease_until IS NOT NULL) = coalesce(delivery_phase IN ('sending', 'committed'), false)
        AND (delivered_at IS NOT NULL) = coalesce(delivery_status = 'sent', false)
        AND (delivery_error_code IS NOT NULL) = coalesce(delivery_status = 'failed', false)
        AND (delivery_error_code IS NULL OR delivery_error_code IN (
            'auth', 'recipient_rejected', 'message_too_large', 'connection', 'profile_unavailable',
            'rejected', 'temporary_failure', 'interrupted', 'unknown_result'))
    );
