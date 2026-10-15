-- +goose Up
-- Transactional Outbox, written with the change it describes and published by the
-- leader. No foreign keys: the payload is self-contained and outlives its sources.
CREATE TABLE email.outbox (
    event_id        uuid PRIMARY KEY,
    domain_id       bigint NOT NULL,
    event_type      text NOT NULL,
    -- Sent as a header, so the relay never decodes the payload to learn it.
    schema_version  integer NOT NULL,
    -- Makes a repeated write of the same business event a no-op; NULL for event
    -- types that may legitimately repeat.
    dedup_key       text NULL,

    exchange        text NOT NULL,
    routing_key     text NOT NULL,
    -- Kept as written, so the relay publishes exactly what the writer validated.
    payload         json NOT NULL,

    state           text NOT NULL DEFAULT 'pending',
    -- Counts only failures caused by this row, never broker-wide outages.
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    -- Lease of the relay instance publishing the row; set only while processing.
    locked_by       text NULL,
    locked_until    timestamptz NULL,
    published_at    timestamptz NULL,
    last_error      text NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- The relay could not route a row missing any of these.
    CONSTRAINT outbox_envelope_complete
        CHECK (domain_id > 0 AND event_type <> '' AND exchange <> '' AND routing_key <> ''),
    CONSTRAINT outbox_state_valid
        CHECK (state IN ('pending', 'processing', 'published', 'failed')),
    CONSTRAINT outbox_schema_version_valid
        CHECK (schema_version > 0),
    CONSTRAINT outbox_attempts_valid
        CHECK (attempts >= 0),
    CONSTRAINT outbox_dedup_key_not_empty
        CHECK (dedup_key IS NULL OR dedup_key <> ''),
    CONSTRAINT outbox_last_error_length
        CHECK (last_error IS NULL OR char_length(last_error) <= 1024),
    -- A lease exists exactly while a relay holds the row.
    CONSTRAINT outbox_lease_complete
        CHECK (
            (state = 'processing' AND locked_by IS NOT NULL AND locked_until IS NOT NULL)
            OR (state <> 'processing' AND locked_by IS NULL AND locked_until IS NULL)
        ),
    CONSTRAINT outbox_published_complete
        CHECK ((state = 'published') = (published_at IS NOT NULL)),
    -- A row given up on must say why.
    CONSTRAINT outbox_failed_has_error
        CHECK (state <> 'failed' OR last_error IS NOT NULL)
);

-- One business event per key and type; other types keep their own keys.
CREATE UNIQUE INDEX outbox_event_type_dedup_key_uidx
    ON email.outbox (event_type, dedup_key)
    WHERE dedup_key IS NOT NULL;

-- Rows due for publication, in the order the relay reads them; a retried row
-- moves behind fresh ones through its next_attempt_at.
CREATE INDEX outbox_pending_idx
    ON email.outbox (next_attempt_at, created_at, event_id)
    WHERE state = 'pending';

-- Leases of a relay that stopped without releasing them.
CREATE INDEX outbox_processing_lease_idx
    ON email.outbox (locked_until)
    WHERE state = 'processing';

-- Rows given up on, counted by the relay's backlog report.
CREATE INDEX outbox_failed_idx
    ON email.outbox (created_at)
    WHERE state = 'failed';

-- Cleanup of published rows after the retention period.
CREATE INDEX outbox_published_at_idx
    ON email.outbox (published_at)
    WHERE state = 'published';

-- +goose Down
DROP TABLE IF EXISTS email.outbox;
