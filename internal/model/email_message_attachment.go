package model

import "time"

// EmailAttachmentState is the lifecycle state of one MIME part of a message.
type EmailAttachmentState string

// Attachment part states.
const (
	// EmailAttachmentStatePending means the file is not in storage yet.
	EmailAttachmentStatePending EmailAttachmentState = "pending"
	// EmailAttachmentStateStored means storage returned a file id for the part.
	EmailAttachmentStateStored EmailAttachmentState = "stored"
	// EmailAttachmentStateSkipped means the part is deliberately not stored.
	EmailAttachmentStateSkipped EmailAttachmentState = "skipped"
)

// EmailMessageAttachment is one attachment or inline part of a stored message.
// The file lives in storage; FileID is a logical reference to it.
type EmailMessageAttachment struct {
	ID        int64
	DomainID  int64
	MessageID int64

	// FileID is nil until the part reaches EmailAttachmentStateStored.
	FileID *int64

	ContentID   string
	Disposition EmailPartDisposition
	FileName    string
	MimeType    string
	// Size is the decoded size in bytes, known for a skipped part as well.
	Size int64

	// Position is the zero-based index of the part within the email.
	Position int32
	State    EmailAttachmentState
	// SkippedReason is set only for EmailAttachmentStateSkipped.
	SkippedReason EmailPartSkippedReason

	CreatedAt time.Time
	UpdatedAt time.Time
}
