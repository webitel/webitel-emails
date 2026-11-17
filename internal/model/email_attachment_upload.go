package model

import (
	"time"

	"github.com/google/uuid"
)

// EmailAttachmentUpload is a stored file its author may attach to one outbound email.
type EmailAttachmentUpload struct {
	DomainID     int64
	Reference    uuid.UUID
	AuthorUserID int64
	FileID       int64
	FileName     string
	MimeType     string
	Size         int64
	ExpiresAt    time.Time
}
