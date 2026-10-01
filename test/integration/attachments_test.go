//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/webitel/webitel-emails/config"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// stubUploader stands in for the storage service: the contract with the real one
// is checked by a local smoke run, not here.
type stubUploader struct {
	uploads    int
	nextFileID int64
	failWith   error
	// afterUpload runs once the file is stored but before its id is written,
	// which is the window the fenced transactions have to protect.
	afterUpload func()
}

func (s *stubUploader) UploadFile(
	_ context.Context,
	_ storageinfra.UploadRequest,
) (storageinfra.UploadResult, error) {
	if s.failWith != nil {
		return storageinfra.UploadResult{}, s.failWith
	}

	s.uploads++
	s.nextFileID++
	if s.afterUpload != nil {
		s.afterUpload()
	}

	return storageinfra.UploadResult{FileID: 5000 + s.nextFileID}, nil
}

func newAttachmentHandler(
	db *sql.DB,
	files inbound.FileUploader,
	maxAttempts int32,
) *inbound.PersistenceHandler {
	uow := storepostgres.NewUnitOfWork(db)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Storage: config.StorageConfig{MaxAttachmentAttempts: maxAttempts}}

	completer := inbound.NewMessageCompleter(uow, files, cfg, log)

	return inbound.NewPersistenceHandler(uow, completer, log)
}

func emailWithAttachment(profileID int64, messageID string, uid uint32) *model.ParsedEmail {
	email := newEmail(profileID, messageID, uid)
	email.Parts = []model.EmailPart{
		{Name: "a.pdf", ContentType: "application/pdf",
			Disposition: model.EmailPartDispositionAttachment, Size: 3, Content: []byte("pdf")},
	}

	return email
}

