-- +goose Up
-- Tells apart the reasons a Thread has no contact: never searched, nothing found,
-- several matches, Contacts unreachable, or unlinked by an operator. Automatic
-- resolution may only write over "pending" and "unavailable".
-- The column arrives nullable and without constraints, so existing rows can be
-- given a state that agrees with what they already hold.
ALTER TABLE email.thread
    ADD COLUMN contact_resolution_state text;

-- Auto-replies and bounces never resolve a contact; a conversation that already
-- carries one is resolved by definition. Everything else is still to be searched.
UPDATE email.thread
SET contact_resolution_state = CASE
        WHEN kind = 'service' THEN 'not_applicable'
        WHEN contact_id IS NOT NULL THEN 'resolved'
        ELSE 'pending'
    END,
    -- A service Thread carries no contact under the new rule, and nothing can
    -- read or bind one on it any more, so a value left from before is dropped.
    contact_id = CASE WHEN kind = 'service' THEN NULL ELSE contact_id END;

ALTER TABLE email.thread
    ALTER COLUMN contact_resolution_state SET NOT NULL,
    ALTER COLUMN contact_resolution_state SET DEFAULT 'pending',
    ADD CONSTRAINT thread_contact_resolution_state_valid
        CHECK (contact_resolution_state IN (
            'pending',
            'resolved',
            'not_found',
            'ambiguous',
            'unavailable',
            'unlinked',
            'not_applicable'
        )),
    -- Only a resolved Thread holds a contact; the database must not store any
    -- other combination.
    ADD CONSTRAINT thread_contact_resolution_complete
        CHECK (
            (contact_resolution_state = 'resolved' AND contact_id IS NOT NULL)
            OR (contact_resolution_state <> 'resolved' AND contact_id IS NULL)
        ),
    -- The kind decides whether a contact is looked for at all, and it is the only
    -- kind that may say so: a regular Thread marked "not_applicable" would
    -- silently drop out of resolution with nothing recording why.
    ADD CONSTRAINT thread_contact_resolution_kind_valid
        CHECK ((kind = 'service') = (contact_resolution_state = 'not_applicable'));

-- +goose Down
ALTER TABLE email.thread
    DROP CONSTRAINT IF EXISTS thread_contact_resolution_kind_valid,
    DROP CONSTRAINT IF EXISTS thread_contact_resolution_complete,
    DROP CONSTRAINT IF EXISTS thread_contact_resolution_state_valid,
    DROP COLUMN IF EXISTS contact_resolution_state;
