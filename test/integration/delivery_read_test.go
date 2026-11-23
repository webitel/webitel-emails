//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/infra/mail/mailtest"
	"github.com/webitel/webitel-emails/internal/event"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
)

// read is one email as the read API gives it to a user.
func (e *commandEnv) read(token string, threadID, messageID int64) *emailpb.EmailMessage {
	e.t.Helper()
	m, err := e.client.LocateEmailThreadMessage(by(token), &emailpb.LocateEmailThreadMessageRequest{ThreadId: threadID, MessageId: messageID})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *commandEnv) page(token string, threadID int64) []int64 {
	e.t.Helper()
	list, err := e.client.ListEmailThreadMessages(by(token), &emailpb.ListEmailThreadMessagesRequest{ThreadId: threadID})
	if err != nil {
		e.t.Fatal(err)
	}
	ids := make([]int64, 0, len(list.GetItems()))
	for _, m := range list.GetItems() {
		ids = append(ids, m.GetId())
	}
	return ids
}

func lastMessageAt(t *testing.T, env *commandEnv, threadID int64) time.Time {
	t.Helper()
	var at time.Time
	if err := env.db.QueryRow(context.Background(), `SELECT last_message_at FROM email.thread WHERE id = $1`, threadID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestDeliveryReadAPI(t *testing.T) {
	later := map[string]string{"FINAL": "451 4.3.0 later"}

	t.Run("states of a delivery", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later}, time.Hour)
		message := env.queue()
		ids, last := env.page("u70", message.ThreadID), lastMessageAt(t, env, message.ThreadID)

		// Accepted: pending, no retry time, version 0.
		if m := env.read("u70", message.ThreadID, message.ID); m.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_PENDING ||
			m.DeliveryRetryAt != nil || m.GetDeliveryVersion() != 0 || m.GetManualRetryAvailable() {
			t.Fatalf("accepted = %+v", m)
		}
		// A scheduled retry: pending with its time and no reason.
		env.attempt()
		m := env.read("u70", message.ThreadID, message.ID)
		if wait := time.Until(time.UnixMilli(m.GetDeliveryRetryAt())); m.DeliveryRetryAt == nil || wait < 59*time.Minute ||
			m.GetDeliveryVersion() != 1 || m.GetDeliveryErrorCode() != emailpb.EmailDeliveryError_EMAIL_DELIVERY_ERROR_UNSPECIFIED ||
			m.GetDeliveryError() != "" || m.GetManualRetryAvailable() {
			t.Fatalf("waiting = %+v", m)
		}
		// The time stays while the next attempt runs, until its result.
		env.due(message.ID)
		d := env.claim(time.Minute)
		if m := env.read("u70", message.ThreadID, message.ID); m.DeliveryRetryAt == nil || m.GetDeliveryVersion() != 1 {
			t.Fatalf("running = %+v", m)
		}
		env.worker().Deliver(context.Background(), d)
		// Final failure: the reason and a retry for the author.
		m = env.read("u70", message.ThreadID, message.ID)
		if m.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_FAILED || m.DeliveryRetryAt != nil ||
			m.GetDeliveryErrorCode() != emailpb.EmailDeliveryError_EMAIL_DELIVERY_ERROR_TEMPORARY_FAILURE || !m.GetManualRetryAvailable() ||
			m.GetDeliveryVersion() != 2 {
			t.Fatalf("failed = %+v", m)
		}
		// A manual retry: pending at once, no time, no further retry.
		if _, err := env.retry("u70", message.ThreadID, message.ID, 2, 0); err != nil {
			t.Fatal(err)
		}
		if m := env.read("u70", message.ThreadID, message.ID); m.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_PENDING ||
			m.DeliveryRetryAt != nil || m.GetDeliveryVersion() != 3 || m.GetManualRetryAvailable() {
			t.Fatalf("retried = %+v", m)
		}
		// Cancelled: final, no reason.
		if _, err := env.cancel("u70", message.ThreadID, message.ID, 3); err != nil {
			t.Fatal(err)
		}
		if m := env.read("u70", message.ThreadID, message.ID); m.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_CANCELLED ||
			m.GetDeliveryError() != "" || m.GetDeliveredAt() != 0 || m.GetDeliveryVersion() != 4 || m.GetManualRetryAvailable() ||
			len(m.GetBcc()) != 1 {
			t.Fatalf("cancelled = %+v", m)
		}
		// The same email in the same place.
		if got := env.page("u70", message.ThreadID); len(got) != len(ids) || got[0] != ids[0] ||
			!lastMessageAt(t, env, message.ThreadID).Equal(last) {
			t.Fatalf("page %v, was %v", got, ids)
		}
	})

	t.Run("sent", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{})
		message := env.queue()
		env.attempt()
		if m := env.read("u70", message.ThreadID, message.ID); m.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_SENT ||
			m.GetDeliveredAt() == 0 || m.GetDeliveryVersion() != 1 || m.GetManualRetryAvailable() {
			t.Fatalf("sent = %+v", m)
		}
	})

	t.Run("the retry flag follows the reply rights", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		threadID, messageID := env.current("flag")
		env.attempt()
		if !env.read("u70", threadID, messageID).GetManualRetryAvailable() {
			t.Fatal("current agent cannot retry")
		}
		env.apply(takenSnapshot(threadID, 12, 101, event.AttemptStateActive, 8, 80))
		if env.read("u70", threadID, messageID).GetManualRetryAvailable() {
			t.Fatal("author may retry while another agent works")
		}
		other := env.read("u80", threadID, messageID)
		if other.GetManualRetryAvailable() || len(other.GetBcc()) != 0 ||
			other.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_FAILED {
			t.Fatalf("new agent = %+v", other)
		}
		env.apply(projectionSnapshot(threadID, 13, 101, event.AttemptStateLeaving))
		if !env.read("u70", threadID, messageID).GetManualRetryAvailable() {
			t.Fatal("author cannot retry from history")
		}
		if _, err := env.db.Exec(context.Background(), `UPDATE email.message SET received_at = now() - interval '25 hours' WHERE id = $1`, messageID); err != nil {
			t.Fatal(err)
		}
		if env.read("u70", threadID, messageID).GetManualRetryAvailable() {
			t.Fatal("retry offered past the window")
		}
	})

	// A valid snapshot may leave an active attempt without an agent: the former
	// participant still reads the Thread, and a repeated Send still returns its email.
	t.Run("an active attempt without an agent", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		threadID := env.thread("no-agent@x")
		env.apply(takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))
		in := reply(threadID, 100)
		sent, err := env.service(true, true).SendReply(context.Background(), session(70), in)
		if err != nil {
			t.Fatal(err)
		}
		noAgent := projectionSnapshot(threadID, 11, 101, event.AttemptStateActive)
		noAgent.AgentID, noAgent.AgentUserID = nil, nil
		env.apply(noAgent)
		var current *int64
		if err := env.db.QueryRow(context.Background(), `SELECT current_user_id FROM email.thread WHERE id = $1`, threadID).Scan(&current); err != nil || current != nil {
			t.Fatalf("current user = %v, %v", current, err)
		}
		if ids := env.page("u70", threadID); len(ids) != 2 {
			t.Fatalf("page = %v", ids)
		}
		if m := env.read("u70", threadID, sent.ID); m.GetManualRetryAvailable() {
			t.Fatalf("message = %+v", m)
		}
		again, err := env.service(true, true).SendReply(context.Background(), session(70), in)
		if err != nil || again.ID != sent.ID {
			t.Fatalf("repeated send = %v, %v", again, err)
		}
	})

	// The flag needs update as the command does.
	t.Run("the retry flag needs update", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		message := env.queue()
		env.attempt()
		if env.read("r70", message.ThreadID, message.ID).GetManualRetryAvailable() {
			t.Fatal("offered to a reader without update")
		}
		list, err := env.client.ListEmailThreadMessages(by("r70"), &emailpb.ListEmailThreadMessagesRequest{ThreadId: message.ThreadID})
		if err != nil || len(list.GetItems()) != 1 || list.GetItems()[0].GetManualRetryAvailable() {
			t.Fatalf("list = %v, %v", list, err)
		}
		if !env.read("u70", message.ThreadID, message.ID).GetManualRetryAvailable() {
			t.Fatal("not offered with read and update")
		}
		list, err = env.client.ListEmailThreadMessages(by("u70"), &emailpb.ListEmailThreadMessagesRequest{ThreadId: message.ThreadID})
		if err != nil || len(list.GetItems()) != 1 || !list.GetItems()[0].GetManualRetryAvailable() {
			t.Fatalf("list = %v, %v", list, err)
		}
	})

	t.Run("sending disabled", func(t *testing.T) {
		env := newCommandEnvWith(t, mailtest.Config{Replies: later}, false)
		message := env.queue()
		env.attempt()
		if m := env.read("u70", message.ThreadID, message.ID); m.GetManualRetryAvailable() ||
			m.GetDeliveryStatus() != emailpb.EmailDeliveryStatus_EMAIL_DELIVERY_STATUS_FAILED {
			t.Fatalf("disabled = %+v", m)
		}
		_, err := env.retry("u70", message.ThreadID, message.ID, 1, 0)
		wantCode(t, err, codes.FailedPrecondition)
	})
}

