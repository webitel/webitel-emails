//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/infra/mail/mailtest"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
)

// report is an inbound bounce or auto-reply, newer than everything before it.
func report(kind model.EmailKind, from string, at time.Time, options ...emailOption) emailOption {
	return func(e *model.ParsedEmail) {
		e.Kind, e.ReceivedAt = kind, at
		e.From = []model.EmailAddress{{Address: from, NormalizedAddress: from}}
		if kind == model.EmailKindBounce {
			e.Bounce = &model.EmailBounceInfo{Recipient: "hidden@x.com", Status: "5.1.1"}
		}
		for _, option := range options {
			option(e)
		}
	}
}

func bouncing(messageID string) emailOption {
	return func(e *model.ParsedEmail) { e.Bounce.OriginalMessageID = messageID }
}

// finish stores the result of the queued attempt of a message; only a committed
// attempt may be sent.
func (e *deliveryEnv) finish(status model.EmailDeliveryStatus, code model.EmailDeliveryError) {
	d := e.claim(time.Minute)
	if status == model.EmailDeliveryStatusSent {
		if ok, err := e.deliveries.Commit(context.Background(), d); err != nil || !ok {
			e.t.Fatalf("commit = %v, %v", ok, err)
		}
	}
	outcome := store.EmailDeliveryOutcome{Status: status, Code: code}
	if applied, _, err := e.deliveries.Finish(context.Background(), d, outcome); err != nil || !applied {
		e.t.Fatalf("finish = %v, %v", applied, err)
	}
}

