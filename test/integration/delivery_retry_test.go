//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra/mail/mailtest"
	"github.com/webitel/webitel-emails/internal/delivery"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
)

// closedPort is a local port nobody listens on.
func closedPort() int32 {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	return int32(port)
}

// retrying makes the store of env retry a series with delays.
func (e *deliveryEnv) retrying(delays ...time.Duration) *deliveryEnv {
	e.deliveries = storepostgres.NewEmailDeliveryStore(e.db, true, delays)
	return e
}

// due makes a scheduled retry due now, as the passing of time would.
func (e *deliveryEnv) due(messageID int64) {
	e.t.Helper()
	if _, err := e.db.Exec(context.Background(),
		`UPDATE email.message SET delivery_retry_at = now() - interval '1 second' WHERE id = $1 AND delivery_retry_at IS NOT NULL`,
		messageID); err != nil {
		e.t.Fatal(err)
	}
}

// series reads the token, the start of the series and the delivery version.
func (e *deliveryEnv) series(messageID int64) (attempts, start, version int) {
	e.t.Helper()
	if err := e.db.QueryRow(context.Background(), `
SELECT delivery_attempts, delivery_series_start_attempt, delivery_version FROM email.message WHERE id = $1`,
		messageID).Scan(&attempts, &start, &version); err != nil {
		e.t.Fatal(err)
	}
	return attempts, start, version
}

// retryIn is the time left until the scheduled retry; a negative value means none.
func (e *deliveryEnv) retryIn(messageID int64) time.Duration {
	e.t.Helper()
	var seconds *float64
	if err := e.db.QueryRow(context.Background(), `
SELECT extract(epoch FROM delivery_retry_at - now())::float8 FROM email.message WHERE id = $1`, messageID).Scan(&seconds); err != nil {
		e.t.Fatal(err)
	}
	if seconds == nil {
		return -1
	}
	return time.Duration(*seconds * float64(time.Second))
}

// recoverExpired runs one batch of recovery from the start; a failed candidate
// fails the test.
func (e *deliveryEnv) recoverExpired() (recovered, scanned int, err error) {
	e.t.Helper()
	batch, err := e.deliveries.RecoverExpired(context.Background(), nil, 100)
	if len(batch.Failed) > 0 {
		e.t.Fatalf("recovery failed: %v", batch.Failed)
	}
	return batch.Recovered, batch.Scanned, err
}

func (e *deliveryEnv) noClaim() {
	e.t.Helper()
	if d, err := e.deliveries.Claim(context.Background(), time.Minute, nil); err != nil || d != nil {
		e.t.Fatalf("claim = %+v, %v", d, err)
	}
}

