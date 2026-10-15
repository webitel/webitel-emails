package grpc

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/service"
)

// EmailThreadsServer exposes manual contact decisions for Email Threads.
type EmailThreadsServer struct {
	emailpb.UnimplementedEmailThreadsServer

	service *service.EmailThreadService
}

var _ emailpb.EmailThreadsServer = (*EmailThreadsServer)(nil)

// NewEmailThreadsServer creates the Email Thread gRPC handler.
func NewEmailThreadsServer(threadService *service.EmailThreadService) *EmailThreadsServer {
	return &EmailThreadsServer{service: threadService}
}

// BindEmailThreadContact links a Thread to an existing Contact of the caller's domain.
func (s *EmailThreadsServer) BindEmailThreadContact(
	ctx context.Context,
	req *emailpb.BindEmailThreadContactRequest,
) (*emailpb.BindEmailThreadContactResponse, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.service.BindContact(
		ctx,
		session.DomainID,
		req.GetThreadId(),
		req.GetContactId(),
	); err != nil {
		return nil, err
	}

	return &emailpb.BindEmailThreadContactResponse{}, nil
}

// UnbindEmailThreadContact removes the Contact selected for a Thread.
func (s *EmailThreadsServer) UnbindEmailThreadContact(
	ctx context.Context,
	req *emailpb.UnbindEmailThreadContactRequest,
) (*emailpb.UnbindEmailThreadContactResponse, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.service.UnbindContact(ctx, session.DomainID, req.GetThreadId()); err != nil {
		return nil, err
	}

	return &emailpb.UnbindEmailThreadContactResponse{}, nil
}

func emailThreadSession(ctx context.Context) (*auth.Session, error) {
	session, ok := auth.FromContext(ctx)
	if !ok {
		return nil, kiterrors.Unauthenticated(
			"unauthorized",
			kiterrors.WithID("email.thread.auth.session_missing"),
		)
	}

	return session, nil
}
