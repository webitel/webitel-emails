package postgres

import (
	"context"
	"database/sql"
	stderrors "errors"
	"time"

	"github.com/lib/pq"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const emailThreadColumns = `t.id,
    t.domain_id,
    t.profile_id,
    t.contact_id,
    t.contact_resolution_state,
    t.kind,
    t.subject,
    t.status,
    t.last_message_at,
    t.completed_at,
    t.created_at,
    t.updated_at`

// Resolves the Thread of a stored message; the join keeps both sides in one domain.
const emailThreadByMessage = `SELECT ` + emailThreadColumns + `
FROM email.thread t
JOIN email.message m
    ON m.domain_id = t.domain_id AND m.profile_id = t.profile_id AND m.thread_id = t.id
WHERE t.domain_id = $1 AND t.profile_id = $2 AND (NOT $4 OR t.kind = 'regular')`

type emailThreadStore struct {
	db Querier
}

var _ store.EmailThreadStore = (*emailThreadStore)(nil)

// NewEmailThreadStore creates a Thread store over q.
func NewEmailThreadStore(q Querier) *emailThreadStore {
	return &emailThreadStore{db: q}
}

func (s *emailThreadStore) Create(ctx context.Context, thread *model.EmailThread) (*model.EmailThread, error) {
	const query = `
INSERT INTO email.thread AS t (
    domain_id, profile_id, contact_id, contact_resolution_state,
    kind, subject, status, last_message_at, completed_at
)
-- An unset resolution state falls back to the one the kind requires, so a caller
-- that does not resolve contacts cannot create a Thread the schema refuses.
VALUES (
    $1, $2, $3,
    COALESCE(
        NULLIF($4, ''),
        CASE WHEN $5 = 'service' THEN 'not_applicable' ELSE 'pending' END
    ),
    $5, $6, $7, $8, $9
)
RETURNING ` + emailThreadColumns

	created, err := scanEmailThread(s.db.QueryRowContext(
		ctx,
		query,
		thread.DomainID,
		thread.ProfileID,
		thread.ContactID,
		string(thread.ContactResolutionState),
		thread.Kind,
		thread.Subject,
		thread.Status,
		thread.LastMessageAt,
		thread.CompletedAt,
	))
	if err != nil {
		return nil, ParseError(err)
	}

	return created, nil
}

func (s *emailThreadStore) LocateByMessageIDs(
	ctx context.Context,
	lookup store.EmailThreadLookup,
	messageIDs []string,
) (*model.EmailThread, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	// One statement for the whole ancestor chain: the match at the highest
	// position wins, which is the most specific identifier the caller listed.
	const query = emailThreadByMessage + ` AND m.message_id = ANY($3::text[])
ORDER BY array_position($3::text[], m.message_id) DESC
LIMIT 1`

	return locateEmailThread(s.db.QueryRowContext(
		ctx, query, lookup.DomainID, lookup.ProfileID, pq.Array(messageIDs), lookup.OnlyRegular,
	))
}

func (s *emailThreadStore) LocateByInReplyTo(
	ctx context.Context,
	lookup store.EmailThreadLookup,
	inReplyTo string,
) (*model.EmailThread, error) {
	if inReplyTo == "" {
		return nil, nil
	}

	// Several messages may reply to the same parent. A regular Thread is always
	// preferred, so a service one only wins when nothing else matches.
	const query = emailThreadByMessage + ` AND m.in_reply_to = $3
ORDER BY (t.kind <> 'regular'), m.received_at, m.id
LIMIT 1`

	return locateEmailThread(s.db.QueryRowContext(
		ctx, query, lookup.DomainID, lookup.ProfileID, inReplyTo, lookup.OnlyRegular,
	))
}

// ResolveContact writes the resolution outcome under the states that still allow
// it, so a late automatic answer cannot replace what an operator chose.
func (s *emailThreadStore) ResolveContact(
	ctx context.Context,
	domainID, threadID int64,
	state model.EmailContactResolutionState,
	contactID *int64,
) (bool, error) {
	const query = `
UPDATE email.thread SET
    contact_id = $4,
    contact_resolution_state = $5,
    updated_at = now()
WHERE domain_id = $1 AND id = $2 AND kind = $3
    AND contact_resolution_state IN ('pending', 'unavailable')`

	result, err := s.db.ExecContext(
		ctx,
		query,
		domainID,
		threadID,
		string(model.EmailThreadKindRegular),
		nullableInt64(contactID),
		string(state),
	)
	if err != nil {
		return false, ParseError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected > 0, nil
}

// SetContactManually is not limited to the states automatic resolution may write:
// an operator may correct a resolved Thread as well. The regular kind is still
// required, so an auto-reply or a bounce never receives a contact.
func (s *emailThreadStore) SetContactManually(
	ctx context.Context,
	domainID, threadID int64,
	contactID *int64,
) (bool, error) {
	const query = `
UPDATE email.thread SET
    contact_id = $4,
    contact_resolution_state = CASE WHEN $4::bigint IS NULL THEN 'unlinked' ELSE 'resolved' END,
    updated_at = now()
WHERE domain_id = $1 AND id = $2 AND kind = $3`

	result, err := s.db.ExecContext(
		ctx,
		query,
		domainID,
		threadID,
		string(model.EmailThreadKindRegular),
		nullableInt64(contactID),
	)
	if err != nil {
		return false, ParseError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected > 0, nil
}

// FirstSenderAddress takes the sender of the earliest message of the Thread by
// identity order rather than by received_at: the stored order is what decides
// which email opened the conversation, whatever dates the headers carry. That
// message is chosen first and only then asked for its sender, so a later email
// can never stand in for it; the message may still be processing, which is the
// case right after a crash.
func (s *emailThreadStore) FirstSenderAddress(
	ctx context.Context,
	domainID, threadID int64,
) (string, error) {
	const query = `
WITH first_message AS (
    SELECT m.domain_id, m.id
    FROM email.message m
    WHERE m.domain_id = $1 AND m.thread_id = $2
    ORDER BY m.id
    LIMIT 1
)
SELECT r.normalized_address
FROM first_message f
LEFT JOIN email.recipient r
    ON r.domain_id = f.domain_id AND r.message_id = f.id
    AND r.type = 'from' AND r.ordinal = 0`

	var address sql.NullString
	if err := s.db.QueryRowContext(ctx, query, domainID, threadID).Scan(&address); err != nil {
		// No rows at all means the Thread holds no message yet; an absent sender
		// comes back as NULL. Both leave the Thread without an address to search.
		if stderrors.Is(err, sql.ErrNoRows) {
			return "", nil
		}

		return "", ParseError(err)
	}

	return address.String, nil
}

func (s *emailThreadStore) AdvanceLastMessage(
	ctx context.Context,
	domainID, profileID, threadID int64,
	at time.Time,
) error {
	// Monotonic: a late or out-of-order email never moves the Thread backwards.
	const query = `
UPDATE email.thread
SET last_message_at = $4, updated_at = now()
WHERE domain_id = $1 AND profile_id = $2 AND id = $3
  AND (last_message_at IS NULL OR last_message_at < $4)`

	_, err := s.db.ExecContext(ctx, query, domainID, profileID, threadID, at)

	return ParseError(err)
}

// locateEmailThread turns "no rows" into a nil Thread, because a threading rule
// that does not match is an ordinary outcome, not a failure.
func locateEmailThread(row rowScanner) (*model.EmailThread, error) {
	thread, err := scanEmailThread(row)
	if err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, ParseError(err)
	}

	return thread, nil
}

func scanEmailThread(row rowScanner) (*model.EmailThread, error) {
	var (
		thread          model.EmailThread
		contactID       sql.NullInt64
		resolutionState string
		kind            string
		status          string
		lastMessageAt   sql.NullTime
		completedAt     sql.NullTime
	)
	if err := row.Scan(
		&thread.ID,
		&thread.DomainID,
		&thread.ProfileID,
		&contactID,
		&resolutionState,
		&kind,
		&thread.Subject,
		&status,
		&lastMessageAt,
		&completedAt,
		&thread.CreatedAt,
		&thread.UpdatedAt,
	); err != nil {
		return nil, err
	}

	thread.ContactResolutionState = model.EmailContactResolutionState(resolutionState)
	thread.Kind = model.EmailThreadKind(kind)
	thread.Status = model.EmailThreadStatus(status)
	thread.LastMessageAt = nullTimeValue(lastMessageAt)
	thread.CompletedAt = nullTimeValue(completedAt)
	if contactID.Valid {
		thread.ContactID = &contactID.Int64
	}

	return &thread, nil
}
