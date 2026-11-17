// Package outbound prepares and encodes outbound emails without I/O.
package outbound

import (
	"fmt"
	netmail "net/mail"
	"strings"
	"unicode"

	"github.com/k3a/html2text"
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/emailhtml"
	"github.com/webitel/webitel-emails/internal/model"
)

const (
	maxAddressLength = 254
	maxNameRunes     = 200
	maxSubjectRunes  = 255
	replyPrefix      = "Re: "

	// headerReserve covers fixed headers, References and MIME boundaries.
	headerReserve = 32 << 10
	partReserve   = 1 << 10
)

// Draft is an outbound email as the operator composed it.
type Draft struct {
	To, Cc, Bcc []model.EmailAddress
	Subject     string
	// Reply prefixes the subject with "Re:"; an empty one is derived from ThreadSubject.
	Reply         bool
	ThreadSubject string
	HTMLBody      string
	TextBody      string
	Attachments   []AttachmentMeta
}

// AttachmentMeta describes an uploaded attachment by its reference.
type AttachmentMeta struct {
	FileName string
	Size     int64
}

// Prepared is a validated email ready to be stored.
type Prepared struct {
	To, Cc, Bcc []model.EmailAddress
	Subject     string
	HTMLBody    string
	TextBody    string
}

// Prepare validates the draft and sanitizes its bodies; nothing is truncated
// except a subject the server derives.
func Prepare(cfg config.OutboundConfig, d Draft) (*Prepared, error) {
	if len(d.To) == 0 {
		return nil, invalid("at least one To recipient is required", "email.message.no_recipient")
	}
	if len(d.To)+len(d.Cc)+len(d.Bcc) > cfg.MaxRecipients {
		return nil, invalid("too many recipients", "email.message.too_many_recipients")
	}

	p := &Prepared{}
	var err error
	if p.To, err = recipients(d.To); err != nil {
		return nil, err
	}
	if p.Cc, err = recipients(d.Cc); err != nil {
		return nil, err
	}
	if p.Bcc, err = recipients(d.Bcc); err != nil {
		return nil, err
	}
	if p.Subject, err = subject(d); err != nil {
		return nil, err
	}
	if p.HTMLBody, p.TextBody, err = bodies(d.HTMLBody, d.TextBody, cfg.MaxBodySize); err != nil {
		return nil, err
	}
	if p.TextBody == "" && len(d.Attachments) == 0 {
		return nil, invalid("email has no content", "email.message.empty")
	}

	if len(d.Attachments) > cfg.MaxAttachments {
		return nil, invalid("too many attachments", "email.message.too_many_attachments")
	}
	var total int64
	for _, a := range d.Attachments {
		total += a.Size
	}
	if total > cfg.MaxAttachmentsTotalSize {
		return nil, invalid("attachments are too large", "email.message.attachments_too_large")
	}
	if estimateSize(p, d.Attachments) > cfg.MaxMessageSize {
		return nil, invalid("email is too large", "email.message.too_large")
	}

	return p, nil
}

func recipients(list []model.EmailAddress) ([]model.EmailAddress, error) {
	result := make([]model.EmailAddress, 0, len(list))
	for _, a := range list {
		address, ok := checkAddress(a.Address)
		name := strings.TrimSpace(a.Name)
		if !ok || !checkName(name) {
			return nil, invalid(fmt.Sprintf("recipient %q is invalid", a.Address), "email.message.invalid_recipient")
		}
		result = append(result, model.EmailAddress{
			Name:              name,
			Address:           address,
			NormalizedAddress: strings.ToLower(address),
		})
	}

	return result, nil
}

// checkAddress accepts a bare addr-spec, without a name or angle brackets.
func checkAddress(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxAddressLength {
		return "", false
	}
	parsed, err := netmail.ParseAddress(value)

	return value, err == nil && parsed.Name == "" && parsed.Address == value
}

func checkName(value string) bool {
	return !hasControl(value) && len([]rune(value)) <= maxNameRunes
}

func subject(d Draft) (string, error) {
	if value := strings.TrimSpace(d.Subject); value != "" {
		if d.Reply {
			value = withReplyPrefix(value)
		}
		if hasControl(value) || len([]rune(value)) > maxSubjectRunes {
			return "", invalid("subject is invalid or longer than 255 characters", "email.message.invalid_subject")
		}

		return value, nil
	}

	value := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, d.ThreadSubject))
	if !d.Reply || value == "" {
		return "", nil
	}

	return truncateRunes(withReplyPrefix(value), maxSubjectRunes), nil
}

func withReplyPrefix(value string) string {
	if len(value) >= 3 && strings.EqualFold(value[:3], "re:") {
		return value
	}

	return replyPrefix + value
}

// bodies limits each form: sanitizing may expand the HTML and the text is new.
func bodies(rawHTML, rawText string, limit int64) (string, string, error) {
	tooLarge := invalid("email body is too large", "email.message.body_too_large")
	if int64(len(rawHTML)) > limit || int64(len(rawText)) > limit {
		return "", "", tooLarge
	}

	var htmlBody string
	if strings.TrimSpace(rawHTML) != "" {
		doc, err := emailhtml.Sanitize(rawHTML)
		if err != nil {
			return "", "", fmt.Errorf("outbound: sanitize html: %w", err)
		}
		if htmlBody, err = doc.Render(nil); err != nil {
			return "", "", fmt.Errorf("outbound: render html: %w", err)
		}
		if int64(len(htmlBody)) > limit {
			return "", "", tooLarge
		}
	}

	text := rawText
	if strings.TrimSpace(text) == "" && htmlBody != "" {
		text = html2text.HTML2Text(htmlBody)
		if int64(len(text)) > limit {
			return "", "", tooLarge
		}
	}
	// Markup without text, such as <p><br></p>, is not content.
	if strings.TrimSpace(text) == "" {
		return "", "", nil
	}

	return htmlBody, text, nil
}

// estimateSize is an upper bound of the MIME size: base64 with line breaks for
// attachments and quoted-printable with soft line breaks for text.
func estimateSize(p *Prepared, attachments []AttachmentMeta) int64 {
	size := int64(headerReserve+3*len(p.Subject)) + quotedPrintableSize(len(p.HTMLBody)) + quotedPrintableSize(len(p.TextBody))
	for _, list := range [][]model.EmailAddress{p.To, p.Cc} {
		for _, a := range list {
			size += int64(3*len(a.Name) + len(a.Address) + 8)
		}
	}
	for _, a := range attachments {
		encoded := (a.Size + 2) / 3 * 4
		size += encoded + encoded/76*2 + partReserve + int64(3*len(a.FileName))
	}

	return size
}

// quotedPrintableSize allows three bytes per byte and a soft break "=\r\n"
// per 73 encoded bytes, the shortest full line.
func quotedPrintableSize(n int) int64 {
	encoded := 3 * int64(n)

	return encoded + (encoded/73+1)*3
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func truncateRunes(value string, limit int) string {
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit])
	}

	return value
}

func invalid(message, id string) error {
	return kiterrors.InvalidArgument(message, kiterrors.WithID(id))
}
