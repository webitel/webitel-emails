package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/webitel/webitel-emails/internal/model"
)

// EmailOutboundRequest is the stored result of an author's Send request.
type EmailOutboundRequest struct {
	MessageID   int64
	Fingerprint []byte
}

// EmailReplyThread is a Thread locked for a reply, as the caller sees it.
type EmailReplyThread struct {
	ProfileID int64
	Subject   string
	Status    model.EmailThreadStatus
	// The projection, which may lag behind call_center.
	CurrentUserID         *int64
	CurrentAttemptID      *int64
	CurrentAttemptVersion *int64
	// Participant: the caller is in the journal of the Thread.
	Participant bool
}

// EmailReplyParent is the message a reply answers.
type EmailReplyParent struct {
	MessageID  string
	References []string
}

// EmailOutboundStore accepts outbound emails; every method runs in the caller's
// transaction.
type EmailOutboundStore interface {
	// LockRequest serializes the requests of an author with one request id until the
	// transaction ends, whatever their target.
	LockRequest(ctx context.Context, domainID, authorID int64, requestID uuid.UUID) error
	// LocateRequest returns the message of a request; nil means none.
	LocateRequest(ctx context.Context, domainID, authorID int64, requestID uuid.UUID) (*EmailOutboundRequest, error)
	// LockReplyThread locks a Thread visible to the user; nil means not visible,
	// including a Thread of another domain.
	LockReplyThread(ctx context.Context, domainID, userID, threadID int64) (*EmailReplyThread, error)
	// SenderProfile returns the profile of the domain; a missing one is a not-found error.
	SenderProfile(ctx context.Context, domainID, profileID int64) (*model.EmailProfile, error)
	// LockUploads locks the unused uploads of the author, ordered by reference;
	// a missing one is absent from the result.
	LockUploads(ctx context.Context, domainID, authorID int64, references []uuid.UUID) ([]*model.EmailAttachmentUpload, error)
	// ReplyParent returns the parent of a reply: the chosen message when it fits,
	// otherwise, with messageID 0, the latest inbound regular or, failing that,
	// the latest sent message. Nil means none fits.
	ReplyParent(ctx context.Context, domainID, threadID, messageID int64) (*EmailReplyParent, error)
	// CreateMessage stores an outbound message and sets its id; false means the
	// request id is taken and nothing was written.
	CreateMessage(ctx context.Context, message *model.EmailMessage) (bool, error)
	// AddAuthor puts the author of a new Thread into its journal, before any snapshot.
	AddAuthor(ctx context.Context, domainID, threadID, userID int64, at time.Time) error
	// UseUploads binds the locked uploads to a message.
	UseUploads(ctx context.Context, domainID int64, references []uuid.UUID, messageID int64) error
}
