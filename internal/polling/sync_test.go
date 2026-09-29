package polling

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/config"
	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

func TestSyncAdvancesCursorAfterMIMEHandling(t *testing.T) {
	next := new(recordingParsedMessageHandler)
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), next))
	session := &testIMAPSession{raw: []byte("From: sender@example.org\r\nTo: receiver@example.org\r\nSubject: Hello\r\n\r\nBody")}

	result := scheduler.sync(context.Background(), session, testProfile(), testCursor(), testLogger())

	if result.imapErr != nil || result.handlerErr != nil {
		t.Fatalf("sync errors: imap=%v, handler=%v", result.imapErr, result.handlerErr)
	}
	if result.cursor == nil || result.cursor.IMAP == nil || result.cursor.IMAP.LastUID != 2 {
		t.Fatalf("cursor = %+v, want LastUID 2", result.cursor)
	}
	if next.calls != 1 || next.message == nil || next.message.UID != 2 {
		t.Fatalf("handler calls = %d, message = %+v", next.calls, next.message)
	}
}

func TestSyncRetriesAfterMIMEHandlerError(t *testing.T) {
	malformed, err := os.ReadFile("../inbound/testdata/malformed.eml")
	if err != nil {
		t.Fatalf("read malformed fixture: %v", err)
	}

	next := new(recordingParsedMessageHandler)
	scheduler := newTestScheduler(inbound.NewMIMEHandler(newTestMIMEParser(), next))
	valid := []byte("From: sender@example.org\r\nTo: receiver@example.org\r\n\r\nValid")
	session := &testIMAPSession{
		availableUIDs: []uint32{2, 3, 4},
		rawByUID: map[uint32][]byte{
			2: valid,
			3: malformed,
			4: valid,
		},
	}
	stored := testCursor()

	failed := scheduler.sync(context.Background(), session, testProfile(), stored, testLogger())
	if failed.imapErr != nil || failed.handlerErr == nil {
		t.Fatalf("sync errors: imap=%v, handler=%v", failed.imapErr, failed.handlerErr)
	}
	if failed.cursor == nil || failed.cursor.IMAP == nil || failed.cursor.IMAP.LastUID != 2 {
		t.Fatalf("cursor after failure = %+v, want LastUID 2", failed.cursor)
	}

	session.rawByUID[3] = valid
	retried := scheduler.sync(context.Background(), session, testProfile(), failed.cursor, testLogger())
	if retried.imapErr != nil || retried.handlerErr != nil {
		t.Fatalf("retry errors: imap=%v, handler=%v", retried.imapErr, retried.handlerErr)
	}
	if retried.cursor == nil || retried.cursor.IMAP == nil || retried.cursor.IMAP.LastUID != 4 {
		t.Fatalf("cursor after retry = %+v, want LastUID 4", retried.cursor)
	}
	if session.fetchCalls != 2 || next.calls != 3 {
		t.Fatalf("fetch calls = %d, handler calls = %d", session.fetchCalls, next.calls)
	}
}

func TestSyncAdvancesPastDurablyQuarantinedEmail(t *testing.T) {
	malformed, err := os.ReadFile("../inbound/testdata/malformed.eml")
	if err != nil {
		t.Fatalf("read malformed fixture: %v", err)
	}

	failures := new(pollingFailureStore)
	next := new(recordingParsedMessageHandler)
	mime := inbound.NewMIMEHandler(newTestMIMEParser(), next)
	handler := inbound.NewQuarantineHandler(mime, failures, testLogger())
	scheduler := newTestScheduler(handler)
	valid := []byte("Message-ID: <valid@x>\r\nFrom: sender@example.org\r\nTo: receiver@example.org\r\n\r\nValid")
	session := &testIMAPSession{
		availableUIDs: []uint32{2, 3},
		rawByUID: map[uint32][]byte{
			2: malformed,
			3: valid,
		},
	}

	result := scheduler.sync(context.Background(), session, testProfile(), testCursor(), testLogger())
	if result.imapErr != nil || result.handlerErr != nil {
		t.Fatalf("sync errors: imap=%v handler=%v", result.imapErr, result.handlerErr)
	}
	if result.cursor.IMAP.LastUID != 3 {
		t.Fatalf("cursor = %+v, want LastUID 3", result.cursor.IMAP)
	}
	if len(failures.records) != 1 || failures.records[0].UID != 2 ||
		failures.records[0].Category != model.InboundFailureMIMEParse {
		t.Fatalf("quarantine records = %+v", failures.records)
	}
	if next.calls != 1 || next.message == nil || next.message.UID != 3 {
		t.Fatalf("next calls = %d, message = %+v", next.calls, next.message)
	}
}

func TestSyncDoesNotAdvanceWhenQuarantineWriteFails(t *testing.T) {
	malformed, err := os.ReadFile("../inbound/testdata/malformed.eml")
	if err != nil {
		t.Fatalf("read malformed fixture: %v", err)
	}

	writeErr := errors.New("quarantine database unavailable")
	failures := &pollingFailureStore{err: writeErr}
	mime := inbound.NewMIMEHandler(newTestMIMEParser(), new(recordingParsedMessageHandler))
	scheduler := newTestScheduler(inbound.NewQuarantineHandler(mime, failures, testLogger()))
	session := &testIMAPSession{availableUIDs: []uint32{2}, rawByUID: map[uint32][]byte{2: malformed}}

	result := scheduler.sync(context.Background(), session, testProfile(), testCursor(), testLogger())
	if !errors.Is(result.handlerErr, writeErr) {
		t.Fatalf("handler error = %v, want quarantine write error", result.handlerErr)
	}
	if result.cursor.IMAP.LastUID != 1 {
		t.Fatalf("cursor advanced to %d after failed quarantine write", result.cursor.IMAP.LastUID)
	}
}

