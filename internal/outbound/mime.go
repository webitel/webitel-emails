package outbound

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"

	"github.com/webitel/webitel-emails/internal/model"
)

// ErrMessageTooLarge means the encoded email exceeds the size limit.
var ErrMessageTooLarge = errors.New("outbound: message is too large")

const defaultAttachmentType = "application/octet-stream"

// Email holds the stored values the MIME is built from, so a rebuild is identical.
type Email struct {
	MessageID   string
	Date        time.Time
	Sender      Sender
	To, Cc, Bcc []model.EmailAddress
	Subject     string
	HTMLBody    string
	TextBody    string
	InReplyTo   string
	References  []string
	Attachments []Attachment
}

// Attachment is the content of an attachment.
type Attachment struct {
	FileName string
	MimeType string
	Content  io.Reader
}

// BuildMIME encodes the email; Bcc goes only to the envelope.
func BuildMIME(e *Email, maxSize int64) ([]byte, error) {
	buf := &limitedBuffer{limit: maxSize}
	if err := writeMIME(buf, e); err != nil {
		if errors.Is(err, ErrMessageTooLarge) {
			return nil, ErrMessageTooLarge
		}
		return nil, fmt.Errorf("outbound: build mime: %w", err)
	}

	return buf.Bytes(), nil
}

// Envelope returns MAIL FROM and the recipients To, Cc, Bcc without case-insensitive duplicates.
func Envelope(e *Email) (string, []string) {
	seen := map[string]bool{}
	var rcpt []string
	for _, list := range [][]model.EmailAddress{e.To, e.Cc, e.Bcc} {
		for _, a := range list {
			if key := strings.ToLower(a.Address); !seen[key] {
				seen[key] = true
				rcpt = append(rcpt, a.Address)
			}
		}
	}

	return e.Sender.Address, rcpt
}

func writeMIME(w io.Writer, e *Email) error {
	h := header(e)
	if len(e.Attachments) == 0 {
		if e.HTMLBody == "" {
			h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
			part, err := mail.CreateSingleInlineWriter(w, h)
			if err != nil {
				return err
			}
			return writePart(part, strings.NewReader(e.TextBody))
		}
		iw, err := mail.CreateInlineWriter(w, h)
		if err != nil {
			return err
		}
		return writeAlternative(iw, e)
	}

	mw, err := mail.CreateWriter(w, h)
	if err != nil {
		return err
	}
	switch {
	case e.HTMLBody != "":
		iw, err := mw.CreateInline()
		if err != nil {
			return err
		}
		if err := writeAlternative(iw, e); err != nil {
			return err
		}
	case e.TextBody != "":
		part, err := mw.CreateSingleInline(textHeader("text/plain"))
		if err != nil {
			return err
		}
		if err := writePart(part, strings.NewReader(e.TextBody)); err != nil {
			return err
		}
	}
	for _, a := range e.Attachments {
		var ah mail.AttachmentHeader
		ah.Set("Content-Type", attachmentType(a.MimeType))
		ah.SetFilename(a.FileName)
		part, err := mw.CreateAttachment(ah)
		if err != nil {
			return err
		}
		if err := writePart(part, a.Content); err != nil {
			return err
		}
	}

	return mw.Close()
}

func header(e *Email) mail.Header {
	var h mail.Header
	h.SetDate(e.Date)
	h.SetAddressList("From", []*mail.Address{{Name: e.Sender.Name, Address: e.Sender.Address}})
	if e.Sender.ReplyTo != "" {
		h.SetAddressList("Reply-To", []*mail.Address{{Address: e.Sender.ReplyTo}})
	}
	h.SetAddressList("To", addresses(e.To))
	h.SetAddressList("Cc", addresses(e.Cc))
	if e.Subject != "" {
		h.SetSubject(e.Subject)
	}
	h.SetMessageID(e.MessageID)
	if e.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{e.InReplyTo})
	}
	h.SetMsgIDList("References", e.References)

	return h
}

func writeAlternative(iw *mail.InlineWriter, e *Email) error {
	for _, body := range []struct{ contentType, value string }{
		{"text/plain", e.TextBody},
		{"text/html", e.HTMLBody},
	} {
		part, err := iw.CreatePart(textHeader(body.contentType))
		if err != nil {
			return err
		}
		if err := writePart(part, strings.NewReader(body.value)); err != nil {
			return err
		}
	}

	return iw.Close()
}

// attachmentType keeps attachment bytes intact: the writer treats multipart/* as
// a container and refuses charsets other than UTF-8, and message/* must not be base64.
func attachmentType(value string) string {
	mediaType, params, err := mime.ParseMediaType(value)
	switch charset := strings.ToLower(params["charset"]); {
	case err != nil,
		strings.HasPrefix(mediaType, "multipart/"),
		strings.HasPrefix(mediaType, "message/"),
		charset != "" && charset != "utf-8" && charset != "us-ascii":
		return defaultAttachmentType
	}

	return value
}

func textHeader(contentType string) mail.InlineHeader {
	var h mail.InlineHeader
	h.SetContentType(contentType, map[string]string{"charset": "utf-8"})

	return h
}

func writePart(part io.WriteCloser, content io.Reader) error {
	if _, err := io.Copy(part, content); err != nil {
		return err
	}

	return part.Close()
}

func addresses(list []model.EmailAddress) []*mail.Address {
	result := make([]*mail.Address, len(list))
	for i, a := range list {
		result[i] = &mail.Address{Name: a.Name, Address: a.Address}
	}

	return result
}

// limitedBuffer refuses to grow beyond limit instead of holding an oversized email.
type limitedBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.limit {
		return 0, ErrMessageTooLarge
	}

	return b.Buffer.Write(p)
}
