//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/config"
	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/infra/mail/mailtest"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/delivery"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
)

// smtpProfiles reads profiles from the database and points them at the fake server.
type smtpProfiles struct {
	profiles interface {
		Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error)
	}
	server *mailtest.Server
	// fail is returned instead of credentials.
	fail error
}

func (p smtpProfiles) Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error) {
	return p.profiles.Locate(ctx, domainID, id)
}

func (p smtpProfiles) SMTPConnection(context.Context, *model.EmailProfile) (mailinfra.SMTPConnection, error) {
	if p.fail != nil {
		return mailinfra.SMTPConnection{}, p.fail
	}
	return mailinfra.SMTPConnection{Host: p.server.Host, Port: p.server.Port,
		Security: model.EmailConnectionSecurityTLS, Username: "agent", Password: "p"}, nil
}

// storedFiles serves file 500 of every upload; short cuts it with a clean end.
type storedFiles struct {
	mu    sync.Mutex
	opens int
	short bool
}

func (f *storedFiles) Open(_ context.Context, _, _, size int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.short {
		return io.NopCloser(io.MultiReader(strings.NewReader("abc"), errReader{storageinfra.ErrFileIncomplete})), nil
	}
	return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), int(size)))), nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type deliveryEnv struct {
	*sendEnv
	server     *mailtest.Server
	files      *storedFiles
	deliveries store.EmailDeliveryStore
	cfg        *config.Config
	logs       *bytes.Buffer
	// log is shared by every worker: one handler serializes writes to logs.
	log     *slog.Logger
	credErr error
}

func newDeliveryEnv(t *testing.T, smtp mailtest.Config) *deliveryEnv {
	env := &deliveryEnv{sendEnv: newSendEnv(t), server: mailtest.Start(t, smtp), files: &storedFiles{}, logs: &bytes.Buffer{}}
	env.log = slog.New(slog.NewTextHandler(env.logs, nil))
	env.allowSend(env.profile)
	env.deliveries = storepostgres.NewEmailDeliveryStore(env.db, true)
	env.cfg = &config.Config{Outbound: config.OutboundConfig{
		MaxMessageSize: 1 << 20, DeliveryTickInterval: 20 * time.Millisecond,
		MaxConcurrentDeliveries: 8, MaxDeliveriesPerProfile: 2,
		SMTPCommandTimeout: 2 * time.Second, SMTPDataTimeout: 2 * time.Second, SMTPFinalReplyTimeout: 300 * time.Millisecond,
		DeliverySendTimeout: 5 * time.Second, DeliveryLease: 6 * time.Second,
	}}

	return env
}

func (e *deliveryEnv) worker() *delivery.Worker {
	o := e.cfg.Outbound
	return delivery.New(delivery.Params{
		Config: e.cfg, Deliveries: e.deliveries, UoW: storepostgres.NewUnitOfWork(e.db),
		Profiles: smtpProfiles{profiles: storepostgres.NewEmailProfileStore(e.db), server: e.server, fail: e.credErr},
		Files:    e.files,
		SMTP: mailinfra.NewSMTPSender(mailinfra.SMTPSenderConfig{
			CommandTimeout: o.SMTPCommandTimeout, DataTimeout: o.SMTPDataTimeout,
			FinalReplyTimeout: o.SMTPFinalReplyTimeout, SendTimeout: o.DeliverySendTimeout, RootCAs: e.server.RootCAs,
		}),
		Log: e.log,
	})
}

// queue accepts a new email of user 70 with one attachment and Bcc.
func (e *deliveryEnv) queue() *model.EmailMessageView {
	in := newThread(e.profile, service.ContactModeNone, 0, e.upload(70, time.Now().Add(time.Hour)))
	in.Message.Bcc = []model.EmailAddress{{Address: "hidden@x.com"}}
	_, message, err := e.service(true, true).CreateThread(context.Background(), session(70), in)
	if err != nil {
		e.t.Fatal(err)
	}
	return message
}

func (e *deliveryEnv) claim(lease time.Duration) store.EmailDelivery {
	d, err := e.deliveries.Claim(context.Background(), lease, nil)
	if err != nil || d == nil {
		e.t.Fatalf("claim = %v, %v", d, err)
	}
	return *d
}

// result reads the delivery columns of a message.
func (e *deliveryEnv) result(messageID int64) string {
	var status, phase, code string
	var delivered bool
	err := e.db.QueryRow(context.Background(), `
SELECT delivery_status, coalesce(delivery_phase, ''), coalesce(delivery_error_code, ''), delivered_at IS NOT NULL
FROM email.message WHERE id = $1`, messageID).Scan(&status, &phase, &code, &delivered)
	if err != nil {
		e.t.Fatal(err)
	}
	if delivered {
		status += "+delivered"
	}
	return strings.TrimRight(status+" "+phase+" "+code, " ")
}