// Every public change of a delivery tells the participants once, with ids and
// versions only; without events the version still grows.
func TestDeliveryEvents(t *testing.T) {
	later := map[string]string{"FINAL": "451 4.3.0 later"}
	deliveryEvents := func(env *commandEnv, threadID int64) []string {
		var out []string
		for _, e := range workspaceEvents(env.t, env.db, threadID) {
			if strings.HasPrefix(e, "message.delivery") {
				out = append(out, e)
			}
		}
		return out
	}

	t.Run("one event per public change", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later}, time.Hour)
		message := env.queue()
		env.attempt() // scheduled retry
		env.due(message.ID)
		env.attempt() // final failure
		if _, err := env.retry("u70", message.ThreadID, message.ID, 2, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := env.cancel("u70", message.ThreadID, message.ID, 3); err != nil {
			t.Fatal(err)
		}
		// Refused commands add nothing.
		_, _ = env.retry("u70", message.ThreadID, message.ID, 3, 0)
		_, _ = env.cancel("u70", message.ThreadID, message.ID, 4)
		if got := deliveryEvents(env, message.ThreadID); len(got) != 4 {
			t.Fatalf("events = %v", got)
		}
		var payloads []string
		rows, err := env.db.Query(context.Background(), `SELECT payload::text FROM email.outbox
WHERE event_type = 'email.workspace.message.delivery' AND (payload->>'thread_id')::bigint = $1`, message.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var p string
			_ = rows.Scan(&p)
			payloads = append(payloads, p)
		}
		rows.Close()
		for _, p := range payloads {
			for _, secret := range []string{"hidden@x.com", "client@", "Hello", "4.3.0", "later", "delivery_status",
				"delivery_error", "temporary_failure", "pending", "cancelled"} {
				if strings.Contains(strings.ToLower(p), strings.ToLower(secret)) {
					t.Fatalf("payload carries %q: %s", secret, p)
				}
			}
		}
	})

	t.Run("without events the version still grows", func(t *testing.T) {
		env := newCommandEnv(t, mailtest.Config{Replies: later})
		env.deliveries = storepostgres.NewEmailDeliveryStore(env.db, false, nil)
		message := env.queue()
		before := len(deliveryEvents(env, message.ThreadID))
		version := workspaceVersion(t, env.db, message.ThreadID)
		env.attempt()
		if workspaceVersion(t, env.db, message.ThreadID) != version+1 || len(deliveryEvents(env, message.ThreadID)) != before {
			t.Fatalf("version %d, events %d", workspaceVersion(t, env.db, message.ThreadID), len(deliveryEvents(env, message.ThreadID)))
		}
	})
}
