package grpc_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	kitinterceptors "github.com/webitel/webitel-go-kit/pkg/interceptors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/infra/server/grpc/interceptors"
	"github.com/webitel/webitel-emails/internal/auth"
	handler "github.com/webitel/webitel-emails/internal/handler/grpc"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
	"github.com/webitel/webitel-emails/internal/store"
)

// fakeAuth resolves the token header into a prepared session.
type fakeAuth struct{ sessions map[string]*auth.Session }

func (f fakeAuth) AuthorizeFromContext(ctx context.Context, _ string, _ auth.AccessMode) (*auth.Session, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if tokens := md.Get("x-webitel-access"); len(tokens) > 0 {
		if session, ok := f.sessions[tokens[0]]; ok {
			return session, nil
		}
	}
	return nil, status.Error(codes.Unauthenticated, "unknown token")
}

// readStore sees thread 1 as current and assigned to user 70, thread 2 as its
// history, and nothing else.
type readStore struct{ store.EmailThreadReadStore }

func (readStore) Rights(_ context.Context, domainID, userID, threadID int64) (store.EmailThreadRights, error) {
	if domainID != 1 || userID != 70 {
		return store.EmailThreadRights{}, nil
	}
	switch threadID {
	case 1:
		return store.EmailThreadRights{Visible: true, Assigned: true}, nil
	case 2:
		return store.EmailThreadRights{Visible: true}, nil
	}
	return store.EmailThreadRights{}, nil
}

func (s readStore) LocateThread(ctx context.Context, domainID, userID, threadID int64) (*model.EmailThreadView, error) {
	if rights, _ := s.Rights(ctx, domainID, userID, threadID); !rights.Visible {
		return nil, nil
	}
	return &model.EmailThreadView{ID: threadID, Access: model.EmailThreadAccessCurrent}, nil
}

func (readStore) ListMessages(context.Context, model.EmailMessageFilter) ([]*model.EmailMessageView, bool, error) {
	fileID := int64(777)
	return []*model.EmailMessageView{{
		ID: 10, RawHTML: `<p>hi</p><img src="cid:logo@x">`,
		Attachments: []model.EmailAttachmentView{{
			ID: 1, FileID: &fileID, ContentID: "logo@x", MimeType: "image/png",
			Disposition: model.EmailPartDispositionInline, State: model.EmailAttachmentStateStored,
		}},
	}}, false, nil
}

func (readStore) LocateAttachment(context.Context, int64, int64, int64, int64, int64) (*model.EmailAttachmentView, error) {
	fileID := int64(777)
	return &model.EmailAttachmentView{ID: 1, FileID: &fileID, State: model.EmailAttachmentStateStored}, nil
}

type threadStore struct {
	store.EmailThreadStore
	mu    sync.Mutex
	calls int
}

func (s *threadStore) SetContactManually(context.Context, int64, int64, int64, *int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return true, nil
}

type contacts struct{}

func (contacts) VerifyContact(context.Context, int64, int64) (bool, error) { return true, nil }

// linker issues a recognizable link, or fails as unavailable storage.
type linker struct{ unavailable bool }

func (l linker) InlineLinks(_ context.Context, _ int64, ids []int64) (map[int64]string, error) {
	if l.unavailable {
		return nil, service.ErrFileLinksUnavailable
	}
	links := map[int64]string{}
	for _, id := range ids {
		links[id] = "https://storage.example/secret-link"
	}
	return links, nil
}

func (l linker) Link(context.Context, int64, int64) (string, time.Time, error) {
	if l.unavailable {
		return "", time.Time{}, service.ErrFileLinksUnavailable
	}
	return "https://storage.example/secret-link", time.Now().Add(time.Minute), nil
}

type harness struct {
	client  emailpb.EmailThreadsClient
	threads *threadStore
	logs    *bytes.Buffer
}

func start(t *testing.T, links linker) harness {
	t.Helper()
	readOnly := map[string]auth.Scope{"email_thread": {Access: "r", OBAC: true}}
	readWrite := map[string]auth.Scope{"email_thread": {Access: "rw", OBAC: true}}
	sessions := map[string]*auth.Session{
		"reader":    {UserID: 70, DomainID: 1, Scopes: readOnly},
		"writer":    {UserID: 70, DomainID: 1, Scopes: readWrite},
		"no-scope":  {UserID: 70, DomainID: 1, Scopes: map[string]auth.Scope{}},
		"superread": {UserID: 99, DomainID: 1, SuperRead: true, Scopes: map[string]auth.Scope{"email_thread": {OBAC: true}}},
	}

	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	threads := &threadStore{}
	reads := readStore{}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(
		kitinterceptors.UnaryServerErrorInterceptor(),
		interceptors.NewUnaryAuthInterceptor(fakeAuth{sessions: sessions}),
	))
	emailpb.RegisterEmailThreadsServer(server, handler.NewEmailThreadsServer(
		service.NewEmailThreadService(threads, reads, contacts{}),
		service.NewEmailThreadReadService(reads, links, log),
	))

	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return harness{client: emailpb.NewEmailThreadsClient(conn), threads: threads, logs: logs}
}