func TestDeliverySends(t *testing.T) {
	env := newDeliveryEnv(t, mailtest.Config{})
	ctx := context.Background()
	message := env.queue()
	version := workspaceVersion(t, env.db, message.ThreadID)

	env.worker().Deliver(ctx, env.claim(time.Minute))

	accepted := env.server.Accepted()
	switch {
	case env.result(message.ID) != "sent+delivered":
		t.Fatalf("result = %s", env.result(message.ID))
	case len(accepted) != 1 || accepted[0].From != "send@example.org" || len(accepted[0].Recipients) != 2:
		t.Fatalf("accepted = %+v", accepted)
	case bytes.Contains(accepted[0].Data, []byte("hidden@x.com")) ||
		!bytes.Contains(accepted[0].Data, []byte(message.InternetMessageID)):
		t.Fatal("Bcc in the email or Message-ID missing")
	case workspaceVersion(t, env.db, message.ThreadID) != version+1:
		t.Fatal("workspace version not raised")
	}
	if got := workspaceEvents(t, env.db, message.ThreadID); got[len(got)-1] != "message.delivery 70 history " {
		t.Fatalf("events = %v", got)
	}

	// A late write of the same attempt changes nothing and tells nobody.
	if applied, err := env.deliveries.Finish(ctx, store.EmailDelivery{DomainID: 1, MessageID: message.ID,
		ThreadID: message.ThreadID, Token: 1}, model.EmailDeliveryStatusFailed, model.EmailDeliveryErrorRejected); err != nil || applied {
		t.Fatalf("late finish = %v, %v", applied, err)
	}
}

func TestDeliveryFailures(t *testing.T) {
	tests := []struct {
		name     string
		smtp     mailtest.Config
		want     string
		accepted int
	}{
		{"one recipient refused", mailtest.Config{Replies: map[string]string{"RCPT hidden@x.com": "550 5.1.1 <hidden@x.com> unknown"}},
			"failed  recipient_rejected", 0},
		{"recipient 552", mailtest.Config{Replies: map[string]string{"RCPT": "552 5.2.2 full"}}, "failed  recipient_rejected", 0},
		{"larger than SIZE", mailtest.Config{Size: 100}, "failed  message_too_large", 0},
		{"mail 552", mailtest.Config{Replies: map[string]string{"MAIL": "552 storage"}}, "failed  rejected", 0},
		{"mail 5.3.4", mailtest.Config{Replies: map[string]string{"MAIL": "552 5.3.4 too big"}}, "failed  message_too_large", 0},
		{"final 5xx", mailtest.Config{Replies: map[string]string{"FINAL": "554 5.7.1 spam"}}, "failed  rejected", 0},
		{"final 552", mailtest.Config{Replies: map[string]string{"FINAL": "552 too big"}}, "failed  message_too_large", 0},
		{"final 4xx", mailtest.Config{Replies: map[string]string{"FINAL": "451 4.3.0 later"}}, "failed  temporary_failure", 0},
		{"dropped before the boundary", mailtest.Config{Replies: map[string]string{"DATA": mailtest.FinalDrop}}, "failed  interrupted", 0},
		{"dropped after the dot", mailtest.Config{Replies: map[string]string{"FINAL": mailtest.FinalDrop}}, "failed  unknown_result", 1},
		{"no reply after the dot", mailtest.Config{Replies: map[string]string{"FINAL": mailtest.FinalHang}}, "failed  unknown_result", 1},
		{"greeting refused", mailtest.Config{Replies: map[string]string{"EHLO": "451 4.3.2 busy"}}, "failed  connection", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newDeliveryEnv(t, tt.smtp)
			message := env.queue()
			env.worker().Deliver(context.Background(), env.claim(time.Minute))

			if got := env.result(message.ID); got != tt.want || len(env.server.Accepted()) != tt.accepted {
				t.Fatalf("result = %q, accepted %d", got, len(env.server.Accepted()))
			}
			// Logs keep codes only: no address and no reply text.
			if logs := env.logs.String(); strings.Contains(logs, "@x.com") || strings.Contains(logs, "spam") ||
				strings.Contains(logs, "later") || strings.Contains(logs, "full") || strings.Contains(logs, "storage") {
				t.Fatalf("logs = %s", logs)
			}
		})
	}

	// The provider's own text may name the account; the log keeps the error ID only.
	t.Run("credentials error", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		env.credErr = kiterrors.Unauthenticated("OAuth refresh token was rejected",
			kiterrors.WithID("email.profile.oauth_not_connected"), kiterrors.AppendMessagef("owner marker@provider.example"))
		message := env.queue()
		env.worker().Deliver(context.Background(), env.claim(time.Minute))
		logs := env.logs.String()
		if env.result(message.ID) != "failed  profile_unavailable" || strings.Contains(logs, "marker") ||
			!strings.Contains(logs, "email.profile.oauth_not_connected") {
			t.Fatalf("result = %s, logs = %s", env.result(message.ID), logs)
		}
	})

	t.Run("incomplete attachment", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		env.files.short = true
		message := env.queue()
		env.worker().Deliver(context.Background(), env.claim(time.Minute))
		if sessions, _ := env.server.Sessions(); env.result(message.ID) != "failed  interrupted" || sessions != 0 {
			t.Fatalf("result = %s, sessions %d", env.result(message.ID), sessions)
		}
	})
}

func TestDeliveryLeaseAndRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("a crash under the lease is interrupted", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		message := env.queue()
		env.claim(time.Millisecond)
		time.Sleep(10 * time.Millisecond)
		if n, err := env.deliveries.RecoverExpired(ctx, 100); err != nil || n != 1 || env.result(message.ID) != "failed  interrupted" {
			t.Fatalf("recovered %d, %v, %s", n, err, env.result(message.ID))
		}
		if got := workspaceEvents(t, env.db, message.ThreadID); got[len(got)-1] != "message.delivery 70 history " {
			t.Fatalf("events = %v", got)
		}
	})

	// The lease runs out while the commit waits for the row: the server never
	// gets the final dot, and recovery fails the attempt.
	t.Run("lease lost while the commit waits", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		message := env.queue()
		d := env.claim(time.Second)
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.message WHERE id = $1 FOR UPDATE`, message.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { env.worker().Deliver(ctx, d); close(done) }()
		waitForLocks(t, env.db, "Lock", 1)
		time.Sleep(1100 * time.Millisecond)
		_ = tx.Rollback(ctx)
		<-done

		time.Sleep(50 * time.Millisecond)
		_, discarded := env.server.Sessions()
		if len(env.server.Accepted()) != 0 || discarded != 1 || env.result(message.ID) != "pending sending" {
			t.Fatalf("accepted %d, discarded %d, %s", len(env.server.Accepted()), discarded, env.result(message.ID))
		}
		if _, err := env.deliveries.RecoverExpired(ctx, 100); err != nil || env.result(message.ID) != "failed  interrupted" {
			t.Fatalf("recovery: %v, %s", err, env.result(message.ID))
		}
	})

	// The commit applied and the worker is gone: recovery cannot know the result,
	// and a late write of the old attempt is refused.
	t.Run("committed attempt without a result", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		message := env.queue()
		d := env.claim(500 * time.Millisecond)
		if ok, err := env.deliveries.Commit(ctx, d); err != nil || !ok {
			t.Fatalf("commit = %v, %v", ok, err)
		}
		time.Sleep(600 * time.Millisecond)
		if _, err := env.deliveries.RecoverExpired(ctx, 100); err != nil || env.result(message.ID) != "failed  unknown_result" {
			t.Fatalf("recovery: %v, %s", err, env.result(message.ID))
		}
		if applied, err := env.deliveries.Finish(ctx, d, model.EmailDeliveryStatusSent, ""); err != nil || applied {
			t.Fatalf("late finish = %v, %v", applied, err)
		}
	})
}

func TestDeliveryWorkers(t *testing.T) {
	waitDone := func(t *testing.T, env *deliveryEnv) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); count(t, env.db,
			`SELECT count(*) FROM email.message WHERE delivery_status = 'pending'`) > 0; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("emails still pending")
			}
		}
	}

	t.Run("two workers send each email once", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		for range 6 {
			env.queue()
		}
		first, second := env.worker(), env.worker()
		first.Start()
		second.Start()
		waitDone(t, env)
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = first.Stop(stop), second.Stop(stop)

		ids := map[string]bool{}
		for _, m := range env.server.Accepted() {
			// The fake keeps the body as read, with bare line feeds.
			id, _, _ := strings.Cut(string(m.Data[bytes.Index(m.Data, []byte("Message-Id:")):]), "\n")
			ids[id] = true
		}
		if len(env.server.Accepted()) != 6 || len(ids) != 6 ||
			count(t, env.db, `SELECT count(*) FROM email.message WHERE delivery_status = 'sent' AND delivery_attempts = 1`) != 6 {
			t.Fatalf("accepted %d, distinct %d", len(env.server.Accepted()), len(ids))
		}
	})

	t.Run("one profile at a time", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{Delay: 100 * time.Millisecond})
		env.cfg.Outbound.MaxDeliveriesPerProfile = 1
		for range 3 {
			env.queue()
		}
		w := env.worker()
		w.Start()
		waitDone(t, env)
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Stop(stop)
		if env.server.MaxActive() != 1 || len(env.server.Accepted()) != 3 {
			t.Fatalf("max active %d, accepted %d", env.server.MaxActive(), len(env.server.Accepted()))
		}
	})
}
