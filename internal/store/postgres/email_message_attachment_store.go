package postgres

import (
	"context"
	"database/sql"

	"github.com/lib/pq"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const emailMessageAttachmentColumns = `a.id,
    a.domain_id,
    a.message_id,
    a.file_id,
    a.content_id,
    a.disposition,
    a.file_name,
    a.mime_type,
    a.size,
    a.position,
    a.state,
    a.skipped_reason,
    a.created_at,
    a.updated_at`

type emailMessageAttachmentStore struct {
	db Querier
}

var _ store.EmailMessageAttachmentStore = (*emailMessageAttachmentStore)(nil)

// NewEmailMessageAttachmentStore creates an attachment manifest store over q.
func NewEmailMessageAttachmentStore(q Querier) *emailMessageAttachmentStore {
	return &emailMessageAttachmentStore{db: q}
}

func (s *emailMessageAttachmentStore) CreateBulk(
	ctx context.Context,
	domainID, messageID int64,
	attachments []*model.EmailMessageAttachment,
) error {
	if len(attachments) == 0 {
		return nil
	}

	// unnest keeps the parameter count fixed whatever the number of parts. No
	// file id is written here: the manifest is created before any upload.
	const query = `
INSERT INTO email.message_attachment (
    domain_id, message_id, content_id, disposition,
    file_name, mime_type, size, position, state, skipped_reason
)
SELECT $1, $2, NULLIF(a.content_id, ''), a.disposition,
       a.file_name, a.mime_type, a.size, a.position, a.state, NULLIF(a.skipped_reason, '')
FROM unnest(
    $3::text[], $4::text[], $5::text[],
    $6::text[], $7::bigint[], $8::int[], $9::text[], $10::text[]
) AS a(content_id, disposition, file_name, mime_type, size, position, state, skipped_reason)`

	var (
		contentIDs  = make([]string, 0, len(attachments))
		disposition = make([]string, 0, len(attachments))
		fileNames   = make([]string, 0, len(attachments))
		mimeTypes   = make([]string, 0, len(attachments))
		sizes       = make([]int64, 0, len(attachments))
		positions   = make([]int32, 0, len(attachments))
		states      = make([]string, 0, len(attachments))
		reasons     = make([]string, 0, len(attachments))
	)
	for _, attachment := range attachments {
		contentIDs = append(contentIDs, attachment.ContentID)
		disposition = append(disposition, string(attachment.Disposition))
		fileNames = append(fileNames, attachment.FileName)
		mimeTypes = append(mimeTypes, attachment.MimeType)
		sizes = append(sizes, attachment.Size)
		positions = append(positions, attachment.Position)
		states = append(states, string(attachment.State))
		reasons = append(reasons, string(attachment.SkippedReason))
	}

	_, err := s.db.ExecContext(
		ctx,
		query,
		domainID,
		messageID,
		pq.Array(contentIDs),
		pq.Array(disposition),
		pq.Array(fileNames),
		pq.Array(mimeTypes),
		pq.Array(sizes),
		pq.Array(positions),
		pq.Array(states),
		pq.Array(reasons),
	)

	return ParseError(err)
}

func (s *emailMessageAttachmentStore) List(
	ctx context.Context,
	domainID, messageID int64,
) ([]*model.EmailMessageAttachment, error) {
	const query = `SELECT ` + emailMessageAttachmentColumns + `
FROM email.message_attachment a
WHERE a.domain_id = $1 AND a.message_id = $2
ORDER BY a.position`

	rows, err := s.db.QueryContext(ctx, query, domainID, messageID)
	if err != nil {
		return nil, ParseError(err)
	}
	defer rows.Close()

	var attachments []*model.EmailMessageAttachment
	for rows.Next() {
		attachment, err := scanEmailMessageAttachment(rows)
		if err != nil {
			return nil, ParseError(err)
		}

		attachments = append(attachments, attachment)
	}

	if err := rows.Err(); err != nil {
		return nil, ParseError(err)
	}

	return attachments, nil
}

// MarkStored requires the pending state as well as an absent file id: a skipped
// part has no file id either, and must never receive one.
func (s *emailMessageAttachmentStore) MarkStored(
	ctx context.Context,
	domainID, messageID int64,
	position int32,
	fileID int64,
) (bool, error) {
	const query = `
UPDATE email.message_attachment SET
    file_id = $4,
    state = 'stored',
    updated_at = now()
WHERE domain_id = $1 AND message_id = $2 AND position = $3
    AND state = 'pending' AND file_id IS NULL`

	result, err := s.db.ExecContext(ctx, query, domainID, messageID, position, fileID)
	if err != nil {
		return false, ParseError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected > 0, nil
}

func scanEmailMessageAttachment(row rowScanner) (*model.EmailMessageAttachment, error) {
	var (
		attachment  model.EmailMessageAttachment
		fileID      sql.NullInt64
		contentID   sql.NullString
		disposition string
		state       string
		reason      sql.NullString
	)
	if err := row.Scan(
		&attachment.ID,
		&attachment.DomainID,
		&attachment.MessageID,
		&fileID,
		&contentID,
		&disposition,
		&attachment.FileName,
		&attachment.MimeType,
		&attachment.Size,
		&attachment.Position,
		&state,
		&reason,
		&attachment.CreatedAt,
		&attachment.UpdatedAt,
	); err != nil {
		return nil, err
	}

	if fileID.Valid {
		attachment.FileID = &fileID.Int64
	}
	attachment.ContentID = contentID.String
	attachment.Disposition = model.EmailPartDisposition(disposition)
	attachment.State = model.EmailAttachmentState(state)
	attachment.SkippedReason = model.EmailPartSkippedReason(reason.String)

	return &attachment, nil
}
