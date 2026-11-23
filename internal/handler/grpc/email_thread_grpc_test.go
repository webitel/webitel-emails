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

	"github.com/google/uuid"
	kitinterceptors "github.com/webitel/webitel-go-kit/pkg/interceptors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra/server/grpc/interceptors"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
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
type readStore struct {
	store.EmailThreadReadStore
	sent *sendUoW
}

func (readStore) UserName(context.Context, int64, int64) (string, error) { return "Olena", nil }

// LocateMessageByRequest returns the one email sendUoW accepted.
func (s readStore) LocateMessageByRequest(_ context.Context, _, _ int64, requestID uuid.UUID) (*model.EmailMessageView, []byte, error) {
	s.sent.mu.Lock()
	defer s.sent.mu.Unlock()
	if s.sent.message == nil || s.sent.message.RequestID != requestID {
		return nil, nil, nil
	}
	m := s.sent.message
	return &model.EmailMessageView{
		ID: m.ID, ThreadID: m.ThreadID, Direction: model.EmailMessageDirectionOutbound,
		Subject: m.Subject, DeliveryStatus: model.EmailDeliveryStatusPending,
	}, m.RequestFingerprint, nil
}

// sendUoW accepts replies in thread 1, the current attempt 100 of user 70, and keeps
// the last one; the writes around the message are no-ops.
type sendUoW struct {
	store.UnitOfWork
	mu      sync.Mutex
	message *model.EmailMessage
	stores  int
}

func (u *sendUoW) WithinTransaction(ctx context.Context, fn func(context.Context, store.UnitOfWork) error) error {
	return fn(ctx, u)
}
func (u *sendUoW) EmailOutboundStore() store.EmailOutboundStore { return sendOutbound{u: u} }
func (u *sendUoW) EmailRecipientStore() store.EmailRecipientStore {
	return noRecipients{}
}
func (u *sendUoW) EmailMessageAttachmentStore() store.EmailMessageAttachmentStore {
	return noParts{}
}
func (u *sendUoW) EmailThreadStore() store.EmailThreadStore                 { return noThreads{} }
func (u *sendUoW) EmailWorkspaceEventStore() store.EmailWorkspaceEventStore { return noEvents{} }

type sendOutbound struct {
	store.EmailOutboundStore
	u *sendUoW
}

func (sendOutbound) LockRequest(context.Context, int64, int64, uuid.UUID) error { return nil }
func (s sendOutbound) LocateRequest(_ context.Context, _, _ int64, requestID uuid.UUID) (*store.EmailOutboundRequest, error) {
	s.u.mu.Lock()
	defer s.u.mu.Unlock()
	if m := s.u.message; m != nil && m.RequestID == requestID {
		return &store.EmailOutboundRequest{MessageID: m.ID, Fingerprint: m.RequestFingerprint}, nil
	}
	return nil, nil
}
func (sendOutbound) LockReplyThread(_ context.Context, _, _, threadID int64) (*store.EmailReplyThread, error) {
	if threadID != 1 {
		return nil, nil
	}
	user, attempt, version := int64(70), int64(100), int64(0)
	return &store.EmailReplyThread{ProfileID: 3, Subject: "Order", Status: model.EmailThreadStatusInProgress,
		CurrentUserID: &user, CurrentAttemptID: &attempt, CurrentAttemptVersion: &version, Participant: true}, nil
}
func (sendOutbound) SenderProfile(context.Context, int64, int64) (*model.EmailProfile, error) {
	return &model.EmailProfile{EmailAddress: "support@example.org", SMTPHost: "smtp.example.org", SMTPPort: 465,
		AuthType: model.EmailAuthTypeBasic, AllowSend: true}, nil
}
func (sendOutbound) LockUploads(context.Context, int64, int64, []uuid.UUID) ([]*model.EmailAttachmentUpload, error) {
	return nil, nil
}
func (sendOutbound) ReplyParent(context.Context, int64, int64, int64) (*store.EmailReplyParent, error) {
	return &store.EmailReplyParent{MessageID: "parent@x"}, nil
}
func (s sendOutbound) CreateMessage(_ context.Context, m *model.EmailMessage) (bool, error) {
	s.u.mu.Lock()
	defer s.u.mu.Unlock()
	s.u.stores++
	m.ID = int64(50 + s.u.stores)
	s.u.message = m
	return true, nil
}
func (sendOutbound) UseUploads(context.Context, int64, []uuid.UUID, int64) error     { return nil }
func (sendOutbound) AddAuthor(context.Context, int64, int64, int64, time.Time) error { return nil }

