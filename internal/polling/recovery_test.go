package polling

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
)

var recoveryRawMessage = []byte("From: sender@example.org\r\nTo: receiver@example.org\r\nSubject: Hello\r\n\r\nBody")

// storedCursor is the state left by an earlier epoch of the mailbox.
func storedCursor(lastUID uint32, confirmed time.Time) *model.ProviderCursor {
	return &model.ProviderCursor{IMAP: &model.IMAPCursor{
		Mailbox:          "INBOX",
		UIDValidity:      10,
		LastUID:          lastUID,
		LastInternalDate: confirmed,
	}}
}

func TestRecoveryReadsOnlyThePartAfterTheCheckpoint(t *testing.T) {
	checkpoint := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	next := new(recordingParsedMessageHandler)
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), next))

	// The mailbox reassigned its UIDs: the same emails now sit at 1..4.
	session := &testIMAPSession{
		raw:           recoveryRawMessage,
		uidValidity:   11,
		availableUIDs: []uint32{1, 2, 3, 4},
		datesByUID: map[uint32]time.Time{
			1: checkpoint.Add(-48 * time.Hour), // history, never read
			2: checkpoint.Add(-time.Hour),      // already read under the old UIDs
			3: checkpoint,                      // the checkpoint itself
			4: checkpoint.Add(time.Hour),       // genuinely new
		},
	}

	result := scheduler.sync(context.Background(), session, testProfile(), storedCursor(7, checkpoint), testLogger())

	if result.imapErr != nil || result.handlerErr != nil {
		t.Fatalf("imapErr=%v handlerErr=%v", result.imapErr, result.handlerErr)
	}
	// The scan must never restart from the beginning of the mailbox.
	if session.searchedSince.IsZero() {
		t.Fatal("recovery searched without a date window")
	}
	if want := checkpoint.AddDate(0, 0, -1); !session.searchedSince.Equal(want) {
		t.Fatalf("SEARCH SINCE = %v, want %v", session.searchedSince, want)
	}
	if len(session.events) < 2 || session.events[0] != "metadata" || session.events[1] != "raw" {
		t.Fatalf("fetch order = %v, want metadata before raw", session.events)
	}
	// UID 3 shares the checkpoint's timestamp, so it is passed on rather than
	// assumed to be the email the checkpoint came from.
	if len(session.fetchedUIDs) != 2 || session.fetchedUIDs[0] != 3 || session.fetchedUIDs[1] != 4 {
		t.Fatalf("downloaded UIDs = %v, want the ones at and after the checkpoint", session.fetchedUIDs)
	}
	if next.calls != 2 {
		t.Fatalf("handler calls = %d, want 2", next.calls)
	}
	if result.cursor.IMAP.Recovery != nil {
		t.Fatalf("recovery did not finish: %+v", result.cursor.IMAP.Recovery)
	}
	if result.cursor.IMAP.UIDValidity != 11 || result.cursor.IMAP.LastUID != 4 {
		t.Fatalf("cursor = %+v, want the end of the new epoch", result.cursor.IMAP)
	}
}

func TestRecoveryResumesAcrossPolls(t *testing.T) {
	checkpoint := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), new(recordingParsedMessageHandler)))
	scheduler.cfg.MaxMessagesPerPoll = 2

	dates := map[uint32]time.Time{}
	for uid := uint32(1); uid <= 5; uid++ {
		dates[uid] = checkpoint.Add(time.Duration(uid) * time.Hour)
	}
	session := &testIMAPSession{
		raw:           recoveryRawMessage,
		uidValidity:   11,
		availableUIDs: []uint32{1, 2, 3, 4, 5},
		datesByUID:    dates,
	}

	first := scheduler.sync(context.Background(), session, testProfile(), storedCursor(7, checkpoint), testLogger())
	if first.imapErr != nil || first.handlerErr != nil {
		t.Fatalf("first poll: %v %v", first.imapErr, first.handlerErr)
	}
	if first.cursor.IMAP.Recovery == nil {
		t.Fatal("recovery finished too early")
	}
	if !first.more {
		t.Fatal("the scheduler was not asked to continue")
	}
	if first.cursor.IMAP.LastUID != 2 {
		t.Fatalf("progress = %d, want 2", first.cursor.IMAP.LastUID)
	}
	if first.cursor.IMAP.Recovery.ToUID != 5 {
		t.Fatalf("recovery_to_uid = %d, want fixed upper bound 5", first.cursor.IMAP.Recovery.ToUID)
	}

	// The second poll continues from the saved progress instead of starting over.
	session.fetchedUIDs = nil
	second := scheduler.sync(context.Background(), session, testProfile(), first.cursor, testLogger())
	if len(session.fetchedUIDs) != 2 || session.fetchedUIDs[0] != 3 || session.fetchedUIDs[1] != 4 {
		t.Fatalf("second poll downloaded %v, want [3 4]", session.fetchedUIDs)
	}
	if second.cursor.IMAP.LastUID != 4 {
		t.Fatalf("progress = %d, want 4", second.cursor.IMAP.LastUID)
	}
	if second.cursor.IMAP.Recovery == nil || second.cursor.IMAP.Recovery.ToUID != 5 {
		t.Fatalf("recovery upper bound changed: %+v", second.cursor.IMAP.Recovery)
	}

	session.fetchedUIDs = nil
	third := scheduler.sync(context.Background(), session, testProfile(), second.cursor, testLogger())
	if len(session.fetchedUIDs) != 1 || session.fetchedUIDs[0] != 5 {
		t.Fatalf("third poll downloaded %v, want [5]", session.fetchedUIDs)
	}
	if third.cursor.IMAP.Recovery != nil || third.cursor.IMAP.LastUID != 5 {
		t.Fatalf("recovery did not finish at its fixed upper bound: %+v", third.cursor.IMAP)
	}
}

