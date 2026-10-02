//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func newHandler(db *sql.DB) *inbound.PersistenceHandler {
	return inbound.NewPersistenceHandler(
		storepostgres.NewUnitOfWork(db),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

type emailOption func(*model.ParsedEmail)

func withInReplyTo(value string) emailOption {
	return func(email *model.ParsedEmail) { email.InReplyTo = value }
}

func withReferences(values ...string) emailOption {
	return func(email *model.ParsedEmail) { email.References = values }
}

func withKind(kind model.EmailKind) emailOption {
	return func(email *model.ParsedEmail) { email.Kind = kind }
}

func newEmail(profileID int64, messageID string, uid uint32, options ...emailOption) *model.ParsedEmail {
	checksum := sha256.Sum256([]byte("raw-" + messageID))
	sent := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	email := &model.ParsedEmail{
		DomainID: 1, ProfileID: profileID,
		Mailbox: "INBOX", UIDValidity: 10, UID: uid,
		MessageID: messageID, RawSHA256: checksum[:],
		Subject: "Subject " + messageID, Kind: model.EmailKindRegular,
		TextBody: "text", HTMLBody: "<p>text</p>",
		SentAt: &sent, ReceivedAt: sent.Add(time.Minute),
		From:   []model.EmailAddress{{Name: "Sender", Address: "A@X.com", NormalizedAddress: "a@x.com"}},
		Sender: &model.EmailAddress{Name: "Envelope Sender", Address: "sender@X.com", NormalizedAddress: "sender@x.com"},
		ReplyTo: []model.EmailAddress{
			{Name: "Reply", Address: "Reply@X.com", NormalizedAddress: "reply@x.com"},
		},
		To: []model.EmailAddress{
			{Address: "B@X.com", NormalizedAddress: "b@x.com"},
			{Address: "C@X.com", NormalizedAddress: "c@x.com"},
		},
		Cc:  []model.EmailAddress{{Address: "D@X.com", NormalizedAddress: "d@x.com"}},
		Bcc: []model.EmailAddress{{Address: "E@X.com", NormalizedAddress: "e@x.com"}},
	}
	for _, option := range options {
		option(email)
	}

	return email
}

// TestPersistenceRoundTrip checks that everything written by the aggregate comes
// back unchanged, including the array and time columns the fake cannot model.
func TestPersistenceRoundTrip(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")

	email := newEmail(profileID, "<first@x>", 5, withReferences(`<a,b@x>`, `<"quoted"@x>`))
	email.Kind = model.EmailKindBounce
	email.Bounce = &model.EmailBounceInfo{
		OriginalMessageID: "<original@x>",
		Recipient:         "missing@x.com",
		Status:            "5.1.1",
		DiagnosticCode:    "550 user unknown",
	}
	if err := newHandler(db).Handle(context.Background(), email); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var (
		references                              []string
		messageID, direction, kind, state       string
		subject, bodyText, bodyHTML             string
		bounceOriginal, bounceRecipient         sql.NullString
		bounceStatus, bounceDiagnostic          sql.NullString
		sentAt, receivedAt                      time.Time
		threadSubject, threadKind, threadStatus string
		lastMessageAt                           sql.NullTime
	)
	err := db.QueryRowContext(context.Background(), `
SELECT m.message_id, m.direction, m.kind, m.state, m.message_references,
       m.subject, m.body_text, m.body_html,
       m.bounce_original_message_id, m.bounce_recipient, m.bounce_status, m.bounce_diagnostic_code,
       m.sent_at, m.received_at, t.subject, t.kind, t.status, t.last_message_at
FROM email.message m JOIN email.thread t ON t.id = m.thread_id`).
		Scan(&messageID, &direction, &kind, &state, pqArray(&references),
			&subject, &bodyText, &bodyHTML,
			&bounceOriginal, &bounceRecipient, &bounceStatus, &bounceDiagnostic,
			&sentAt, &receivedAt, &threadSubject, &threadKind, &threadStatus, &lastMessageAt)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if len(references) != 2 || references[0] != `<a,b@x>` || references[1] != `<"quoted"@x>` {
		t.Fatalf("references = %#v", references)
	}
	if messageID != email.MessageID || direction != "inbound" || kind != "bounce" || state != "ready" {
		t.Fatalf("message identity = %q/%q/%q/%q", messageID, direction, kind, state)
	}
	if subject != email.Subject || bodyText != email.TextBody || bodyHTML != email.HTMLBody {
		t.Fatalf("message content = %q/%q/%q", subject, bodyText, bodyHTML)
	}
	if bounceOriginal.String != email.Bounce.OriginalMessageID ||
		bounceRecipient.String != email.Bounce.Recipient ||
		bounceStatus.String != email.Bounce.Status ||
		bounceDiagnostic.String != email.Bounce.DiagnosticCode {
		t.Fatalf("bounce = %q/%q/%q/%q", bounceOriginal.String, bounceRecipient.String,
			bounceStatus.String, bounceDiagnostic.String)
	}
	if !sentAt.Equal(*email.SentAt) || !receivedAt.Equal(email.ReceivedAt) {
		t.Fatalf("sent_at = %v, received_at = %v", sentAt, receivedAt)
	}
	if threadSubject != email.Subject || threadKind != "service" || threadStatus != "processed" {
		t.Fatalf("thread = subject %q, kind %q, status %q", threadSubject, threadKind, threadStatus)
	}
	if !lastMessageAt.Valid || !lastMessageAt.Time.Equal(email.ReceivedAt) {
		t.Fatalf("last_message_at = %v, want %v", lastMessageAt, email.ReceivedAt)
	}

	recipients := readRecipients(t, db)
	want := []string{
		"from:a@x.com:0",
		"sender:sender@x.com:0",
		"reply_to:reply@x.com:0",
		"to:b@x.com:0",
		"to:c@x.com:1",
		"cc:d@x.com:0",
		"bcc:e@x.com:0",
	}
	if len(recipients) != len(want) {
		t.Fatalf("recipients = %v, want %v", recipients, want)
	}
	for i, value := range want {
		if recipients[i] != value {
			t.Fatalf("recipients = %v, want %v", recipients, want)
		}
	}
}

func TestPersistenceRollsBackTheAggregateOnRecipientError(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")

	email := newEmail(profileID, "<invalid-recipient@x>", 6)
	email.To[0].NormalizedAddress = ""
	if err := newHandler(db).Handle(context.Background(), email); err == nil {
		t.Fatal("an invalid recipient was accepted")
	}
	if threads, messages, recipients :=
		count(t, db, `SELECT count(*) FROM email.thread`),
		count(t, db, `SELECT count(*) FROM email.message`),
		count(t, db, `SELECT count(*) FROM email.recipient`); threads != 0 || messages != 0 || recipients != 0 {
		t.Fatalf("partial aggregate survived rollback: threads=%d messages=%d recipients=%d",
			threads, messages, recipients)
	}
}

// TestConcurrentDeliveryKeepsOneConversation is the reason the profile lock
// exists: two different emails of one conversation may arrive at the same time,
// and no unique index can keep them together.
func TestConcurrentDeliveryKeepsOneConversation(t *testing.T) {
	db := testhelpers.Database(t)

	t.Run("the same email delivered many times", func(t *testing.T) {
		testhelpers.Reset(t, db)
		profileID := testhelpers.SeedProfile(t, db, 1, "owner")
		handler := newHandler(db)

		deliverConcurrently(t, 8, func() error {
			return handler.Handle(context.Background(), newEmail(profileID, "<same@x>", 5))
		})

		if threads, messages := count(t, db, `SELECT count(*) FROM email.thread`),
			count(t, db, `SELECT count(*) FROM email.message`); threads != 1 || messages != 1 {
			t.Fatalf("threads=%d messages=%d, want 1/1", threads, messages)
		}
	})

	t.Run("different replies to the same unknown parent", func(t *testing.T) {
		testhelpers.Reset(t, db)
		profileID := testhelpers.SeedProfile(t, db, 1, "owner")
		handler := newHandler(db)

		emails := make([]*model.ParsedEmail, 0, 6)
		for index := range 6 {
			emails = append(emails, newEmail(profileID,
				messageIDAt(index), uint32(index+1), withInReplyTo("<unknown-parent@x>")))
		}

		deliverConcurrentlyEach(t, emails, handler)

		if threads := count(t, db, `SELECT count(*) FROM email.thread`); threads != 1 {
			t.Fatalf("threads = %d, want 1: the siblings were split", threads)
		}
		if messages := count(t, db, `SELECT count(*) FROM email.message`); messages != len(emails) {
			t.Fatalf("messages = %d, want %d", messages, len(emails))
		}
	})
}

func deliverConcurrently(t *testing.T, writers int, deliver func() error) {
	t.Helper()

	var group sync.WaitGroup
	errs := make([]error, writers)
	for index := range writers {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			errs[index] = deliver()
		}(index)
	}
	group.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", index, err)
		}
	}
}

func deliverConcurrentlyEach(t *testing.T, emails []*model.ParsedEmail, handler *inbound.PersistenceHandler) {
	t.Helper()

	var group sync.WaitGroup
	errs := make([]error, len(emails))
	for index, email := range emails {
		group.Add(1)
		go func(index int, email *model.ParsedEmail) {
			defer group.Done()
			errs[index] = handler.Handle(context.Background(), email)
		}(index, email)
	}
	group.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", index, err)
		}
	}
}

func messageIDAt(index int) string {
	return "<sibling-" + string(rune('a'+index)) + "@x>"
}
