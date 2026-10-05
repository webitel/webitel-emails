package event

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/webitel/webitel-emails/internal/model"
)

var defaultLimits = Limits{
	MaxBodyTextSize:     64 << 10,
	MaxAddressesPerType: 100,
	MaxAttachments:      100,
	MaxFieldSize:        1024,
	MaxPayloadSize:      1 << 20,
}

func source() InboundMessageSource {
	contactID, flowID := int64(77), int64(9)
	fileID := int64(501)

	return InboundMessageSource{
		OccurredAt: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
		FlowID:     &flowID,
		Thread: &model.EmailThread{
			ID: 10, DomainID: 1, ProfileID: 2, Kind: model.EmailThreadKindRegular, ContactID: &contactID,
		},
		Message: &model.EmailMessage{
			ID: 20, DomainID: 1, ThreadID: 10, ProfileID: 2,
			Direction: model.EmailMessageDirectionInbound, Kind: model.EmailKindRegular,
			MessageID: "<m1@example.com>", Subject: "Hello", TextBody: "Body",
			HTMLBody: "<p>Body</p>", ReceivedAt: time.Date(2026, 10, 2, 9, 59, 0, 0, time.UTC),
		},
		Recipients: []*model.EmailRecipient{
			{Type: model.EmailRecipientTypeTo, Address: "b@x", Ordinal: 1},
			{Type: model.EmailRecipientTypeTo, Address: "a@x", Ordinal: 0},
			{Type: model.EmailRecipientTypeFrom, Address: "client@x", DisplayName: "Client"},
			{Type: model.EmailRecipientTypeBcc, Address: "hidden@x"},
		},
		Attachments: []*model.EmailMessageAttachment{
			{Position: 0, State: model.EmailAttachmentStateStored, FileID: &fileID,
				Disposition: model.EmailPartDispositionInline, FileName: "logo.png"},
			{Position: 1, State: model.EmailAttachmentStateSkipped, SkippedReason: model.EmailPartSkippedSizeLimit,
				Disposition: model.EmailPartDispositionAttachment, FileName: "big.zip"},
			{Position: 2, State: model.EmailAttachmentStateStored, FileID: &fileID,
				Disposition: model.EmailPartDispositionAttachment, FileName: "a.pdf", MimeType: "application/pdf", Size: 3},
		},
	}
}