func as(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-webitel-access", token)
}

func TestEmailThreadsAuthorization(t *testing.T) {
	h := start(t, linker{})

	if _, err := h.client.LocateEmailThread(as("reader"), &emailpb.LocateEmailThreadRequest{ThreadId: 1}); err != nil {
		t.Fatalf("read with email_thread:read: %v", err)
	}
	if _, err := h.client.LocateEmailThread(as("no-scope"), &emailpb.LocateEmailThreadRequest{ThreadId: 1}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read without the scope: %v", err)
	}
	bind := &emailpb.BindEmailThreadContactRequest{ThreadId: 1, ContactId: 5}
	if _, err := h.client.BindEmailThreadContact(as("reader"), bind); status.Code(err) != codes.PermissionDenied || h.threads.calls != 0 {
		t.Fatalf("bind with read only: %v, store calls %d", err, h.threads.calls)
	}
	if _, err := h.client.BindEmailThreadContact(as("writer"), bind); err != nil || h.threads.calls != 1 {
		t.Fatalf("bind with update: %v, store calls %d", err, h.threads.calls)
	}

	// SuperRead passes the object class check but never the visibility boundary.
	if _, err := h.client.LocateEmailThread(as("superread"), &emailpb.LocateEmailThreadRequest{ThreadId: 1}); status.Code(err) != codes.NotFound {
		t.Fatalf("SuperRead opened a thread: %v", err)
	}
}

func TestEmailThreadsErrorCodes(t *testing.T) {
	h := start(t, linker{unavailable: true})
	ctx := as("writer")

	cases := map[string]struct {
		call func() error
		want codes.Code
	}{
		"invisible thread": {func() error {
			_, err := h.client.LocateEmailThread(ctx, &emailpb.LocateEmailThreadRequest{ThreadId: 3})
			return err
		}, codes.NotFound},
		"contact of a history thread": {func() error {
			_, err := h.client.BindEmailThreadContact(ctx, &emailpb.BindEmailThreadContactRequest{ThreadId: 2, ContactId: 5})
			return err
		}, codes.FailedPrecondition},
		"page too large": {func() error {
			_, err := h.client.ListEmailThreads(ctx, &emailpb.ListEmailThreadsRequest{Size: 101})
			return err
		}, codes.InvalidArgument},
		"bad cursor": {func() error {
			_, err := h.client.ListEmailThreadMessages(ctx, &emailpb.ListEmailThreadMessagesRequest{ThreadId: 1, Cursor: "x"})
			return err
		}, codes.InvalidArgument},
		"attachment link without storage": {func() error {
			_, err := h.client.GetEmailAttachmentLink(ctx, &emailpb.GetEmailAttachmentLinkRequest{ThreadId: 1, MessageId: 10, AttachmentId: 1})
			return err
		}, codes.Unavailable},
	}
	for name, c := range cases {
		if got := status.Code(c.call()); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}

	// Without storage the messages are still read, without their inline images.
	page, err := h.client.ListEmailThreadMessages(ctx, &emailpb.ListEmailThreadMessagesRequest{ThreadId: 1})
	if err != nil || len(page.GetItems()) != 1 {
		t.Fatalf("messages without storage: %v", err)
	}
	message := page.GetItems()[0]
	if message.GetInlineContentStatus() != emailpb.EmailInlineContentStatus_EMAIL_INLINE_CONTENT_STATUS_UNAVAILABLE ||
		!strings.Contains(message.GetBodyHtml(), "<p>hi</p>") || strings.Contains(message.GetBodyHtml(), "cid:") {
		t.Fatalf("degraded message %+v", message)
	}
}

func TestEmailThreadsNeverLogLinks(t *testing.T) {
	h := start(t, linker{})
	ctx := as("reader")

	link, err := h.client.GetEmailAttachmentLink(ctx, &emailpb.GetEmailAttachmentLinkRequest{ThreadId: 1, MessageId: 10, AttachmentId: 1})
	if err != nil || link.GetUrl() == "" || link.GetExpiresAt() == 0 {
		t.Fatalf("link %+v, %v", link, err)
	}
	page, err := h.client.ListEmailThreadMessages(ctx, &emailpb.ListEmailThreadMessagesRequest{ThreadId: 1})
	if err != nil || !strings.Contains(page.GetItems()[0].GetBodyHtml(), "secret-link") {
		t.Fatalf("inline link missing: %v", err)
	}
	if strings.Contains(h.logs.String(), "secret-link") {
		t.Fatalf("a link reached the log: %s", h.logs.String())
	}
}
