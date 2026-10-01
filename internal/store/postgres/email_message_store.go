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

const emailMessageColumns = `m.id,
    m.domain_id,
    m.thread_id,
    m.profile_id,
    m.direction,
    m.kind,
    m.state,
    m.attachment_attempts,
    m.message_id,
    m.message_id_generated,
    m.raw_sha256,
    m.mailbox,
    m.uid_validity,
    m.uid,
    m.in_reply_to,
    m.message_references,
    m.subject,
    m.body_text,
    m.body_html,
    m.bounce_original_message_id,
    m.bounce_recipient,
    m.bounce_status,
    m.bounce_diagnostic_code,
    m.sent_at,
    m.received_at,
    m.created_at,
    m.updated_at`

const emailMessageIdentityColumns = `m.id, m.thread_id, m.state`

type emailMessageStore struct {
	db Querier
}

var _ store.EmailMessageStore = (*emailMessageStore)(nil)

// NewEmailMessageStore creates a Message store over q.
func NewEmailMessageStore(q Querier) *emailMessageStore {
	return &emailMessageStore{db: q}
}

func (s *emailMessageStore) LocateByMessageID(
	ctx context.Context,
	domainID, profileID int64,
	messageID string,
) (*store.EmailMessageIdentity, error) {
	if messageID == "" {
		return nil, nil
	}

	const query = `SELECT ` + emailMessageIdentityColumns + `
FROM email.message m
WHERE m.domain_id = $1 AND m.profile_id = $2 AND m.message_id = $3`

	return locateEmailMessageIdentity(s.db.QueryRowContext(ctx, query, domainID, profileID, messageID))
}

func (s *emailMessageStore) LocateByIMAPIdentity(
	ctx context.Context,
	domainID, profileID int64,
	identity model.EmailIMAPIdentity,
) (*store.EmailMessageIdentity, error) {
	if identity.Mailbox == "" {
		return nil, nil
	}

	const query = `SELECT ` + emailMessageIdentityColumns + `
FROM email.message m
WHERE m.domain_id = $1 AND m.profile_id = $2
  AND m.mailbox = $3 AND m.uid_validity = $4 AND m.uid = $5`

	return locateEmailMessageIdentity(s.db.QueryRowContext(
		ctx, query, domainID, profileID, identity.Mailbox, int64(identity.UIDValidity), int64(identity.UID),
	))
}

func (s *emailMessageStore) LocateByRawChecksum(
	ctx context.Context,
	domainID, profileID int64,
	checksum []byte,
) (*store.EmailMessageIdentity, error) {
	if len(checksum) == 0 {
		return nil, nil
	}

	// Only emails whose identifier had to be generated are deduplicated this way.
	const query = `SELECT ` + emailMessageIdentityColumns + `
FROM email.message m
WHERE m.domain_id = $1 AND m.profile_id = $2
  AND m.raw_sha256 = $3 AND m.message_id_generated`

	return locateEmailMessageIdentity(s.db.QueryRowContext(ctx, query, domainID, profileID, checksum))
}

// MarkReady completes a message only while no part of its manifest is pending.
// Parts are all created in the first transaction and never added later, so the
// NOT EXISTS check is exact.
func (s *emailMessageStore) MarkReady(
	ctx context.Context,
	domainID, messageID int64,
) (model.EmailMessageState, error) {
	// The state is read back in the same statement, so a message a concurrent
	// delivery has just completed is not mistaken for an unfinished one.
	const query = `
WITH completed AS (
    UPDATE email.message m SET
        state = 'ready',
        updated_at = now()
    WHERE m.domain_id = $1 AND m.id = $2 AND m.state = 'processing'
        AND NOT EXISTS (
            SELECT 1 FROM email.message_attachment a
            WHERE a.domain_id = m.domain_id AND a.message_id = m.id AND a.state = 'pending'
        )
    RETURNING m.state
)
SELECT COALESCE(
    (SELECT state FROM completed),
    (SELECT state FROM email.message WHERE domain_id = $1 AND id = $2)
)`

	var state sql.NullString
	if err := s.db.QueryRowContext(ctx, query, domainID, messageID).Scan(&state); err != nil {
		return "", ParseError(err)
	}

	return model.EmailMessageState(state.String), nil
}

func (s *emailMessageStore) MarkFailed(ctx context.Context, domainID, messageID int64) (bool, error) {
	const query = `
UPDATE email.message SET
    state = 'failed',
    updated_at = now()
WHERE domain_id = $1 AND id = $2 AND state = 'processing'`

	result, err := s.db.ExecContext(ctx, query, domainID, messageID)
	if err != nil {
		return false, ParseError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected > 0, nil
}

func (s *emailMessageStore) IncrementAttachmentAttempts(
	ctx context.Context,
	domainID, messageID int64,
) (int32, error) {
	const query = `
UPDATE email.message SET
    attachment_attempts = attachment_attempts + 1,
    updated_at = now()
WHERE domain_id = $1 AND id = $2 AND state = 'processing'
RETURNING attachment_attempts`

	var attempts int32
	if err := s.db.QueryRowContext(ctx, query, domainID, messageID).Scan(&attempts); err != nil {
		return 0, ParseError(err)
	}

	return attempts, nil
}

func (s *emailMessageStore) LastReceivedAt(ctx context.Context, domainID, profileID int64) (time.Time, error) {
	const query = `
SELECT max(m.received_at)
FROM email.message m
WHERE m.domain_id = $1 AND m.profile_id = $2 AND m.state = 'ready'`

	var received sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, domainID, profileID).Scan(&received); err != nil {
		return time.Time{}, ParseError(err)
	}

	return received.Time, nil
}

