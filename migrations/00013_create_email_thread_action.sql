-- +goose Up
-- Every transition of a Thread from the call_center snapshots: the Email history
-- of the Thread. Rows are only added, once per projection_version; retention may
-- delete old ones.
CREATE TABLE email.thread_action (
    domain_id            bigint      NOT NULL,
    thread_id            bigint      NOT NULL,
    projection_version   bigint      NOT NULL,
    occurred_at          timestamptz NOT NULL,
    transition           text        NOT NULL,
    source               text        NOT NULL,
    -- Attempt fields are NULL for the inbound_reopened marker.
    attempt_id           bigint      NULL,
    attempt_state        text        NULL,
    result               text        NULL,
    queue_id             bigint      NULL,
    agent_id             bigint      NULL,
    agent_user_id        bigint      NULL,
    -- Set by engine commands only.
    actor_user_id        bigint      NULL,
    transfer_to_queue_id bigint      NULL,
    transfer_to_user_id  bigint      NULL,
    -- Plain text of the operator, never HTML.
    comment              text        NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT thread_action_pkey
        PRIMARY KEY (domain_id, thread_id, projection_version),
    CONSTRAINT thread_action_thread_fk
        FOREIGN KEY (domain_id, thread_id)
        REFERENCES email.thread (domain_id, id)
        ON DELETE CASCADE,
    CONSTRAINT thread_action_ids_valid
        CHECK (domain_id > 0 AND thread_id > 0 AND projection_version > 0),
    CONSTRAINT thread_action_comment_valid
        CHECK (comment IS NULL OR char_length(comment) BETWEEN 1 AND 500)
);

-- Retention deletes the oldest actions first.
CREATE INDEX thread_action_occurred_at_idx ON email.thread_action (occurred_at);

-- +goose Down
DROP TABLE IF EXISTS email.thread_action;
