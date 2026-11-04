//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func addRecipient(t *testing.T, db *pgxpool.Pool, messageID int64, kind, address string, ordinal int) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
INSERT INTO email.recipient (domain_id, message_id, type, address, normalized_address, display_name, ordinal)
VALUES (1, $1, $2, $3, lower($3), upper($3), $4)`, messageID, kind, address, ordinal); err != nil {
		t.Fatalf("add recipient: %v", err)
	}
}

func messageIDs(views []*model.EmailMessageView) []int64 {
	ids := make([]int64, 0, len(views))
	for _, view := range views {
		ids = append(ids, view.ID)
	}
	return ids
}

func TestMessageReadModel(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "messages")
	s := storepostgres.NewEmailThreadReadStore(db)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	thread := seedThread(t, db, 1, profile, "regular", "new", nil)
	m1 := insertMessage(t, db, 1, profile, thread, "<m1@x>", "ready", "a@x.com", "one", base)
	m2 := insertMessage(t, db, 1, profile, thread, "<m2@x>", "ready", "b@x.com", "two", base)
	insertMessage(t, db, 1, profile, thread, "<m3@x>", "processing", "c@x.com", "three", base.Add(time.Minute))
	insertMessage(t, db, 1, profile, thread, "<m4@x>", "failed", "d@x.com", "four", base.Add(2*time.Minute))
	m5 := insertMessage(t, db, 1, profile, thread, "<m5@x>", "ready", "mailer@x.com", "", base.Add(time.Hour))
	if _, err := db.Exec(ctx, `
UPDATE email.message SET kind = 'bounce', bounce_recipient = 'gone@y.com', bounce_status = '5.1.1'
WHERE id = $1`, m5); err != nil {
		t.Fatal(err)
	}
	assignThread(t, db, thread, "in_progress", 70)

	addRecipient(t, db, m1, "to", "to1@x.com", 0)
	addRecipient(t, db, m1, "to", "to2@x.com", 1)
	addRecipient(t, db, m1, "cc", "cc@x.com", 0)
	addRecipient(t, db, m1, "bcc", "secret@x.com", 0)
	addRecipient(t, db, m1, "reply_to", "reply@x.com", 0)
	if _, err := db.Exec(ctx, `
