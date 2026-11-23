//go:build integration

package integration_test

import (
	"context"
	"io"
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
	"google.golang.org/protobuf/proto"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra/mail/mailtest"
	"github.com/webitel/webitel-emails/infra/server/grpc/interceptors"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/event"
	handler "github.com/webitel/webitel-emails/internal/handler/grpc"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
)

// tokenAuth resolves the token header into a session.
type tokenAuth map[string]*auth.Session

func (a tokenAuth) AuthorizeFromContext(ctx context.Context, _ string, _ auth.AccessMode) (*auth.Session, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if tokens := md.Get("x-webitel-access"); len(tokens) > 0 {
		if s, ok := a[tokens[0]]; ok {
			return s, nil
		}
	}
	return nil, status.Error(codes.Unauthenticated, "unknown token")
}

// commandEnv serves Retry and Cancel over gRPC with the service interceptors and
// the stores of the test database.
type commandEnv struct {
	*deliveryEnv
	cfg    *config.Config
	client emailpb.EmailThreadsClient
}

func newCommandEnv(t *testing.T, smtp mailtest.Config, delays ...time.Duration) *commandEnv {
	return newCommandEnvWith(t, smtp, true, delays...)
}

// newCommandEnvWith also serves the read API, with sending enabled or not.
func newCommandEnvWith(t *testing.T, smtp mailtest.Config, enabled bool, delays ...time.Duration) *commandEnv {
	env := &commandEnv{deliveryEnv: newDeliveryEnv(t, smtp).retrying(delays...),
		cfg: &config.Config{Outbound: config.OutboundConfig{Enabled: enabled, DeliveryManualRetries: 1}}}
	reads := service.NewEmailThreadReadService(storepostgres.NewEmailThreadReadStore(env.db), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	rw := map[string]auth.Scope{"email_thread": {Access: "rw", OBAC: true}}
	sessions := tokenAuth{
		"u70": {DomainID: 1, UserID: 70, Scopes: rw},
		"u80": {DomainID: 1, UserID: 80, Scopes: rw},
		"u90": {DomainID: 1, UserID: 90, Scopes: rw, SuperRead: true},
		"d2":  {DomainID: 2, UserID: 70, Scopes: rw},
		"r70": {DomainID: 1, UserID: 70, Scopes: map[string]auth.Scope{"email_thread": {Access: "r", OBAC: true}}},
		"w70": {DomainID: 1, UserID: 70, Scopes: map[string]auth.Scope{"email_thread": {Access: "w", OBAC: true}}},
	}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(
		kitinterceptors.UnaryServerErrorInterceptor(), interceptors.NewUnaryAuthInterceptor(sessions)))
	emailpb.RegisterEmailThreadsServer(server, handler.NewEmailThreadsServer(nil, reads, nil, nil,
		service.NewEmailDeliveryService(env.cfg, env.deliveries)))
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
	env.client = emailpb.NewEmailThreadsClient(conn)

	return env
}

func by(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-webitel-access", token)
}

// delivery reads the version, manual series and offset of a message.
func (e *commandEnv) delivery(messageID int64) (version, manual, start int) {
	e.t.Helper()
	if err := e.db.QueryRow(context.Background(), `
SELECT delivery_version, delivery_manual_retry_count, delivery_series_start_attempt FROM email.message WHERE id = $1`,
		messageID).Scan(&version, &manual, &start); err != nil {
		e.t.Fatal(err)
	}
	return version, manual, start
}

func (e *commandEnv) retry(token string, threadID, messageID, version, attemptID int64) (*emailpb.EmailDeliveryReceipt, error) {
	return e.client.RetryEmailDelivery(by(token), &emailpb.RetryEmailDeliveryRequest{ThreadId: threadID, MessageId: messageID,
		ExpectedDeliveryVersion: proto.Int64(version), AttemptId: attemptID})
}

func (e *commandEnv) cancel(token string, threadID, messageID, version int64) (*emailpb.EmailDeliveryReceipt, error) {
	return e.client.CancelEmailDelivery(by(token), &emailpb.CancelEmailDeliveryRequest{ThreadId: threadID, MessageId: messageID,
		ExpectedDeliveryVersion: proto.Int64(version)})
}

// attempt runs one claimed attempt of the queue.
func (e *commandEnv) attempt() {
	e.worker().Deliver(context.Background(), e.claim(time.Minute))
}