func TestInboundMessageOutboxEvent(t *testing.T) {
	t.Run("regular message keeps only what Flow reads", func(t *testing.T) {
		row, err := NewInboundMessageOutboxEvent(uuid.New(), source(), defaultLimits)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if row.DedupKey != "1:20" || row.Exchange != Exchange || row.RoutingKey != InboundMessageRoutingKey {
			t.Fatalf("row = %+v", row)
		}
		if bytes.Contains(row.Payload, []byte("<p>")) || bytes.Contains(row.Payload, []byte("hidden@x")) {
			t.Fatal("payload carries the HTML body or a Bcc address")
		}

		event, err := UnmarshalInboundMessage(row.Payload)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		s := event.Snapshot
		if *event.FlowID != 9 || *s.ContactID != 77 || s.BodyText != "Body" {
			t.Fatalf("snapshot = %+v", s)
		}
		if len(s.To) != 2 || s.To[0].Address != "a@x" || s.From[0].Name != "Client" {
			t.Fatalf("addresses not in header order: %+v %+v", s.To, s.From)
		}
		if len(s.Attachments) != 1 || s.Attachments[0].ID != 501 || s.Attachments[0].Channel != "mail" {
			t.Fatalf("attachments = %+v, want only the stored attachment part", s.Attachments)
		}
	})

	t.Run("oversized input is cut deterministically and fits the payload limit", func(t *testing.T) {
		src := source()
		// A three-byte rune straddles the body limit, so a byte cut would split it.
		src.Message.TextBody = strings.Repeat("a", defaultLimits.MaxBodyTextSize-1) + "€tail"
		src.Message.Subject = strings.Repeat(`"`, 5000) + "\x00\x1b"
		for i := range 5000 {
			src.Recipients = append(src.Recipients, &model.EmailRecipient{
				Type: model.EmailRecipientTypeCc, Address: strings.Repeat("c", 400), DisplayName: strings.Repeat("n", 400), Ordinal: int32(i),
			})
		}
		for i := range 1000 {
			fileID := int64(i + 1)
			src.Attachments = append(src.Attachments, &model.EmailMessageAttachment{
				Position: int32(i + 3), State: model.EmailAttachmentStateStored, FileID: &fileID,
				Disposition: model.EmailPartDispositionAttachment, FileName: strings.Repeat("f", 4000),
			})
		}

		row, err := NewInboundMessageOutboxEvent(uuid.New(), src, defaultLimits)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if len(row.Payload) > defaultLimits.MaxPayloadSize {
			t.Fatalf("payload of %d bytes exceeds the limit", len(row.Payload))
		}

		event, err := UnmarshalInboundMessage(row.Payload)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		s := event.Snapshot
		if !s.BodyTextTruncated || !utf8.ValidString(s.BodyText) || len(s.BodyText) != defaultLimits.MaxBodyTextSize-1 {
			t.Fatalf("body cut to %d bytes, truncated=%v", len(s.BodyText), s.BodyTextTruncated)
		}
		if len(s.Subject) != defaultLimits.MaxFieldSize || strings.ContainsAny(s.Subject, "\x00\x1b") {
			t.Fatalf("subject of %d bytes keeps control characters", len(s.Subject))
		}
		if len(s.Cc) != 100 || !s.CcTruncated || len(s.Cc[0].Address) != maxAddressSize || s.ToTruncated {
			t.Fatalf("cc = %d truncated=%v", len(s.Cc), s.CcTruncated)
		}
		if len(s.Attachments) != 100 || !s.AttachmentsTruncated || s.Attachments[0].ID != 501 {
			t.Fatalf("attachments = %d truncated=%v", len(s.Attachments), s.AttachmentsTruncated)
		}
	})

	t.Run("auto-replies and service threads never start Flow", func(t *testing.T) {
		autoReply := source()
		autoReply.Message.Kind = model.EmailKindAutoReply
		service := source()
		service.Thread.Kind = model.EmailThreadKindService

		for name, src := range map[string]InboundMessageSource{"auto_reply": autoReply, "service": service} {
			if _, err := NewInboundMessageOutboxEvent(uuid.New(), src, defaultLimits); err == nil {
				t.Errorf("%s: event was built", name)
			}
		}
	})

	t.Run("a single cut value marks its list", func(t *testing.T) {
		src := source()
		src.Recipients[0].DisplayName = strings.Repeat("n", maxDisplayNameSize+1)
		src.Attachments[2].FileName = strings.Repeat("f", defaultLimits.MaxFieldSize+1)

		event, err := NewInboundMessage(uuid.New(), src, defaultLimits)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		s := event.Snapshot
		if !s.ToTruncated || !s.AttachmentsTruncated || s.FromTruncated || len(s.To) != 2 {
			t.Fatalf("to=%v attachments=%v from=%v", s.ToTruncated, s.AttachmentsTruncated, s.FromTruncated)
		}
	})

	t.Run("worst case size matches a real event at every limit", func(t *testing.T) {
		limits := Limits{MaxBodyTextSize: 100, MaxAddressesPerType: 3, MaxAttachments: 2, MaxFieldSize: 50, MaxPayloadSize: 1 << 20}
		maxID := int64(math.MaxInt64)
		latest := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
		fill := func(size int) string { return strings.Repeat(`"`, size) }

		src := source()
		src.OccurredAt, src.FlowID = latest, &maxID
		src.Thread.ID, src.Thread.DomainID, src.Thread.ProfileID, src.Thread.ContactID = maxID, maxID, maxID, &maxID
		m := src.Message
		m.ID, m.ThreadID, m.DomainID, m.ProfileID, m.ReceivedAt = maxID, maxID, maxID, maxID, latest
		m.MessageID, m.InReplyTo, m.Subject, m.TextBody = fill(50), fill(50), fill(50), fill(100)
		src.Recipients, src.Attachments = nil, nil
		for _, kind := range []model.EmailRecipientType{"from", "sender", "reply_to", "to", "cc"} {
			for range 3 {
				src.Recipients = append(src.Recipients, &model.EmailRecipient{
					Type: kind, Address: fill(maxAddressSize), DisplayName: fill(maxDisplayNameSize),
				})
			}
		}
		for i := range 2 {
			src.Attachments = append(src.Attachments, &model.EmailMessageAttachment{
				Position: int32(i), State: model.EmailAttachmentStateStored, FileID: &maxID, Size: maxID,
				Disposition: model.EmailPartDispositionAttachment, FileName: fill(50), MimeType: fill(maxMimeTypeSize),
			})
		}

		row, err := NewInboundMessageOutboxEvent(uuid.Max, src, limits)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		worst, err := limits.WorstCaseSize()
		if err != nil || worst != int64(len(row.Payload)) {
			t.Fatalf("worst case %d, real event %d bytes (%v)", worst, len(row.Payload), err)
		}
	})

	t.Run("limits that could exceed the payload are rejected", func(t *testing.T) {
		tooMany := defaultLimits
		tooMany.MaxAddressesPerType = 1000
		// Far above the hard bound: rejected before anything is allocated.
		absurd := defaultLimits
		absurd.MaxAddressesPerType = 100_000_000

		for name, limits := range map[string]Limits{"oversized payload": tooMany, "above hard bound": absurd} {
			if err := limits.Validate(); err == nil {
				t.Errorf("%s: limits were accepted", name)
			}
		}
		if err := defaultLimits.Validate(); err != nil {
			t.Fatalf("default limits: %v", err)
		}
	})
}
