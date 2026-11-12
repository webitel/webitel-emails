-- +goose Up
-- Outbound email (stage 15). Every statement changes metadata only; the new
-- constraints are validated by 00017 outside this transaction.
ALTER TABLE email.profile
    ADD COLUMN allow_send boolean NOT NULL DEFAULT false;

-- Delivery of an outbound message. delivery_attempts is the token of the SMTP
-- attempt: a claim raises it, and every later write of the worker requires it.
-- The phase exists only while pending: queued, sending, or committed once the
-- server may have accepted the email.
ALTER TABLE email.message
    ADD COLUMN author_user_id       bigint NULL,
    ADD COLUMN request_id           uuid NULL,
    ADD COLUMN request_fingerprint  bytea NULL,
    ADD COLUMN delivery_status      text NULL,
    ADD COLUMN delivery_phase       text NULL,
    ADD COLUMN delivery_attempts    integer NOT NULL DEFAULT 0,
    ADD COLUMN delivery_lease_until timestamptz NULL,
    ADD COLUMN delivery_error_code  text NULL,
    ADD COLUMN delivered_at         timestamptz NULL,
    ADD CONSTRAINT message_outbound_valid CHECK (
        CASE WHEN direction = 'outbound' THEN
            state = 'ready'
            AND author_user_id IS NOT NULL AND author_user_id > 0
            AND request_id IS NOT NULL
            AND request_fingerprint IS NOT NULL AND octet_length(request_fingerprint) = 32
            AND delivery_status IS NOT NULL
        ELSE
            author_user_id IS NULL AND request_id IS NULL AND request_fingerprint IS NULL
            AND delivery_status IS NULL AND delivery_attempts = 0
            AND delivery_lease_until IS NULL AND delivered_at IS NULL
        END
    ) NOT VALID,
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
    ) NOT VALID;

-- The author of a new outbound Thread joins its journal before any snapshot:
-- outbound_at keeps that origin, as backfilled_at does, and the agent comes
-- with the first snapshot that proves a Take.
ALTER TABLE email.thread_participant
    ADD COLUMN outbound_at timestamptz NULL,
    ALTER COLUMN agent_id DROP NOT NULL,
    DROP CONSTRAINT thread_participant_ids_valid,
    ADD CONSTRAINT thread_participant_ids_valid CHECK (
        domain_id > 0 AND thread_id > 0 AND user_id > 0
        -- Only an outbound author may lack the agent; each term is never NULL.
        AND (agent_id IS NOT NULL OR outbound_at IS NOT NULL)
        AND (agent_id IS NULL OR agent_id > 0)
    ) NOT VALID,
    DROP CONSTRAINT thread_participant_origin_valid,
    ADD CONSTRAINT thread_participant_origin_valid CHECK (
        backfilled_at IS NOT NULL
        OR (outbound_at IS NOT NULL AND first_participated_at IS NOT NULL)
        OR (first_version IS NOT NULL AND first_participated_at IS NOT NULL)
    ) NOT VALID;

-- +goose Down
-- Refuses while outbound emails exist: their authors, delivery state and request
-- ids would be lost and 00017 could not validate them again.
-- +goose StatementBegin
DO $$
BEGIN
    LOCK TABLE email.message IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM email.message WHERE direction = 'outbound') THEN
        RAISE EXCEPTION 'outbound emails exist: rolling back would lose their authors, delivery and request ids';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE email.thread_participant
    DROP CONSTRAINT thread_participant_origin_valid,
    ADD CONSTRAINT thread_participant_origin_valid
        CHECK (backfilled_at IS NOT NULL OR (first_version IS NOT NULL AND first_participated_at IS NOT NULL)),
    DROP CONSTRAINT thread_participant_ids_valid,
    ADD CONSTRAINT thread_participant_ids_valid
        CHECK (domain_id > 0 AND thread_id > 0 AND user_id > 0 AND agent_id > 0),
    ALTER COLUMN agent_id SET NOT NULL,
    DROP COLUMN outbound_at;

ALTER TABLE email.message
    DROP CONSTRAINT IF EXISTS message_delivery_valid,
    DROP CONSTRAINT IF EXISTS message_outbound_valid,
    DROP COLUMN IF EXISTS delivered_at,
    DROP COLUMN IF EXISTS delivery_error_code,
    DROP COLUMN IF EXISTS delivery_lease_until,
    DROP COLUMN IF EXISTS delivery_attempts,
    DROP COLUMN IF EXISTS delivery_phase,
    DROP COLUMN IF EXISTS delivery_status,
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS request_id,
    DROP COLUMN IF EXISTS author_user_id;

ALTER TABLE email.profile DROP COLUMN IF EXISTS allow_send;
