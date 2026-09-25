// Package inbound defines how raw incoming emails are handed over for processing.
package inbound

import (
	"context"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
)

// Message is one raw RFC822 email identified by its IMAP position.
type Message struct {
	DomainID     int64
	ProfileID    int64
	Mailbox      string
	UIDValidity  uint32
	UID          uint32
	InternalDate time.Time
	Raw          []byte
}

// Handler accepts a raw email; nil means it is safely processed and the cursor may move past it.
// The same message can be delivered again after a failure, so handling must be idempotent.
type Handler interface {
	Handle(ctx context.Context, message *Message) error
}

// Parser converts a raw inbound message into the internal parsed email model.
type Parser interface {
	Parse(ctx context.Context, message *Message) (*model.ParsedEmail, error)
}

// ParsedMessageHandler accepts an email after MIME parsing.
// Returning nil confirms that the parsed email is safe for the IMAP cursor to pass.
type ParsedMessageHandler interface {
	Handle(ctx context.Context, message *model.ParsedEmail) error
}
