package service

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/store"
)

// ContactVerifier reports whether a contact belongs to a domain, so a manual
// binding cannot reach the contacts of another tenant.
type ContactVerifier interface {
	VerifyContact(ctx context.Context, domainID, contactID int64) (bool, error)
}

// EmailThreadService serves the operator decisions about the contact of a Thread.
// Creating a contact is not part of it: that happens in Contacts, under the
// session of the operator who creates it.
type EmailThreadService struct {
	threads  store.EmailThreadStore
	contacts ContactVerifier
}

// NewEmailThreadService creates the Thread service.
func NewEmailThreadService(threads store.EmailThreadStore, contacts ContactVerifier) *EmailThreadService {
	return &EmailThreadService{threads: threads, contacts: contacts}
}

// BindContact links a Thread to a contact chosen by an operator. It outranks any
// automatic outcome, including one that arrives later.
func (s *EmailThreadService) BindContact(ctx context.Context, domainID, threadID, contactID int64) error {
	if threadID <= 0 {
		return errThreadIDRequired()
	}
	if contactID <= 0 {
		return kiterrors.InvalidArgument(
			"contact id is required",
			kiterrors.WithID("email.thread.contact_id_required"),
		)
	}

	// Checked against the caller's domain, because the Thread update below knows
	// nothing about the contacts of another tenant.
	own, err := s.contacts.VerifyContact(ctx, domainID, contactID)
	if err != nil {
		return err
	}
	if !own {
		return kiterrors.NotFound(
			"contact not found",
			kiterrors.WithID("email.thread.contact_not_found"),
		)
	}

	return s.setContact(ctx, domainID, threadID, &contactID)
}

// UnbindContact removes the contact of a Thread and keeps it that way: automatic
// resolution does not write over an unlinked Thread.
func (s *EmailThreadService) UnbindContact(ctx context.Context, domainID, threadID int64) error {
	if threadID <= 0 {
		return errThreadIDRequired()
	}

	return s.setContact(ctx, domainID, threadID, nil)
}

func (s *EmailThreadService) setContact(
	ctx context.Context,
	domainID, threadID int64,
	contactID *int64,
) error {
	applied, err := s.threads.SetContactManually(ctx, domainID, threadID, contactID)
	if err != nil {
		return err
	}
	// A Thread of another domain and a service Thread are both reported as absent:
	// an operator has no business with either, and the difference would tell them
	// something about data they cannot see.
	if !applied {
		return kiterrors.NotFound(
			"email thread not found",
			kiterrors.WithID("email.thread.not_found"),
		)
	}

	return nil
}

func errThreadIDRequired() error {
	return kiterrors.InvalidArgument(
		"email thread id is required",
		kiterrors.WithID("email.thread.id_required"),
	)
}