func TestDeliveryRetrySeries(t *testing.T) {
	ctx := context.Background()
	later := map[string]string{"FINAL": "451 4.3.0 later"}

	t.Run("a temporary refusal is retried and then sent", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{Replies: later}).retrying(time.Hour, 2*time.Hour)
		message := env.queue()
		version := workspaceVersion(t, env.db, message.ThreadID)

		env.worker().Deliver(ctx, env.claim(time.Minute))
		attempts, start, delivery := env.series(message.ID)
		switch wait := env.retryIn(message.ID); {
		case env.result(message.ID) != "pending queued":
			t.Fatalf("result = %s", env.result(message.ID))
		case wait < 59*time.Minute || wait > time.Hour:
			t.Fatalf("retry in %s", wait)
		case attempts != 1 || start != 0 || delivery != 1 || workspaceVersion(t, env.db, message.ThreadID) != version+1:
			t.Fatalf("series %d %d %d", attempts, start, delivery)
		}
		if got := workspaceEvents(t, env.db, message.ThreadID); got[len(got)-1] != "message.delivery 70 history " {
			t.Fatalf("events = %v", got)
		}
		if logs := env.logs.String(); !strings.Contains(logs, "retry scheduled") || strings.Contains(logs, "later") ||
			strings.Contains(logs, "@x.com") {
			t.Fatalf("logs = %s", logs)
		}
		env.noClaim()

		env.due(message.ID)
		env.server = mailtest.Start(t, mailtest.Config{})
		env.worker().Deliver(ctx, env.claim(time.Minute))
		attempts, _, delivery = env.series(message.ID)
		if env.result(message.ID) != "sent+delivered" || env.retryIn(message.ID) >= 0 || attempts != 2 || delivery != 2 ||
			len(env.server.Accepted()) != 1 || workspaceVersion(t, env.db, message.ThreadID) != version+2 {
			t.Fatalf("result = %s, series %d %d", env.result(message.ID), attempts, delivery)
		}
	})

	t.Run("a series runs out", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{Replies: later}).retrying(time.Hour, time.Hour)
		message := env.queue()
		for i := range 3 {
			env.due(message.ID)
			env.worker().Deliver(ctx, env.claim(time.Minute))
			want := "pending queued"
			if i == 2 {
				want = "failed  temporary_failure"
			}
			if got := env.result(message.ID); got != want {
				t.Fatalf("attempt %d: %s", i+1, got)
			}
		}
		if attempts, _, delivery := env.series(message.ID); attempts != 3 || delivery != 3 || env.retryIn(message.ID) >= 0 {
			t.Fatalf("series %d %d", attempts, delivery)
		}
		env.noClaim()
	})

	for _, tt := range []struct {
		name string
		env  func(*deliveryEnv)
		smtp mailtest.Config
		want string
	}{
		{"too many recipients is retried", nil, mailtest.Config{Replies: map[string]string{"RCPT": "552 5.5.3 too many"}}, "pending queued"},
		{"a full mailbox is final", nil, mailtest.Config{Replies: map[string]string{"RCPT": "552 5.2.2 full"}}, "failed  recipient_rejected"},
		{"a refused greeting is retried", nil, mailtest.Config{Replies: map[string]string{"GREETING": "421 4.3.2 busy"}}, "pending queued"},
		{"a refused first EHLO is retried though EHLO works again", nil,
			mailtest.Config{Replies: map[string]string{"EHLO#1": "451 4.3.2 busy", "HELO": "250 fake"}}, "pending queued"},
		{"a refused connection is retried", func(e *deliveryEnv) { e.refused = true }, mailtest.Config{}, "pending queued"},
		{"an untrusted certificate is final", func(e *deliveryEnv) { e.untrusted = true }, mailtest.Config{}, "failed  connection"},
		{"an incomplete file is retried", func(e *deliveryEnv) { e.files.short = true }, mailtest.Config{}, "pending queued"},
		{"no reply after the dot is final", nil, mailtest.Config{Replies: map[string]string{"FINAL": mailtest.FinalDrop}}, "failed  unknown_result"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newDeliveryEnv(t, tt.smtp).retrying(time.Hour)
			if tt.env != nil {
				tt.env(env)
			}
			message := env.queue()
			env.worker().Deliver(ctx, env.claim(time.Minute))
			if got := env.result(message.ID); got != tt.want {
				t.Fatalf("result = %q", got)
			}
		})
	}

	// A series counts from its start: after a rollback and a new upgrade the start
	// is 0 again, so earlier attempts use up the series (16.2, Down of 00019).
	t.Run("the series counts from its start", func(t *testing.T) {
		for _, tt := range []struct {
			start int
			want  string
		}{{0, "failed  temporary_failure"}, {3, "pending queued"}} {
			env := newDeliveryEnv(t, mailtest.Config{Replies: later}).retrying(time.Hour, time.Hour, time.Hour)
			message := env.queue()
			if _, err := env.db.Exec(ctx, `UPDATE email.message SET delivery_attempts = 3, delivery_series_start_attempt = $2
WHERE id = $1`, message.ID, tt.start); err != nil {
				t.Fatal(err)
			}
			d := env.claim(time.Minute)
			env.worker().Deliver(ctx, d)
			if d.Token != 4 || env.result(message.ID) != tt.want {
				t.Fatalf("start %d: token %d, %s", tt.start, d.Token, env.result(message.ID))
			}
		}
	})
}

func TestDeliveryRetryRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("an expired sending attempt is queued again", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{}).retrying(time.Hour)
		message := env.queue()
		d := env.claim(time.Millisecond)
		time.Sleep(10 * time.Millisecond)
		if n, scanned, err := env.recoverExpired(); err != nil || n != 1 || scanned != 1 {
			t.Fatalf("recovered %d of %d, %v", n, scanned, err)
		}
		if env.result(message.ID) != "pending queued" || env.retryIn(message.ID) < 59*time.Minute {
			t.Fatalf("result = %s, retry in %s", env.result(message.ID), env.retryIn(message.ID))
		}
		if got := workspaceEvents(t, env.db, message.ThreadID); got[len(got)-1] != "message.delivery 70 history " {
			t.Fatalf("events = %v", got)
		}
		// One transition per token: neither recovery nor the old worker writes again.
		if n, _, err := env.recoverExpired(); err != nil || n != 0 {
			t.Fatalf("second recovery %d, %v", n, err)
		}
		for _, outcome := range []store.EmailDeliveryOutcome{failed(model.EmailDeliveryErrorTemporaryFailure, true),
			{Status: model.EmailDeliveryStatusSent}} {
			if applied, _, err := env.deliveries.Finish(ctx, d, outcome); err != nil || applied {
				t.Fatalf("late finish = %v, %v", applied, err)
			}
		}
		if _, _, version := env.series(message.ID); version != 1 {
			t.Fatalf("version = %d", version)
		}

		env.noClaim()
		env.due(message.ID)
		if next := env.claim(time.Minute); next.Token != 2 {
			t.Fatalf("token = %d", next.Token)
		}
		// A live lease is not a candidate.
		if n, scanned, err := env.recoverExpired(); err != nil || n != 0 || scanned != 0 {
			t.Fatalf("recovered %d of %d, %v", n, scanned, err)
		}
	})

	t.Run("an expired committed attempt stays unknown", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{}).retrying(time.Hour)
		message := env.queue()
		d := env.claim(300 * time.Millisecond)
		if ok, err := env.deliveries.Commit(ctx, d); err != nil || !ok {
			t.Fatalf("commit = %v, %v", ok, err)
		}
		time.Sleep(400 * time.Millisecond)
		if _, _, err := env.recoverExpired(); err != nil || env.result(message.ID) != "failed  unknown_result" {
			t.Fatalf("recovery: %v, %s", err, env.result(message.ID))
		}
	})

	t.Run("a busy Thread is left for the next run", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{}).retrying(time.Hour)
		message := env.queue()
		env.claim(time.Millisecond)
		time.Sleep(10 * time.Millisecond)
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.thread WHERE id = $1 FOR UPDATE`, message.ThreadID); err != nil {
			t.Fatal(err)
		}
		n, scanned, err := env.recoverExpired()
		_ = tx.Rollback(ctx)
		if err != nil || n != 0 || scanned != 1 || env.result(message.ID) != "pending sending" {
			t.Fatalf("busy: recovered %d of %d, %v, %s", n, scanned, err, env.result(message.ID))
		}
		if n, _, err := env.recoverExpired(); err != nil || n != 1 {
			t.Fatalf("free: recovered %d, %v", n, err)
		}
	})
}

// Finish locks Thread before Message. If it times out after SMTP accepts the email,
// recovery writes unknown_result without another send.
func TestDeliveryFinishWaitsForThread(t *testing.T) {
	ctx := context.Background()
	hold := func(t *testing.T, env *deliveryEnv, threadID int64) func() {
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.thread WHERE id = $1 FOR UPDATE`, threadID); err != nil {
			t.Fatal(err)
		}
		return func() { _ = tx.Rollback(ctx) }
	}

	t.Run("released within the budget", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		message := env.queue()
		release := hold(t, env, message.ThreadID)
		done := make(chan struct{})
		go func() { env.worker().Deliver(ctx, env.claim(time.Minute)); close(done) }()
		waitForLocks(t, env.db, "Lock", 1)
		time.Sleep(300 * time.Millisecond)
		release()
		<-done
		if env.result(message.ID) != "sent+delivered" || len(env.server.Accepted()) != 1 {
			t.Fatalf("result = %s", env.result(message.ID))
		}
	})

	t.Run("held past the budget", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		message := env.queue()
		events := len(workspaceEvents(t, env.db, message.ThreadID))
		release := hold(t, env, message.ThreadID)
		d := env.claim(3 * time.Second)
		done := make(chan struct{})
		go func() { env.worker().Deliver(ctx, d); close(done) }()
		<-done
		release()
		if env.result(message.ID) != "pending committed" || len(env.server.Accepted()) != 1 ||
			len(workspaceEvents(t, env.db, message.ThreadID)) != events {
			t.Fatalf("result = %s", env.result(message.ID))
		}
		time.Sleep(1200 * time.Millisecond)
		if _, _, err := env.recoverExpired(); err != nil || env.result(message.ID) != "failed  unknown_result" {
			t.Fatalf("recovery: %v, %s", err, env.result(message.ID))
		}
		if sessions, _ := env.server.Sessions(); sessions != 1 || len(workspaceEvents(t, env.db, message.ThreadID)) != events+1 {
			t.Fatalf("sessions %d", sessions)
		}
	})
}

