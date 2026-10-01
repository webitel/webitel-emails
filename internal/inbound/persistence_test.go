package inbound

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/config"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/model"
)

func testHandler(fake *fakeStore) *PersistenceHandler {
	return testUploadingHandler(fake, &fakeUploader{})
}

func testUploadingHandler(fake *fakeStore, files *fakeUploader) *PersistenceHandler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Storage: config.StorageConfig{MaxAttachmentAttempts: 3}}
	completer := NewMessageCompleter(fake, files, cfg, log)

	return NewPersistenceHandler(fake, completer, log)
}

// fakeUploader stores files in memory and can fail after a chosen number of
// successful uploads, which is how an interrupted email is simulated.
type fakeUploader struct {
	requests   []storageinfra.UploadRequest
	failAfter  int
	failWith   error
	nextFileID int64
}

func (f *fakeUploader) UploadFile(
	_ context.Context,
	req storageinfra.UploadRequest,
) (storageinfra.UploadResult, error) {
	if f.failWith != nil && len(f.requests) >= f.failAfter {
		return storageinfra.UploadResult{}, f.failWith
	}

	f.requests = append(f.requests, req)
	f.nextFileID++

	return storageinfra.UploadResult{FileID: 1000 + f.nextFileID, Size: int64(len(req.Content))}, nil
}

// withParts gives the email two uploadable parts and one the parser skipped.
func withParts(email *model.ParsedEmail) *model.ParsedEmail {
	email.Parts = []model.EmailPart{
		{Name: "a.pdf", ContentType: "application/pdf", Disposition: model.EmailPartDispositionAttachment,
			Size: 3, Content: []byte("pdf")},
		{Name: "b.png", ContentType: "image/png", ContentID: "cid-b",
			Disposition: model.EmailPartDispositionInline, Size: 3, Content: []byte("png")},
		{Name: "big.bin", ContentType: "application/octet-stream",
			Disposition: model.EmailPartDispositionAttachment, Size: 99,
			SkippedReason: model.EmailPartSkippedSizeLimit},
	}

	return email
}

func testEmail(messageID string, generated bool, raw string, uidValidity, uid uint32) *model.ParsedEmail {
	sum := sha256.Sum256([]byte(raw))
	sent := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	return &model.ParsedEmail{
		DomainID: 1, ProfileID: 7,
		Mailbox: "INBOX", UIDValidity: uidValidity, UID: uid,
		MessageID: messageID, MessageIDGenerated: generated, RawSHA256: sum[:],
		Subject: "Subject A", Kind: model.EmailKindRegular, TextBody: "text",
		SentAt:     &sent,
		ReceivedAt: sent.Add(time.Minute),
		From:       []model.EmailAddress{{Name: "A", Address: "A@X.com", NormalizedAddress: "a@x.com"}},
		To: []model.EmailAddress{
			{Address: "B@X.com", NormalizedAddress: "b@x.com"},
			{Address: "C@X.com", NormalizedAddress: "c@x.com"},
		},
		Cc: []model.EmailAddress{{Address: "D@X.com", NormalizedAddress: "d@x.com"}},
	}
}

func TestPersistenceStoresEmail(t *testing.T) {
	fake := newFakeStore()
	if err := testHandler(fake).Handle(context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(fake.threads) != 1 || len(fake.messages) != 1 {
		t.Fatalf("threads=%d messages=%d, want 1/1", len(fake.threads), len(fake.messages))
	}
	if fake.calls[0] != "lock" {
		t.Fatalf("the profile lock must come first, call order: %v", fake.calls)
	}

	message := fake.messages[0]
	if message.State != model.EmailMessageStateReady {
		t.Fatalf("state = %q, want ready", message.State)
	}
	if message.IMAP == nil || message.IMAP.UID != 5 {
		t.Fatalf("imap identity = %#v", message.IMAP)
	}
	if thread := fake.threads[0]; thread.LastMessageAt == nil || !thread.LastMessageAt.Equal(message.ReceivedAt) {
		t.Fatalf("last_message_at = %v, want %v", thread.LastMessageAt, message.ReceivedAt)
	}

	recipients := fake.recipients[message.ID]
	got := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		got = append(got, string(recipient.Type)+":"+recipient.NormalizedAddress)
	}
	want := []string{"from:a@x.com", "to:b@x.com", "to:c@x.com", "cc:d@x.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	if recipients[1].Ordinal != 0 || recipients[2].Ordinal != 1 {
		t.Fatalf("ordinals within To = %d,%d, want 0,1", recipients[1].Ordinal, recipients[2].Ordinal)
	}
	if recipients[0].Address != "A@X.com" {
		t.Fatalf("the original address must be kept, got %q", recipients[0].Address)
	}
}

