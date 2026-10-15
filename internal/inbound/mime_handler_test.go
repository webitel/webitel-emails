package inbound

import (
	"context"
	"testing"

	"github.com/webitel/webitel-emails/internal/model"
)

func TestMIMEHandler(t *testing.T) {
	t.Run("forwards parsed message", func(t *testing.T) {
		next := new(recordingParsedMessageHandler)
		handler := NewMIMEHandler(newTestMIMEParser(), next)
		message := &Message{
			DomainID:    7,
			ProfileID:   11,
			Mailbox:     "INBOX",
			UIDValidity: 23,
			UID:         42,
			Raw:         readFixture(t, "nested.eml"),
		}

		if err := handler.Handle(context.Background(), message); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if next.calls != 1 || next.message == nil {
			t.Fatalf("next calls = %d, message = %+v", next.calls, next.message)
		}
		if next.message.DomainID != message.DomainID || next.message.ProfileID != message.ProfileID ||
			next.message.Mailbox != message.Mailbox || next.message.UIDValidity != message.UIDValidity ||
			next.message.UID != message.UID || next.message.Subject != "Тестовий лист" {
			t.Errorf("parsed message = %+v", next.message)
		}
	})

	t.Run("does not forward malformed message", func(t *testing.T) {
		next := new(recordingParsedMessageHandler)
		handler := NewMIMEHandler(newTestMIMEParser(), next)

		err := handler.Handle(context.Background(), &Message{Raw: readFixture(t, "malformed.eml")})
		if err == nil {
			t.Fatal("Handle error = nil")
		}
		if next.calls != 0 || next.message != nil {
			t.Errorf("next calls = %d, message = %+v", next.calls, next.message)
		}
	})
}

type recordingParsedMessageHandler struct {
	calls   int
	message *model.ParsedEmail
}

func (h *recordingParsedMessageHandler) Handle(_ context.Context, message *model.ParsedEmail) error {
	h.calls++
	h.message = message

	return nil
}
