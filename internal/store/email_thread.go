package store

import (
	"context"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/internal/model"
)

// ErrEmailMessageExists means the email is already stored under one of its
// identities, so the delivery is a duplicate rather than a failure.
var ErrEmailMessageExists = kiterrors.New(
	"email message already exists",
	kiterrors.WithID("store.email_message.exists"),
	kiterrors.WithCode(codes.AlreadyExists),
)

// EmailThreadLookup narrows a threading search to one tenant and mailbox.
type EmailThreadLookup struct {
	DomainID  int64
	ProfileID int64
	// OnlyRegular excludes service threads, so an ordinary email never lands in one.
	OnlyRegular bool
}

// EmailMessageIdentity is the stored state of a message, enough to decide
// whether a redelivery is a duplicate.
type EmailMessageIdentity struct {
	ID       int64
	ThreadID int64
	State    model.EmailMessageState
}

// EmailThreadStore persists Threads and resolves the Thread of an incoming email.
// A nil result from a lookup means no match and is not an error.
type EmailThreadStore interface {
	// Create stores a new Thread.
	Create(ctx context.Context, thread *model.EmailThread) (*model.EmailThread, error)
	// LocateByMessageIDs returns the Thread of the message matching one of these
	// Message-IDs. The list is ordered by increasing priority, so the match
	// nearest its end wins.
	LocateByMessageIDs(ctx context.Context, lookup EmailThreadLookup, messageIDs []string) (*model.EmailThread, error)
	// LocateByInReplyTo returns the Thread of the oldest message whose In-Reply-To
	// is inReplyTo, which serves both the sibling and the reverse-sibling rule.
	LocateByInReplyTo(ctx context.Context, lookup EmailThreadLookup, inReplyTo string) (*model.EmailThread, error)
	// AdvanceLastMessage moves last_message_at forward; an older time is ignored.
	// Callers hold the Thread from the same transaction, so a missing row is a no-op.
	AdvanceLastMessage(ctx context.Context, domainID, profileID, threadID int64, at time.Time) error
}

// EmailMessageStore persists Messages and finds already stored ones by the three
// identities an inbound email can be recognized by.
type EmailMessageStore interface {
	// LocateByMessageID finds a message by its mail identifier, in both directions.
	LocateByMessageID(ctx context.Context, domainID, profileID int64, messageID string) (*EmailMessageIdentity, error)
	// LocateByIMAPIdentity finds a message by its position in a mailbox.
	LocateByIMAPIdentity(ctx context.Context, domainID, profileID int64, identity model.EmailIMAPIdentity) (*EmailMessageIdentity, error)
	// LocateByRawChecksum finds a message whose Message-ID was generated, by the
	// checksum of its raw RFC822 source.
	LocateByRawChecksum(ctx context.Context, domainID, profileID int64, checksum []byte) (*EmailMessageIdentity, error)
	// Create stores a message of an existing Thread.
	Create(ctx context.Context, message *model.EmailMessage) (*model.EmailMessage, error)
	// MarkReady completes a message once its manifest holds no pending part and
	// returns the state the message ends up in. "ready" means it is complete,
	// whether this call or a concurrent delivery finished it; "processing" means a
	// part is still pending.
	MarkReady(ctx context.Context, domainID, messageID int64) (model.EmailMessageState, error)
	// MarkFailed gives up on a processing message. False means it was already
	// finished, so the caller must not record a reason for it.
	MarkFailed(ctx context.Context, domainID, messageID int64) (bool, error)
	// IncrementAttachmentAttempts counts one failed attempt to finish the
	// attachments of a message and returns the new total.
	IncrementAttachmentAttempts(ctx context.Context, domainID, messageID int64) (int32, error)
	// LastReceivedAt returns when the newest ready message of a profile arrived,
	// or the zero time when the profile has none. It is the fallback checkpoint
	// for a cursor that predates recovery support.
	LastReceivedAt(ctx context.Context, domainID, profileID int64) (time.Time, error)
}

// InboundFailureStore records emails that cannot be processed as they are.
type InboundFailureStore interface {
	// Record stores a quarantined email. A redelivery of the same one updates
	// the existing row, so the caller may retry without creating duplicates.
	Record(ctx context.Context, failure *model.InboundFailure) error
}

// EmailRecipientStore persists the participant addresses of a message.
type EmailRecipientStore interface {
	// CreateBulk stores the recipients of one message in a single statement,
	// keeping their type and original order.
	CreateBulk(ctx context.Context, domainID, messageID int64, recipients []*model.EmailRecipient) error
}