INSERT INTO email.message_attachment (domain_id, message_id, file_id, content_id, disposition, file_name, mime_type, size, position, state, skipped_reason)
VALUES (1, $1, NULL, NULL, 'attachment', 'big.zip', 'application/zip', 999, 2, 'skipped', 'attachment_size_limit_exceeded'),
       (1, $1, 777, 'logo@x', 'inline', 'logo.png', 'image/png', 10, 0, 'stored', NULL),
       (1, $1, 778, NULL, 'attachment', 'doc.pdf', 'application/pdf', 20, 1, 'stored', NULL)`, m1); err != nil {
		t.Fatal(err)
	}

	page := func(after *model.EmailMessageCursor, desc bool, size int, user int64) ([]*model.EmailMessageView, bool) {
		t.Helper()
		views, next, err := s.ListMessages(ctx, model.EmailMessageFilter{
			DomainID: 1, UserID: user, ThreadID: thread, After: after, Desc: desc, Size: size,
		})
		if err != nil {
			t.Fatal(err)
		}
		return views, next
	}

	t.Run("ascending traversal without repeats or unfinished messages", func(t *testing.T) {
		first, next := page(nil, false, 2, 70)
		if !sameIDs(messageIDs(first), []int64{m1, m2}) || !next {
			t.Fatalf("first page %v, next %v", messageIDs(first), next)
		}
		last := first[len(first)-1]
		second, next := page(&model.EmailMessageCursor{ReceivedAt: last.ReceivedAt, ID: last.ID}, false, 2, 70)
		if !sameIDs(messageIDs(second), []int64{m5}) || next {
			t.Fatalf("second page %v, next %v", messageIDs(second), next)
		}
	})

	t.Run("descending traversal", func(t *testing.T) {
		first, next := page(nil, true, 2, 70)
		if !sameIDs(messageIDs(first), []int64{m5, m2}) || !next {
			t.Fatalf("first page %v", messageIDs(first))
		}
		second, next := page(&model.EmailMessageCursor{ReceivedAt: first[1].ReceivedAt, ID: first[1].ID}, true, 2, 70)
		if !sameIDs(messageIDs(second), []int64{m1}) || next {
			t.Fatalf("second page %v", messageIDs(second))
		}
	})

	t.Run("details of a message", func(t *testing.T) {
		view, err := s.LocateMessage(ctx, 1, 70, thread, m1)
		if err != nil || view == nil {
			t.Fatalf("locate: %v, %v", view, err)
		}
		if len(view.To) != 2 || view.To[0].Address != "to1@x.com" || view.To[1].Address != "to2@x.com" ||
			len(view.Cc) != 1 || len(view.ReplyTo) != 1 || len(view.From) != 1 || view.From[0].Address != "a@x.com" {
			t.Fatalf("recipients %+v", view)
		}
		for _, list := range [][]model.EmailAddress{view.From, view.Sender, view.ReplyTo, view.To, view.Cc} {
			for _, address := range list {
				if address.Address == "secret@x.com" {
					t.Fatal("a blind copy recipient was returned")
				}
			}
		}
		if len(view.Attachments) != 3 || view.Attachments[0].FileName != "logo.png" ||
			view.Attachments[0].ContentID != "logo@x" || view.Attachments[1].FileName != "doc.pdf" ||
			view.Attachments[2].State != model.EmailAttachmentStateSkipped ||
			view.Attachments[2].SkippedReason != model.EmailPartSkippedSizeLimit {
			t.Fatalf("attachments %+v", view.Attachments)
		}

		bounce, err := s.LocateMessage(ctx, 1, 70, thread, m5)
		if err != nil || bounce == nil || bounce.Kind != model.EmailKindBounce || bounce.Bounce == nil ||
			bounce.Bounce.Recipient != "gone@y.com" || bounce.Bounce.Status != "5.1.1" {
			t.Fatalf("bounce %+v, %v", bounce, err)
		}
	})

	t.Run("nothing outside the boundary", func(t *testing.T) {
		other := seedThread(t, db, 1, profile, "regular", "new", nil)
		insertMessage(t, db, 1, profile, other, "<o1@x>", "ready", "o@x.com", "o", base)
		addParticipant(t, db, 1, other, 70)

		if views, _ := page(nil, false, 10, 71); len(views) != 0 {
			t.Fatalf("another agent read %v", messageIDs(views))
		}
		for _, c := range []struct{ domain, user, thread, message int64 }{
			{1, 70, other, m1},  // message of another visible thread
			{1, 70, thread, 0},  // missing
			{1, 71, thread, m1}, // another agent
			{2, 70, thread, m1}, // another domain
		} {
			if view, err := s.LocateMessage(ctx, c.domain, c.user, c.thread, c.message); err != nil || view != nil {
				t.Errorf("%+v: %+v, %v", c, view, err)
			}
		}
		var processing int64
		if err := db.QueryRow(ctx, `SELECT id FROM email.message WHERE message_id = '<m3@x>'`).Scan(&processing); err != nil {
			t.Fatal(err)
		}
		if view, err := s.LocateMessage(ctx, 1, 70, thread, processing); err != nil || view != nil {
			t.Fatalf("processing message: %+v, %v", view, err)
		}
	})
}

func TestAttachmentLocateChain(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "attachments")
	s := storepostgres.NewEmailThreadReadStore(db)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	thread := seedThread(t, db, 1, profile, "regular", "new", nil)
	ready := insertMessage(t, db, 1, profile, thread, "<a1@x>", "ready", "a@x.com", "a", base)
	processing := insertMessage(t, db, 1, profile, thread, "<a2@x>", "processing", "a@x.com", "a", base)
	assignThread(t, db, thread, "in_progress", 70)
	other := seedThread(t, db, 1, profile, "regular", "new", nil)
	otherMessage := insertMessage(t, db, 1, profile, other, "<o1@x>", "ready", "o@x.com", "o", base)
	addParticipant(t, db, 1, other, 70)

	insertPart := func(messageID int64, position int, state string, fileID *int64) int64 {
		t.Helper()
		var reason *string
		if state == "skipped" {
			r := "attachment_size_limit_exceeded"
			reason = &r
		}
		var id int64
		if err := db.QueryRow(ctx, `
INSERT INTO email.message_attachment (domain_id, message_id, file_id, disposition, file_name, mime_type, size, position, state, skipped_reason)
VALUES (1, $1, $2, 'attachment', 'f.pdf', 'application/pdf', 5, $3, $4, $5) RETURNING id`,
			messageID, fileID, position, state, reason).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	stored := insertPart(ready, 0, "stored", ptrInt64(777))
	skipped := insertPart(ready, 1, "skipped", nil)
	ofProcessing := insertPart(processing, 0, "stored", ptrInt64(778))
	ofOther := insertPart(otherMessage, 0, "stored", ptrInt64(779))

	part, err := s.LocateAttachment(ctx, 1, 70, thread, ready, stored)
	if err != nil || part == nil || part.FileID == nil || *part.FileID != 777 || part.State != model.EmailAttachmentStateStored {
		t.Fatalf("stored part %+v, %v", part, err)
	}
	if part, err := s.LocateAttachment(ctx, 1, 70, thread, ready, skipped); err != nil || part == nil ||
		part.State != model.EmailAttachmentStateSkipped || part.FileID != nil {
		t.Fatalf("skipped part %+v, %v", part, err)
	}

	for _, c := range []struct {
		name                                      string
		domain, user, thread, message, attachment int64
	}{
		{"part of another message", 1, 70, thread, ready, ofOther},
		{"message of another thread", 1, 70, thread, otherMessage, ofOther},
		{"own message under another visible thread", 1, 70, other, ready, stored},
		{"processing message", 1, 70, thread, processing, ofProcessing},
		{"another agent", 1, 71, thread, ready, stored},
		{"another domain", 2, 70, thread, ready, stored},
	} {
		if part, err := s.LocateAttachment(ctx, c.domain, c.user, c.thread, c.message, c.attachment); err != nil || part != nil {
			t.Errorf("%s: %+v, %v", c.name, part, err)
		}
	}
}
