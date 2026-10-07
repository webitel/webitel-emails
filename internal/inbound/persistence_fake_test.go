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
	threads        []*model.EmailThread
	messages       []*model.EmailMessage
	recipients     map[int64][]*model.EmailRecipient
	attachments    map[int64][]*model.EmailMessageAttachment
	failures       []*model.InboundFailure
	nextID         int64
	calls          []string
	messageErr     error
	firstSenderErr error
	transactions   int
	// staleAssignment makes every fenced check report a lost profile.
	staleAssignment bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		recipients:  make(map[int64][]*model.EmailRecipient),
		attachments: make(map[int64][]*model.EmailMessageAttachment),
	}
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
	f.transactions++
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

func (f *fakeStore) EmailMessageAttachmentStore() store.EmailMessageAttachmentStore {
	return fakeAttachmentStore{f}
}

func (f *fakeStore) InboundFailureStore() store.InboundFailureStore { return fakeFailureStore{f} }

func (f *fakeStore) EnsureEmailProfileAssignment(
	context.Context,
	model.EmailProfileAssignment,
) error {
	f.record("ensure_assignment")

	if f.staleAssignment {
		return store.ErrStaleEmailProfileAssignment
	}

	return nil
}

func (f *fakeStore) messageByID(id int64) *model.EmailMessage {
	for _, message := range f.messages {
		if message.ID == id {
			return message
		}
	}

	return nil
}

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
		if !match(message) {
			continue
		}

		identity := &store.EmailMessageIdentity{
			ID: message.ID, ThreadID: message.ThreadID, State: message.State,
		}
		for _, thread := range f.threads {
			if thread.ID == message.ThreadID {
				identity.ThreadKind = thread.Kind
				identity.ContactResolution = thread.ContactResolutionState
			}
		}

		return identity
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

func (f fakeThreadStore) ResolveContact(
	_ context.Context,
	domainID, threadID int64,
	state model.EmailContactResolutionState,
	contactID *int64,
) (bool, error) {
	f.record("resolve_contact")

	for _, thread := range f.threads {
		if thread.ID != threadID || thread.DomainID != domainID || thread.Kind != model.EmailThreadKindRegular {
			continue
		}
		if thread.ContactResolutionState != model.EmailContactResolutionPending &&
			thread.ContactResolutionState != model.EmailContactResolutionUnavailable {
			return false, nil
		}
		thread.ContactResolutionState = state
		thread.ContactID = contactID

		return true, nil
	}

	return false, nil
}

func (f fakeThreadStore) SetContactManually(
	_ context.Context,
	domainID, threadID int64,
	contactID *int64,
) (bool, error) {
	f.record("set_contact_manually")

	for _, thread := range f.threads {
		if thread.ID != threadID || thread.DomainID != domainID || thread.Kind != model.EmailThreadKindRegular {
			continue
		}
		thread.ContactID = contactID
		thread.ContactResolutionState = model.EmailContactResolutionUnlinked
		if contactID != nil {
			thread.ContactResolutionState = model.EmailContactResolutionResolved
		}

		return true, nil
	}

	return false, nil
}

func (f fakeThreadStore) FirstSenderAddress(_ context.Context, domainID, threadID int64) (string, error) {
	f.record("first_sender_address")
	if f.firstSenderErr != nil {
		return "", f.firstSenderErr
	}

	var first *model.EmailMessage
	for _, message := range f.messages {
		if message.DomainID != domainID || message.ThreadID != threadID ||
			(first != nil && message.ID >= first.ID) {
			continue
		}
		first = message
	}
	if first == nil {
		return "", nil
	}
	for _, recipient := range f.recipients[first.ID] {
		if recipient.Type == model.EmailRecipientTypeFrom && recipient.Ordinal == 0 {
			return recipient.NormalizedAddress, nil
		}
	}

	return "", nil
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

func (f fakeMessageStore) MarkReady(
	_ context.Context,
	_, messageID int64,
) (model.EmailMessageState, error) {
	f.record("mark_ready")

	message := f.messageByID(messageID)
	if message == nil {
		return "", nil
	}
	if message.State != model.EmailMessageStateProcessing {
		return message.State, nil
	}
	for _, attachment := range f.attachments[messageID] {
		if attachment.State == model.EmailAttachmentStatePending {
			return model.EmailMessageStateProcessing, nil
		}
	}
	message.State = model.EmailMessageStateReady

	return message.State, nil
}

func (f fakeMessageStore) MarkFailed(_ context.Context, _, messageID int64) (bool, error) {
	f.record("mark_failed")

	message := f.messageByID(messageID)
	if message == nil || message.State != model.EmailMessageStateProcessing {
		return false, nil
	}
	message.State = model.EmailMessageStateFailed

	return true, nil
}

func (f fakeMessageStore) IncrementAttachmentAttempts(
	_ context.Context,
	_, messageID int64,
) (int32, error) {
	f.record("increment_attempts")

	message := f.messageByID(messageID)
	if message == nil {
		return 0, nil
	}
	message.AttachmentAttempts++

	return message.AttachmentAttempts, nil
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

type fakeAttachmentStore struct{ *fakeStore }

var _ store.EmailMessageAttachmentStore = fakeAttachmentStore{}

func (f fakeAttachmentStore) CreateBulk(
	_ context.Context,
	_, messageID int64,
	attachments []*model.EmailMessageAttachment,
) error {
	f.record("create_attachments")
	for _, attachment := range attachments {
		stored := *attachment
		stored.MessageID = messageID
		f.attachments[messageID] = append(f.attachments[messageID], &stored)
	}

	return nil
}

func (f fakeAttachmentStore) List(
	_ context.Context,
	_, messageID int64,
) ([]*model.EmailMessageAttachment, error) {
	f.record("list_attachments")

	return f.attachments[messageID], nil
}

func (f fakeAttachmentStore) MarkStored(
	_ context.Context,
	_, messageID int64,
	position int32,
	fileID int64,
) (bool, error) {
	f.record("mark_stored")
	for _, attachment := range f.attachments[messageID] {
		if attachment.Position != position || attachment.State != model.EmailAttachmentStatePending {
			continue
		}
		stored := fileID
		attachment.FileID = &stored
		attachment.State = model.EmailAttachmentStateStored

		return true, nil
	}

	return false, nil
}

type fakeFailureStore struct{ *fakeStore }

var _ store.InboundFailureStore = fakeFailureStore{}

func (f fakeFailureStore) Record(_ context.Context, failure *model.InboundFailure) error {
	f.record("record_failure")
	f.failures = append(f.failures, failure)

	return nil
}
