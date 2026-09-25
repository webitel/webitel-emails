package inbound

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	gomessage "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/k3a/html2text"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"
)

const (
	maxHeaderTextRunes = 255
	maxMessageIDBytes  = 998
	maxReferencesBytes = 32 << 10

	partSkippedCountLimit = "attachment_count_limit_exceeded"
	partSkippedSizeLimit  = "attachment_size_limit_exceeded"
	partSkippedTotalLimit = "attachments_total_size_limit_exceeded"
	partSkippedReadError  = "attachment_read_error"
)

// MIMEParser converts raw RFC822 messages into the internal email model.
type MIMEParser struct {
	maxBodySize             int64
	maxAttachmentSize       int64
	maxAttachmentsTotalSize int64
	maxAttachments          int
}

func NewMIMEParser(cfg *config.Config) *MIMEParser {
	return &MIMEParser{
		maxBodySize:             cfg.MIME.MaxBodySize,
		maxAttachmentSize:       cfg.MIME.MaxAttachmentSize,
		maxAttachmentsTotalSize: cfg.MIME.MaxAttachmentsTotalSize,
		maxAttachments:          cfg.MIME.MaxAttachments,
	}
}

func (p *MIMEParser) Parse(ctx context.Context, message *Message) (*model.ParsedEmail, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if message == nil {
		return nil, fmt.Errorf("mime: message is required")
	}

	entity, err := gomessage.ReadWithOptions(bytes.NewReader(message.Raw), &gomessage.ReadOptions{
		MaxHeaderBytes: -1,
	})
	if entity == nil {
		return nil, fmt.Errorf("mime: read message: %w", err)
	}
	if err != nil && !isRecoverableDecodeError(err) {
		return nil, fmt.Errorf("mime: read message: %w", err)
	}
	header := mail.Header{Header: entity.Header}

	messageID, err := header.MessageID()
	messageID = cleanMessageID(messageID)
	if err != nil || messageID == "" {
		messageID = fallbackMessageID(message)
	}

	inReplyTo, _ := header.MsgIDList("In-Reply-To")
	references, _ := header.MsgIDList("References")
	inReplyTo = cleanMessageIDs(inReplyTo)
	references = limitReferences(cleanMessageIDs(references))

	subject, _ := header.Subject()
	subject = truncateRunes(cleanHeaderText(subject), maxHeaderTextRunes)

	date, err := header.Date()
	if err != nil || date.IsZero() {
		date = message.InternalDate
	}
	if date.IsZero() {
		date = time.Now().UTC()
	}

	from := parseAddressList(&header, "From")
	sender := parseAddressList(&header, "Sender")
	replyTo := parseAddressList(&header, "Reply-To")
	to := parseAddressList(&header, "To")
	cc := parseAddressList(&header, "Cc")
	bcc := parseAddressList(&header, "Bcc")

	parsed := &model.ParsedEmail{
		DomainID:    message.DomainID,
		ProfileID:   message.ProfileID,
		Mailbox:     message.Mailbox,
		UIDValidity: message.UIDValidity,
		UID:         message.UID,
		MessageID:   messageID,
		References:  references,
		Subject:     subject,
		Date:        date,
		From:        from,
		ReplyTo:     replyTo,
		To:          to,
		Cc:          cc,
		Bcc:         bcc,
		Kind:        model.EmailKindRegular,
	}
	switch {
	case isBounce(entity.Header, from, sender):
		parsed.Kind = model.EmailKindBounce
		parsed.Bounce = newBounceInfo(entity.Header)
	case isAutoReply(entity.Header):
		parsed.Kind = model.EmailKindAutoReply
	case hasEmptyReturnPath(entity.Header):
		parsed.Kind = model.EmailKindBounce
		parsed.Bounce = newBounceInfo(entity.Header)
	}
	if len(inReplyTo) > 0 {
		parsed.InReplyTo = inReplyTo[0]
	}
	if len(sender) > 0 {
		parsed.Sender = &sender[0]
	}
	if err := p.parseMIMEParts(ctx, entity, parsed); err != nil {
		return nil, err
	}
	finalizeEmailParts(parsed)
	if parsed.TextBody == "" && parsed.HTMLBody != "" {
		parsed.TextBody = limitBody([]byte(html2text.HTML2Text(parsed.HTMLBody)), p.maxBodySize)
	}

	return parsed, nil
}

