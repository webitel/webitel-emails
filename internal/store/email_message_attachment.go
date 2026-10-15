package store

import (
	"context"

	"github.com/webitel/webitel-emails/internal/model"
)

// EmailMessageAttachmentStore persists the attachment manifest of a message:
// every MIME part of the email, including the ones deliberately skipped.
type EmailMessageAttachmentStore interface {
	// CreateBulk writes the whole manifest of one message in a single statement.
	// Parts are never added later, so this runs once per message.
	CreateBulk(ctx context.Context, domainID, messageID int64, attachments []*model.EmailMessageAttachment) error
	// List returns the manifest of a message ordered by MIME position.
	List(ctx context.Context, domainID, messageID int64) ([]*model.EmailMessageAttachment, error)
	// MarkStored binds an uploaded file to its part. The write applies only to a
	// part that is still pending, so a repeated upload cannot overwrite a result;
	// false means the part was already finished or is not meant for upload.
	MarkStored(ctx context.Context, domainID, messageID int64, position int32, fileID int64) (bool, error)
}
