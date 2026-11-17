package postgres

import (
	"context"
	stderrors "errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailOutboundStore struct {
	db Querier
}

var _ store.EmailOutboundStore = (*emailOutboundStore)(nil)

// NewEmailOutboundStore creates the store of outbound emails over q.
func NewEmailOutboundStore(q Querier) *emailOutboundStore {
	return &emailOutboundStore{db: q}
}

// LockRequest uses the two-key advisory space, which never meets the one-key
// profile locks; a hash collision only serializes two requests.
func (s *emailOutboundStore) LockRequest(ctx context.Context, domainID, authorID int64, requestID uuid.UUID) error {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strconv.FormatInt(domainID, 10) + ":" + strconv.FormatInt(authorID, 10) + ":"))
	_, _ = h.Write(requestID[:])

	if _, err := s.db.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int4, $2::int4)`,
		int32(emailProfileLockNamespace), int32(h.Sum32())); err != nil {
		return fmt.Errorf("store: lock outbound request: %w", err)
	}

	return nil
}

func (s *emailOutboundStore) LocateRequest(
	ctx context.Context,
	domainID, authorID int64,
	requestID uuid.UUID,
) (*store.EmailOutboundRequest, error) {
	var r store.EmailOutboundRequest
	err := s.db.QueryRow(ctx, `
SELECT id, request_fingerprint FROM email.message
WHERE domain_id = $1 AND author_user_id = $2 AND request_id = $3`,
		domainID, authorID, requestID).Scan(&r.MessageID, &r.Fingerprint)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: locate outbound request: %w", ParseError(err))
	}

	return &r, nil
}

// LockReplyThread locks first and reads in a second statement, so the state and
// the journal come from one snapshot taken after the lock.
func (s *emailOutboundStore) LockReplyThread(
	ctx context.Context,
	domainID, userID, threadID int64,
) (*store.EmailReplyThread, error) {
	_, err := s.db.Exec(ctx, `SELECT 1 FROM email.thread t WHERE t.domain_id = $1 AND t.id = $2 FOR NO KEY UPDATE OF t`,
		domainID, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: lock reply thread: %w", ParseError(err))
	}

	var t store.EmailReplyThread
	err = s.db.QueryRow(ctx, `
SELECT t.profile_id, t.subject, t.status, t.current_user_id, t.current_attempt_id,
    t.current_attempt_version, `+threadParticipant+`
FROM email.thread t
WHERE t.id = @thread_id AND `+visibleThread(model.EmailThreadAssignmentAll), pgx.NamedArgs{
		"domain_id": domainID,
		"user_id":   userID,
		"thread_id": threadID,
	}).Scan(&t.ProfileID, &t.Subject, &t.Status, &t.CurrentUserID, &t.CurrentAttemptID,
		&t.CurrentAttemptVersion, &t.Participant)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read reply thread: %w", ParseError(err))
	}

	return &t, nil
}

func (s *emailOutboundStore) SenderProfile(ctx context.Context, domainID, profileID int64) (*model.EmailProfile, error) {
	return NewEmailProfileStore(s.db).Locate(ctx, domainID, profileID)
}

// LockUploads locks in reference order, so requests sharing files cannot deadlock;
// a row used meanwhile is rechecked after the wait and left out.
func (s *emailOutboundStore) LockUploads(
	ctx context.Context,
	domainID, authorID int64,
	references []uuid.UUID,
) ([]*model.EmailAttachmentUpload, error) {
	if len(references) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT domain_id, reference, author_user_id, file_id, file_name, mime_type, size, expires_at
FROM email.attachment_upload
WHERE domain_id = $1 AND author_user_id = $2 AND reference = ANY($3) AND used_by_message_id IS NULL
ORDER BY reference
FOR UPDATE`, domainID, authorID, references)
	if err != nil {
		return nil, fmt.Errorf("store: lock attachment uploads: %w", ParseError(err))
	}

	uploads, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*model.EmailAttachmentUpload, error) {
		var u model.EmailAttachmentUpload
		err := row.Scan(&u.DomainID, &u.Reference, &u.AuthorUserID, &u.FileID, &u.FileName, &u.MimeType, &u.Size, &u.ExpiresAt)

		return &u, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: lock attachment uploads: %w", ParseError(err))
	}

	return uploads, nil
}

