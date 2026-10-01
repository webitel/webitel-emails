package inbound

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// PersistenceHandler stores a parsed email as a Message of a Thread. A
// redelivery of a finished email writes nothing; a redelivery of an unfinished
// one continues its attachments.
type PersistenceHandler struct {
	uow       store.UnitOfWork
	completer *MessageCompleter
	log       *slog.Logger
}

var _ ParsedMessageHandler = (*PersistenceHandler)(nil)

// NewPersistenceHandler creates the handler that owns the persistence transaction.
func NewPersistenceHandler(
	uow store.UnitOfWork,
	completer *MessageCompleter,
	log *slog.Logger,
) *PersistenceHandler {
	return &PersistenceHandler{
		uow:       uow,
		completer: completer,
		log:       log.With("component", "inbound_persistence"),
	}
}

// Handle writes the email, or confirms that it is already stored. Returning nil
// lets the IMAP cursor move past the email, so it only happens after a commit.
func (h *PersistenceHandler) Handle(ctx context.Context, parsed *model.ParsedEmail) error {
	if parsed == nil {
		return fmt.Errorf("persistence: parsed email is required")
	}

	// Set when the email still has attachments to upload, which happens after
	// the transaction below has committed.
	var unfinished *storedMessage

	err := h.uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
		// Held until the transaction ends, so a concurrent delivery of the same
		// conversation cannot resolve its Thread against a half-written one.
		if err := uow.LockEmailProfile(ctx, parsed.ProfileID); err != nil {
			return err
		}
		if err := uow.EnsureEmailProfileAssignment(ctx, parsed.Assignment); err != nil {
			return err
		}

		stored, err := locateStored(ctx, uow, parsed)
		if err != nil {
			return err
		}
		if stored != nil {
			if stored.State == model.EmailMessageStateProcessing {
				unfinished = &storedMessage{
					ID:                stored.ID,
					ThreadID:          stored.ThreadID,
					ThreadKind:        stored.ThreadKind,
					ContactResolution: stored.ContactResolution,
				}
				h.log.Info("continuing an unfinished inbound email",
					"domain_id", parsed.DomainID,
					"profile_id", parsed.ProfileID,
					"message_id", stored.ID,
					"thread_id", stored.ThreadID,
				)

				return nil
			}

			return h.duplicateOutcome(parsed, stored)
		}

		unfinished, err = h.persist(ctx, uow, parsed)

		return err
	})
	if err != nil || unfinished == nil {
		return err
	}

	return h.completer.Complete(ctx, parsed, *unfinished)
}

// persist writes the Thread, the Message, its recipients and the whole
// attachment manifest. A Message with a part to upload stays processing and is
// returned for completion; anything else is ready at once, as before.
func (h *PersistenceHandler) persist(
	ctx context.Context,
	uow store.UnitOfWork,
	parsed *model.ParsedEmail,
) (*storedMessage, error) {
	thread, err := resolveThread(ctx, uow, parsed)
	if err != nil {
		return nil, err
	}

	attachments := buildAttachments(parsed)
	unfinished := storedMessage{
		ThreadID:          thread.ID,
		ThreadKind:        thread.Kind,
		ContactResolution: thread.ContactResolutionState,
	}

	// The email stays invisible while anything it owns is unfinished: a file still
	// to upload, or a conversation still to match to a contact.
	state := model.EmailMessageStateReady
	if hasPendingAttachments(attachments) || unfinished.needsContactResolution() {
		state = model.EmailMessageStateProcessing
	}

	// A conflict here means a delivery slipped past both the lookup and the
	// lock. Returning it rolls back this Thread too, so nothing is left behind;
	// the next delivery finds the stored email and finishes as a duplicate.
	message, err := uow.EmailMessageStore().Create(ctx, buildMessage(parsed, thread.ID, state))
	if err != nil {
		return nil, err
	}

	if err := uow.EmailRecipientStore().CreateBulk(ctx, parsed.DomainID, message.ID, buildRecipients(parsed)); err != nil {
		return nil, err
	}

	if err := uow.EmailMessageAttachmentStore().CreateBulk(
		ctx, parsed.DomainID, message.ID, attachments,
	); err != nil {
		return nil, err
	}

	if state == model.EmailMessageStateProcessing {
		// The Thread advances only when the Message becomes visible.
		unfinished.ID = message.ID

		return &unfinished, nil
	}

	return nil, uow.EmailThreadStore().AdvanceLastMessage(
		ctx, parsed.DomainID, parsed.ProfileID, thread.ID, message.ReceivedAt,
	)
}

