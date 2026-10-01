package model

import "time"

// EmailMessageDirection tells an incoming email from an outgoing one.
type EmailMessageDirection string

// Email message directions.
const (
	EmailMessageDirectionInbound  EmailMessageDirection = "inbound"
	EmailMessageDirectionOutbound EmailMessageDirection = "outbound"
)

// EmailMessageState is the processing state of a stored email, independent of
// the Thread status and of SMTP delivery.
type EmailMessageState string

// Email message processing states.
const (
	EmailMessageStateProcessing EmailMessageState = "processing"
	EmailMessageStateReady      EmailMessageState = "ready"
	EmailMessageStateFailed     EmailMessageState = "failed"
)

// EmailRecipientType is the header an address was taken from.
type EmailRecipientType string

// Email recipient types.
const (
	EmailRecipientTypeFrom    EmailRecipientType = "from"
	EmailRecipientTypeSender  EmailRecipientType = "sender"
	EmailRecipientTypeReplyTo EmailRecipientType = "reply_to"
	EmailRecipientTypeTo      EmailRecipientType = "to"
	EmailRecipientTypeCc      EmailRecipientType = "cc"
	EmailRecipientTypeBcc     EmailRecipientType = "bcc"
)

// EmailIMAPIdentity locates an email inside a mailbox.
type EmailIMAPIdentity struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// EmailMessage is one stored email of a Thread.
type EmailMessage struct {
	ID        int64
	DomainID  int64
	ThreadID  int64
	ProfileID int64

	Direction EmailMessageDirection
	Kind      EmailKind
	State     EmailMessageState
	// AttachmentAttempts counts the failed attempts to finish the attachments.
	AttachmentAttempts int32

	MessageID string
	// True when the MIME parser had to generate the identifier itself.
	MessageIDGenerated bool
	RawSHA256          []byte
	// Present for an email read from a mailbox.
	IMAP *EmailIMAPIdentity

	InReplyTo  string
	References []string
	Subject    string
	TextBody   string
	HTMLBody   string
	Bounce     *EmailBounceInfo

	// SentAt comes from the Date header and is never used for ordering.
	SentAt *time.Time
	// ReceivedAt comes from IMAP INTERNALDATE and orders the Thread.
	ReceivedAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EmailRecipient is one participant address of a message.
type EmailRecipient struct {
	ID        int64
	DomainID  int64
	MessageID int64

	Type              EmailRecipientType
	Address           string
	NormalizedAddress string
	DisplayName       string
	// Zero-based position within its own type list.
	Ordinal int32
}
