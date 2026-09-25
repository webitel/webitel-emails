package inbound

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"
)

func TestMIMEParserParseBodies(t *testing.T) {
	tests := []struct {
		name        string
		fixture     string
		wantSubject string
		wantText    string
		wantHTML    string
	}{
		{
			name:        "nested multipart",
			fixture:     "nested.eml",
			wantSubject: "Тестовий лист",
			wantText:    "Hello from plain text.",
			wantHTML:    "<p>Hello from <strong>HTML</strong>.</p>",
		},
		{
			name:        "windows-1251 body",
			fixture:     "non_utf8.eml",
			wantSubject: "Windows-1251 body",
			wantText:    "Привіт, світе!",
		},
	}

	parser := newTestMIMEParser()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parser.Parse(context.Background(), &Message{Raw: readFixture(t, tt.fixture)})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if parsed.Subject != tt.wantSubject {
				t.Errorf("Subject = %q, want %q", parsed.Subject, tt.wantSubject)
			}
			if strings.TrimSpace(parsed.TextBody) != tt.wantText {
				t.Errorf("TextBody = %q, want %q", parsed.TextBody, tt.wantText)
			}
			if strings.TrimSpace(parsed.HTMLBody) != tt.wantHTML {
				t.Errorf("HTMLBody = %q, want %q", parsed.HTMLBody, tt.wantHTML)
			}

			if tt.fixture == "nested.eml" {
				if len(parsed.From) != 1 || parsed.From[0].Name != "Alice Example" ||
					parsed.From[0].Address != "Alice.Smith@Example.COM" ||
					parsed.From[0].NormalizedAddress != "alice.smith@example.com" {
					t.Errorf("From = %+v", parsed.From)
				}
				if len(parsed.To) != 2 || parsed.To[0].Address != "bob@example.org" || parsed.To[1].Address != "carol@example.org" {
					t.Errorf("To = %+v", parsed.To)
				}
			}
		})
	}
}

func TestMIMEParserFallbackMessageID(t *testing.T) {
	parser := newTestMIMEParser()
	message := &Message{
		ProfileID:   17,
		Mailbox:     "INBOX",
		UIDValidity: 42,
		UID:         9,
		Raw:         []byte("From: sender@example.org\r\nTo: receiver@example.org\r\n\r\nHello"),
	}

	first, err := parser.Parse(context.Background(), message)
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	second, err := parser.Parse(context.Background(), message)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if first.MessageID == "" || first.MessageID != second.MessageID {
		t.Fatalf("fallback MessageID is not stable: %q, %q", first.MessageID, second.MessageID)
	}

	other := *message
	other.Mailbox = "Archive"
	third, err := parser.Parse(context.Background(), &other)
	if err != nil {
		t.Fatalf("third Parse: %v", err)
	}
	if third.MessageID == first.MessageID {
		t.Fatalf("fallback MessageID = %q for different mailboxes", third.MessageID)
	}
}

func TestMIMEParserLimitsReferences(t *testing.T) {
	var references strings.Builder
	for i := range 2000 {
		references.WriteString(" <ref-")
		references.WriteString(string(rune('a' + i%26)))
		references.WriteString("-")
		references.WriteString(strings.Repeat("x", 12))
		references.WriteString("@example.org>")
	}

	raw := "From: sender@example.org\r\nTo: receiver@example.org\r\nReferences:" + references.String() + "\r\n\r\nHello"
	parsed, err := newTestMIMEParser().Parse(context.Background(), &Message{Raw: []byte(raw)})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.References) == 0 {
		t.Fatal("References is empty")
	}
	if len(strings.Join(parsed.References, " ")) > maxReferencesBytes {
		t.Fatalf("References size exceeds %d bytes", maxReferencesBytes)
	}
	if got := parsed.References[len(parsed.References)-1]; got != "ref-x-xxxxxxxxxxxx@example.org" {
		t.Errorf("last Reference = %q", got)
	}
}

func TestMIMEParserAcceptsLargeHeader(t *testing.T) {
	raw := "From: sender@example.org\r\nTo: receiver@example.org\r\nSubject: " +
		strings.Repeat("a", 300<<10) + "\r\n\r\nHello"

	parsed, err := newTestMIMEParser().Parse(context.Background(), &Message{Raw: []byte(raw)})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len([]rune(parsed.Subject)) != maxHeaderTextRunes {
		t.Fatalf("Subject length = %d, want %d", len([]rune(parsed.Subject)), maxHeaderTextRunes)
	}
}

