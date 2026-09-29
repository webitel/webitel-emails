package inbound

import (
	"context"
	"slices"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// fakeStore is an in-memory stand-in for the storage layer. It records the order
// of operations so a test can assert that the profile lock comes first.
type fakeStore struct {
	threads    []*model.EmailThread
	messages   []*model.EmailMessage
	recipients map[int64][]*model.EmailRecipient
	nextID     int64
	calls      []string
	messageErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{recipients: make(map[int64][]*model.EmailRecipient)}
}

var _ store.UnitOfWork = (*fakeStore)(nil)

func (f *fakeStore) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeStore) id() int64 {
	f.nextID++

	return f.nextID
}

// WithinTransaction restores the snapshot on error, so a test sees the same
// all-or-nothing outcome the database would give.
func (f *fakeStore) WithinTransaction(ctx context.Context, fn func(context.Context, store.UnitOfWork) error) error {
	threads := slices.Clone(f.threads)
	messages := slices.Clone(f.messages)

	if err := fn(ctx, f); err != nil {
		f.threads, f.messages = threads, messages

		return err
	}

	return nil
}

func (f *fakeStore) LockEmailProfile(context.Context, int64) error {
	f.record("lock")

	return nil
}

func (f *fakeStore) EmailThreadStore() store.EmailThreadStore       { return fakeThreadStore{f} }
func (f *fakeStore) EmailMessageStore() store.EmailMessageStore     { return fakeMessageStore{f} }
func (f *fakeStore) EmailRecipientStore() store.EmailRecipientStore { return fakeRecipientStore{f} }

func (f *fakeStore) threadByID(id int64, lookup store.EmailThreadLookup) *model.EmailThread {
	for _, thread := range f.threads {
		if thread.ID != id {
			continue
		}
		if lookup.OnlyRegular && thread.Kind != model.EmailThreadKindRegular {
			return nil
		}

		return thread
	}

	return nil
}

func (f *fakeStore) findMessage(match func(*model.EmailMessage) bool) *store.EmailMessageIdentity {
	for _, message := range f.messages {
		if match(message) {
			return &store.EmailMessageIdentity{ID: message.ID, ThreadID: message.ThreadID, State: message.State}
		}
	}

	return nil
}

// Thread, message and recipient stores share one data set but need separate
// types, because both contracts declare a Create of their own.
type fakeThreadStore struct{ *fakeStore }

var _ store.EmailThreadStore = fakeThreadStore{}

func (f fakeThreadStore) Create(_ context.Context, thread *model.EmailThread) (*model.EmailThread, error) {
	f.record("create_thread")
	created := *thread
	created.ID = f.id()
	f.threads = append(f.threads, &created)

	return &created, nil
}

// LocateByMessageIDs mirrors the SQL ordering: the match at the highest position
// in the list wins, not the first message that happens to be stored.
func (f fakeThreadStore) LocateByMessageIDs(
	_ context.Context,
	lookup store.EmailThreadLookup,
	messageIDs []string,
) (*model.EmailThread, error) {
	f.record("thread_by_message_ids")

	best := -1
	var winner *model.EmailThread
	for _, message := range f.messages {
		if message.DomainID != lookup.DomainID || message.ProfileID != lookup.ProfileID {
			continue
		}
		// Excluded Threads drop out before the winner is picked, exactly as the
		// SQL filter runs before its ordering.
		thread := f.threadByID(message.ThreadID, lookup)
		if thread == nil {
			continue
		}
		if position := slices.Index(messageIDs, message.MessageID); position > best {
			best, winner = position, thread
		}
	}

	return winner, nil
}

func (f fakeThreadStore) LocateByInReplyTo(
	_ context.Context,
	lookup store.EmailThreadLookup,
	inReplyTo string,
) (*model.EmailThread, error) {
	f.record("thread_by_in_reply_to")

	var winner *model.EmailThread
	for _, message := range f.messages {
		if message.DomainID != lookup.DomainID || message.ProfileID != lookup.ProfileID ||
			message.InReplyTo == "" || message.InReplyTo != inReplyTo {
			continue
		}
		thread := f.threadByID(message.ThreadID, lookup)
		if thread == nil {
			continue
		}
		// A regular Thread always beats a service one, as the SQL ordering does.
		if winner == nil ||
			(winner.Kind != model.EmailThreadKindRegular && thread.Kind == model.EmailThreadKindRegular) {
			winner = thread
		}
	}

	return winner, nil
}

func (f fakeThreadStore) AdvanceLastMessage(
	_ context.Context,
	domainID, profileID, threadID int64,
	at time.Time,
) error {
	f.record("advance_last_message")
	for _, thread := range f.threads {
		if thread.ID != threadID || thread.DomainID != domainID || thread.ProfileID != profileID {
			continue
		}
		if thread.LastMessageAt == nil || thread.LastMessageAt.Before(at) {
			moment := at
			thread.LastMessageAt = &moment
		}
	}

	return nil
}

type fakeMessageStore struct{ *fakeStore }

var _ store.EmailMessageStore = fakeMessageStore{}

func (f fakeMessageStore) LastReceivedAt(context.Context, int64, int64) (time.Time, error) {
	return time.Time{}, nil
}

func (f fakeMessageStore) Create(_ context.Context, message *model.EmailMessage) (*model.EmailMessage, error) {
	f.record("create_message")
	if f.messageErr != nil {
		return nil, f.messageErr
	}
	created := *message
	created.ID = f.id()
	f.messages = append(f.messages, &created)

	return &created, nil
}

func (f fakeMessageStore) LocateByMessageID(
	_ context.Context,
	domainID, profileID int64,
	messageID string,
) (*store.EmailMessageIdentity, error) {
	f.record("message_by_message_id")

	return f.findMessage(func(message *model.EmailMessage) bool {
		return message.DomainID == domainID && message.ProfileID == profileID && message.MessageID == messageID
	}), nil
}

func (f fakeMessageStore) LocateByIMAPIdentity(
	_ context.Context,
	domainID, profileID int64,
	identity model.EmailIMAPIdentity,
) (*store.EmailMessageIdentity, error) {
	f.record("message_by_imap_identity")

	return f.findMessage(func(message *model.EmailMessage) bool {
		return message.DomainID == domainID && message.ProfileID == profileID &&
			message.IMAP != nil && *message.IMAP == identity
	}), nil
}

func (f fakeMessageStore) LocateByRawChecksum(
	_ context.Context,
	domainID, profileID int64,
	checksum []byte,
) (*store.EmailMessageIdentity, error) {
	f.record("message_by_raw_checksum")

	return f.findMessage(func(message *model.EmailMessage) bool {
		return message.DomainID == domainID && message.ProfileID == profileID &&
			message.MessageIDGenerated && slices.Equal(message.RawSHA256, checksum)
	}), nil
}

type fakeRecipientStore struct{ *fakeStore }

var _ store.EmailRecipientStore = fakeRecipientStore{}

func (f fakeRecipientStore) CreateBulk(
	_ context.Context,
	_, messageID int64,
	recipients []*model.EmailRecipient,
) error {
	f.record("create_recipients")
	f.recipients[messageID] = append(f.recipients[messageID], recipients...)

	return nil
}
