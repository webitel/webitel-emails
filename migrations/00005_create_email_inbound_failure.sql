-- +goose Up
-- An email that fails the same way on every retry. The raw message stays in the
-- mailbox, so only the coordinates needed to find it again are kept here.
CREATE TABLE email.inbound_failure (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    domain_id    bigint NOT NULL,
    profile_id   bigint NOT NULL,

    mailbox      text NOT NULL,
    uid_validity bigint NOT NULL,
    uid          bigint NOT NULL,

    category     text NOT NULL,
    reason       text NOT NULL DEFAULT '',
    -- Server-reported RFC822.SIZE; absent when the fetch never got that far.
    message_size bigint NULL,

    -- Whatever could be read from the headers, for diagnosis.
    message_id   text NULL,
    sender       text NULL,
    subject      text NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Operational rows must not hold a profile that has no conversation history.
    CONSTRAINT inbound_failure_profile_fk
        FOREIGN KEY (domain_id, profile_id)
        REFERENCES email.profile (domain_id, id)
        ON DELETE CASCADE,
    CONSTRAINT inbound_failure_category_valid
        CHECK (category IN ('mime_parse', 'message_too_large', 'attachment_upload')),
    CONSTRAINT inbound_failure_mailbox_not_empty
        CHECK (trim(mailbox) <> ''),
    CONSTRAINT inbound_failure_uid_validity_valid
        CHECK (uid_validity BETWEEN 1 AND 4294967295),
    CONSTRAINT inbound_failure_uid_valid
        CHECK (uid BETWEEN 1 AND 4294967295),
    CONSTRAINT inbound_failure_message_size_valid
        CHECK (message_size IS NULL OR message_size >= 0),
    -- A redelivery of the same broken email updates its row instead of adding one.
    CONSTRAINT inbound_failure_identity_uniq
        UNIQUE (domain_id, profile_id, mailbox, uid_validity, uid)
);

CREATE INDEX inbound_failure_profile_created_idx
    ON email.inbound_failure (domain_id, profile_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS email.inbound_failure;
