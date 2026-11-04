package service

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/internal/store"
)

// ContactVerifier reports whether a contact belongs to a domain, so a manual
// binding cannot reach the contacts of another tenant.
type ContactVerifier interface {
	VerifyContact(ctx context.Context, domainID, contactID int64) (bool, error)
}

// ThreadRightsReader tells what a user may do with a Thread.
type ThreadRightsReader interface {
	Rights(ctx context.Context, domainID, userID, threadID int64) (store.EmailThreadRights, error)
}

// EmailThreadService serves operator decisions about a Thread's contact. Only the
// current agent after Take may change it, by the possibly lagging projection.
type EmailThreadService struct {
	threads  store.EmailThreadStore
	rights   ThreadRightsReader
	contacts ContactVerifier
}

// NewEmailThreadService creates the Thread service.
func NewEmailThreadService(
	threads store.EmailThreadStore,
	rights ThreadRightsReader,
	contacts ContactVerifier,
) *EmailThreadService {
	return &EmailThreadService{threads: threads, rights: rights, contacts: contacts}
}

// BindContact links a Thread to a contact chosen by an operator. It outranks any
// automatic outcome, including one that arrives later.
func (s *EmailThreadService) BindContact(ctx context.Context, domainID, userID, threadID, contactID int64) error {
	if threadID <= 0 {
		return errThreadIDRequired()
	}
	// The Thread is checked first, so Contacts is never asked on behalf of a
	// user who may not change it.
	if err := s.requireAssigned(ctx, domainID, userID, threadID); err != nil {
		return err
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

	return s.setContact(ctx, domainID, userID, threadID, &contactID)
}

// UnbindContact removes the contact of a Thread and keeps it that way: automatic
// resolution does not write over an unlinked Thread.
func (s *EmailThreadService) UnbindContact(ctx context.Context, domainID, userID, threadID int64) error {
	if threadID <= 0 {
		return errThreadIDRequired()
	}
	if err := s.requireAssigned(ctx, domainID, userID, threadID); err != nil {
		return err
	}

	return s.setContact(ctx, domainID, userID, threadID, nil)
}

func (s *EmailThreadService) setContact(
	ctx context.Context,
	domainID, userID, threadID int64,
	contactID *int64,
) error {
	applied, err := s.threads.SetContactManually(ctx, domainID, userID, threadID, contactID)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	// The update repeats the checks; when it applied nothing, the assignment or
	// the Thread changed meanwhile, so the reason is read again.
	if err := s.requireAssigned(ctx, domainID, userID, threadID); err != nil {
		return err
	}

	return errThreadNotAssigned()
}

// requireAssigned reports an invisible Thread as not found and a visible one not
// assigned to the user as a failed precondition.
func (s *EmailThreadService) requireAssigned(ctx context.Context, domainID, userID, threadID int64) error {
	rights, err := s.rights.Rights(ctx, domainID, userID, threadID)
	if err != nil {
		return err
	}
	if !rights.Visible {
		return errThreadNotFound()
	}
	if !rights.Assigned {
		return errThreadNotAssigned()
	}

	return nil
}

func errThreadNotAssigned() error {
	return kiterrors.New(
		"email thread is not assigned to the caller",
		kiterrors.WithID("email.thread.not_assigned"),
		kiterrors.WithCode(codes.FailedPrecondition),
	)
}

func errThreadIDRequired() error {
	return kiterrors.InvalidArgument(
		"email thread id is required",
		kiterrors.WithID("email.thread.id_required"),
	)
}
