-- +goose Up
-- Projection of the Email attempts of call_center onto the Thread. Only the
-- consumer of cc.email.attempt.snapshot writes these columns; existing rows keep
-- them NULL, so the constraints below need no backfill.
ALTER TABLE email.thread
    ADD COLUMN queue_id                bigint NULL,
    ADD COLUMN current_attempt_id      bigint NULL,
    ADD COLUMN current_attempt_version bigint NULL,
    ADD COLUMN current_agent_id        bigint NULL,
    ADD COLUMN current_user_id         bigint NULL,
    ADD COLUMN postponed_until         timestamptz NULL,
    -- The last agent who finished an attempt, ordered by its own version.
    ADD COLUMN last_handled_agent_id   bigint NULL,
    ADD COLUMN last_handled_user_id    bigint NULL,
    ADD COLUMN last_handled_version    bigint NULL,
    -- Sequence of the last applied snapshot; NULL until the first one.
    ADD COLUMN projection_version      bigint NULL,
    ADD COLUMN status_changed_at       timestamptz NULL;

ALTER TABLE email.thread
    DROP CONSTRAINT thread_status_valid,
    ADD CONSTRAINT thread_status_valid
        CHECK (status IN ('new', 'distributed', 'in_progress', 'postponed', 'processed')),
    ADD CONSTRAINT thread_postponed_valid
        CHECK ((status = 'postponed') = (postponed_until IS NOT NULL)),
    -- An agent always comes with its user, and a Thread waiting in a queue or
    -- closed has no current agent.
    ADD CONSTRAINT thread_current_agent_valid
        CHECK (
            (current_agent_id IS NULL) = (current_user_id IS NULL)
            AND (current_agent_id IS NULL OR status NOT IN ('new', 'processed'))
        ),
    ADD CONSTRAINT thread_current_attempt_valid
        CHECK ((current_attempt_id IS NULL) = (current_attempt_version IS NULL)),
    ADD CONSTRAINT thread_last_handled_valid
        CHECK (
            (last_handled_agent_id IS NULL) = (last_handled_user_id IS NULL)
            AND (last_handled_agent_id IS NULL) = (last_handled_version IS NULL)
        ),
    ADD CONSTRAINT thread_projection_ids_valid
        CHECK (
            (queue_id IS NULL OR queue_id > 0)
            AND (current_attempt_id IS NULL OR current_attempt_id > 0)
            AND (current_attempt_version IS NULL OR current_attempt_version >= 0)
            AND (current_agent_id IS NULL OR current_agent_id > 0)
            AND (current_user_id IS NULL OR current_user_id > 0)
            AND (last_handled_agent_id IS NULL OR last_handled_agent_id > 0)
            AND (last_handled_user_id IS NULL OR last_handled_user_id > 0)
            AND (last_handled_version IS NULL OR last_handled_version > 0)
            AND (projection_version IS NULL OR projection_version > 0)
        ),
    -- A service Thread never reaches a queue, so nothing projects onto it.
    ADD CONSTRAINT thread_service_not_projected
        CHECK (
            kind <> 'service'
            OR (
                queue_id IS NULL
                AND current_attempt_id IS NULL
                AND current_attempt_version IS NULL
                AND current_agent_id IS NULL
                AND current_user_id IS NULL
                AND postponed_until IS NULL
                AND last_handled_agent_id IS NULL
                AND last_handled_user_id IS NULL
                AND last_handled_version IS NULL
                AND projection_version IS NULL
                AND status_changed_at IS NULL
            )
        );

-- +goose Down
-- Fails while a Thread is postponed: the old status set has no place for it.
ALTER TABLE email.thread
    DROP CONSTRAINT IF EXISTS thread_service_not_projected,
    DROP CONSTRAINT IF EXISTS thread_projection_ids_valid,
    DROP CONSTRAINT IF EXISTS thread_last_handled_valid,
    DROP CONSTRAINT IF EXISTS thread_current_attempt_valid,
    DROP CONSTRAINT IF EXISTS thread_current_agent_valid,
    DROP CONSTRAINT IF EXISTS thread_postponed_valid,
    DROP CONSTRAINT thread_status_valid,
    ADD CONSTRAINT thread_status_valid
        CHECK (status IN ('new', 'distributed', 'in_progress', 'processed')),
    DROP COLUMN IF EXISTS status_changed_at,
    DROP COLUMN IF EXISTS projection_version,
    DROP COLUMN IF EXISTS last_handled_version,
    DROP COLUMN IF EXISTS last_handled_user_id,
    DROP COLUMN IF EXISTS last_handled_agent_id,
    DROP COLUMN IF EXISTS postponed_until,
    DROP COLUMN IF EXISTS current_user_id,
    DROP COLUMN IF EXISTS current_agent_id,
    DROP COLUMN IF EXISTS current_attempt_version,
    DROP COLUMN IF EXISTS current_attempt_id,
    DROP COLUMN IF EXISTS queue_id;
