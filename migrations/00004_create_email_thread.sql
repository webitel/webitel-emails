-- +goose Up
-- One conversation. "service" threads keep auto-replies and bounces that have no
-- known original message; they are closed on creation and never become regular.
CREATE TABLE email.thread (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    domain_id       bigint NOT NULL,
    profile_id      bigint NOT NULL,

    -- Resolved by task 7; the thread keeps the contact of its first message.
    contact_id      bigint NULL,

    kind            text NOT NULL DEFAULT 'regular',
    subject         text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'new',

    -- Advanced only when a message reaches the "ready" state.
    last_message_at timestamptz NULL,
    completed_at    timestamptz NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- Referenced by email.message; carries the profile so a Message can never
    -- join a Thread of another mailbox in the same domain.
    CONSTRAINT thread_domain_profile_id_uniq
        UNIQUE (domain_id, profile_id, id),
    -- Deleting a profile must not erase the conversation history.
    CONSTRAINT thread_profile_fk
        FOREIGN KEY (domain_id, profile_id)
        REFERENCES email.profile (domain_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT thread_kind_valid
        CHECK (kind IN ('regular', 'service')),
    CONSTRAINT thread_status_valid
        CHECK (status IN ('new', 'distributed', 'in_progress', 'processed')),
    CONSTRAINT thread_completed_at_valid
        CHECK (completed_at IS NULL OR status = 'processed'),
    CONSTRAINT thread_service_always_processed
        CHECK (kind <> 'service' OR (status = 'processed' AND completed_at IS NOT NULL))
);

CREATE INDEX thread_domain_status_last_message_idx
    ON email.thread (domain_id, status, last_message_at DESC);

CREATE INDEX thread_domain_profile_kind_status_last_message_idx
    ON email.thread (domain_id, profile_id, kind, status, last_message_at DESC);

-- Contact Timeline reads the threads of one contact.
CREATE INDEX thread_domain_contact_last_message_idx
    ON email.thread (domain_id, contact_id, last_message_at DESC)
    WHERE contact_id IS NOT NULL;

-- One email. "message_id" is the RFC822 Message-ID, generated from the IMAP
-- identity when the header is absent. Delivery columns arrive with tasks 14-15.
CREATE TABLE email.message (
    id                         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    domain_id                  bigint NOT NULL,
    thread_id                  bigint NOT NULL,
    profile_id                 bigint NOT NULL,

    direction                  text NOT NULL,
    kind                       text NOT NULL DEFAULT 'regular',
    -- Processing state of the email itself, not the thread status or SMTP delivery.
    state                      text NOT NULL DEFAULT 'processing',

    message_id                 text NOT NULL,
    message_id_generated       boolean NOT NULL DEFAULT false,
    raw_sha256                 bytea NULL,

    -- IMAP identity of an email read from a mailbox; empty for outbound-only messages.
    mailbox                    text NULL,
    uid_validity               bigint NULL,
    uid                        bigint NULL,

    in_reply_to                text NULL,
    -- Named without the reserved word "references"; keeps the header order.
    message_references         text[] NOT NULL DEFAULT '{}',
    subject                    text NOT NULL DEFAULT '',
    body_text                  text NOT NULL DEFAULT '',
    body_html                  text NOT NULL DEFAULT '',

    bounce_original_message_id text NULL,
    bounce_recipient           text NULL,
    bounce_status              text NULL,
    bounce_diagnostic_code     text NULL,

    -- Set by the sender through the Date header; never used for ordering.
    sent_at                    timestamptz NULL,
    -- IMAP INTERNALDATE or the receive time; orders the thread.
    received_at                timestamptz NOT NULL,

    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),

    -- Referenced by the composite foreign key of email.recipient.
    CONSTRAINT message_domain_id_uniq
        UNIQUE (domain_id, id),
    CONSTRAINT message_thread_fk
        FOREIGN KEY (domain_id, profile_id, thread_id)
        REFERENCES email.thread (domain_id, profile_id, id)
        ON DELETE CASCADE,
    CONSTRAINT message_profile_fk
        FOREIGN KEY (domain_id, profile_id)
        REFERENCES email.profile (domain_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT message_direction_valid
        CHECK (direction IN ('inbound', 'outbound')),
    CONSTRAINT message_kind_valid
        CHECK (kind IN ('regular', 'auto_reply', 'bounce')),
    CONSTRAINT message_state_valid
        CHECK (state IN ('processing', 'ready', 'failed')),
    CONSTRAINT message_message_id_not_empty
        CHECK (trim(message_id) <> ''),
    CONSTRAINT message_raw_sha256_valid
        CHECK (raw_sha256 IS NULL OR octet_length(raw_sha256) = 32),
    -- A generated Message-ID is deduplicated by the raw checksum, so it must exist.
    CONSTRAINT message_generated_id_has_checksum
        CHECK (NOT message_id_generated OR raw_sha256 IS NOT NULL),
    CONSTRAINT message_imap_identity_complete
        CHECK (
            (mailbox IS NULL AND uid_validity IS NULL AND uid IS NULL)
            OR (mailbox IS NOT NULL AND uid_validity IS NOT NULL AND uid IS NOT NULL)
        ),
    CONSTRAINT message_uid_validity_valid
        CHECK (uid_validity IS NULL OR uid_validity BETWEEN 1 AND 4294967295),
    CONSTRAINT message_uid_valid
        CHECK (uid IS NULL OR uid BETWEEN 1 AND 4294967295),
    CONSTRAINT message_bounce_data_requires_bounce_kind
        CHECK (
            kind = 'bounce'
            OR (
                bounce_original_message_id IS NULL
                AND bounce_recipient IS NULL
                AND bounce_status IS NULL
                AND bounce_diagnostic_code IS NULL
            )
        )
);

-- Deduplication by the mail identifier, in both directions: a copy of our own
-- outbound email returning to the mailbox is the same logical message.
CREATE UNIQUE INDEX message_identity_uidx
    ON email.message (domain_id, profile_id, message_id);

-- Deduplication of an ordinary redelivery at the same IMAP cursor.
CREATE UNIQUE INDEX message_imap_identity_uidx
    ON email.message (domain_id, profile_id, mailbox, uid_validity, uid)
    WHERE mailbox IS NOT NULL;

-- Deduplication of an email without a Message-ID after UIDVALIDITY has changed.
CREATE UNIQUE INDEX message_raw_checksum_uidx
    ON email.message (domain_id, profile_id, raw_sha256)
    WHERE message_id_generated;

CREATE INDEX message_thread_received_idx
    ON email.message (domain_id, thread_id, received_at, id)
    WHERE state = 'ready';

-- Sibling and reverse-sibling threading.
CREATE INDEX message_in_reply_to_idx
    ON email.message (domain_id, profile_id, in_reply_to)
    WHERE in_reply_to IS NOT NULL;

-- Participants of one message, keeping the type and the original order.
CREATE TABLE email.recipient (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    domain_id          bigint NOT NULL,
    message_id         bigint NOT NULL,

    type               text NOT NULL,
    address            text NOT NULL,
    -- Lower-cased address used for lookups; the original stays in "address".
    normalized_address text NOT NULL,
    display_name       text NOT NULL DEFAULT '',
    -- Zero-based position within its own type list.
    ordinal            integer NOT NULL,

    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT recipient_message_fk
        FOREIGN KEY (domain_id, message_id)
        REFERENCES email.message (domain_id, id)
        ON DELETE CASCADE,
    CONSTRAINT recipient_type_valid
        CHECK (type IN ('from', 'sender', 'reply_to', 'to', 'cc', 'bcc')),
    CONSTRAINT recipient_address_not_empty
        CHECK (trim(address) <> ''),
    CONSTRAINT recipient_normalized_address_not_empty
        CHECK (trim(normalized_address) <> ''),
    CONSTRAINT recipient_ordinal_valid
        CHECK (ordinal >= 0),
    -- Makes rewriting the recipients of one message idempotent.
    CONSTRAINT recipient_message_type_ordinal_uniq
        UNIQUE (domain_id, message_id, type, ordinal)
);

-- Contact lookup by email address in task 7.
CREATE INDEX recipient_domain_normalized_address_idx
    ON email.recipient (domain_id, normalized_address, type);

-- +goose Down
DROP TABLE IF EXISTS email.recipient;
DROP TABLE IF EXISTS email.message;
DROP TABLE IF EXISTS email.thread;