func (s *emailMessageStore) Create(ctx context.Context, message *model.EmailMessage) (*model.EmailMessage, error) {
	const query = `
INSERT INTO email.message AS m (
    domain_id, thread_id, profile_id,
    direction, kind, state,
    message_id, message_id_generated, raw_sha256,
    mailbox, uid_validity, uid,
    in_reply_to, message_references, subject, body_text, body_html,
    bounce_original_message_id, bounce_recipient, bounce_status, bounce_diagnostic_code,
    sent_at, received_at
)
VALUES (
    $1, $2, $3,
    $4, $5, $6,
    $7, $8, $9,
    $10, $11, $12,
    $13, $14, $15, $16, $17,
    $18, $19, $20, $21,
    $22, $23
)
RETURNING ` + emailMessageColumns

	var (
		mailbox     any
		uidValidity any
		uid         any
	)
	if message.IMAP != nil {
		mailbox = message.IMAP.Mailbox
		uidValidity = int64(message.IMAP.UIDValidity)
		uid = int64(message.IMAP.UID)
	}

	bounce := message.Bounce
	if bounce == nil {
		bounce = new(model.EmailBounceInfo)
	}

	// An absent References header is an empty array, never NULL.
	references := message.References
	if references == nil {
		references = []string{}
	}

	created, err := scanEmailMessage(s.db.QueryRowContext(
		ctx,
		query,
		message.DomainID,
		message.ThreadID,
		message.ProfileID,
		message.Direction,
		message.Kind,
		message.State,
		message.MessageID,
		message.MessageIDGenerated,
		message.RawSHA256,
		mailbox,
		uidValidity,
		uid,
		nullableText(message.InReplyTo),
		pq.Array(references),
		message.Subject,
		message.TextBody,
		message.HTMLBody,
		nullableText(bounce.OriginalMessageID),
		nullableText(bounce.Recipient),
		nullableText(bounce.Status),
		nullableText(bounce.DiagnosticCode),
		message.SentAt,
		message.ReceivedAt,
	))
	if err != nil {
		if isConstraintViolation(err, uniqueViolation,
			"message_identity_uidx", "message_imap_identity_uidx", "message_raw_checksum_uidx") {
			return nil, store.ErrEmailMessageExists
		}

		return nil, ParseError(err)
	}

	return created, nil
}

// nullableText keeps an absent optional header NULL instead of an empty string,
// so partial indexes and checks see it as missing.
func nullableText(value string) any {
	if value == "" {
		return nil
	}

	return value
}

func locateEmailMessageIdentity(row rowScanner) (*store.EmailMessageIdentity, error) {
	var (
		identity store.EmailMessageIdentity
		state    string
	)
	if err := row.Scan(&identity.ID, &identity.ThreadID, &state); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, ParseError(err)
	}
	identity.State = model.EmailMessageState(state)

	return &identity, nil
}

func scanEmailMessage(row rowScanner) (*model.EmailMessage, error) {
	var (
		message     model.EmailMessage
		direction   string
		kind        string
		state       string
		mailbox     sql.NullString
		uidValidity sql.NullInt64
		uid         sql.NullInt64
		inReplyTo   sql.NullString
		bounceOrig  sql.NullString
		bounceRcpt  sql.NullString
		bounceState sql.NullString
		bounceDiag  sql.NullString
		sentAt      sql.NullTime
	)
	if err := row.Scan(
		&message.ID,
		&message.DomainID,
		&message.ThreadID,
		&message.ProfileID,
		&direction,
		&kind,
		&state,
		&message.AttachmentAttempts,
		&message.MessageID,
		&message.MessageIDGenerated,
		&message.RawSHA256,
		&mailbox,
		&uidValidity,
		&uid,
		&inReplyTo,
		pq.Array(&message.References),
		&message.Subject,
		&message.TextBody,
		&message.HTMLBody,
		&bounceOrig,
		&bounceRcpt,
		&bounceState,
		&bounceDiag,
		&sentAt,
		&message.ReceivedAt,
		&message.CreatedAt,
		&message.UpdatedAt,
	); err != nil {
		return nil, err
	}

	message.Direction = model.EmailMessageDirection(direction)
	message.Kind = model.EmailKind(kind)
	message.State = model.EmailMessageState(state)
	message.InReplyTo = inReplyTo.String
	message.SentAt = nullTimeValue(sentAt)
	if mailbox.Valid {
		message.IMAP = &model.EmailIMAPIdentity{
			Mailbox:     mailbox.String,
			UIDValidity: uint32(uidValidity.Int64),
			UID:         uint32(uid.Int64),
		}
	}

	bounce := model.EmailBounceInfo{
		OriginalMessageID: bounceOrig.String,
		Recipient:         bounceRcpt.String,
		Status:            bounceState.String,
		DiagnosticCode:    bounceDiag.String,
	}
	if bounce != (model.EmailBounceInfo{}) {
		message.Bounce = &bounce
	}

	return &message, nil
}
