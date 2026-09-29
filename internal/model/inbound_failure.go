package model

import "time"

// InboundFailureCategory names why an email can never be processed as it is.
type InboundFailureCategory string

// Quarantine categories.
const (
	InboundFailureMIMEParse        InboundFailureCategory = "mime_parse"
	InboundFailureMessageTooLarge  InboundFailureCategory = "message_too_large"
	InboundFailureAttachmentUpload InboundFailureCategory = "attachment_upload"
)

// InboundFailure is a quarantined email. The message itself stays in the
// mailbox; these coordinates are what locates it there.
type InboundFailure struct {
	ID        int64
	DomainID  int64
	ProfileID int64

	Mailbox     string
	UIDValidity uint32
	UID         uint32

	Category InboundFailureCategory
	Reason   string
	// Size is the server-reported RFC822.SIZE; zero when it is unknown.
	Size int64

	MessageID string
	Sender    string
	Subject   string

	CreatedAt time.Time
}