func TestRecoveryResumesAfterTransientError(t *testing.T) {
	checkpoint := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	next := &failOnceParsedMessageHandler{failUID: 4}
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), next))
	session := &testIMAPSession{
		raw:           recoveryRawMessage,
		uidValidity:   11,
		availableUIDs: []uint32{1, 2, 3, 4, 5},
		datesByUID: map[uint32]time.Time{
			1: checkpoint.Add(time.Hour),
			2: checkpoint.Add(2 * time.Hour),
			3: checkpoint.Add(3 * time.Hour),
			4: checkpoint.Add(4 * time.Hour),
			5: checkpoint.Add(5 * time.Hour),
		},
	}

	failed := scheduler.sync(context.Background(), session, testProfile(), storedCursor(7, checkpoint), testLogger())
	if !errors.Is(failed.handlerErr, errTransientTest) {
		t.Fatalf("handler error = %v, want transient failure", failed.handlerErr)
	}
	if failed.cursor.IMAP.LastUID != 3 || failed.cursor.IMAP.Recovery == nil {
		t.Fatalf("cursor after failure = %+v, want confirmed prefix through UID 3", failed.cursor.IMAP)
	}

	session.fetchedUIDs = nil
	retried := scheduler.sync(context.Background(), session, testProfile(), failed.cursor, testLogger())
	if retried.imapErr != nil || retried.handlerErr != nil {
		t.Fatalf("retry: imap=%v handler=%v", retried.imapErr, retried.handlerErr)
	}
	if len(session.fetchedUIDs) != 2 || session.fetchedUIDs[0] != 4 || session.fetchedUIDs[1] != 5 {
		t.Fatalf("retry downloaded %v, want [4 5]", session.fetchedUIDs)
	}
	if retried.cursor.IMAP.Recovery != nil || retried.cursor.IMAP.LastUID != 5 {
		t.Fatalf("recovery did not finish after retry: %+v", retried.cursor.IMAP)
	}
}

func TestRecoveryKeepsCheckpointWhenUIDValidityChangesAgain(t *testing.T) {
	checkpoint := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), new(recordingParsedMessageHandler)))
	scheduler.cfg.MaxMessagesPerPoll = 1
	session := &testIMAPSession{
		raw:           recoveryRawMessage,
		uidValidity:   11,
		availableUIDs: []uint32{1, 2, 3},
		datesByUID: map[uint32]time.Time{
			1: checkpoint.Add(time.Hour),
			2: checkpoint.Add(2 * time.Hour),
			3: checkpoint.Add(3 * time.Hour),
		},
	}

	first := scheduler.sync(context.Background(), session, testProfile(), storedCursor(7, checkpoint), testLogger())
	if first.cursor.IMAP.Recovery == nil {
		t.Fatal("first recovery finished too early")
	}

	session.uidValidity = 12
	session.availableUIDs = []uint32{1, 2, 3, 4}
	second := scheduler.sync(context.Background(), session, testProfile(), first.cursor, testLogger())
	if second.imapErr != nil || second.handlerErr != nil {
		t.Fatalf("second recovery: imap=%v handler=%v", second.imapErr, second.handlerErr)
	}
	if second.cursor.IMAP.UIDValidity != 12 || second.cursor.IMAP.Recovery == nil {
		t.Fatalf("second recovery was not started: %+v", second.cursor.IMAP)
	}
	if !second.cursor.IMAP.Recovery.Checkpoint.Equal(checkpoint) {
		t.Fatalf("checkpoint moved to %v, want %v", second.cursor.IMAP.Recovery.Checkpoint, checkpoint)
	}
	if second.cursor.IMAP.Recovery.ToUID != 4 {
		t.Fatalf("new recovery_to_uid = %d, want 4", second.cursor.IMAP.Recovery.ToUID)
	}
}

