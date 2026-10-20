package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/webitel/webitel-emails/internal/model"
)

// emailMessageViewSelect reads ready messages joined to their Thread; the caller
// appends conditions after the visibility boundary, which is always present.
var emailMessageViewSelect = `
SELECT m.id, m.thread_id, m.profile_id, m.direction, m.kind, m.message_id,
    coalesce(m.in_reply_to, '') AS in_reply_to, m.subject, m.body_text, m.body_html,
    m.bounce_original_message_id, m.bounce_recipient, m.bounce_status, m.bounce_diagnostic_code,
    m.sent_at, m.received_at
FROM email.message m
JOIN email.thread t ON t.domain_id = m.domain_id AND t.id = m.thread_id
WHERE m.domain_id = @domain_id AND m.thread_id = @thread_id AND m.state = 'ready'
    AND ` + visibleThread(model.EmailThreadAssignmentAll)

// ListMessages pages by (received_at, id): pages never repeat a message, but one
// completed later with an earlier time is not added to a traversal in progress.
func (s *emailThreadReadStore) ListMessages(
	ctx context.Context,
	filter model.EmailMessageFilter,
) ([]*model.EmailMessageView, bool, error) {
	args := pgx.NamedArgs{
		"domain_id": filter.DomainID,
		"user_id":   filter.UserID,
		"thread_id": filter.ThreadID,
		"limit":     filter.Size + 1,
	}

	query := emailMessageViewSelect
	order := "\nORDER BY m.received_at ASC, m.id ASC"
	comparison := ">"
	if filter.Desc {
		order = "\nORDER BY m.received_at DESC, m.id DESC"
		comparison = "<"
	}
	if filter.After != nil {
		query += "\n    AND (m.received_at, m.id) " + comparison + " (@after_received_at, @after_id)"
		args["after_received_at"] = filter.After.ReceivedAt
		args["after_id"] = filter.After.ID
	}
	query += order + "\nLIMIT @limit"

	messages, err := s.queryMessages(ctx, query, args)
	if err != nil {
		return nil, false, err
	}

	next := len(messages) > filter.Size
	if next {
		messages = messages[:filter.Size]
	}
	if err := s.attachDetails(ctx, filter.DomainID, messages); err != nil {
		return nil, false, err
	}

	return messages, next, nil
}

// LocateMessage reads one ready message through the visibility boundary.
func (s *emailThreadReadStore) LocateMessage(
	ctx context.Context,
	domainID, userID, threadID, messageID int64,
) (*model.EmailMessageView, error) {
	messages, err := s.queryMessages(ctx, emailMessageViewSelect+"\n    AND m.id = @message_id", pgx.NamedArgs{
		"domain_id":  domainID,
		"user_id":    userID,
		"thread_id":  threadID,
		"message_id": messageID,
	})
	if err != nil || len(messages) == 0 {
		return nil, err
	}
	if err := s.attachDetails(ctx, domainID, messages); err != nil {
		return nil, err
	}

	return messages[0], nil
}

func (s *emailThreadReadStore) queryMessages(
	ctx context.Context,
	query string,
	args pgx.NamedArgs,
) ([]*model.EmailMessageView, error) {
	rows, err := s.db.Query(ctx, query, args)
	if err != nil {
		return nil, ParseError(err)
	}
	records, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByName[emailMessageViewRecord])
	if err != nil {
		return nil, ParseError(err)
	}

	out := make([]*model.EmailMessageView, 0, len(records))
	for _, record := range records {
		out = append(out, record.toModel())
	}

	return out, nil
}

// attachDetails loads recipients (never blind copies) and parts of a visible page,
// one query each.
func (s *emailThreadReadStore) attachDetails(ctx context.Context, domainID int64, messages []*model.EmailMessageView) error {
	if len(messages) == 0 {
		return nil
	}

	byID := make(map[int64]*model.EmailMessageView, len(messages))
	ids := make([]int64, 0, len(messages))
	for _, message := range messages {
		byID[message.ID] = message
		ids = append(ids, message.ID)
	}

	rows, err := s.db.Query(ctx, `
SELECT message_id, type, address, display_name
FROM email.recipient
WHERE domain_id = $1 AND message_id = ANY($2) AND type <> 'bcc'
ORDER BY message_id, type, ordinal`, domainID, ids)
	if err != nil {
		return ParseError(err)
	}
	recipients, err := pgx.CollectRows(rows, pgx.RowToStructByName[emailRecipientViewRecord])
	if err != nil {
		return ParseError(err)
	}
	for _, r := range recipients {
		message := byID[r.MessageID]
		address := model.EmailAddress{Address: r.Address, Name: r.DisplayName}
		switch model.EmailRecipientType(r.Type) {
		case model.EmailRecipientTypeFrom:
			message.From = append(message.From, address)
		case model.EmailRecipientTypeSender:
			message.Sender = append(message.Sender, address)
		case model.EmailRecipientTypeReplyTo:
			message.ReplyTo = append(message.ReplyTo, address)
		case model.EmailRecipientTypeTo:
			message.To = append(message.To, address)
		case model.EmailRecipientTypeCc:
			message.Cc = append(message.Cc, address)
		}
	}

	rows, err = s.db.Query(ctx, `
SELECT id, message_id, file_id, coalesce(content_id, '') AS content_id, disposition, file_name,
    mime_type, size, state, coalesce(skipped_reason, '') AS skipped_reason
FROM email.message_attachment
WHERE domain_id = $1 AND message_id = ANY($2)
ORDER BY message_id, position`, domainID, ids)
	if err != nil {
		return ParseError(err)
	}
	parts, err := pgx.CollectRows(rows, pgx.RowToStructByName[emailAttachmentViewRecord])
	if err != nil {
		return ParseError(err)
	}
	for _, p := range parts {
		message := byID[p.MessageID]
		message.Attachments = append(message.Attachments, model.EmailAttachmentView{
			ID:            p.ID,
			FileID:        p.FileID,
			FileName:      p.FileName,
			MimeType:      p.MimeType,
			Size:          p.Size,
			Disposition:   model.EmailPartDisposition(p.Disposition),
			ContentID:     p.ContentID,
			State:         model.EmailAttachmentState(p.State),
			SkippedReason: model.EmailPartSkippedReason(p.SkippedReason),
		})
	}

	return nil
}