func TestMIMEParserRecoversUnknownEncoding(t *testing.T) {
	tests := []struct {
		name    string
		headers string
	}{
		{
			name:    "charset",
			headers: "Content-Type: text/plain; charset=x-unknown-charset\r\n",
		},
		{
			name:    "transfer encoding",
			headers: "Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: x-unknown-encoding\r\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := "From: sender@example.org\r\nTo: receiver@example.org\r\n" + tt.headers + "\r\nReadable body"
			parsed, err := newTestMIMEParser().Parse(context.Background(), &Message{Raw: []byte(raw)})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if strings.TrimSpace(parsed.TextBody) != "Readable body" {
				t.Fatalf("TextBody = %q, want Readable body", parsed.TextBody)
			}
		})
	}
}

func TestMIMEParserBodyLimitPreservesUTF8(t *testing.T) {
	parser := inboundParserWithBodyLimit(3)
	raw := "From: sender@example.org\r\nTo: receiver@example.org\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nабв"

	parsed, err := parser.Parse(context.Background(), &Message{Raw: []byte(raw)})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.TextBody != "а" || !utf8.ValidString(parsed.TextBody) {
		t.Fatalf("TextBody = %q, want valid UTF-8 %q", parsed.TextBody, "а")
	}
}

func TestMIMEParserRejectsMalformedMultipart(t *testing.T) {
	_, err := newTestMIMEParser().Parse(context.Background(), &Message{Raw: readFixture(t, "malformed.eml")})
	if err == nil {
		t.Fatal("Parse error = nil")
	}
}

func TestMIMEParserAttachments(t *testing.T) {
	parsed, err := newTestMIMEParser().Parse(context.Background(), &Message{Raw: readFixture(t, "attachments.eml")})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Parts) != 2 {
		t.Fatalf("Parts length = %d, want 2", len(parsed.Parts))
	}

	inline := parsed.Parts[0]
	if inline.Name != "inline-1" || inline.ContentType != "image/png" ||
		inline.ContentID != "logo@example.org" || inline.Disposition != model.EmailPartDispositionInline ||
		inline.Size != 5 || string(inline.Content) != "image" || inline.SkippedReason != "" {
		t.Errorf("inline Part = %+v", inline)
	}

	attachment := parsed.Parts[1]
	if attachment.Name != "report.txt" || attachment.ContentType != "text/plain" ||
		attachment.Disposition != model.EmailPartDispositionAttachment || attachment.Size != 6 ||
		string(attachment.Content) != "report" || attachment.SkippedReason != "" {
		t.Errorf("attachment Part = %+v", attachment)
	}
}

func TestMIMEParserAttachmentLimits(t *testing.T) {
	tests := []struct {
		name         string
		sizes        []int
		maxSize      int64
		maxTotal     int64
		maxCount     int
		wantSkipped  int
		wantReason   string
		wantAccepted int
	}{
		{
			name:         "single attachment size",
			sizes:        []int{4},
			maxSize:      3,
			maxTotal:     10,
			maxCount:     3,
			wantSkipped:  0,
			wantReason:   partSkippedSizeLimit,
			wantAccepted: 0,
		},
		{
			name:         "total attachment size",
			sizes:        []int{4, 3},
			maxSize:      5,
			maxTotal:     6,
			maxCount:     3,
			wantSkipped:  1,
			wantReason:   partSkippedTotalLimit,
			wantAccepted: 1,
		},
		{
			name:         "attachment count",
			sizes:        []int{1, 1},
			maxSize:      5,
			maxTotal:     10,
			maxCount:     1,
			wantSkipped:  1,
			wantReason:   partSkippedCountLimit,
			wantAccepted: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := newMIMEParserWithAttachmentLimits(tt.maxSize, tt.maxTotal, tt.maxCount)
			parsed, err := parser.Parse(context.Background(), &Message{Raw: attachmentMessage(tt.sizes...)})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if strings.TrimSpace(parsed.TextBody) != "Hello" {
				t.Errorf("TextBody = %q, want Hello", parsed.TextBody)
			}
			if len(parsed.Parts) != len(tt.sizes) {
				t.Fatalf("Parts length = %d, want %d", len(parsed.Parts), len(tt.sizes))
			}
			if got := parsed.Parts[tt.wantSkipped].SkippedReason; got != tt.wantReason {
				t.Errorf("SkippedReason = %q, want %q", got, tt.wantReason)
			}
			if parsed.Parts[tt.wantSkipped].Content != nil {
				t.Error("skipped attachment content is retained")
			}

			accepted := 0
			for _, part := range parsed.Parts {
				if part.SkippedReason == "" {
					accepted++
				}
			}
			if accepted != tt.wantAccepted {
				t.Errorf("accepted attachments = %d, want %d", accepted, tt.wantAccepted)
			}
		})
	}
}