func (p *MIMEParser) parseMIMEParts(ctx context.Context, entity *gomessage.Entity, parsed *model.ParsedEmail) error {
	acceptedParts := 0
	acceptedSize := int64(0)

	err := entity.Walk(func(_ []int, part *gomessage.Entity, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil && !isRecoverableDecodeError(err) {
			return fmt.Errorf("mime: read part: %w", err)
		}

		contentType, _, err := part.Header.ContentType()
		if err != nil {
			return nil
		}
		disposition, _, _ := part.Header.ContentDisposition()
		if parsed.Bounce != nil {
			switch strings.ToLower(contentType) {
			case "message/delivery-status":
				p.parseDeliveryStatus(part.Body, parsed.Bounce)
				return nil
			case "message/rfc822", "text/rfc822-headers":
				parseOriginalMessageHeaders(part.Body, p.maxBodySize, parsed.Bounce)
				return nil
			}
		}
		if emailPart, ok := parseEmailPart(part, contentType, disposition); ok {
			bufferLimit := min(p.maxAttachmentSize, max(p.maxAttachmentsTotalSize-acceptedSize, 0))
			if acceptedParts >= p.maxAttachments {
				bufferLimit = 0
			}

			content, size, readErr := readPartContent(part.Body, bufferLimit)
			emailPart.Size = size
			switch {
			case readErr != nil:
				emailPart.SkippedReason = partSkippedReadError
			case acceptedParts >= p.maxAttachments:
				emailPart.SkippedReason = partSkippedCountLimit
			case size > p.maxAttachmentSize:
				emailPart.SkippedReason = partSkippedSizeLimit
			case size > p.maxAttachmentsTotalSize-acceptedSize:
				emailPart.SkippedReason = partSkippedTotalLimit
			default:
				emailPart.Content = content
				acceptedParts++
				acceptedSize += size
			}
			parsed.Parts = append(parsed.Parts, emailPart)

			return nil
		}
		if contentType != "text/plain" && contentType != "text/html" {
			return nil
		}
		if contentType == "text/plain" && parsed.TextBody != "" {
			return nil
		}
		if contentType == "text/html" && parsed.HTMLBody != "" {
			return nil
		}

		body, err := io.ReadAll(io.LimitReader(part.Body, p.maxBodySize+1))
		if err != nil {
			return fmt.Errorf("mime: read %s body: %w", contentType, err)
		}
		value := limitBody(body, p.maxBodySize)

		switch contentType {
		case "text/plain":
			if parsed.TextBody == "" {
				parsed.TextBody = value
			}
		case "text/html":
			if parsed.HTMLBody == "" {
				parsed.HTMLBody = value
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("mime: walk parts: %w", err)
	}

	return nil
}

func readPartContent(reader io.Reader, bufferLimit int64) ([]byte, int64, error) {
	content, err := io.ReadAll(io.LimitReader(reader, bufferLimit+1))
	size := int64(len(content))
	if err != nil {
		return nil, size, err
	}

	remainder, err := io.Copy(io.Discard, reader)
	size += remainder
	if err != nil {
		return nil, size, err
	}
	if int64(len(content)) > bufferLimit {
		content = nil
	}

	return content, size, nil
}

func parseEmailPart(part *gomessage.Entity, contentType, disposition string) (model.EmailPart, bool) {
	disposition = strings.ToLower(strings.TrimSpace(disposition))
	contentType = strings.ToLower(strings.TrimSpace(contentType))

	var kind model.EmailPartDisposition
	switch {
	case disposition == "attachment":
		kind = model.EmailPartDispositionAttachment
	case disposition == "inline" && !strings.HasPrefix(contentType, "text/"):
		kind = model.EmailPartDispositionInline
	case !strings.HasPrefix(contentType, "text/") && !strings.HasPrefix(contentType, "multipart/"):
		kind = model.EmailPartDispositionAttachment
	default:
		return model.EmailPart{}, false
	}

	header := mail.AttachmentHeader{Header: part.Header}
	name, _ := header.Filename()

	return model.EmailPart{
		Name:        cleanHeaderText(name),
		ContentType: contentType,
		ContentID:   strings.Trim(cleanHeaderText(part.Header.Get("Content-ID")), "<>"),
		Disposition: kind,
	}, true
}

func finalizeEmailParts(parsed *model.ParsedEmail) {
	for i := range parsed.Parts {
		part := &parsed.Parts[i]
		isImage := strings.HasPrefix(part.ContentType, "image/")
		referenced := isImage && htmlReferencesCID(parsed.HTMLBody, part.ContentID)

		switch {
		case referenced:
			part.Disposition = model.EmailPartDispositionInline
		case part.Disposition == model.EmailPartDispositionInline && (!isImage || parsed.HTMLBody != ""):
			part.Disposition = model.EmailPartDispositionAttachment
		}

		if part.Name == "" {
			part.Name = fmt.Sprintf("%s-%d", part.Disposition, i+1)
		}
	}
}

func htmlReferencesCID(html, contentID string) bool {
	if html == "" || contentID == "" {
		return false
	}

	lowerHTML := strings.ToLower(html)
	for offset := 0; offset < len(html); {
		index := strings.Index(lowerHTML[offset:], "cid:")
		if index < 0 {
			return false
		}

		start := offset + index + len("cid:")
		end := start
		for end < len(html) && !strings.ContainsRune("\"'<> ()\t\r\n", rune(html[end])) {
			end++
		}

		value := html[start:end]
		if value == contentID {
			return true
		}
		if decoded, err := url.PathUnescape(value); err == nil && decoded == contentID {
			return true
		}
		if decoded, err := url.QueryUnescape(value); err == nil && decoded == contentID {
			return true
		}

		offset = start
	}

	return false
}

func isRecoverableDecodeError(err error) bool {
	return gomessage.IsUnknownCharset(err) || gomessage.IsUnknownEncoding(err)
}

func isAutoReply(header gomessage.Header) bool {
	autoSubmitted := firstHeaderToken(header.Get("Auto-Submitted"))
	if autoSubmitted != "" && autoSubmitted != "no" {
		return true
	}
	if isAffirmativeHeader(header.Get("X-Autoreply")) || isAffirmativeHeader(header.Get("X-Autorespond")) {
		return true
	}
	if containsHeaderToken(header.Get("X-Auto-Response-Suppress"), "all", "autoreply", "auto-reply", "oof") {
		return true
	}

	return containsHeaderToken(header.Get("Precedence"), "auto_reply", "auto-reply", "autoreply")
}

func isBounce(header gomessage.Header, addresses ...[]model.EmailAddress) bool {
	contentType, params, err := header.ContentType()
	if err == nil && strings.EqualFold(contentType, "multipart/report") &&
		strings.EqualFold(params["report-type"], "delivery-status") {
		return true
	}
	if strings.TrimSpace(header.Get("X-Failed-Recipients")) != "" {
		return true
	}

	for _, list := range addresses {
		for _, address := range list {
			localPart, _, found := strings.Cut(address.NormalizedAddress, "@")
			if found && localPart == "mailer-daemon" {
				return true
			}
		}
	}

	return false
}

func hasEmptyReturnPath(header gomessage.Header) bool {
	if !header.Has("Return-Path") {
		return false
	}

	returnPath := strings.TrimSpace(header.Get("Return-Path"))
	return returnPath == "" || returnPath == "<>"
}

func newBounceInfo(header gomessage.Header) *model.EmailBounceInfo {
	return &model.EmailBounceInfo{
		OriginalMessageID: cleanRawMessageID(header.Get("X-Original-Message-ID")),
		Recipient:         firstListValue(header.Get("X-Failed-Recipients")),
	}
}

func (p *MIMEParser) parseDeliveryStatus(reader io.Reader, bounce *model.EmailBounceInfo) {
	statusReader := textproto.NewReader(bufio.NewReader(io.LimitReader(reader, p.maxBodySize+1)))
	for {
		header, err := statusReader.ReadMIMEHeader()
		if bounce.OriginalMessageID == "" {
			bounce.OriginalMessageID = cleanRawMessageID(header.Get("Original-Message-ID"))
		}
		if bounce.Recipient == "" {
			bounce.Recipient = deliveryStatusValue(header.Get("Final-Recipient"))
			if bounce.Recipient == "" {
				bounce.Recipient = deliveryStatusValue(header.Get("Original-Recipient"))
			}
		}
		if bounce.Status == "" {
			bounce.Status = cleanHeaderText(header.Get("Status"))
		}
		if bounce.DiagnosticCode == "" {
			bounce.DiagnosticCode = deliveryStatusValue(header.Get("Diagnostic-Code"))
		}
		if err != nil {
			return
		}
	}
}

func parseOriginalMessageHeaders(reader io.Reader, maxHeaderBytes int64, bounce *model.EmailBounceInfo) {
	if bounce.OriginalMessageID != "" {
		return
	}

	entity, err := gomessage.ReadWithOptions(reader, &gomessage.ReadOptions{MaxHeaderBytes: maxHeaderBytes})
	if entity == nil || err != nil && !isRecoverableDecodeError(err) {
		return
	}

	header := mail.Header{Header: entity.Header}
	messageID, _ := header.MessageID()
	bounce.OriginalMessageID = cleanMessageID(messageID)
}

func deliveryStatusValue(value string) string {
	if _, result, found := strings.Cut(value, ";"); found {
		value = result
	}

	return cleanHeaderText(value)
}

func firstListValue(value string) string {
	if first, _, found := strings.Cut(value, ","); found {
		value = first
	}

	return cleanHeaderText(value)
}

func cleanRawMessageID(value string) string {
	value = cleanHeaderText(value)
	if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}

	return cleanMessageID(value)
}

func firstHeaderToken(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = value[:index]
	}

	return strings.TrimSpace(value)
}

func isAffirmativeHeader(value string) bool {
	switch firstHeaderToken(value) {
	case "yes", "true", "1":
		return true
	default:
		return false
	}
}

func containsHeaderToken(value string, expected ...string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';'
	})
	for _, token := range tokens {
		for _, candidate := range expected {
			if token == candidate {
				return true
			}
		}
	}

	return false
}

