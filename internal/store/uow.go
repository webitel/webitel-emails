package store

import (
	"context"

	"github.com/webitel/webitel-emails/internal/model"
)

// UnitOfWork runs storage work, optionally grouping it into one transaction.
// Store accessors are added here as the stores appear.
type UnitOfWork interface {
	// WithinTransaction runs fn in one transaction: nil commits, an error or a
	// panic rolls back. A unit of work already in a transaction joins it.
	WithinTransaction(ctx context.Context, fn func(ctx context.Context, uow UnitOfWork) error) error

	// LockEmailProfile serializes the writers of one profile until the current
	// transaction ends, so concurrent deliveries cannot split one conversation.
	LockEmailProfile(ctx context.Context, profileID int64) error

	// EnsureEmailProfileAssignment fails with ErrStaleEmailProfileAssignment when
	// the profile moved to another owner or generation. It takes no lock: the
	// conditional writes are what keep a stale worker correct, and this only
	// stops its work early.
	EnsureEmailProfileAssignment(ctx context.Context, assignment model.EmailProfileAssignment) error

	// EmailThreadStore accesses Email Threads.
	EmailThreadStore() EmailThreadStore
	// EmailMessageStore accesses Email Messages.
	EmailMessageStore() EmailMessageStore
	// EmailRecipientStore accesses Message recipients.
	EmailRecipientStore() EmailRecipientStore
	// EmailMessageAttachmentStore accesses the attachment manifest.
	EmailMessageAttachmentStore() EmailMessageAttachmentStore
	// InboundFailureStore accesses quarantine records, so a failing Message and
	// its reason are written in one transaction.
	InboundFailureStore() InboundFailureStore
}
