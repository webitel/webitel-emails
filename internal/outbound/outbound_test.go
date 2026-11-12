package outbound

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
)

func testConfig() config.OutboundConfig {
	return config.OutboundConfig{
		MaxRecipients: 5, MaxBodySize: 1 << 10, MaxAttachments: 2,
		MaxAttachmentsTotalSize: 1 << 20, MaxMessageSize: 2 << 20,
	}
}

func to(addresses ...string) []model.EmailAddress {
	var list []model.EmailAddress
	for _, a := range addresses {
		list = append(list, model.EmailAddress{Address: a})
	}
	return list
}

func TestPrepareRejects(t *testing.T) {
	long := strings.Repeat("a", 250) + "@b.cd"
	tests := []struct {
		name  string
		draft Draft
		want  string
	}{
		{"no To", Draft{Cc: to("a@b.cd"), TextBody: "x"}, "email.message.no_recipient"},
		{"display name", Draft{To: to("A <a@b.cd>"), TextBody: "x"}, "email.message.invalid_recipient"},
		{"brackets", Draft{To: to("<a@b.cd>"), TextBody: "x"}, "email.message.invalid_recipient"},
		{"header injection", Draft{To: to("a@b.cd\r\nBcc: c@d.ef"), TextBody: "x"}, "email.message.invalid_recipient"},
		{"too long", Draft{To: to(long), TextBody: "x"}, "email.message.invalid_recipient"},
		{"name with LF", Draft{To: []model.EmailAddress{{Name: "A\nBcc: c@d.ef", Address: "a@b.cd"}}, TextBody: "x"}, "email.message.invalid_recipient"},
		{"subject with LF", Draft{To: to("a@b.cd"), Subject: "a\nBcc: c@d.ef", TextBody: "x"}, "email.message.invalid_subject"},
		{"subject 256", Draft{To: to("a@b.cd"), Subject: strings.Repeat("ж", 256), TextBody: "x"}, "email.message.invalid_subject"},
		{"reply subject 256 with Re", Draft{To: to("a@b.cd"), Reply: true, Subject: strings.Repeat("ж", 252), TextBody: "x"}, "email.message.invalid_subject"},
		{"empty markup", Draft{To: to("a@b.cd"), HTMLBody: "<p><br></p>", TextBody: " "}, "email.message.empty"},
		// "&" grows to "&amp;" when sanitized: 27 bytes become 107.
		{"sanitized body over limit", Draft{To: to("a@b.cd"), HTMLBody: "<p>" + strings.Repeat("&", 20) + "</p>"}, "email.message.body_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			if tt.want == "email.message.body_too_large" {
				cfg.MaxBodySize = 100
			}
			if _, err := Prepare(cfg, tt.draft); kiterrors.ID(err) != tt.want {
				t.Fatalf("error = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestPrepareSubjectAndContent(t *testing.T) {
	cfg := testConfig()
	p, err := Prepare(cfg, Draft{To: to("a@b.cd"), Subject: strings.Repeat("ж", 255), TextBody: "x"})
	if err != nil || len([]rune(p.Subject)) != 255 {
		t.Fatalf("255 runes: %v", err)
	}

	p, err = Prepare(cfg, Draft{To: to("A@B.cd"), Reply: true, ThreadSubject: strings.Repeat("ж", 300), TextBody: "x"})
	if err != nil || len([]rune(p.Subject)) != 255 || !strings.HasPrefix(p.Subject, "Re: ") {
		t.Fatalf("derived subject = %q, %v", p.Subject, err)
	}
	if p.To[0].Address != "A@B.cd" || p.To[0].NormalizedAddress != "a@b.cd" {
		t.Fatalf("address = %+v", p.To[0])
	}

	p, err = Prepare(cfg, Draft{To: to("a@b.cd"), Reply: true, Subject: "RE: x", TextBody: "x"})
	if err != nil || p.Subject != "RE: x" {
		t.Fatalf("subject = %q, %v", p.Subject, err)
	}

	// Empty markup is dropped; attachments alone are content.
	p, err = Prepare(cfg, Draft{To: to("a@b.cd"), HTMLBody: "<p><br></p>", Attachments: []AttachmentMeta{{FileName: "a", Size: 1}}})
	if err != nil || p.HTMLBody != "" || p.TextBody != "" {
		t.Fatalf("attachment only = %+v, %v", p, err)
	}
}

func TestReplyHeaders(t *testing.T) {
	if _, _, err := ReplyHeaders("no-at-sign", nil); kiterrors.ID(err) != "email.thread.reply_parent_not_found" {
		t.Fatalf("invalid parent: %v", err)
	}

	_, refs, err := ReplyHeaders("p@x", []string{"a@x", "bad id@x", "a@x", "p@x", "b@x"})
	if err != nil || !slices.Equal(refs, []string{"a@x", "b@x", "p@x"}) {
		t.Fatalf("refs = %v, %v", refs, err)
	}

	var many []string
	for i := range 25 {
		many = append(many, string(rune('A'+i))+"@x")
	}
	_, refs, _ = ReplyHeaders("p@x", many)
	if len(refs) != 20 || refs[0] != "A@x" || refs[1] != "H@x" || refs[19] != "p@x" {
		t.Fatalf("limited refs = %v", refs)
	}
}

func testEmail() *Email {
	return &Email{
		MessageID: NewMessageID("Agent@Example.COM"),
		Date:      time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),
		Sender:    Sender{Name: "Підтримка", Address: "agent@example.com", ReplyTo: "help@example.com"},
		To:        []model.EmailAddress{{Name: "Олена", Address: "Olena@Client.com"}},
		Cc:        to("olena@client.com", "boss@client.com"),
		Bcc:       to("hidden@client.com"),
		Subject:   "Re: Замовлення №5",
		HTMLBody:  "<p>Добрий день</p>",
		TextBody:  "Добрий день",
		InReplyTo: "p@client.com",
		References: []string{
			"a@client.com", "p@client.com",
		},
		Attachments: []Attachment{{FileName: "звіт.pdf", MimeType: "application/pdf", Content: strings.NewReader("%PDF-1")}},
	}
}

func TestBuildMIMERoundTrip(t *testing.T) {
	e := testEmail()
	raw, err := BuildMIME(e, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hidden@client.com") {
		t.Fatal("Bcc is in the MIME")
	}

	parser := inbound.NewMIMEParser(&config.Config{MIME: config.MIMEConfig{
		MaxBodySize: 1 << 20, MaxAttachmentSize: 1 << 20, MaxAttachmentsTotalSize: 1 << 20, MaxAttachments: 5,
	}})
	parsed, err := parser.Parse(context.Background(), &inbound.Message{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case parsed.MessageID != e.MessageID || !strings.HasSuffix(parsed.MessageID, "@example.com"):
		t.Fatalf("Message-ID = %q", parsed.MessageID)
	case parsed.InReplyTo != "p@client.com" || !slices.Equal(parsed.References, e.References):
		t.Fatalf("threading = %v %v", parsed.InReplyTo, parsed.References)
	case parsed.Subject != e.Subject || parsed.From[0].Name != "Підтримка":
		t.Fatalf("subject/from = %q %+v", parsed.Subject, parsed.From)
	case parsed.To[0].Address != "Olena@Client.com" || len(parsed.Cc) != 2 || len(parsed.Bcc) != 0:
		t.Fatalf("recipients = %+v %+v %+v", parsed.To, parsed.Cc, parsed.Bcc)
	case strings.TrimSpace(parsed.TextBody) != e.TextBody || !strings.Contains(parsed.HTMLBody, "Добрий день"):
		t.Fatalf("bodies = %q %q", parsed.TextBody, parsed.HTMLBody)
	}
	var found bool
	for _, part := range parsed.Parts {
		found = found || part.Name == "звіт.pdf" && string(part.Content) == "%PDF-1"
	}
	if !found {
		t.Fatalf("attachment missing: %+v", parsed.Parts)
	}

	// The case of the address is kept; the duplicate Cc differs only in case.
	from, rcpt := Envelope(e)
	if from != "agent@example.com" || !slices.Equal(rcpt, []string{"Olena@Client.com", "boss@client.com", "hidden@client.com"}) {
		t.Fatalf("envelope = %s %v", from, rcpt)
	}
}

type countingReader struct{ n int64 }

func (r *countingReader) Read(p []byte) (int, error) {
	if r.n >= 10<<20 {
		return 0, io.EOF
	}
	r.n += int64(len(p))
	return len(p), nil
}

func TestBuildMIMEStopsAtLimit(t *testing.T) {
	e := testEmail()
	reader := &countingReader{}
	e.Attachments = []Attachment{{FileName: "big.bin", MimeType: "application/octet-stream", Content: reader}}
	if _, err := BuildMIME(e, 1<<20); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("error = %v", err)
	}
	if reader.n >= 10<<20 {
		t.Fatal("the whole attachment was read")
	}
}

// The estimate must reject what BuildMIME cannot fit and accept what it can.
func TestPrepareEstimateBoundsMIME(t *testing.T) {
	cfg := config.OutboundConfig{
		MaxRecipients: 50, MaxBodySize: 1 << 20, MaxAttachments: 15,
		MaxAttachmentsTotalSize: 20 << 20, MaxMessageSize: 30 << 20,
	}
	text := strings.Repeat("ж", 1<<19)
	for _, tt := range []struct {
		size int64
		fits bool
	}{{10_300_000, false}, {10_000_000, true}} {
		_, err := Prepare(cfg, Draft{To: to("a@b.cd"), TextBody: text,
			Attachments: []AttachmentMeta{{FileName: "a", Size: tt.size}, {FileName: "b", Size: tt.size}}})
		if !tt.fits {
			if kiterrors.ID(err) != "email.message.too_large" {
				t.Fatalf("size %d: error = %v", tt.size, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("size %d: %v", tt.size, err)
		}
		e := testEmail()
		e.HTMLBody, e.TextBody = "", text
		e.Attachments = []Attachment{
			{FileName: "a", MimeType: "application/pdf", Content: bytes.NewReader(make([]byte, tt.size))},
			{FileName: "b", MimeType: "application/pdf", Content: bytes.NewReader(make([]byte, tt.size))},
		}
		if _, err := BuildMIME(e, cfg.MaxMessageSize); err != nil {
			t.Fatalf("size %d: build: %v", tt.size, err)
		}
	}
}

func TestBuildMIMEKeepsAttachmentBytes(t *testing.T) {
	parser := inbound.NewMIMEParser(&config.Config{MIME: config.MIMEConfig{
		MaxBodySize: 1 << 20, MaxAttachmentSize: 1 << 20, MaxAttachmentsTotalSize: 1 << 20, MaxAttachments: 5,
	}})
	for _, mimeType := range []string{"multipart/mixed; boundary=b", "text/plain; charset=iso-8859-1"} {
		t.Run(mimeType, func(t *testing.T) {
			content := "--b\r\nx\xe9\r\n--b--\r\n"
			e := testEmail()
			e.Attachments = []Attachment{{FileName: "a.bin", MimeType: mimeType, Content: strings.NewReader(content)}}
			raw, err := BuildMIME(e, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parser.Parse(context.Background(), &inbound.Message{Raw: raw})
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, part := range parsed.Parts {
				found = found || part.Name == "a.bin" && string(part.Content) == content
			}
			if !found || parsed.TextBody == "" {
				t.Fatalf("parts = %+v", parsed.Parts)
			}
		})
	}
}
