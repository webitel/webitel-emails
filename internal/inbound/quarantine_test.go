package inbound

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/webitel/webitel-emails/internal/model"
)

type recordingFailureStore struct {
	records []*model.InboundFailure
}

func (s *recordingFailureStore) Record(_ context.Context, failure *model.InboundFailure) error {
	s.records = append(s.records, failure)

	return nil
}

// failingHandler stands in for the rest of the pipeline.
type failingHandler struct {
	err    error
	called int
}

func (h *failingHandler) Handle(context.Context, *Message) error {
	h.called++

	return h.err
}

func rawMessage() *Message {
	return &Message{
		DomainID: 1, ProfileID: 7,
		Mailbox: "INBOX", UIDValidity: 10, UID: 5,
		Raw: []byte("Message-ID: <broken@x>\r\nFrom: A <a@x.com>\r\nSubject: Broken\r\n\r\nbody"),
	}
}

// quarantineWith builds the decorator over a stub pipeline.
func quarantineWith(next Handler, failures *recordingFailureStore) *QuarantineHandler {
	return &QuarantineHandler{
		next:     next,
		failures: failures,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestQuarantineRecordsPermanentFailure(t *testing.T) {
	failures := new(recordingFailureStore)
	next := &failingHandler{err: Permanent(model.InboundFailureMIMEParse, errors.New("bad mime"))}
	handler := quarantineWith(next, failures)

	// nil lets the cursor pass the email, which is the point of quarantine.
	if err := handler.Handle(context.Background(), rawMessage()); err != nil {
		t.Fatalf("a quarantined email must not block the cursor: %v", err)
	}
	if len(failures.records) != 1 {
		t.Fatalf("records = %d, want 1", len(failures.records))
	}

	record := failures.records[0]
	if record.Category != model.InboundFailureMIMEParse {
		t.Fatalf("category = %q", record.Category)
	}
	// The headers are what identifies the email left in the mailbox.
	if record.MessageID != "broken@x" || record.Sender != "a@x.com" || record.Subject != "Broken" {
		t.Fatalf("header hints = %q / %q / %q", record.MessageID, record.Sender, record.Subject)
	}
	if record.Mailbox != "INBOX" || record.UID != 5 || record.UIDValidity != 10 {
		t.Fatalf("coordinates = %s/%d/%d", record.Mailbox, record.UIDValidity, record.UID)
	}
}

func TestQuarantineRecordsOversizedWithoutParsing(t *testing.T) {
	failures := new(recordingFailureStore)
	next := &failingHandler{}
	handler := quarantineWith(next, failures)

	message := rawMessage()
	message.TooLarge = true
	message.Size = 50 << 20

	if err := handler.Handle(context.Background(), message); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if next.called != 0 {
		t.Fatal("an oversized email must not reach the parser")
	}
	if len(failures.records) != 1 || failures.records[0].Category != model.InboundFailureMessageTooLarge {
		t.Fatalf("records = %+v", failures.records)
	}
	if failures.records[0].Size != 50<<20 {
		t.Fatalf("size = %d, want the reported RFC822.SIZE", failures.records[0].Size)
	}
}

func TestQuarantineKeepsTransientFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{
			name: "database error",
			err:  errors.New("connection refused"),
			ctx:  func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
		},
		{
			// Cancellation looks permanent only because shutdown interrupted it.
			name: "cancelled during a permanent-looking failure",
			err:  Permanent(model.InboundFailureMIMEParse, context.Canceled),
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, func() {}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failures := new(recordingFailureStore)
			handler := quarantineWith(&failingHandler{err: tt.err}, failures)
			ctx, cancel := tt.ctx()
			defer cancel()

			if err := handler.Handle(ctx, rawMessage()); err == nil {
				t.Fatal("a transient failure must keep the email for the next poll")
			}
			if len(failures.records) != 0 {
				t.Fatalf("a transient failure was quarantined: %+v", failures.records)
			}
		})
	}
}

func TestQuarantineReasonDoesNotCarrySenderContent(t *testing.T) {
	secret := "X-Broken: " + string(make([]byte, 4096)) + " card 4111111111111111"
	failures := new(recordingFailureStore)
	handler := quarantineWith(&failingHandler{
		err: Permanent(model.InboundFailureMIMEParse, errors.New("mime: read message: "+secret)),
	}, failures)

	if err := handler.Handle(context.Background(), rawMessage()); err != nil {
		t.Fatalf("handle: %v", err)
	}

	reason := failures.records[0].Reason
	if strings.Contains(reason, "card 4111111111111111") || len(reason) > 128 {
		t.Fatalf("the stored reason repeats the parser error: %q", reason)
	}
	if reason != "failed to parse MIME message" {
		t.Fatalf("reason = %q, want the stable text for the category", reason)
	}
}