func (s *emailOutboundStore) ReplyParent(
	ctx context.Context,
	domainID, threadID, messageID int64,
) (*store.EmailReplyParent, error) {
	const fits = `m.domain_id = @domain_id AND m.thread_id = @thread_id AND m.state = 'ready' AND m.kind = 'regular'`
	query := `SELECT m.message_id, m.message_references FROM email.message m
WHERE ` + fits + ` AND m.id = @message_id
    AND (m.direction = 'inbound' OR m.delivery_status = 'sent')`
	if messageID == 0 {
		query = `
SELECT message_id, message_references FROM (
    (SELECT m.message_id, m.message_references, 0 AS priority FROM email.message m
     WHERE ` + fits + ` AND m.direction = 'inbound'
     ORDER BY m.received_at DESC, m.id DESC LIMIT 1)
    UNION ALL
    (SELECT m.message_id, m.message_references, 1 FROM email.message m
     WHERE ` + fits + ` AND m.direction = 'outbound' AND m.delivery_status = 'sent'
     ORDER BY m.received_at DESC, m.id DESC LIMIT 1)
) p
ORDER BY priority LIMIT 1`
	}

	var p store.EmailReplyParent
	err := s.db.QueryRow(ctx, query, pgx.NamedArgs{
		"domain_id":  domainID,
		"thread_id":  threadID,
		"message_id": messageID,
	}).Scan(&p.MessageID, &p.References)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read reply parent: %w", ParseError(err))
	}

	return &p, nil
}

// CreateMessage skips a taken request id instead of failing, because a unique
// violation would abort the whole transaction.
func (s *emailOutboundStore) CreateMessage(ctx context.Context, m *model.EmailMessage) (bool, error) {
	references := m.References
	if references == nil {
		references = []string{}
	}

	err := s.db.QueryRow(ctx, `
INSERT INTO email.message (
    domain_id, thread_id, profile_id, direction, kind, state,
    message_id, in_reply_to, message_references, subject, body_text, body_html,
    sent_at, received_at, author_user_id, request_id, request_fingerprint,
    delivery_status, delivery_phase
)
VALUES (
    @domain_id, @thread_id, @profile_id, 'outbound', 'regular', 'ready',
    @message_id, @in_reply_to, @message_references, @subject, @body_text, @body_html,
    @sent_at, @received_at, @author_user_id, @request_id, @request_fingerprint,
    'pending', 'queued'
)
ON CONFLICT (domain_id, author_user_id, request_id) WHERE request_id IS NOT NULL DO NOTHING
RETURNING id, created_at, updated_at`, pgx.NamedArgs{
		"domain_id":           m.DomainID,
		"thread_id":           m.ThreadID,
		"profile_id":          m.ProfileID,
		"message_id":          m.MessageID,
		"in_reply_to":         nullIfEmpty(m.InReplyTo),
		"message_references":  references,
		"subject":             m.Subject,
		"body_text":           m.TextBody,
		"body_html":           m.HTMLBody,
		"sent_at":             m.SentAt,
		"received_at":         m.ReceivedAt,
		"author_user_id":      m.AuthorUserID,
		"request_id":          m.RequestID,
		"request_fingerprint": m.RequestFingerprint,
	}).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: create outbound message: %w", ParseError(err))
	}
	m.Direction, m.Kind, m.State = model.EmailMessageDirectionOutbound, model.EmailKindRegular, model.EmailMessageStateReady
	m.DeliveryStatus = model.EmailDeliveryStatusPending

	return true, nil
}

func (s *emailOutboundStore) AddAuthor(ctx context.Context, domainID, threadID, userID int64, at time.Time) error {
	if _, err := s.db.Exec(ctx, `
INSERT INTO email.thread_participant
    (domain_id, thread_id, user_id, outbound_at, first_participated_at, last_participated_at)
VALUES ($1, $2, $3, $4, $4, $4)`, domainID, threadID, userID, at); err != nil {
		return fmt.Errorf("store: add thread author: %w", ParseError(err))
	}

	return nil
}

func (s *emailOutboundStore) UseUploads(ctx context.Context, domainID int64, references []uuid.UUID, messageID int64) error {
	if len(references) == 0 {
		return nil
	}

	tag, err := s.db.Exec(ctx, `
UPDATE email.attachment_upload SET used_by_message_id = $3
WHERE domain_id = $1 AND reference = ANY($2) AND used_by_message_id IS NULL`,
		domainID, references, messageID)
	if err != nil {
		return fmt.Errorf("store: use attachment uploads: %w", ParseError(err))
	}
	// The rows are locked by LockUploads, so all of them must apply.
	if tag.RowsAffected() != int64(len(references)) {
		return fmt.Errorf("store: use attachment uploads: %d of %d applied", tag.RowsAffected(), len(references))
	}

	return nil
}
