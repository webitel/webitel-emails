-- +goose Up
-- Manifest of the attachment and inline parts of one email, including the ones
-- deliberately skipped. The file itself lives in "storage"; only its id is kept.
CREATE TABLE email.message_attachment (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    domain_id      bigint NOT NULL,
    message_id     bigint NOT NULL,

    -- Logical reference to storage.files; no foreign key, and that table is
    -- never read directly. NULL until the part reaches the "stored" state.
    file_id        bigint NULL,

    -- Content-ID header without the angle brackets; NULL when absent.
    content_id     text NULL,
    disposition    text NOT NULL,
    -- Display name as the MIME parser resolved it; the name sent to storage is
    -- sanitized separately and is not kept here.
    file_name      text NOT NULL,
    mime_type      text NOT NULL,
    -- Decoded size in bytes, known for a skipped part as well.
    size           bigint NOT NULL,

    -- Zero-based position among the parts of the email, assigned by the parser.
    position       integer NOT NULL,
    state          text NOT NULL DEFAULT 'pending',
    -- Why the part is not stored; set only for the "skipped" state.
    skipped_reason text NULL,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT message_attachment_message_fk
        FOREIGN KEY (domain_id, message_id)
        REFERENCES email.message (domain_id, id)
        ON DELETE CASCADE,
    -- Makes resuming an interrupted upload update the existing row instead of
    -- adding a second one for the same part.
    CONSTRAINT message_attachment_message_position_uniq
        UNIQUE (domain_id, message_id, position),
    CONSTRAINT message_attachment_disposition_valid
        CHECK (disposition IN ('attachment', 'inline')),
    CONSTRAINT message_attachment_state_valid
        CHECK (state IN ('pending', 'stored', 'skipped')),
    CONSTRAINT message_attachment_skipped_reason_valid
        CHECK (skipped_reason IN (
            'attachment_count_limit_exceeded',
            'attachment_size_limit_exceeded',
            'attachments_total_size_limit_exceeded',
            'attachment_read_error'
        )),
    CONSTRAINT message_attachment_size_valid
        CHECK (size >= 0),
    CONSTRAINT message_attachment_position_valid
        CHECK (position >= 0),
    -- The state, the file id and the skip reason must agree; no intermediate
    -- combination is storable.
    CONSTRAINT message_attachment_state_complete
        CHECK (
            (state = 'pending' AND file_id IS NULL AND skipped_reason IS NULL)
            OR (state = 'stored' AND file_id IS NOT NULL AND skipped_reason IS NULL)
            OR (state = 'skipped' AND file_id IS NULL AND skipped_reason IS NOT NULL)
        )
);

-- Bounds the retries of an email whose attachments cannot be uploaded.
ALTER TABLE email.message
    ADD COLUMN attachment_attempts integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT message_attachment_attempts_valid
        CHECK (attachment_attempts >= 0);

-- +goose Down
ALTER TABLE email.message
    DROP CONSTRAINT IF EXISTS message_attachment_attempts_valid,
    DROP COLUMN IF EXISTS attachment_attempts;

DROP TABLE IF EXISTS email.message_attachment;