func TestRecoveryKeepsEmailsSharingTheCheckpointTimestamp(t *testing.T) {
	checkpoint := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	next := new(recordingParsedMessageHandler)
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), next))

	// Two different emails arrived in the same second; only one of them was
	// confirmed before the UIDs were reassigned. Skipping both would lose one.
	session := &testIMAPSession{
		raw:           recoveryRawMessage,
		uidValidity:   11,
		availableUIDs: []uint32{1, 2},
		datesByUID:    map[uint32]time.Time{1: checkpoint, 2: checkpoint},
	}

	result := scheduler.sync(context.Background(), session, testProfile(), storedCursor(7, checkpoint), testLogger())
	if result.imapErr != nil || result.handlerErr != nil {
		t.Fatalf("imapErr=%v handlerErr=%v", result.imapErr, result.handlerErr)
	}
	if len(session.fetchedUIDs) != 2 {
		t.Fatalf("downloaded UIDs = %v, want both", session.fetchedUIDs)
	}
}

func TestRecoveryFallsBackToTheNewestStoredEmail(t *testing.T) {
	checkpoint := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), new(recordingParsedMessageHandler)))
	scheduler.messages = &testMessageStore{lastReceivedAt: checkpoint}

	session := &testIMAPSession{
		raw:           recoveryRawMessage,
		uidValidity:   11,
		availableUIDs: []uint32{1, 2},
		datesByUID: map[uint32]time.Time{
			1: checkpoint.Add(-time.Hour),
			2: checkpoint.Add(time.Hour),
		},
	}

	// The cursor predates recovery support, so it carries no checkpoint of its own.
	result := scheduler.sync(context.Background(), session, testProfile(), storedCursor(7, time.Time{}), testLogger())
	if result.imapErr != nil || result.handlerErr != nil {
		t.Fatalf("imapErr=%v handlerErr=%v", result.imapErr, result.handlerErr)
	}
	if len(session.fetchedUIDs) != 1 || session.fetchedUIDs[0] != 2 {
		t.Fatalf("downloaded UIDs = %v, want only the one after the stored email", session.fetchedUIDs)
	}
}

func TestRecoveryWithoutAnyCheckpointStartsAtTheEnd(t *testing.T) {
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), new(recordingParsedMessageHandler)))
	session := &testIMAPSession{raw: recoveryRawMessage, uidValidity: 11, availableUIDs: []uint32{1, 2, 3}}

	// Nothing was ever confirmed, so there is no gap and no history to import.
	stored := storedCursor(0, time.Time{})
	result := scheduler.sync(context.Background(), session, testProfile(), stored, testLogger())

	if result.cursor.IMAP.Recovery != nil {
		t.Fatalf("recovery started without a checkpoint: %+v", result.cursor.IMAP.Recovery)
	}
	if len(session.fetchedUIDs) != 0 {
		t.Fatalf("history was imported: %v", session.fetchedUIDs)
	}
	if result.cursor.IMAP.LastUID != 3 || result.cursor.IMAP.UIDValidity != 11 {
		t.Fatalf("cursor = %+v, want the end of the mailbox", result.cursor.IMAP)
	}
}

func TestNextCheckAtHoldsBackAfterHandlerError(t *testing.T) {
	if at := nextCheckAt(syncResult{more: true}); at == nil {
		t.Fatal("a clean backlog must be polled again at once")
	}
	// Otherwise a transient failure would be retried on every scheduler tick.
	if at := nextCheckAt(syncResult{more: true, handlerErr: context.Canceled}); at != nil {
		t.Fatalf("next check = %v, want the configured interval", at)
	}
}

var errTransientTest = errors.New("transient test failure")

type failOnceParsedMessageHandler struct {
	failUID uint32
	failed  bool
}

func (h *failOnceParsedMessageHandler) Handle(_ context.Context, message *model.ParsedEmail) error {
	if message.UID == h.failUID && !h.failed {
		h.failed = true

		return errTransientTest
	}

	return nil
}
