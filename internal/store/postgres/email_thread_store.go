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
    domain_id, profile_id, contact_id, kind, subject, status, last_message_at, completed_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING ` + emailThreadColumns

	created, err := scanEmailThread(s.db.QueryRowContext(
		ctx,
		query,
		thread.DomainID,
		thread.ProfileID,
		thread.ContactID,
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
		thread        model.EmailThread
		contactID     sql.NullInt64
		kind          string
		status        string
		lastMessageAt sql.NullTime
		completedAt   sql.NullTime
	)
	if err := row.Scan(
		&thread.ID,
		&thread.DomainID,
		&thread.ProfileID,
		&contactID,
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

	thread.Kind = model.EmailThreadKind(kind)
	thread.Status = model.EmailThreadStatus(status)
	thread.LastMessageAt = nullTimeValue(lastMessageAt)
	thread.CompletedAt = nullTimeValue(completedAt)
	if contactID.Valid {
		thread.ContactID = &contactID.Int64
	}

	return &thread, nil
}
