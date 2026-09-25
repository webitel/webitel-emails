package model

import "time"

// EmailKind identifies regular and service-generated incoming emails.
type EmailKind string

const (
	EmailKindRegular   EmailKind = "regular"
	EmailKindAutoReply EmailKind = "auto_reply"
	EmailKindBounce    EmailKind = "bounce"
)

// EmailPartDisposition identifies an attachment or an inline MIME part.
type EmailPartDisposition string

const (
	EmailPartDispositionAttachment EmailPartDisposition = "attachment"
	EmailPartDispositionInline     EmailPartDisposition = "inline"
)

// EmailAddress keeps the original address and its normalized search value.
type EmailAddress struct {
	Name              string
	Address           string
	NormalizedAddress string
}

// EmailPart is one parsed attachment or inline MIME part.
type EmailPart struct {
	Name          string
	ContentType   string
	ContentID     string
	Disposition   EmailPartDisposition
	Size          int64
	Content       []byte
	SkippedReason string
}

// EmailBounceInfo contains the available delivery failure details.
type EmailBounceInfo struct {
	OriginalMessageID string
	Recipient         string
	Status            string
	DiagnosticCode    string
}

// ParsedEmail is an inbound RFC822 message converted to a transport-independent form.
type ParsedEmail struct {
	DomainID  int64
	ProfileID int64

	Mailbox     string
	UIDValidity uint32
	UID         uint32

	MessageID  string
	InReplyTo  string
	References []string
	Subject    string
	Date       time.Time

	From    []EmailAddress
	Sender  *EmailAddress
	ReplyTo []EmailAddress
	To      []EmailAddress
	Cc      []EmailAddress
	Bcc     []EmailAddress

	TextBody string
	HTMLBody string
	Parts    []EmailPart
	Kind     EmailKind
	Bounce   *EmailBounceInfo
}