// current takes an inbound Thread into work by user 70 (attempt 100) and replies.
func (e *commandEnv) current(name string) (threadID, messageID int64) {
	threadID = e.thread(name + "@x")
	e.apply(takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))
	view, err := e.service(true, true).SendReply(context.Background(), session(70), reply(threadID, 100))
	if err != nil {
		e.t.Fatal(err)
	}
	return threadID, view.ID
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestDeliveryRetryCommand(t *testing.T) {
	later := map[string]string{"FINAL": "451 4.3.0 later"}

	t.Run("a failed new email is retried by its author", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		message := env.queue()
		env.attempt()
		events := len(workspaceEvents(t, env.db, message.ThreadID))
		version := workspaceVersion(t, env.db, message.ThreadID)

		receipt, err := env.retry("u70", message.ThreadID, message.ID, 1, 0)
		if err != nil || receipt.GetDeliveryVersion() != 2 ||
			receipt.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_PENDING {
			t.Fatalf("retry = %v, %v", receipt, err)
		}
		if v, manual, start := env.delivery(message.ID); env.result(message.ID) != "pending queued" || v != 2 || manual != 1 || start != 1 ||
			env.retryIn(message.ID) >= 0 || workspaceVersion(t, env.db, message.ThreadID) != version+1 ||
			len(workspaceEvents(t, env.db, message.ThreadID)) != events+1 {
			t.Fatalf("after retry: %s %d %d %d", env.result(message.ID), v, manual, start)
		}

		// The same request again: the version moved, nothing changes.
		_, err = env.retry("u70", message.ThreadID, message.ID, 1, 0)
		wantCode(t, err, codes.Aborted)
		if v, _, _ := env.delivery(message.ID); v != 2 || len(workspaceEvents(t, env.db, message.ThreadID)) != events+1 {
			t.Fatalf("replay changed the version to %d", v)
		}

		// The same email goes out, with its Bcc and file, after its upload expired.
		if _, err := env.db.Exec(context.Background(), `UPDATE email.attachment_upload SET expires_at = now() - interval '1 day'`); err != nil {
			t.Fatal(err)
		}
		env.server = mailtest.Start(t, mailtest.Config{})
		d := env.claim(time.Minute)
		env.worker().Deliver(context.Background(), d)
		accepted := env.server.Accepted()
		if d.Token != 2 || env.result(message.ID) != "sent+delivered" || len(accepted) != 1 || len(accepted[0].Recipients) != 2 ||
			!strings.Contains(string(accepted[0].Data), message.InternetMessageID) || env.files.opens != 2 {
			t.Fatalf("resend: token %d, %s, %+v, opens %d", d.Token, env.result(message.ID), accepted, env.files.opens)
		}
	})

	t.Run("a manual series retries a temporary refusal on its own", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later}, time.Hour)
		message := env.queue()
		env.attempt()
		env.due(message.ID)
		env.attempt()
		if env.result(message.ID) != "failed  temporary_failure" {
			t.Fatalf("result = %s", env.result(message.ID))
		}
		if _, err := env.retry("u70", message.ThreadID, message.ID, 2, 0); err != nil {
			t.Fatal(err)
		}
		env.attempt()
		if _, _, start := env.delivery(message.ID); env.result(message.ID) != "pending queued" || start != 2 {
			t.Fatalf("new series: %s, start %d", env.result(message.ID), start)
		}
	})

	t.Run("refusals change nothing", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		message := env.queue()
		env.attempt()
		other := env.queue()
		inbound := env.thread("inbound@x")
		inboundMessage := int64(count(t, env.db, `SELECT id FROM email.message WHERE thread_id = $1`, inbound))
		env.apply(takenSnapshot(inbound, 10, 100, event.AttemptStateActive, 7, 70))

		for _, tt := range []struct {
			name string
			call func() error
			code codes.Code
		}{
			{"no version", func() error {
				_, err := env.client.RetryEmailDelivery(by("u70"), &emailpb.RetryEmailDeliveryRequest{ThreadId: message.ThreadID, MessageId: message.ID})
				return err
			}, codes.InvalidArgument},
			{"negative version", func() error { _, err := env.retry("u70", message.ThreadID, message.ID, -1, 0); return err }, codes.InvalidArgument},
			{"read only", func() error { _, err := env.retry("r70", message.ThreadID, message.ID, 1, 0); return err }, codes.PermissionDenied},
			{"update only", func() error { _, err := env.retry("w70", message.ThreadID, message.ID, 1, 0); return err }, codes.PermissionDenied},
			{"not a participant", func() error { _, err := env.retry("u80", message.ThreadID, message.ID, 1, 0); return err }, codes.NotFound},
			{"SuperRead", func() error { _, err := env.retry("u90", message.ThreadID, message.ID, 1, 0); return err }, codes.NotFound},
			{"another domain", func() error { _, err := env.retry("d2", message.ThreadID, message.ID, 1, 0); return err }, codes.NotFound},
			{"message of another Thread", func() error { _, err := env.retry("u70", other.ThreadID, message.ID, 1, 0); return err }, codes.NotFound},
			{"an inbound email", func() error { _, err := env.retry("u70", inbound, inboundMessage, 0, 100); return err }, codes.PermissionDenied},
			{"stale version", func() error { _, err := env.retry("u70", message.ThreadID, message.ID, 0, 0); return err }, codes.Aborted},
			{"still pending", func() error { _, err := env.retry("u70", other.ThreadID, other.ID, 0, 0); return err }, codes.FailedPrecondition},
		} {
			if err := tt.call(); status.Code(err) != tt.code {
				t.Fatalf("%s: %v, want %s", tt.name, err, tt.code)
			}
		}
		if v, manual, _ := env.delivery(message.ID); v != 1 || manual != 0 || env.result(message.ID) != "failed  temporary_failure" {
			t.Fatalf("changed: %d %d %s", v, manual, env.result(message.ID))
		}

		// Sending disabled refuses a retry but not a cancel.
		disabled := service.NewEmailDeliveryService(&config.Config{Outbound: config.OutboundConfig{DeliveryManualRetries: 1}}, env.deliveries)
		author := &auth.Session{DomainID: 1, UserID: 70, Scopes: map[string]auth.Scope{"email_thread": {Access: "rw", OBAC: true}}}
		_, err := disabled.Retry(context.Background(), author, service.DeliveryCommandInput{ThreadID: message.ThreadID,
			MessageID: message.ID, ExpectedDeliveryVersion: proto.Int64(1)})
		wantID(t, err, "email.outbound.disabled")
		if _, err := disabled.Cancel(context.Background(), author, service.DeliveryCommandInput{ThreadID: other.ThreadID,
			MessageID: other.ID, ExpectedDeliveryVersion: proto.Int64(0)}); err != nil {
			t.Fatalf("cancel with sending disabled: %v", err)
		}
	})

	for _, tt := range []struct {
		name  string
		smtp  mailtest.Config
		setup func(env *commandEnv, message *model.EmailMessageView)
	}{
		{"unknown result", mailtest.Config{Replies: map[string]string{"FINAL": mailtest.FinalDrop}}, nil},
		{"refused recipient", mailtest.Config{Replies: map[string]string{"RCPT": "550 5.1.1 unknown"}}, nil},
		{"sent", mailtest.Config{}, nil},
		{"outside the window", mailtest.Config{Replies: later}, func(env *commandEnv, m *model.EmailMessageView) {
			if _, err := env.db.Exec(context.Background(), `UPDATE email.message SET received_at = now() - interval '24 hours 1 second' WHERE id = $1`, m.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"manual series used up", mailtest.Config{Replies: later}, func(env *commandEnv, m *model.EmailMessageView) {
			if _, err := env.retry("u70", m.ThreadID, m.ID, 1, 0); err != nil {
				t.Fatal(err)
			}
			env.attempt()
		}},
	} {
		t.Run("not retried: "+tt.name, func(t *testing.T) {
			env := newCommandEnv(t, tt.smtp)
			message := env.queue()
			env.attempt()
			if tt.setup != nil {
				tt.setup(env, message)
			}
			version, manual, _ := env.delivery(message.ID)
			_, err := env.retry("u70", message.ThreadID, message.ID, int64(version), 0)
			wantCode(t, err, codes.FailedPrecondition)
			if v, m, _ := env.delivery(message.ID); v != version || m != manual {
				t.Fatalf("changed: %d %d", v, m)
			}
		})
	}

	t.Run("rights follow the reply rules", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		threadID, messageID := env.current("rights")
		env.attempt()

		// CURRENT: the attempt and its version are checked.
		_, err := env.client.RetryEmailDelivery(by("u70"), &emailpb.RetryEmailDeliveryRequest{ThreadId: threadID, MessageId: messageID,
			ExpectedDeliveryVersion: proto.Int64(1), AttemptId: 100, ExpectedAttemptVersion: 5})
		wantCode(t, err, codes.Aborted)

		// Transferred to user 80: the author may not retry while another agent works,
		// the new agent may not touch the email of another author; Cancel stays.
		env.apply(takenSnapshot(threadID, 12, 101, event.AttemptStateActive, 8, 80))
		_, err = env.retry("u70", threadID, messageID, 1, 0)
		wantCode(t, err, codes.FailedPrecondition)
		_, err = env.retry("u80", threadID, messageID, 1, 101)
		wantCode(t, err, codes.PermissionDenied)

		// Completed: the author retries from history.
		env.apply(projectionSnapshot(threadID, 13, 101, event.AttemptStateLeaving))
		if receipt, err := env.retry("u70", threadID, messageID, 1, 0); err != nil || receipt.GetDeliveryVersion() != 2 {
			t.Fatalf("retry from history: %v, %v", receipt, err)
		}
	})

	// The window is checked by the clock after the locks: a retry that waited for
	// the message past 24 hours is refused.
	t.Run("the window counts after the wait for the message", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		message := env.queue()
		env.attempt()
		ctx := context.Background()
		if _, err := env.db.Exec(ctx, `UPDATE email.message SET received_at = now() - interval '24 hours' + interval '1 second' WHERE id = $1`, message.ID); err != nil {
			t.Fatal(err)
		}
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.message WHERE id = $1 FOR UPDATE`, message.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := env.retry("u70", message.ThreadID, message.ID, 1, 0); done <- err }()
		waitForLocks(t, env.db, "Lock", 1)
		time.Sleep(1500 * time.Millisecond)
		_ = tx.Rollback(ctx)
		wantCode(t, <-done, codes.FailedPrecondition)
		if v, manual, _ := env.delivery(message.ID); v != 1 || manual != 0 || env.result(message.ID) != "failed  temporary_failure" {
			t.Fatalf("changed: %d %d %s", v, manual, env.result(message.ID))
		}
	})

	// A repeated request after a Transfer still tells that the delivery moved on.
	t.Run("the same retry after a Transfer", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		threadID, messageID := env.current("repeat")
		env.attempt()
		if _, err := env.retry("u70", threadID, messageID, 1, 100); err != nil {
			t.Fatal(err)
		}
		env.apply(takenSnapshot(threadID, 12, 101, event.AttemptStateActive, 8, 80))
		_, err := env.retry("u70", threadID, messageID, 1, 100)
		wantCode(t, err, codes.Aborted)
		if v, manual, _ := env.delivery(messageID); v != 2 || manual != 1 {
			t.Fatalf("changed: %d %d", v, manual)
		}
	})

	t.Run("a lagging projection still allows the retry", func(t *testing.T) {
		// Р3: rights follow the applied projection; call_center may have moved on.
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		threadID, messageID := env.current("lagging")
		env.attempt()
		if _, err := env.retry("u70", threadID, messageID, 1, 100); err != nil {
			t.Fatalf("retry before the snapshot: %v", err)
		}
	})

	t.Run("an outbox failure rolls the retry back", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		message := env.queue()
		env.attempt()
		refuseOutbox(t, env.deliveryEnv, message.ThreadID)
		_, err := env.retry("u70", message.ThreadID, message.ID, 1, 0)
		wantCode(t, err, codes.Internal)
		if v, manual, start := env.delivery(message.ID); v != 1 || manual != 0 || start != 0 || env.result(message.ID) != "failed  temporary_failure" {
			t.Fatalf("not rolled back: %d %d %d %s", v, manual, start, env.result(message.ID))
		}
	})
}

func TestDeliveryCancelCommand(t *testing.T) {
	ctx := context.Background()

	t.Run("a queued email is never sent", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{})
		message := env.queue()
		events := len(workspaceEvents(t, env.db, message.ThreadID))
		receipt, err := env.cancel("u70", message.ThreadID, message.ID, 0)
		if err != nil || receipt.GetDeliveryVersion() != 1 ||
			receipt.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_CANCELLED {
			t.Fatalf("cancel = %v, %v", receipt, err)
		}
		env.noClaim()
		if env.result(message.ID) != "cancelled" || len(workspaceEvents(t, env.db, message.ThreadID)) != events+1 {
			t.Fatalf("result = %s", env.result(message.ID))
		}
		// Final: neither a retry nor a second cancel.
		_, err = env.retry("u70", message.ThreadID, message.ID, 1, 0)
		wantCode(t, err, codes.FailedPrecondition)
		_, err = env.cancel("u70", message.ThreadID, message.ID, 1)
		wantCode(t, err, codes.FailedPrecondition)
	})

	t.Run("a sending attempt never commits", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{})
		message := env.queue()
		d := env.claim(time.Minute)
		if _, err := env.cancel("u70", message.ThreadID, message.ID, 0); err != nil {
			t.Fatal(err)
		}
		env.worker().Deliver(ctx, d)
		time.Sleep(50 * time.Millisecond)
		_, discarded := env.server.Sessions()
		if len(env.server.Accepted()) != 0 || discarded != 1 || env.result(message.ID) != "cancelled" {
			t.Fatalf("accepted %d, discarded %d, %s", len(env.server.Accepted()), discarded, env.result(message.ID))
		}
	})

	t.Run("a scheduled retry is stopped", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: map[string]string{"FINAL": "451 4.3.0 later"}}, time.Hour)
		message := env.queue()
		env.attempt()
		if _, err := env.cancel("u70", message.ThreadID, message.ID, 1); err != nil {
			t.Fatal(err)
		}
		if env.result(message.ID) != "cancelled" || env.retryIn(message.ID) >= 0 {
			t.Fatalf("result = %s", env.result(message.ID))
		}
	})

	t.Run("refused after the commit and once final", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{})
		message := env.queue()
		d := env.claim(time.Minute)
		if ok, err := env.deliveries.Commit(ctx, d); err != nil || !ok {
			t.Fatalf("commit = %v, %v", ok, err)
		}
		_, err := env.cancel("u70", message.ThreadID, message.ID, 0)
		wantCode(t, err, codes.FailedPrecondition)
		if applied, _, err := env.deliveries.Finish(ctx, d, store.EmailDeliveryOutcome{Status: model.EmailDeliveryStatusSent}); err != nil || !applied {
			t.Fatalf("finish = %v, %v", applied, err)
		}
		_, err = env.cancel("u70", message.ThreadID, message.ID, 1)
		wantCode(t, err, codes.FailedPrecondition)
	})

	t.Run("the author cancels while another agent works", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{})
		threadID, messageID := env.current("cancel-history")
		env.apply(takenSnapshot(threadID, 12, 101, event.AttemptStateActive, 8, 80))
		_, err := env.cancel("u80", threadID, messageID, 0)
		wantCode(t, err, codes.PermissionDenied)
		if _, err := env.cancel("u70", threadID, messageID, 0); err != nil {
			t.Fatalf("author: %v", err)
		}
	})
}

// Commands race each other, the worker, recovery and the projection: at most one
// of two contenders wins, with one version step and one event.
func TestDeliveryCommandRaces(t *testing.T) {
	ctx := context.Background()
	later := map[string]string{"FINAL": "451 4.3.0 later"}
	// settle stops what an iteration left pending, so the next one claims its own.
	settle := func(env *commandEnv) {
		if _, err := env.db.Exec(ctx, `UPDATE email.message SET delivery_status = 'cancelled', delivery_phase = NULL,
delivery_retry_at = NULL, delivery_lease_until = NULL WHERE delivery_status = 'pending'`); err != nil {
			t.Fatal(err)
		}
	}
	both := func(a, b func() error) (errA, errB error) {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); errA = a() }()
		go func() { defer wg.Done(); errB = b() }()
		wg.Wait()
		return errA, errB
	}

	// Finish and the projection hold the Thread; a command waiting for it must not
	// hold the message, or the two would deadlock.
	t.Run("a command takes the Thread before the message", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{})
		message := env.queue()
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.thread WHERE id = $1 FOR NO KEY UPDATE`, message.ThreadID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := env.cancel("u70", message.ThreadID, message.ID, 0); done <- err }()
		waitForLocks(t, env.db, "Lock", 1)
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.message WHERE id = $1 FOR UPDATE NOWAIT`, message.ID); err != nil {
			t.Fatalf("the waiting command holds the message: %v", err)
		}
		_ = tx.Rollback(ctx)
		if err := <-done; err != nil || env.result(message.ID) != "cancelled" {
			t.Fatalf("cancel = %v, %s", err, env.result(message.ID))
		}
	})

	t.Run("retry and retry, retry and cancel", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		for i := range 10 {
			message := env.queue()
			env.attempt()
			events := len(workspaceEvents(t, env.db, message.ThreadID))
			second := func() error { _, err := env.retry("u70", message.ThreadID, message.ID, 1, 0); return err }
			if i%2 == 1 {
				second = func() error { _, err := env.cancel("u70", message.ThreadID, message.ID, 1); return err }
			}
			errA, errB := both(func() error { _, err := env.retry("u70", message.ThreadID, message.ID, 1, 0); return err }, second)
			if (errA == nil) == (errB == nil) && errA == nil {
				t.Fatalf("iteration %d: both won", i)
			}
			if v, _, _ := env.delivery(message.ID); v != 2 || len(workspaceEvents(t, env.db, message.ThreadID)) != events+1 {
				t.Fatalf("iteration %d: version %d, %v %v", i, v, errA, errB)
			}
			settle(env)
		}
	})

	t.Run("cancel and claim, commit, finish", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{})
		for i := range 15 {
			message := env.queue()
			var cancelErr error
			done := make(chan struct{})
			go func() { _, cancelErr = env.cancel("u70", message.ThreadID, message.ID, 0); close(done) }()
			if d, err := env.deliveries.Claim(ctx, time.Minute, nil); err == nil && d != nil {
				env.worker().Deliver(ctx, *d)
			}
			<-done
			sent := len(env.server.Accepted())
			switch result := env.result(message.ID); {
			case cancelErr == nil && result != "cancelled":
				t.Fatalf("iteration %d: cancelled but %s", i, result)
			case cancelErr == nil && sent != 0:
				t.Fatalf("iteration %d: cancelled and sent", i)
			case cancelErr != nil && status.Code(cancelErr) != codes.FailedPrecondition && status.Code(cancelErr) != codes.Aborted:
				t.Fatalf("iteration %d: %v", i, cancelErr)
			}
			env.server = mailtest.Start(t, mailtest.Config{})
			settle(env)
		}
	})

	t.Run("cancel and recovery", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{}, time.Hour)
		for i := range 10 {
			message := env.queue()
			env.claim(time.Millisecond)
			time.Sleep(5 * time.Millisecond)
			var batch store.EmailDeliveryRecovery
			_, errCancel := both(func() error {
				var err error
				batch, err = env.deliveries.RecoverExpired(ctx, nil, 100)
				return err
			}, func() error { _, err := env.cancel("u70", message.ThreadID, message.ID, 0); return err })
			v, _, _ := env.delivery(message.ID)
			switch {
			case errCancel == nil && (env.result(message.ID) != "cancelled" || batch.Recovered != 0 || v != 1):
				t.Fatalf("iteration %d: cancel won but %s, recovered %d, version %d", i, env.result(message.ID), batch.Recovered, v)
			case errCancel != nil && (status.Code(errCancel) != codes.Aborted || env.result(message.ID) != "pending queued" || v != 1):
				t.Fatalf("iteration %d: %v, %s, version %d", i, errCancel, env.result(message.ID), v)
			}
			settle(env)
		}
	})

	t.Run("retry and projection", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		for i := range 10 {
			threadID, messageID := env.current("projection-" + string(rune('a'+i)))
			env.attempt()
			var retryErr error
			done := make(chan struct{})
			go func() { _, retryErr = env.retry("u70", threadID, messageID, 1, 100); close(done) }()
			env.apply(takenSnapshot(threadID, 12, 101, event.AttemptStateActive, 8, 80))
			<-done
			v, _, _ := env.delivery(messageID)
			switch {
			case retryErr == nil && v != 2:
				t.Fatalf("iteration %d: retried, version %d", i, v)
			case retryErr != nil && (status.Code(retryErr) != codes.FailedPrecondition || v != 1):
				t.Fatalf("iteration %d: %v, version %d", i, retryErr, v)
			}
			settle(env)
		}
	})
}

// refuseOutbox makes every event of a Thread fail until the test ends.
func refuseOutbox(t *testing.T, env *deliveryEnv, threadID int64) func() {
	t.Helper()
	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `
CREATE FUNCTION test_outbox_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.payload->>'thread_id')::bigint = `+itoa(threadID)+` THEN RAISE EXCEPTION 'outbox refused'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER outbox_refuse BEFORE INSERT ON email.outbox FOR EACH ROW EXECUTE FUNCTION test_outbox_refuse();`); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		_, _ = env.db.Exec(ctx, `DROP TRIGGER IF EXISTS outbox_refuse ON email.outbox; DROP FUNCTION IF EXISTS test_outbox_refuse()`)
	}
	t.Cleanup(drop)

	return drop
}