// resolveThread applies the agreed lookup order and stops at the first match:
// a known ancestor, then a sibling, then a reply that arrived first, then a new
// Thread. Threads are never merged, so only the first rule that matches decides.
func resolveThread(ctx context.Context, uow store.UnitOfWork, parsed *model.ParsedEmail) (*model.EmailThread, error) {
	threads := uow.EmailThreadStore()
	lookup := store.EmailThreadLookup{
		DomainID:  parsed.DomainID,
		ProfileID: parsed.ProfileID,
		// An ordinary email must never continue a closed service Thread.
		OnlyRegular: parsed.Kind == model.EmailKindRegular,
	}

	if ancestors := ancestorMessageIDs(parsed); len(ancestors) > 0 {
		thread, err := threads.LocateByMessageIDs(ctx, lookup, ancestors)
		if thread != nil || err != nil {
			return thread, err
		}
	}

	// The parent is unknown, but another reply to it may already be stored.
	if parsed.InReplyTo != "" {
		thread, err := threads.LocateByInReplyTo(ctx, lookup, parsed.InReplyTo)
		if thread != nil || err != nil {
			return thread, err
		}
	}

	// The reverse case: a reply to this email arrived before the email itself.
	// A generated identifier is ours alone, so nothing can ever reference it.
	if !parsed.MessageIDGenerated {
		thread, err := threads.LocateByInReplyTo(ctx, lookup, parsed.MessageID)
		if thread != nil || err != nil {
			return thread, err
		}
	}

	return createThread(ctx, threads, parsed)
}

// ancestorMessageIDs lists the identifiers this email may descend from, ordered
// by increasing precision: References oldest first, then the direct parent, then
// the email a bounce reports on.
func ancestorMessageIDs(parsed *model.ParsedEmail) []string {
	ancestors := make([]string, 0, len(parsed.References)+2)
	ancestors = append(ancestors, parsed.References...)
	if parsed.InReplyTo != "" {
		ancestors = append(ancestors, parsed.InReplyTo)
	}
	// A delivery report names the email it failed to deliver, which is a stronger
	// link than any header chain it happens to carry.
	if parsed.Bounce != nil && parsed.Bounce.OriginalMessageID != "" {
		ancestors = append(ancestors, parsed.Bounce.OriginalMessageID)
	}

	return keepLastOccurrence(ancestors)
}

// keepLastOccurrence drops earlier copies of a repeated identifier. A parent
// usually appears in References as well, and only its last position carries the
// priority the caller intended.
func keepLastOccurrence(values []string) []string {
	strongest := make(map[string]int, len(values))
	for index, value := range values {
		strongest[value] = index
	}

	result := make([]string, 0, len(strongest))
	for index, value := range values {
		if strongest[value] == index {
			result = append(result, value)
		}
	}

	return result
}

// createThread opens a conversation for an email with no known ancestor. An
// auto-reply or a bounce starts a closed service Thread, so it never reaches an
// operator and never waits in a queue.
func createThread(
	ctx context.Context,
	threads store.EmailThreadStore,
	parsed *model.ParsedEmail,
) (*model.EmailThread, error) {
	thread := &model.EmailThread{
		DomainID:  parsed.DomainID,
		ProfileID: parsed.ProfileID,
		Kind:      model.EmailThreadKindRegular,
		Subject:   parsed.Subject,
		Status:    model.EmailThreadStatusNew,
		// The contact is resolved after the Message is stored, by task 7.
		ContactResolutionState: model.EmailContactResolutionPending,
	}
	if parsed.Kind != model.EmailKindRegular {
		completedAt := parsed.ReceivedAt
		thread.Kind = model.EmailThreadKindService
		thread.Status = model.EmailThreadStatusProcessed
		thread.CompletedAt = &completedAt
		// A service Thread never resolves a contact.
		thread.ContactResolutionState = model.EmailContactResolutionNotApplicable
	}

	return threads.Create(ctx, thread)
}

// locateStored finds the email by the identity it can be recognized by. A real
// Message-ID is enough; a generated one moves with the mailbox, so the raw
// checksum is what survives a UIDVALIDITY change.
func locateStored(
	ctx context.Context,
	uow store.UnitOfWork,
	parsed *model.ParsedEmail,
) (*store.EmailMessageIdentity, error) {
	messages := uow.EmailMessageStore()

	stored, err := messages.LocateByMessageID(ctx, parsed.DomainID, parsed.ProfileID, parsed.MessageID)
	if stored != nil || err != nil {
		return stored, err
	}

	if parsed.Mailbox != "" {
		stored, err = messages.LocateByIMAPIdentity(ctx, parsed.DomainID, parsed.ProfileID, model.EmailIMAPIdentity{
			Mailbox:     parsed.Mailbox,
			UIDValidity: parsed.UIDValidity,
			UID:         parsed.UID,
		})
		if stored != nil || err != nil {
			return stored, err
		}
	}

	// A generated identifier moves with the mailbox coordinates, so only the raw
	// checksum recognizes the same email after UIDVALIDITY changed.
	if !parsed.MessageIDGenerated {
		return nil, nil
	}

	return messages.LocateByRawChecksum(ctx, parsed.DomainID, parsed.ProfileID, parsed.RawSHA256)
}