func parseAddressList(header *mail.Header, field string) []model.EmailAddress {
	addresses, err := header.AddressList(field)
	if err != nil {
		return nil
	}

	result := make([]model.EmailAddress, 0, len(addresses))
	for _, address := range addresses {
		email := cleanHeaderText(address.Address)
		if email == "" {
			continue
		}

		result = append(result, model.EmailAddress{
			Name:              truncateRunes(cleanHeaderText(address.Name), maxHeaderTextRunes),
			Address:           email,
			NormalizedAddress: strings.ToLower(email),
		})
	}

	return result
}

func cleanHeaderText(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, value))
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit])
}

func limitBody(body []byte, limit int64) string {
	if int64(len(body)) > limit {
		body = body[:limit]
	}

	value := strings.ToValidUTF8(string(body), "\uFFFD")
	if int64(len(value)) <= limit {
		return value
	}

	end := int(limit)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}

	return value[:end]
}

func cleanMessageID(value string) string {
	value = cleanHeaderText(value)
	if value == "" || len(value) > maxMessageIDBytes {
		return ""
	}

	return value
}

func cleanMessageIDs(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = cleanMessageID(value); value != "" {
			result = append(result, value)
		}
	}

	return result
}

func limitReferences(values []string) []string {
	start := len(values)
	size := 0
	for start > 0 {
		next := len(values[start-1])
		if start < len(values) {
			next++
		}
		if size+next > maxReferencesBytes {
			break
		}

		size += next
		start--
	}

	return values[start:]
}

func fallbackMessageID(message *Message) string {
	key := fmt.Sprintf(
		"%d:%d:%s:%d:%d",
		message.ProfileID,
		len(message.Mailbox),
		message.Mailbox,
		message.UIDValidity,
		message.UID,
	)
	sum := sha256.Sum256([]byte(key))

	return "imap." + hex.EncodeToString(sum[:]) + "@webitel.local"
}