type emailMessageViewRecord struct {
	ID                      int64      `db:"id"`
	ThreadID                int64      `db:"thread_id"`
	ProfileID               int64      `db:"profile_id"`
	Direction               string     `db:"direction"`
	Kind                    string     `db:"kind"`
	MessageID               string     `db:"message_id"`
	InReplyTo               string     `db:"in_reply_to"`
	Subject                 string     `db:"subject"`
	BodyText                string     `db:"body_text"`
	BodyHTML                string     `db:"body_html"`
	BounceOriginalMessageID *string    `db:"bounce_original_message_id"`
	BounceRecipient         *string    `db:"bounce_recipient"`
	BounceStatus            *string    `db:"bounce_status"`
	BounceDiagnosticCode    *string    `db:"bounce_diagnostic_code"`
	SentAt                  *time.Time `db:"sent_at"`
	ReceivedAt              time.Time  `db:"received_at"`
}

func (r *emailMessageViewRecord) toModel() *model.EmailMessageView {
	view := &model.EmailMessageView{
		ID:                r.ID,
		ThreadID:          r.ThreadID,
		ProfileID:         r.ProfileID,
		Direction:         model.EmailMessageDirection(r.Direction),
		Kind:              model.EmailKind(r.Kind),
		InternetMessageID: r.MessageID,
		InReplyTo:         r.InReplyTo,
		Subject:           r.Subject,
		SentAt:            r.SentAt,
		ReceivedAt:        r.ReceivedAt,
		BodyText:          r.BodyText,
		RawHTML:           r.BodyHTML,
	}
	if view.Kind == model.EmailKindBounce {
		view.Bounce = &model.EmailBounceInfo{
			OriginalMessageID: derefString(r.BounceOriginalMessageID),
			Recipient:         derefString(r.BounceRecipient),
			Status:            derefString(r.BounceStatus),
			DiagnosticCode:    derefString(r.BounceDiagnosticCode),
		}
	}

	return view
}

type emailRecipientViewRecord struct {
	MessageID   int64  `db:"message_id"`
	Type        string `db:"type"`
	Address     string `db:"address"`
	DisplayName string `db:"display_name"`
}

type emailAttachmentViewRecord struct {
	ID            int64  `db:"id"`
	MessageID     int64  `db:"message_id"`
	FileID        *int64 `db:"file_id"`
	ContentID     string `db:"content_id"`
	Disposition   string `db:"disposition"`
	FileName      string `db:"file_name"`
	MimeType      string `db:"mime_type"`
	Size          int64  `db:"size"`
	State         string `db:"state"`
	SkippedReason string `db:"skipped_reason"`
}

// LocateAttachment checks the whole chain (part, ready message, Thread, visibility)
// in one query.
func (s *emailThreadReadStore) LocateAttachment(
	ctx context.Context,
	domainID, userID, threadID, messageID, attachmentID int64,
) (*model.EmailAttachmentView, error) {
	query := `
SELECT a.id, a.message_id, a.file_id, coalesce(a.content_id, '') AS content_id, a.disposition, a.file_name,
    a.mime_type, a.size, a.state, coalesce(a.skipped_reason, '') AS skipped_reason
FROM email.message_attachment a
JOIN email.message m ON m.domain_id = a.domain_id AND m.id = a.message_id
JOIN email.thread t ON t.domain_id = m.domain_id AND t.id = m.thread_id
WHERE a.domain_id = @domain_id AND a.id = @attachment_id AND m.id = @message_id
    AND m.thread_id = @thread_id AND m.state = 'ready'
    AND ` + visibleThread(model.EmailThreadAssignmentAll)

	rows, err := s.db.Query(ctx, query, pgx.NamedArgs{
		"domain_id":     domainID,
		"user_id":       userID,
		"thread_id":     threadID,
		"message_id":    messageID,
		"attachment_id": attachmentID,
	})
	if err != nil {
		return nil, ParseError(err)
	}
	parts, err := pgx.CollectRows(rows, pgx.RowToStructByName[emailAttachmentViewRecord])
	if err != nil {
		return nil, ParseError(err)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	p := parts[0]

	return &model.EmailAttachmentView{
		ID:            p.ID,
		FileID:        p.FileID,
		FileName:      p.FileName,
		MimeType:      p.MimeType,
		Size:          p.Size,
		Disposition:   model.EmailPartDisposition(p.Disposition),
		ContentID:     p.ContentID,
		State:         model.EmailAttachmentState(p.State),
		SkippedReason: model.EmailPartSkippedReason(p.SkippedReason),
	}, nil
}