// A file storage refuses can never be uploaded, so the email is given up on and
// the reason must be committed together with its state.
func TestFailedAttachmentAndQuarantineAreWrittenTogether(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")

	files := &stubUploader{failWith: fmt.Errorf("%w: policy", storageinfra.ErrFileRejected)}
	handler := newAttachmentHandler(db, files, 3)

	// The email is confirmed to the cursor: it must not block the mailbox.
	if err := handler.Handle(context.Background(), emailWithAttachment(profileID, "<refused@x>", 5)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var state string
	var lastMessageAt sql.NullTime
	if err := db.QueryRowContext(context.Background(), `
SELECT m.state, t.last_message_at
FROM email.message m JOIN email.thread t ON t.id = m.thread_id
WHERE m.message_id = '<refused@x>'`).Scan(&state, &lastMessageAt); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if state != string(model.EmailMessageStateFailed) {
		t.Fatalf("state = %s, want failed", state)
	}
	if lastMessageAt.Valid {
		t.Fatal("a failed email advanced its thread")
	}

	var category string
	if err := db.QueryRowContext(context.Background(), `
SELECT category FROM email.inbound_failure
WHERE domain_id = 1 AND profile_id = $1 AND mailbox = 'INBOX' AND uid_validity = 10 AND uid = 5`,
		profileID).Scan(&category); err != nil {
		t.Fatalf("read quarantine record: %v", err)
	}
	if category != string(model.InboundFailureAttachmentUpload) {
		t.Fatalf("category = %s, want attachment_upload", category)
	}
	if rows := count(t, db, `SELECT count(*) FROM email.message_attachment WHERE file_id IS NOT NULL`); rows != 0 {
		t.Fatalf("parts with a file = %d, want 0", rows)
	}
}

// The fencing token is checked in SQL, so an owner that loses the profile while
// a file is in flight cannot bind that file or finish the email.
func TestStaleAssignmentCannotFinishAttachments(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")

	files := &stubUploader{}
	// The profile moves to another owner between the upload and the write of its
	// file id, which is exactly when the manifest transaction is fenced.
	files.afterUpload = func() {
		testhelpers.SeedAssignment(t, db, 1, profileID, "other-instance", 2)
	}

	handler := newAttachmentHandler(db, files, 5)
	email := emailWithAttachment(profileID, "<stale@x>", 6)

	if err := handler.Handle(context.Background(), email); !errors.Is(
		err, store.ErrStaleEmailProfileAssignment,
	) {
		t.Fatalf("Handle error = %v, want a stale assignment", err)
	}
	if files.uploads != 1 {
		t.Fatalf("uploads = %d, want 1", files.uploads)
	}

	var state string
	var attempts int32
	if err := db.QueryRowContext(context.Background(), `
SELECT state, attachment_attempts FROM email.message WHERE message_id = '<stale@x>'`,
	).Scan(&state, &attempts); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if state != string(model.EmailMessageStateProcessing) {
		t.Fatalf("state = %s, want processing", state)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0: a stale write is not a spent attempt", attempts)
	}
	if rows := count(t, db, `SELECT count(*) FROM email.message_attachment WHERE file_id IS NOT NULL`); rows != 0 {
		t.Fatalf("parts with a file = %d, want 0", rows)
	}

	// A redelivery under the new owner finishes the email, so the fence only
	// stops the worker that lost the profile.
	files.afterUpload = nil
	email.Assignment = model.EmailProfileAssignment{
		ProfileID: profileID, DomainID: 1, OwnerInstanceID: "other-instance", Generation: 2,
	}
	if err := handler.Handle(context.Background(), email); err != nil {
		t.Fatalf("redelivery under the new owner: %v", err)
	}
	if err := db.QueryRowContext(context.Background(),
		`SELECT state FROM email.message WHERE message_id = '<stale@x>'`).Scan(&state); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if state != string(model.EmailMessageStateReady) {
		t.Fatalf("state = %s, want ready", state)
	}
}

// A Message must not become visible while any part of its manifest is pending,
// and that rule lives in SQL rather than in the caller.
func TestMarkReadyWaitsForEveryPart(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")

	// Storage is unreachable, so both parts stay pending and the email is stored
	// as processing.
	files := &stubUploader{failWith: fmt.Errorf("%w: down", storageinfra.ErrUnavailable)}
	email := emailWithAttachment(profileID, "<waiting@x>", 8)
	email.Parts = append(email.Parts, model.EmailPart{
		Name: "b.pdf", ContentType: "application/pdf",
		Disposition: model.EmailPartDispositionAttachment, Size: 3, Content: []byte("two"),
	})
	if err := newAttachmentHandler(db, files, 5).Handle(context.Background(), email); err == nil {
		t.Fatal("an unfinished email was confirmed to the cursor")
	}

	ctx := context.Background()
	uow := storepostgres.NewUnitOfWork(db)
	messages := uow.EmailMessageStore()
	attachments := uow.EmailMessageAttachmentStore()

	var messageID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM email.message WHERE message_id = '<waiting@x>'`).Scan(&messageID); err != nil {
		t.Fatalf("read message: %v", err)
	}

	state, err := messages.MarkReady(ctx, 1, messageID)
	if err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if state != model.EmailMessageStateProcessing {
		t.Fatalf("state = %s, want processing while two parts are pending", state)
	}

	if stored, err := attachments.MarkStored(ctx, 1, messageID, 0, 777); err != nil || !stored {
		t.Fatalf("MarkStored(0) = %v, %v", stored, err)
	}
	state, err = messages.MarkReady(ctx, 1, messageID)
	if err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if state != model.EmailMessageStateProcessing {
		t.Fatalf("state = %s, want processing while one part is pending", state)
	}

	if stored, err := attachments.MarkStored(ctx, 1, messageID, 1, 778); err != nil || !stored {
		t.Fatalf("MarkStored(1) = %v, %v", stored, err)
	}
	state, err = messages.MarkReady(ctx, 1, messageID)
	if err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if state != model.EmailMessageStateReady {
		t.Fatalf("state = %s, want ready once every part is stored", state)
	}

	// A second upload of a finished part changes nothing.
	if stored, err := attachments.MarkStored(ctx, 1, messageID, 1, 999); err != nil || stored {
		t.Fatalf("MarkStored of a stored part = %v, %v, want false", stored, err)
	}
}

// The attempt, the move to failed and the quarantine record are one transaction:
// if the record cannot be written, none of them happened.
func TestQuarantineFailureRollsBackTheAttempt(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")

	// An email without a mailbox cannot be quarantined: email.inbound_failure
	// requires one. The Message itself stores fine, which is what makes this a
	// rollback test rather than a validation test.
	email := emailWithAttachment(profileID, "<rollback@x>", 9)
	email.Mailbox = ""

	files := &stubUploader{failWith: fmt.Errorf("%w: down", storageinfra.ErrUnavailable)}
	// One attempt, so the first failure is already the last one.
	if err := newAttachmentHandler(db, files, 1).Handle(context.Background(), email); err == nil {
		t.Fatal("a failed quarantine write was reported as success")
	}

	var state string
	var attempts int32
	if err := db.QueryRowContext(context.Background(), `
SELECT state, attachment_attempts FROM email.message WHERE message_id = '<rollback@x>'`,
	).Scan(&state, &attempts); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if state != string(model.EmailMessageStateProcessing) {
		t.Fatalf("state = %s, want processing", state)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0 after a rolled back transaction", attempts)
	}
	if rows := count(t, db, `SELECT count(*) FROM email.inbound_failure`); rows != 0 {
		t.Fatalf("quarantine rows = %d, want 0", rows)
	}
}
