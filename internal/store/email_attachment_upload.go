package store

import (
	"context"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
)

// EmailAttachmentUploadStore keeps the uploads of outbound attachments.
type EmailAttachmentUploadStore interface {
	Create(ctx context.Context, upload *model.EmailAttachmentUpload) error
	// DeleteExpiredUnused removes up to limit unused uploads that expired before.
	DeleteExpiredUnused(ctx context.Context, before time.Time, limit int) (int64, error)
}
