package inbound

import (
	"context"
	"log/slog"

	contactsinfra "github.com/webitel/webitel-emails/infra/contacts"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// ContactResolver finds the contact of an email address outside the database
// transaction.
type ContactResolver interface {
	ResolveByEmail(ctx context.Context, domainID int64, address string) (contactsinfra.Resolution, error)
}

// contactResolution is what the final transaction has to store about the contact
// of a Thread. A nil resolution means nothing is to be written.
type contactResolution struct {
	State     model.EmailContactResolutionState
	ContactID *int64
}

// resolveContact matches the Thread to a contact by the address of its earliest
// message. An unreachable Contacts service does not decide the fate of the email:
// it only leaves the Thread without a contact, and a later message of the same
// conversation tries again. A failure of our own database is different — it is
// returned, so the email stays processing and is resolved on redelivery instead
// of being confirmed with a state it never really reached.
func (u *MessageCompleter) resolveContact(
	ctx context.Context,
	parsed *model.ParsedEmail,
	message storedMessage,
	log *slog.Logger,
) (*contactResolution, error) {
	if !message.needsContactResolution() {
		return nil, nil
	}

	// The earliest message of the Thread decides its contact, not the email that
	// happens to drive this processing.
	address, err := u.uow.EmailThreadStore().FirstSenderAddress(ctx, parsed.DomainID, message.ThreadID)
	if err != nil {
		return nil, err
	}
	if address == "" {
		log.Info("conversation has no sender address, so it has no contact")

		return &contactResolution{State: model.EmailContactResolutionNotFound}, nil
	}

	resolved, err := u.contacts.ResolveByEmail(ctx, parsed.DomainID, address)
	if err != nil {
		// Draining or shutdown is not an answer about the contact, so nothing is
		// written and the email is processed again later.
		if ctx.Err() != nil {
			log.Debug("contact resolution interrupted", "err", err)

			return nil, err
		}

		log.Warn("contact resolution failed", "category", "unavailable", "err", err)

		return &contactResolution{State: model.EmailContactResolutionUnavailable}, nil
	}

	switch resolved.Outcome {
	case contactsinfra.OutcomeResolved:
		contactID := resolved.ContactID

		return &contactResolution{State: model.EmailContactResolutionResolved, ContactID: &contactID}, nil
	case contactsinfra.OutcomeAmbiguous:
		return &contactResolution{State: model.EmailContactResolutionAmbiguous}, nil
	default:
		return &contactResolution{State: model.EmailContactResolutionNotFound}, nil
	}
}

// storeResolution writes the outcome in the caller's transaction. A write that
// applies to no row means an operator decided meanwhile, which always wins.
func (u *MessageCompleter) storeResolution(
	ctx context.Context,
	uow store.UnitOfWork,
	parsed *model.ParsedEmail,
	message storedMessage,
	resolution *contactResolution,
	log *slog.Logger,
) error {
	if resolution == nil {
		return nil
	}

	applied, err := uow.EmailThreadStore().ResolveContact(
		ctx, parsed.DomainID, message.ThreadID, resolution.State, resolution.ContactID,
	)
	if err != nil {
		return err
	}
	if !applied {
		log.Info("contact resolution skipped, the conversation was already decided",
			"thread_id", message.ThreadID, "state", resolution.State)

		return nil
	}

	log.Info("contact resolution stored",
		"thread_id", message.ThreadID,
		"state", resolution.State,
		"contact_id", resolution.ContactID,
	)

	return nil
}