// A command takes the Thread and then the message, as Send does; Finish follows the
// same order, so the two only wait for each other and never deadlock.
func TestDeliveryLockOrder(t *testing.T) {
	ctx := context.Background()
	env := newDeliveryEnv(t, mailtest.Config{})
	for i := range 20 {
		message := env.queue()
		d := env.claim(time.Minute)
		if ok, err := env.deliveries.Commit(ctx, d); err != nil || !ok {
			t.Fatalf("commit = %v, %v", ok, err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, err := env.deliveries.Finish(ctx, d, store.EmailDeliveryOutcome{Status: model.EmailDeliveryStatusSent})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			tx, err := env.db.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SELECT 1 FROM email.thread WHERE id = $1 FOR NO KEY UPDATE`, message.ThreadID); err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Duration(i%3) * 5 * time.Millisecond)
			if _, err := tx.Exec(ctx, `SELECT 1 FROM email.message WHERE id = $1 FOR UPDATE`, message.ID); err != nil {
				errs <- err
				return
			}
			_, err = tx.Exec(ctx, `UPDATE email.thread SET workspace_version = workspace_version + 1 WHERE id = $1`, message.ThreadID)
			if err == nil {
				err = tx.Commit(ctx)
			}
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("iteration %d: deadlock", i)
			}
			if err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
		if env.result(message.ID) != "sent+delivered" {
			t.Fatalf("iteration %d: %s", i, env.result(message.ID))
		}
	}
}

func TestDeliveryClaimOrder(t *testing.T) {
	ctx := context.Background()
	env := newDeliveryEnv(t, mailtest.Config{}).retrying(time.Hour)
	first, second := env.queue(), env.queue()
	// The first one waits for a retry: the second goes first.
	if _, err := env.db.Exec(ctx, `UPDATE email.message SET delivery_retry_at = now() + interval '1 hour' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if d := env.claim(time.Minute); d.MessageID != second.ID {
		t.Fatalf("claimed %d, want %d", d.MessageID, second.ID)
	}
	// Once due long ago it goes before an email queued after it.
	third := env.queue()
	if _, err := env.db.Exec(ctx, `UPDATE email.message SET delivery_retry_at = '2000-01-01' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if d := env.claim(time.Minute); d.MessageID != first.ID {
		t.Fatalf("claimed %d, want %d", d.MessageID, first.ID)
	}
	if d := env.claim(time.Minute); d.MessageID != third.ID {
		t.Fatalf("claimed %d, want %d", d.MessageID, third.ID)
	}
}

// Recovery moves past attempts it cannot take now: neither busy Threads nor a
// failing candidate hold back the ones after them.
func TestDeliveryRecoveryProgress(t *testing.T) {
	ctx := context.Background()
	recovery := func(env *deliveryEnv) *delivery.Recovery {
		return delivery.NewRecovery(&config.Config{Outbound: config.OutboundConfig{DeliveryRecoveryInterval: time.Minute}},
			env.deliveries, env.log)
	}

	t.Run("past a full batch of busy Threads", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		var threads []int64
		var last int64
		for range 101 {
			message := env.queue()
			env.claim(time.Millisecond)
			threads, last = append(threads, message.ThreadID), message.ID
		}
		time.Sleep(10 * time.Millisecond)
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.thread WHERE id = ANY($1) FOR UPDATE`, threads[:100]); err != nil {
			t.Fatal(err)
		}
		recovery(env).Recover(ctx)
		if env.result(last) != "failed  interrupted" ||
			count(t, env.db, `SELECT count(*) FROM email.message WHERE delivery_phase = 'sending'`) != 100 {
			t.Fatalf("last = %s", env.result(last))
		}
	})

	t.Run("past a candidate whose event fails", func(t *testing.T) {
		env := newDeliveryEnv(t, mailtest.Config{})
		first := env.queue()
		env.claim(time.Millisecond)
		second := env.queue()
		env.claim(time.Millisecond)
		time.Sleep(10 * time.Millisecond)
		drop := refuseOutbox(t, env, first.ThreadID)

		recovery(env).Recover(ctx)
		if env.result(first.ID) != "pending sending" || env.result(second.ID) != "failed  interrupted" ||
			!strings.Contains(env.logs.String(), "recover outbound email") {
			t.Fatalf("first = %s, second = %s", env.result(first.ID), env.result(second.ID))
		}
		drop()
		recovery(env).Recover(ctx)
		if env.result(first.ID) != "failed  interrupted" {
			t.Fatalf("first = %s", env.result(first.ID))
		}
	})
}