// duplicateOutcome confirms an email that is already finished, successfully or
// not. An unfinished one never reaches here: it is continued instead.
func (h *PersistenceHandler) duplicateOutcome(
	parsed *model.ParsedEmail,
	stored *store.EmailMessageIdentity,
) error {
	// The identifiers are the idempotent result; task 8 writes the Outbox row
	// from them inside this same transaction.
	h.log.Debug("inbound email is already stored",
		"profile_id", parsed.ProfileID,
		"message_id", parsed.MessageID,
		"stored_message_id", stored.ID,
		"thread_id", stored.ThreadID,
		"state", stored.State,
	)

	return nil
}

func buildMessage(
	parsed *model.ParsedEmail,
	threadID int64,
	state model.EmailMessageState,
) *model.EmailMessage {
	message := &model.EmailMessage{
		DomainID:           parsed.DomainID,
		ThreadID:           threadID,
		ProfileID:          parsed.ProfileID,
		Direction:          model.EmailMessageDirectionInbound,
		Kind:               parsed.Kind,
		State:              state,
		MessageID:          parsed.MessageID,
		MessageIDGenerated: parsed.MessageIDGenerated,
		RawSHA256:          parsed.RawSHA256,
		InReplyTo:          parsed.InReplyTo,
		References:         parsed.References,
		Subject:            parsed.Subject,
		TextBody:           parsed.TextBody,
		HTMLBody:           parsed.HTMLBody,
		Bounce:             parsed.Bounce,
		SentAt:             parsed.SentAt,
		ReceivedAt:         parsed.ReceivedAt,
	}
	if parsed.Mailbox != "" {
		message.IMAP = &model.EmailIMAPIdentity{
			Mailbox:     parsed.Mailbox,
			UIDValidity: parsed.UIDValidity,
			UID:         parsed.UID,
		}
	}

	return message
}

// buildAttachments turns the parsed parts into the manifest, keeping their
// order. A part the parser skipped is recorded with its reason and never
// uploaded; everything else starts as pending.
func buildAttachments(parsed *model.ParsedEmail) []*model.EmailMessageAttachment {
	attachments := make([]*model.EmailMessageAttachment, 0, len(parsed.Parts))
	for index, part := range parsed.Parts {
		attachment := &model.EmailMessageAttachment{
			DomainID:      parsed.DomainID,
			ContentID:     part.ContentID,
			Disposition:   part.Disposition,
			FileName:      part.Name,
			MimeType:      part.ContentType,
			Size:          part.Size,
			Position:      int32(index),
			State:         model.EmailAttachmentStatePending,
			SkippedReason: part.SkippedReason,
		}
		if part.SkippedReason != "" {
			attachment.State = model.EmailAttachmentStateSkipped
		}

		attachments = append(attachments, attachment)
	}

	return attachments
}

func hasPendingAttachments(attachments []*model.EmailMessageAttachment) bool {
	for _, attachment := range attachments {
		if attachment.State == model.EmailAttachmentStatePending {
			return true
		}
	}

	return false
}

// buildRecipients flattens the address headers, keeping each address under its
// own header and in the order the email listed it.
func buildRecipients(parsed *model.ParsedEmail) []*model.EmailRecipient {
	var sender []model.EmailAddress
	if parsed.Sender != nil {
		sender = []model.EmailAddress{*parsed.Sender}
	}

	groups := []struct {
		kind      model.EmailRecipientType
		addresses []model.EmailAddress
	}{
		{model.EmailRecipientTypeFrom, parsed.From},
		{model.EmailRecipientTypeSender, sender},
		{model.EmailRecipientTypeReplyTo, parsed.ReplyTo},
		{model.EmailRecipientTypeTo, parsed.To},
		{model.EmailRecipientTypeCc, parsed.Cc},
		{model.EmailRecipientTypeBcc, parsed.Bcc},
	}

	recipients := make([]*model.EmailRecipient, 0, len(parsed.To)+len(parsed.Cc)+len(parsed.From))
	for _, group := range groups {
		for index, address := range group.addresses {
			if address.Address == "" {
				continue
			}
			recipients = append(recipients, &model.EmailRecipient{
				DomainID:          parsed.DomainID,
				Type:              group.kind,
				Address:           address.Address,
				NormalizedAddress: address.NormalizedAddress,
				DisplayName:       address.Name,
				Ordinal:           int32(index),
			})
		}
	}

	return recipients
}