func TestAdvanceConfirmedStopsAtFirstGap(t *testing.T) {
	confirmed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	cursor := model.IMAPCursor{LastUID: 1}
	advanceConfirmed(&cursor, []uint32{2, 3, 4}, map[uint32]time.Time{
		2: confirmed,
		4: confirmed.Add(time.Hour),
	})

	if cursor.LastUID != 2 {
		t.Fatalf("LastUID = %d, want 2", cursor.LastUID)
	}
	// The checkpoint follows the same prefix, not the highest confirmed date.
	if !cursor.LastInternalDate.Equal(confirmed) {
		t.Fatalf("checkpoint = %v, want %v", cursor.LastInternalDate, confirmed)
	}
}

type testIMAPSession struct {
	raw           []byte
	availableUIDs []uint32
	rawByUID      map[uint32][]byte
	datesByUID    map[uint32]time.Time
	uidValidity   uint32
	fetchErr      error
	fetchCalls    int
	searchedSince time.Time
	fetchedUIDs   []uint32
	events        []string
}

func (s *testIMAPSession) Noop() error  { return nil }
func (s *testIMAPSession) Close() error { return nil }
func (s *testIMAPSession) Abort()       {}

func (s *testIMAPSession) Examine(mailbox string) (*mailinfra.IMAPMailbox, error) {
	uidNext := uint32(3)
	for _, uid := range s.availableUIDs {
		if uid >= uidNext {
			uidNext = uid + 1
		}
	}

	uidValidity := s.uidValidity
	if uidValidity == 0 {
		uidValidity = 10
	}

	return &mailinfra.IMAPMailbox{Name: mailbox, UIDValidity: uidValidity, UIDNext: uidNext}, nil
}

func (s *testIMAPSession) SearchUIDsRange(from, to uint32, since time.Time) ([]uint32, error) {
	s.searchedSince = since

	uids := s.availableUIDs
	if len(uids) == 0 {
		uids = []uint32{2}
	}

	result := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if uid >= from && (to == 0 || uid <= to) {
			result = append(result, uid)
		}
	}

	return result, nil
}

func (s *testIMAPSession) FetchInternalDates(uids []uint32) (map[uint32]time.Time, error) {
	s.events = append(s.events, "metadata")
	dates := make(map[uint32]time.Time, len(uids))
	for _, uid := range uids {
		if date, ok := s.datesByUID[uid]; ok {
			dates[uid] = date
		}
	}

	return dates, nil
}

func (s *testIMAPSession) FetchRaw(uids []uint32, handle func(*mailinfra.IMAPMessage)) error {
	s.events = append(s.events, "raw")
	s.fetchCalls++
	s.fetchedUIDs = append(s.fetchedUIDs, uids...)
	if s.fetchErr != nil {
		return s.fetchErr
	}
	for _, uid := range uids {
		raw := s.raw
		if value, ok := s.rawByUID[uid]; ok {
			raw = value
		}
		date, ok := s.datesByUID[uid]
		if !ok {
			date = time.Now()
		}
		handle(&mailinfra.IMAPMessage{UID: uid, InternalDate: date, Raw: raw})
	}

	return nil
}

type recordingParsedMessageHandler struct {
	calls   int
	message *model.ParsedEmail
}

type pollingFailureStore struct {
	records []*model.InboundFailure
	err     error
}

func (s *pollingFailureStore) Record(_ context.Context, failure *model.InboundFailure) error {
	s.records = append(s.records, failure)

	return s.err
}

func (h *recordingParsedMessageHandler) Handle(_ context.Context, message *model.ParsedEmail) error {
	h.calls++
	h.message = message

	return nil
}

func newTestScheduler(handler inbound.Handler) *Scheduler {
	return &Scheduler{
		cfg: config.IMAPPollingConfig{
			FetchBatchSize:     10,
			MaxMessagesPerPoll: 10,
		},
		handler:  handler,
		messages: new(testMessageStore),
	}
}

// testMessageStore answers the fallback checkpoint lookup only.
type testMessageStore struct {
	lastReceivedAt time.Time
}

func (s *testMessageStore) LastReceivedAt(context.Context, int64, int64) (time.Time, error) {
	return s.lastReceivedAt, nil
}

func (s *testMessageStore) LocateByMessageID(
	context.Context, int64, int64, string,
) (*store.EmailMessageIdentity, error) {
	return nil, nil
}

func (s *testMessageStore) LocateByIMAPIdentity(
	context.Context, int64, int64, model.EmailIMAPIdentity,
) (*store.EmailMessageIdentity, error) {
	return nil, nil
}

func (s *testMessageStore) LocateByRawChecksum(
	context.Context, int64, int64, []byte,
) (*store.EmailMessageIdentity, error) {
	return nil, nil
}

func (s *testMessageStore) Create(context.Context, *model.EmailMessage) (*model.EmailMessage, error) {
	return nil, nil
}

func newTestMIMEParser() *inbound.MIMEParser {
	return inbound.NewMIMEParser(&config.Config{MIME: config.MIMEConfig{
		MaxBodySize:             1 << 20,
		MaxAttachmentSize:       1 << 20,
		MaxAttachmentsTotalSize: 2 << 20,
		MaxAttachments:          5,
	}})
}

func testProfile() *model.EmailProfile {
	return &model.EmailProfile{ID: 11, DomainID: 7, Mailbox: "INBOX"}
}

func testCursor() *model.ProviderCursor {
	return &model.ProviderCursor{IMAP: &model.IMAPCursor{Mailbox: "INBOX", UIDValidity: 10, LastUID: 1}}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