type noRecipients struct{ store.EmailRecipientStore }

func (noRecipients) CreateBulk(context.Context, int64, int64, []*model.EmailRecipient) error {
	return nil
}

type noParts struct {
	store.EmailMessageAttachmentStore
}

func (noParts) CreateBulk(context.Context, int64, int64, []*model.EmailMessageAttachment) error {
	return nil
}

type noThreads struct{ store.EmailThreadStore }

func (noThreads) AdvanceLastMessage(context.Context, int64, int64, int64, time.Time) error {
	return nil
}

func (noThreads) Create(_ context.Context, t *model.EmailThread) (*model.EmailThread, error) {
	created := *t
	created.ID = 9
	return &created, nil
}

// senderProfiles holds one profile a new email may come from and one it may not.
type senderProfiles struct{ store.EmailProfileStore }

func (senderProfiles) List(context.Context, int64, model.EmailProfileFilter) ([]*model.EmailProfile, bool, error) {
	usable := model.EmailProfile{ID: 3, Name: "Support", EmailAddress: "support@example.org", SMTPHost: "smtp.example.org",
		SMTPPort: 465, AuthType: model.EmailAuthTypeBasic, AllowSend: true, Signature: "Regards, ${agent_name}"}
	replyOnly := usable
	replyOnly.ID, replyOnly.AllowSend = 4, false
	return []*model.EmailProfile{&usable, &replyOnly}, false, nil
}

type noEvents struct{}

func (noEvents) Record(context.Context, store.EmailWorkspaceChange) (int, error) { return 0, nil }

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
	sent    *sendUoW
	logs    *bytes.Buffer
}

