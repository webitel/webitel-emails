package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailAttachmentUploadStore struct {
	db Querier
}

var _ store.EmailAttachmentUploadStore = (*emailAttachmentUploadStore)(nil)

// NewEmailAttachmentUploadStore creates the store of outbound attachment uploads.
func NewEmailAttachmentUploadStore(db Querier) *emailAttachmentUploadStore {
	return &emailAttachmentUploadStore{db: db}
}

func (s *emailAttachmentUploadStore) Create(ctx context.Context, u *model.EmailAttachmentUpload) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO email.attachment_upload
    (domain_id, reference, author_user_id, file_id, file_name, mime_type, size, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		u.DomainID, u.Reference, u.AuthorUserID, u.FileID, u.FileName, u.MimeType, u.Size, u.ExpiresAt)
	if err != nil {
		return fmt.Errorf("store: create attachment upload: %w", ParseError(err))
	}

	return nil
}

func (s *emailAttachmentUploadStore) DeleteExpiredUnused(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := s.db.Exec(ctx, `
DELETE FROM email.attachment_upload
WHERE (domain_id, reference) IN (
    SELECT domain_id, reference
    FROM email.attachment_upload
    WHERE used_by_message_id IS NULL AND expires_at < $1
    ORDER BY expires_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED)
    AND used_by_message_id IS NULL`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired attachment uploads: %w", err)
	}

	return tag.RowsAffected(), nil
}
