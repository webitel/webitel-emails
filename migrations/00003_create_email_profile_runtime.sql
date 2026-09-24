-- +goose Up
-- Keeps a runtime row in the same domain as its profile.
CREATE UNIQUE INDEX profile_domain_id_uidx ON email.profile (domain_id, id);

CREATE TABLE email.profile_runtime (
    profile_id            bigint PRIMARY KEY,
    domain_id             bigint NOT NULL,

    -- NULL when the profile is not assigned.
    owner_instance_id     text NULL,
    -- Fencing token, incremented on every owner change.
    assignment_generation bigint NOT NULL DEFAULT 0,

    next_check_at         timestamptz NULL,
    -- e.g. {"imap": {"mailbox": "INBOX", "uid_validity": 1, "last_uid": 42}}
    provider_cursor       jsonb NULL,

    last_poll_started_at  timestamptz NULL,
    last_poll_finished_at timestamptz NULL,
    last_poll_error       text NULL,

    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT profile_runtime_profile_fk
        FOREIGN KEY (domain_id, profile_id)
        REFERENCES email.profile (domain_id, id)
        ON DELETE CASCADE,
    CONSTRAINT profile_runtime_owner_not_empty
        CHECK (owner_instance_id IS NULL OR trim(owner_instance_id) <> ''),
    CONSTRAINT profile_runtime_generation_valid
        CHECK (assignment_generation >= 0),
    CONSTRAINT profile_runtime_provider_cursor_object
        CHECK (provider_cursor IS NULL OR jsonb_typeof(provider_cursor) = 'object')
);

CREATE INDEX profile_runtime_owner_next_check_idx
    ON email.profile_runtime (owner_instance_id, next_check_at)
    WHERE owner_instance_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS email.profile_runtime;
DROP INDEX IF EXISTS email.profile_domain_id_uidx;
