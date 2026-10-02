package inbound

import (
	"context"
	"testing"

	"github.com/webitel/webitel-emails/internal/model"
)

func TestAttachmentGateRefusesFiles(t *testing.T) {
	tests := []struct {
		name  string
		parts []model.EmailPart
	}{
		{name: "attachment", parts: []model.EmailPart{{Name: "a.pdf", ContentType: "application/pdf"}}},
		// A skipped part carries no file, but its metadata would be lost too.
		{name: "skipped part", parts: []model.EmailPart{{Name: "big.zip", SkippedReason: "attachment_size_limit_exceeded"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeStore()
			gate := NewAttachmentGate(testHandler(fake))

			email := testEmail("<m1@x>", false, "raw-1", 10, 5)
			email.Parts = tt.parts

			if err := gate.Handle(context.Background(), email); err == nil {
				t.Fatal("an email with files was confirmed to the cursor")
			}
			if len(fake.threads) != 0 || len(fake.messages) != 0 {
				t.Fatalf("the gate wrote data: threads=%d messages=%d", len(fake.threads), len(fake.messages))
			}
		})
	}

	t.Run("email without files passes through", func(t *testing.T) {
		fake := newFakeStore()
		gate := NewAttachmentGate(testHandler(fake))

		if err := gate.Handle(context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5)); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if len(fake.messages) != 1 {
			t.Fatalf("messages=%d, want 1", len(fake.messages))
		}
	})
}
