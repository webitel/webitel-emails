package store

import (
	"context"

	"github.com/webitel/webitel-emails/internal/model"
)

// EmailThreadRights is what a user may do with one Thread.
type EmailThreadRights struct {
	// Visible: a regular Thread of the user's domain with a ready message, whose
	// current agent or participant the user is.
	Visible bool
	// Assigned means the user may change the Thread: the current agent after Take
	// by the latest applied projection, which may lag behind call_center.
	Assigned bool
}

// EmailThreadReadStore answers the read API for one user; every method applies
// the visibility boundary in the same query that reads the data.
type EmailThreadReadStore interface {
	// Rights reports what the user may do with a Thread; an invisible Thread,
	// including one of another domain, yields the zero value.
	Rights(ctx context.Context, domainID, userID, threadID int64) (EmailThreadRights, error)
	// ListThreads returns one page of the user's Threads and whether more follow.
	ListThreads(ctx context.Context, filter model.EmailThreadFilter) ([]*model.EmailThreadView, bool, error)
	// LocateThread returns a Thread the user may see; nil means none, which
	// covers a Thread of another domain or one the user may not see.
	LocateThread(ctx context.Context, domainID, userID, threadID int64) (*model.EmailThreadView, error)
	// ListMessages returns a page of the ready messages of a visible Thread with their
	// recipients and parts; an invisible Thread yields an empty page.
	ListMessages(ctx context.Context, filter model.EmailMessageFilter) ([]*model.EmailMessageView, bool, error)
	// LocateMessage returns one ready message of a Thread the user may see; nil
	// means none.
	LocateMessage(ctx context.Context, domainID, userID, threadID, messageID int64) (*model.EmailMessageView, error)
	// LocateAttachment returns one part of a ready message of a Thread the user
	// may see, with the file id it is stored under; nil means none.
	LocateAttachment(
		ctx context.Context,
		domainID, userID, threadID, messageID, attachmentID int64,
	) (*model.EmailAttachmentView, error)
}
