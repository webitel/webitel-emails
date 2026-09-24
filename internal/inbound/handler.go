// Package inbound defines how raw incoming emails are handed over for processing.
package inbound

import "context"

// Message is one raw RFC822 email identified by its IMAP position.
type Message struct {
	DomainID    int64
	ProfileID   int64
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	Raw         []byte
}

// Handler accepts a raw email; nil means it is safely processed and the cursor may move past it.
// The same message can be delivered again after a failure, so handling must be idempotent.
type Handler interface {
	Handle(ctx context.Context, message *Message) error
}
