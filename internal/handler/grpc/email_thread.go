package grpc

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/service"
)

// EmailThreadsServer exposes Email Threads: reading them and the manual contact
// decisions of their agent.
type EmailThreadsServer struct {
	emailpb.UnimplementedEmailThreadsServer

	service *service.EmailThreadService
	reads   *service.EmailThreadReadService
}

var _ emailpb.EmailThreadsServer = (*EmailThreadsServer)(nil)

// NewEmailThreadsServer creates the Email Thread gRPC handler.
func NewEmailThreadsServer(
	threadService *service.EmailThreadService,
	readService *service.EmailThreadReadService,
) *EmailThreadsServer {
	return &EmailThreadsServer{service: threadService, reads: readService}
}

// ListEmailThreads returns one page of the caller's Threads.
func (s *EmailThreadsServer) ListEmailThreads(
	ctx context.Context,
	req *emailpb.ListEmailThreadsRequest,
) (*emailpb.EmailThreadList, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}
	listRequest, err := threadListRequestFromProto(req)
	if err != nil {
		return nil, err
	}

	views, next, err := s.reads.ListThreads(ctx, session.DomainID, session.UserID, listRequest)
	if err != nil {
		return nil, err
	}

	items := make([]*emailpb.EmailThread, 0, len(views))
	for _, view := range views {
		items = append(items, emailThreadToProto(view))
	}

	return &emailpb.EmailThreadList{Items: items, Next: next}, nil
}

// LocateEmailThread returns one Thread the caller may see.
func (s *EmailThreadsServer) LocateEmailThread(
	ctx context.Context,
	req *emailpb.LocateEmailThreadRequest,
) (*emailpb.EmailThread, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}

	view, err := s.reads.LocateThread(ctx, session.DomainID, session.UserID, req.GetThreadId())
	if err != nil {
		return nil, err
	}

	return emailThreadToProto(view), nil
}

// ListEmailThreadMessages returns one page of the emails of a Thread the caller
// may see.
func (s *EmailThreadsServer) ListEmailThreadMessages(
	ctx context.Context,
	req *emailpb.ListEmailThreadMessagesRequest,
) (*emailpb.EmailMessageList, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}
	desc, err := messageOrderFromProto(req.GetOrder())
	if err != nil {
		return nil, err
	}

	messages, nextCursor, err := s.reads.ListMessages(ctx, session.DomainID, session.UserID, service.EmailMessageListRequest{
		ThreadID: req.GetThreadId(),
		Size:     req.GetSize(),
		Cursor:   req.GetCursor(),
		Desc:     desc,
	})
	if err != nil {
		return nil, err
	}

	items := make([]*emailpb.EmailMessage, 0, len(messages))
	for _, message := range messages {
		items = append(items, emailMessageToProto(message))
	}

	return &emailpb.EmailMessageList{Items: items, Next: nextCursor != "", NextCursor: nextCursor}, nil
}

// GetEmailAttachmentLink returns a short-lived download link to a stored
// attachment of an email the caller may see. The link is never logged.
func (s *EmailThreadsServer) GetEmailAttachmentLink(
	ctx context.Context,
	req *emailpb.GetEmailAttachmentLinkRequest,
) (*emailpb.GetEmailAttachmentLinkResponse, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}

	link, expiresAt, err := s.reads.AttachmentLink(
		ctx, session.DomainID, session.UserID, req.GetThreadId(), req.GetMessageId(), req.GetAttachmentId(),
	)
	if err != nil {
		return nil, err
	}

	return &emailpb.GetEmailAttachmentLinkResponse{Url: link, ExpiresAt: unixMilli(expiresAt)}, nil
}

// ListEmailThreadActions returns one page of the recorded transitions of a Thread
// the caller may see.
func (s *EmailThreadsServer) ListEmailThreadActions(
	ctx context.Context,
	req *emailpb.ListEmailThreadActionsRequest,
) (*emailpb.EmailThreadActionList, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}

	actions, nextCursor, err := s.reads.ListActions(ctx, session.DomainID, session.UserID, service.EmailThreadActionListRequest{
		ThreadID: req.GetThreadId(),
		Size:     req.GetSize(),
		Cursor:   req.GetCursor(),
	})
	if err != nil {
		return nil, err
	}

	items := make([]*emailpb.EmailThreadAction, 0, len(actions))
	for _, action := range actions {
		items = append(items, emailThreadActionToProto(action))
	}

	return &emailpb.EmailThreadActionList{Items: items, Next: nextCursor != "", NextCursor: nextCursor}, nil
}

// LocateEmailThreadMessage returns one email of a Thread the caller may see.
func (s *EmailThreadsServer) LocateEmailThreadMessage(
	ctx context.Context,
	req *emailpb.LocateEmailThreadMessageRequest,
) (*emailpb.EmailMessage, error) {
	session, err := emailThreadSession(ctx)
	if err != nil {
		return nil, err
	}

	message, err := s.reads.LocateMessage(ctx, session.DomainID, session.UserID, req.GetThreadId(), req.GetMessageId())
	if err != nil {
		return nil, err
	}

	return emailMessageToProto(message), nil
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
		session.UserID,
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

	if err := s.service.UnbindContact(ctx, session.DomainID, session.UserID, req.GetThreadId()); err != nil {
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