func TestMIMEParserClassifiesMessages(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		kind model.EmailKind
	}{
		{
			name: "regular",
			raw:  "From: sender@example.org\r\nTo: receiver@example.org\r\n\r\nHello",
			kind: model.EmailKindRegular,
		},
		{
			name: "auto reply",
			raw:  "From: receiver@example.org\r\nAuto-Submitted: auto-replied\r\n\r\nOut of office",
			kind: model.EmailKindAutoReply,
		},
		{
			name: "auto reply with empty return path",
			raw:  "Return-Path: <>\r\nFrom: receiver@example.org\r\nAuto-Submitted: auto-replied\r\n\r\nOut of office",
			kind: model.EmailKindAutoReply,
		},
		{
			name: "bulk newsletter",
			raw:  "From: news@example.org\r\nPrecedence: bulk\r\nList-Id: news.example.org\r\n\r\nNewsletter",
			kind: model.EmailKindRegular,
		},
	}

	parser := newTestMIMEParser()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parser.Parse(context.Background(), &Message{Raw: []byte(tt.raw)})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if parsed.Kind != tt.kind {
				t.Errorf("Kind = %q, want %q", parsed.Kind, tt.kind)
			}
			if parsed.Kind != model.EmailKindBounce && parsed.Bounce != nil {
				t.Errorf("Bounce = %+v for %q message", parsed.Bounce, parsed.Kind)
			}
		})
	}
}

func TestMIMEParserParsesBounce(t *testing.T) {
	parsed, err := newTestMIMEParser().Parse(context.Background(), &Message{Raw: readFixture(t, "bounce.eml")})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Kind != model.EmailKindBounce {
		t.Fatalf("Kind = %q, want %q", parsed.Kind, model.EmailKindBounce)
	}
	if parsed.Bounce == nil {
		t.Fatal("Bounce = nil")
	}
	if parsed.Bounce.OriginalMessageID != "original@example.org" ||
		parsed.Bounce.Recipient != "bob@example.org" ||
		parsed.Bounce.Status != "5.1.1" ||
		parsed.Bounce.DiagnosticCode != "550 5.1.1 User unknown" {
		t.Errorf("Bounce = %+v", parsed.Bounce)
	}
}

func newTestMIMEParser() *MIMEParser {
	return newMIMEParserWithAttachmentLimits(10<<20, 20<<20, 15)
}

func inboundParserWithBodyLimit(limit int64) *MIMEParser {
	return NewMIMEParser(&config.Config{MIME: config.MIMEConfig{
		MaxBodySize:             limit,
		MaxAttachmentSize:       1 << 20,
		MaxAttachmentsTotalSize: 2 << 20,
		MaxAttachments:          5,
	}})
}

func newMIMEParserWithAttachmentLimits(maxSize, maxTotal int64, maxCount int) *MIMEParser {
	return NewMIMEParser(&config.Config{MIME: config.MIMEConfig{
		MaxBodySize:             1 << 20,
		MaxAttachmentSize:       maxSize,
		MaxAttachmentsTotalSize: maxTotal,
		MaxAttachments:          maxCount,
	}})
}

func attachmentMessage(sizes ...int) []byte {
	var raw strings.Builder
	raw.WriteString("From: sender@example.org\r\nTo: receiver@example.org\r\n")
	raw.WriteString("Content-Type: multipart/mixed; boundary=limits\r\n\r\n")
	raw.WriteString("--limits\r\nContent-Type: text/plain\r\n\r\nHello\r\n")
	for i, size := range sizes {
		fmt.Fprintf(&raw, "--limits\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"file-%d.bin\"\r\n\r\n", i+1)
		raw.WriteString(strings.Repeat("x", size))
		raw.WriteString("\r\n")
	}
	raw.WriteString("--limits--\r\n")

	return []byte(raw.String())
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}

	return raw
}