func start(t *testing.T, links linker) harness {
	t.Helper()
	readOnly := map[string]auth.Scope{"email_thread": {Access: "r", OBAC: true}}
	readWrite := map[string]auth.Scope{"email_thread": {Access: "rw", OBAC: true}}
	sessions := map[string]*auth.Session{
		"reader":    {UserID: 70, DomainID: 1, Scopes: readOnly},
		"writer":    {UserID: 70, DomainID: 1, Scopes: readWrite},
		"creator":   {UserID: 70, DomainID: 1, Scopes: map[string]auth.Scope{"email_thread": {Access: "rx", OBAC: true}}},
		"no-scope":  {UserID: 70, DomainID: 1, Scopes: map[string]auth.Scope{}},
		"superread": {UserID: 99, DomainID: 1, SuperRead: true, Scopes: map[string]auth.Scope{"email_thread": {OBAC: true}}},
	}

	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	threads := &threadStore{}
	sent := &sendUoW{}
	reads := readStore{sent: sent}
	readService := service.NewEmailThreadReadService(reads, links, log)
	outboundCfg := &config.Config{Outbound: config.OutboundConfig{
		Enabled: true, MaxAttachmentSize: 8, MaxConcurrentUploads: 1, AttachmentReferenceTTL: time.Hour,
		MaxRecipients: 5, MaxBodySize: 1 << 10, MaxAttachments: 1, MaxAttachmentsTotalSize: 8, MaxMessageSize: 1 << 20,
	}}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(
		kitinterceptors.UnaryServerErrorInterceptor(),
		interceptors.NewUnaryAuthInterceptor(fakeAuth{sessions: sessions}),
	))
	emailpb.RegisterEmailThreadsServer(server, handler.NewEmailThreadsServer(
		service.NewEmailThreadService(threads, reads, contacts{}),
		readService,
		service.NewEmailAttachmentUploadService(outboundCfg, uploader{}, uploadStore{}),
		service.NewEmailSendService(outboundCfg, sent, readService, senderProfiles{}, contacts{}),
		nil,
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

	return harness{client: emailpb.NewEmailThreadsClient(conn), threads: threads, sent: sent, logs: logs}
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

	// Uploading needs read and either create or update.
	upload := &emailpb.UploadEmailAttachmentRequest{FileName: "a.txt", Content: []byte("x")}
	if _, err := h.client.UploadEmailAttachment(as("reader"), upload); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("upload with read only: %v", err)
	}
	if got, err := h.client.UploadEmailAttachment(as("writer"), upload); err != nil || got.GetReference() == "" {
		t.Fatalf("upload with update: %v, %v", got, err)
	}
	upload.Content = []byte("123456789")
	if _, err := h.client.UploadEmailAttachment(as("writer"), upload); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("upload above the limit: %v", err)
	}

	// A reply needs update; a repeated request returns the same pending message.
	send := &emailpb.SendEmailReplyRequest{
		ThreadId: 1, RequestId: uuid.NewString(), AttemptId: 100,
		Message: &emailpb.InputEmailMessage{To: []*emailpb.EmailAddress{{Address: "client@x.com"}}, BodyText: "Hello"},
	}
	if _, err := h.client.SendEmailReply(as("reader"), send); status.Code(err) != codes.PermissionDenied || h.sent.stores != 0 {
		t.Fatalf("send with read only: %v, stored %d", err, h.sent.stores)
	}
	sent, err := h.client.SendEmailReply(as("writer"), send)
	if err != nil || sent.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_PENDING ||
		sent.GetSubject() != "Re: Order" || h.sent.message.ThreadID != 1 || h.sent.message.InReplyTo != "parent@x" {
		t.Fatalf("send with update: %v, %v", sent, err)
	}
	again, err := h.client.SendEmailReply(as("writer"), send)
	if err != nil || again.GetId() != sent.GetId() || again.GetDeliveryStatus() != sent.GetDeliveryStatus() || h.sent.stores != 1 {
		t.Fatalf("replay: %v, %v, stored %d", again, err, h.sent.stores)
	}

	// A new email and its senders need create.
	create := &emailpb.CreateEmailThreadRequest{
		RequestId: uuid.NewString(), ProfileId: 3, ContactMode: emailpb.EmailContactMode_EMAIL_CONTACT_MODE_NONE,
		Message: &emailpb.InputEmailMessage{To: []*emailpb.EmailAddress{{Address: "client@x.com"}}, BodyText: "Hello"},
	}
	if _, err := h.client.CreateEmailThread(as("reader"), create); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("create with read only: %v", err)
	}
	if _, err := h.client.ListEmailSenderProfiles(as("reader"), &emailpb.ListEmailSenderProfilesRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("senders with read only: %v", err)
	}
	created, err := h.client.CreateEmailThread(as("creator"), create)
	if err != nil || created.GetThread().GetId() != 9 || created.GetMessage().GetThreadId() != 9 ||
		created.GetMessage().GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_PENDING {
		t.Fatalf("create with create: %v, %v", created, err)
	}
	senders, err := h.client.ListEmailSenderProfiles(as("creator"), &emailpb.ListEmailSenderProfilesRequest{})
	if err != nil || len(senders.GetItems()) != 1 || senders.GetItems()[0].GetId() != 3 ||
		senders.GetItems()[0].GetSignature() != "Regards, Olena" {
		t.Fatalf("senders with create: %v, %v", senders, err)
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

type uploader struct{}

func (uploader) Upload(context.Context, storageinfra.UploadRequest) (storageinfra.UploadResult, error) {
	return storageinfra.UploadResult{FileID: 1, Size: 1}, nil
}

type uploadStore struct{}

func (uploadStore) Create(context.Context, *model.EmailAttachmentUpload) error { return nil }

func (uploadStore) DeleteExpiredUnused(context.Context, time.Time, int) (int64, error) { return 0, nil }