// visibleIDs pages through a Thread one message at a time.
func visibleIDs(t *testing.T, reads store.EmailThreadReadStore, user, threadID int64) []int64 {
	t.Helper()
	var ids []int64
	filter := model.EmailMessageFilter{DomainID: 1, UserID: user, ThreadID: threadID, Size: 1}
	for {
		page, next, err := reads.ListMessages(context.Background(), filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page {
			ids = append(ids, m.ID)
		}
		if !next {
			return ids
		}
		filter.After = &model.EmailMessageCursor{ReceivedAt: page[0].ReceivedAt, ID: page[0].ID}
	}
}

func TestOutboundReadAPI(t *testing.T) {
	env := newDeliveryEnv(t, mailtest.Config{})
	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `DELETE FROM directory.wbt_user`); err != nil {
		t.Fatal(err)
	}
	seedUser(t, env.db, 70, 1, "Olena")
	t.Cleanup(func() { _, _ = env.db.Exec(context.Background(), `DELETE FROM directory.wbt_user WHERE id = 70`) })
	if _, err := env.db.Exec(ctx, `UPDATE email.profile SET signature = 'Regards, ${agent_name}' WHERE id = $1`, env.profile); err != nil {
		t.Fatal(err)
	}
	reads := storepostgres.NewEmailThreadReadStore(env.db)

	sent := env.queue()
	env.finish(model.EmailDeliveryStatusSent, "")
	failed := env.queue()
	env.finish(model.EmailDeliveryStatusFailed, model.EmailDeliveryErrorRejected)
	env.apply(takenSnapshot(sent.ThreadID, 10, 100, event.AttemptStateActive, 8, 80))

	own, err := reads.LocateMessage(ctx, 1, 70, sent.ThreadID, sent.ID)
	if err != nil || own == nil || own.Author == nil || own.Author.ID != 70 || own.Author.Name != "Olena" ||
		len(own.Bcc) != 1 || own.DeliveryStatus != model.EmailDeliveryStatusSent || own.DeliveredAt == nil {
		t.Fatalf("author view = %+v, %v", own, err)
	}
	other, err := reads.LocateMessage(ctx, 1, 80, sent.ThreadID, sent.ID)
	if err != nil || other == nil || other.Author == nil || len(other.Bcc) != 0 {
		t.Fatalf("participant view = %+v, %v", other, err)
	}
	if f, _ := reads.LocateMessage(ctx, 1, 70, failed.ThreadID, failed.ID); f == nil ||
		f.DeliveryErrorCode != model.EmailDeliveryErrorRejected || f.DeliveredAt != nil {
		t.Fatalf("failed view = %+v", f)
	}

	// An inbound Bcc stays hidden even from the author of the Thread.
	env.uid++
	if err := env.inbound.Handle(ctx, newEmail(env.profile, "answer@x", env.uid, withInReplyTo(sent.InternetMessageID))); err != nil {
		t.Fatal(err)
	}
	answer, _ := reads.LocateMessage(ctx, 1, 70, sent.ThreadID, int64(count(t, env.db, `SELECT id FROM email.message WHERE message_id = 'answer@x'`)))
	if answer == nil || len(answer.Bcc) != 0 {
		t.Fatalf("inbound view = %+v", answer)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	thread, err := service.NewEmailThreadReadService(reads, nil, log).LocateThread(ctx, 1, 70, sent.ThreadID)
	if err != nil || thread.ReplySignature != "Regards, Olena" {
		t.Fatalf("reply signature = %q, %v", thread.ReplySignature, err)
	}

	// A removed user keeps the stored author id.
	if _, err := env.db.Exec(ctx, `DELETE FROM directory.wbt_user WHERE id = 70`); err != nil {
		t.Fatal(err)
	}
	if gone, _ := reads.LocateMessage(ctx, 1, 80, sent.ThreadID, sent.ID); gone == nil || gone.Author == nil ||
		gone.Author.ID != 70 || gone.Author.Name != "" {
		t.Fatalf("removed author = %+v", gone)
	}
}

func TestBlindCopyReports(t *testing.T) {
	env := newDeliveryEnv(t, mailtest.Config{})
	ctx := context.Background()
	reads := storepostgres.NewEmailThreadReadStore(env.db)
	svc := env.service(true, true)
	base := time.Now().Add(time.Hour)

	create := func(to, bcc string) *model.EmailMessageView {
		in := newThread(env.profile, service.ContactModeNone, 0)
		in.Message.To = []model.EmailAddress{{Address: to}}
		if bcc != "" {
			in.Message.Bcc = []model.EmailAddress{{Address: bcc}}
		}
		_, message, err := svc.CreateThread(ctx, session(70), in)
		if err != nil {
			t.Fatal(err)
		}
		env.apply(takenSnapshot(message.ThreadID, 10, 100, event.AttemptStateActive, 8, 80))
		return message
	}
	receive := func(messageID string, option emailOption) int64 {
		env.uid++
		if err := env.inbound.Handle(ctx, newEmail(env.profile, messageID, env.uid, option)); err != nil {
			t.Fatal(err)
		}
		return int64(count(t, env.db, `SELECT id FROM email.message WHERE message_id = $1`, messageID))
	}

	withBcc := create("client@x.com", "hidden@x.com")
	id := withBcc.InternetMessageID
	visible := receive("ooo-to@x", report(model.EmailKindAutoReply, "client@x.com", base, withInReplyTo(id)))
	hidden := []int64{
		receive("bounce-original@x", report(model.EmailKindBounce, "mailer@x.com", base.Add(1*time.Minute), bouncing(id))),
		receive("bounce-references@x", report(model.EmailKindBounce, "mailer@x.com", base.Add(2*time.Minute), withReferences(id))),
		receive("ooo-bcc@x", report(model.EmailKindAutoReply, "hidden@x.com", base.Add(3*time.Minute), withInReplyTo(id))),
		receive("ooo-references@x", report(model.EmailKindAutoReply, "hidden@x.com", base.Add(4*time.Minute), withReferences(id))),
	}
	if _, err := env.db.Exec(ctx, `
INSERT INTO email.message_attachment (domain_id, message_id, file_id, disposition, file_name, mime_type, size, position, state)
VALUES (1, $1, 900, 'attachment', 'report.txt', 'text/plain', 10, 0, 'stored')`, hidden[0]); err != nil {
		t.Fatal(err)
	}
	attachment := int64(count(t, env.db, `SELECT id FROM email.message_attachment WHERE message_id = $1`, hidden[0]))

	// The author sees everything; another participant sees neither the reports nor
	// their parts, on any page, and the preview falls back to the newest visible one.
	if got := visibleIDs(t, reads, 70, withBcc.ThreadID); len(got) != 6 {
		t.Fatalf("author sees %v", got)
	}
	if got := visibleIDs(t, reads, 80, withBcc.ThreadID); len(got) != 2 || got[0] != withBcc.ID || got[1] != visible {
		t.Fatalf("participant sees %v, hidden %v", got, hidden)
	}
	for _, h := range hidden {
		if m, _ := reads.LocateMessage(ctx, 1, 80, withBcc.ThreadID, h); m != nil {
			t.Fatalf("participant located %d", h)
		}
	}
	if a, _ := reads.LocateAttachment(ctx, 1, 80, withBcc.ThreadID, hidden[0], attachment); a != nil {
		t.Fatal("participant reached the part of a hidden report")
	}
	if a, _ := reads.LocateAttachment(ctx, 1, 70, withBcc.ThreadID, hidden[0], attachment); a == nil {
		t.Fatal("author lost the part")
	}
	for user, want := range map[int64]int64{70: hidden[3], 80: visible} {
		thread, _ := reads.LocateThread(ctx, 1, user, withBcc.ThreadID)
		if thread == nil || thread.Preview == nil || thread.Preview.MessageID != want {
			t.Fatalf("preview of %d = %+v", user, thread)
		}
	}

	// An address also in To is no secret; a report on an email without Bcc is not either.
	both := create("both@x.com", "both@x.com")
	plain := create("client@x.com", "")
	shown := []struct{ thread, message int64 }{
		{both.ThreadID, receive("ooo-both@x", report(model.EmailKindAutoReply, "both@x.com", base.Add(5*time.Minute), withInReplyTo(both.InternetMessageID)))},
		{plain.ThreadID, receive("bounce-plain@x", report(model.EmailKindBounce, "mailer@x.com", base.Add(6*time.Minute), bouncing(plain.InternetMessageID)))},
	}
	for _, s := range shown {
		if m, _ := reads.LocateMessage(ctx, 1, 80, s.thread, s.message); m == nil {
			t.Fatalf("participant cannot see %d", s.message)
		}
	}
}

func TestReplayAfterDelivery(t *testing.T) {
	env := newDeliveryEnv(t, mailtest.Config{})
	ctx := context.Background()
	svc := env.service(true, true)

	// A new Thread.
	in := newThread(env.profile, service.ContactModeNone, 0)
	in.Message.Bcc = []model.EmailAddress{{Address: "hidden@x.com"}}
	_, created, err := svc.CreateThread(ctx, session(70), in)
	if err != nil {
		t.Fatal(err)
	}
	env.finish(model.EmailDeliveryStatusSent, "")

	// A reply in a Thread taken by the author.
	threadID := env.thread("replay-first@x")
	env.apply(takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))
	answer := reply(threadID, 100)
	replied, err := svc.SendReply(ctx, session(70), answer)
	if err != nil {
		t.Fatal(err)
	}
	env.finish(model.EmailDeliveryStatusFailed, model.EmailDeliveryErrorRecipientRejected)

	// The author loses both Threads.
	env.apply(projectionSnapshot(threadID, 11, 100, event.AttemptStateLeaving))
	if _, err := env.db.Exec(ctx, `DELETE FROM email.thread_participant`); err != nil {
		t.Fatal(err)
	}
	events := count(t, env.db, `SELECT count(*) FROM email.outbox`)
	versions := count(t, env.db, `SELECT sum(workspace_version) FROM email.thread`)

	_, again, err := svc.CreateThread(ctx, session(70), in)
	if err != nil || again.ID != created.ID || again.DeliveryStatus != model.EmailDeliveryStatusSent || len(again.Bcc) != 1 {
		t.Fatalf("create replay = %+v, %v", again, err)
	}
	repeated, err := svc.SendReply(ctx, session(70), answer)
	if err != nil || repeated.ID != replied.ID || repeated.DeliveryErrorCode != model.EmailDeliveryErrorRecipientRejected ||
		len(repeated.Bcc) != 1 {
		t.Fatalf("reply replay = %+v, %v", repeated, err)
	}
	if count(t, env.db, `SELECT count(*) FROM email.outbox`) != events ||
		count(t, env.db, `SELECT sum(workspace_version) FROM email.thread`) != versions {
		t.Fatal("a replay wrote an event or raised a version")
	}
}
