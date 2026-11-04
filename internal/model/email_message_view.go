package model

import "time"

// EmailInlineContentStatus tells whether the inline images of a sanitized HTML
// body could be linked.
type EmailInlineContentStatus string

// Inline content statuses.
const (
	// EmailInlineContentNone: the body references no stored inline image.
	EmailInlineContentNone EmailInlineContentStatus = "none"
	EmailInlineContentOK   EmailInlineContentStatus = "ok"
	// EmailInlineContentUnavailable: storage could not issue a link, so such
	// images were removed from the body.
	EmailInlineContentUnavailable EmailInlineContentStatus = "unavailable"
)

// EmailMessageCursor is the position after which the next page starts.
type EmailMessageCursor struct {
	ReceivedAt time.Time
	ID         int64
}

// EmailMessageFilter selects a page of the ready messages of one Thread the
// user may see.
type EmailMessageFilter struct {
	DomainID int64
	UserID   int64
	ThreadID int64
	// After is nil for the first page.
	After *EmailMessageCursor
	Desc  bool
	Size  int
}

// EmailAttachmentView is one part of a message as the read API describes it.
type EmailAttachmentView struct {
	ID int64
	// FileID never leaves the service; it is used only to request a link.
	FileID        *int64
	FileName      string
	MimeType      string
	Size          int64
	Disposition   EmailPartDisposition
	ContentID     string
	State         EmailAttachmentState
	SkippedReason EmailPartSkippedReason
}

// EmailMessageView is one ready message as the read API shows it.
type EmailMessageView struct {
	ID        int64
	ThreadID  int64
	ProfileID int64
	Direction EmailMessageDirection
	Kind      EmailKind

	InternetMessageID string
	InReplyTo         string
	Subject           string

	From    []EmailAddress
	Sender  []EmailAddress
	ReplyTo []EmailAddress
	To      []EmailAddress
	Cc      []EmailAddress

	SentAt     *time.Time
	ReceivedAt time.Time
	BodyText   string

	// RawHTML is the stored body exactly as the sender wrote it. It never
	// leaves the service; only SanitizedHTML is returned to a client.
	RawHTML              string
	SanitizedHTML        string
	RemoteContentBlocked bool
	InlineContentStatus  EmailInlineContentStatus

	Bounce      *EmailBounceInfo
	Attachments []EmailAttachmentView
}