func TestPersistenceDeduplicates(t *testing.T) {
	tests := []struct {
		name   string
		first  *model.ParsedEmail
		second *model.ParsedEmail
	}{
		{
			name:   "same mail identifier",
			first:  testEmail("<m1@x>", false, "raw-1", 10, 5),
			second: testEmail("<m1@x>", false, "raw-1", 10, 5),
		},
		{
			// Only the raw checksum still recognizes it: the generated identifier
			// moved with the mailbox coordinates.
			name:   "generated identifier after uidvalidity change",
			first:  testEmail("imap.generated-1@webitel.local", true, "raw-2", 10, 6),
			second: testEmail("imap.generated-2@webitel.local", true, "raw-2", 11, 3),
		},
		{
			// One mailbox position holds one email, so the same coordinates are
			// the same delivery even when the header disagrees.
			name:   "same mailbox position",
			first:  testEmail("<m1@x>", false, "raw-1", 10, 5),
			second: testEmail("<m2@x>", false, "raw-3", 10, 5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeStore()
			handler := testHandler(fake)
			ctx := context.Background()

			if err := handler.Handle(ctx, tt.first); err != nil {
				t.Fatalf("first delivery: %v", err)
			}
			if err := handler.Handle(ctx, tt.second); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			if len(fake.threads) != 1 || len(fake.messages) != 1 {
				t.Fatalf("redelivery duplicated: threads=%d messages=%d", len(fake.threads), len(fake.messages))
			}
		})
	}
}

func TestPersistenceResumesUnfinishedAttachments(t *testing.T) {
	fake := newFakeStore()
	// The first part is stored, the second fails, so the email stays unfinished.
	files := &fakeUploader{failAfter: 1, failWith: errors.New("storage is down")}
	handler := testUploadingHandler(fake, files)
	ctx := context.Background()

	if err := handler.Handle(ctx, withParts(testEmail("<m1@x>", false, "raw-1", 10, 5))); err == nil {
		t.Fatal("an unfinished email was confirmed to the cursor")
	}
	if fake.messages[0].State != model.EmailMessageStateProcessing {
		t.Fatalf("state = %s, want processing", fake.messages[0].State)
	}
	if fake.messages[0].AttachmentAttempts != 1 {
		t.Fatalf("attempts = %d, want 1", fake.messages[0].AttachmentAttempts)
	}
	if fake.threads[0].LastMessageAt != nil {
		t.Fatal("an unfinished email advanced its thread")
	}

	files.failWith = nil
	if err := handler.Handle(ctx, withParts(testEmail("<m1@x>", false, "raw-1", 10, 5))); err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	// Two uploads in total: the part stored before the failure is not sent again,
	// and the skipped part is never sent.
	if len(files.requests) != 2 {
		t.Fatalf("uploads = %d, want 2", len(files.requests))
	}
	if files.requests[1].ReferenceID == files.requests[0].ReferenceID {
		t.Fatalf("the same part was uploaded twice: %s", files.requests[1].ReferenceID)
	}
	if fake.messages[0].State != model.EmailMessageStateReady {
		t.Fatalf("state = %s, want ready", fake.messages[0].State)
	}
	if fake.threads[0].LastMessageAt == nil {
		t.Fatal("a completed email did not advance its thread")
	}
	if len(fake.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(fake.messages))
	}
}

func TestPersistenceCanceledUploadDoesNotSpendAnAttempt(t *testing.T) {
	fake := newFakeStore()
	files := &fakeUploader{failWith: context.Canceled}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := testUploadingHandler(fake, files).Handle(
		ctx, withParts(testEmail("<m1@x>", false, "raw-1", 10, 5)),
	); err == nil {
		t.Fatal("a canceled delivery was confirmed to the cursor")
	}
	if fake.messages[0].AttachmentAttempts != 0 {
		t.Fatalf("attempts = %d, want 0", fake.messages[0].AttachmentAttempts)
	}
}

func TestPersistenceRollsBackThreadWhenMessageFails(t *testing.T) {
	fake := newFakeStore()
	fake.messageErr = errors.New("conflict")

	err := testHandler(fake).Handle(context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5))
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if len(fake.threads) != 0 {
		t.Fatalf("an orphan thread survived the rollback: %d", len(fake.threads))
	}
}
