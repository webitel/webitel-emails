-- +goose Up
-- The index of 00010 becomes the constraint the participant foreign key needs.
ALTER TABLE email.thread
    ADD CONSTRAINT thread_domain_id_uniq UNIQUE USING INDEX thread_domain_id_uidx;

-- Users who took a Thread into work: with the current agent, the only read access.
-- Rows are only added, by the snapshot consumer or a one-off backfill.
CREATE TABLE email.thread_participant (
    domain_id             bigint NOT NULL,
    thread_id             bigint NOT NULL,
    user_id               bigint NOT NULL,
    -- Agent of the newest known participation.
    agent_id              bigint NOT NULL,

    -- projection_version of the snapshots that recorded the participation;
    -- NULL only for a backfilled row until a snapshot refines it.
    first_version         bigint NULL,
    last_version          bigint NULL,
    first_participated_at timestamptz NULL,
    last_participated_at  timestamptz NULL,
    -- Set when the row came from the backfill runbook; kept as its origin.
    backfilled_at         timestamptz NULL,

    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT thread_participant_pkey
        PRIMARY KEY (domain_id, thread_id, user_id),
    CONSTRAINT thread_participant_thread_fk
        FOREIGN KEY (domain_id, thread_id)
        REFERENCES email.thread (domain_id, id)
        ON DELETE CASCADE,
    CONSTRAINT thread_participant_ids_valid
        CHECK (domain_id > 0 AND thread_id > 0 AND user_id > 0 AND agent_id > 0),
    CONSTRAINT thread_participant_versions_valid
        CHECK (
            (first_version IS NULL) = (last_version IS NULL)
            AND (first_version IS NULL OR (first_version > 0 AND first_version <= last_version))
        ),
    CONSTRAINT thread_participant_times_valid
        CHECK (
            (first_participated_at IS NULL) = (last_participated_at IS NULL)
            AND (first_participated_at IS NULL OR first_participated_at <= last_participated_at)
        ),
    -- A row written from a snapshot always has a real version and time.
    CONSTRAINT thread_participant_origin_valid
        CHECK (backfilled_at IS NOT NULL OR (first_version IS NOT NULL AND first_participated_at IS NOT NULL))
);

-- Threads of one user, for the history view of the read API.
CREATE INDEX thread_participant_domain_user_thread_idx
    ON email.thread_participant (domain_id, user_id, thread_id);

-- +goose Down
DROP TABLE IF EXISTS email.thread_participant;
-- Dropping the constraint drops its index too.
ALTER TABLE email.thread DROP CONSTRAINT IF EXISTS thread_domain_id_uniq;
