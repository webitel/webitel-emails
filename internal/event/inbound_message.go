// Package event defines the wire contract of the events email-service emits
// through RabbitMQ. docs/events.md describes it for consumers.
package event

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/webitel/webitel-emails/internal/model"
)

// Contract of the event that starts Flow for a new inbound Message.
// Adding a field keeps the schema version; changing or removing one bumps it.
const (
	TypeInboundMessage       = "email.message.inbound"
	InboundMessageSchema     = 1
	InboundMessageRoutingKey = "email.message.inbound.v1"
)

// Fixed caps of the fields whose size is bounded by a standard, not by config.
const (
	maxAddressSize     = 320 // RFC 5321 path limit
	maxDisplayNameSize = 256
	maxMimeTypeSize    = 255 // RFC 6838 type plus subtype
	attachmentChannel  = "mail"
)

var errInvalidEvent = errors.New("event: invalid inbound message event")

// Limits bound the snapshot so any accepted email fits MaxPayloadSize.
type Limits struct {
	MaxBodyTextSize     int
	MaxAddressesPerType int
	MaxAttachments      int
	MaxFieldSize        int
	MaxPayloadSize      int
}

// Upper bounds checked before any allocation; 16 MiB is the RabbitMQ 4
// default max_message_size.
const (
	maxBodyTextSizeLimit = 8 << 20
	maxFieldSizeLimit    = 64 << 10
	maxListLimit         = 10_000
	maxPayloadSizeLimit  = 16 << 20
)

// Validate rejects limits under which a valid email could exceed MaxPayloadSize.
func (l Limits) Validate() error {
	switch {
	case l.MaxBodyTextSize < 1 || l.MaxBodyTextSize > maxBodyTextSizeLimit:
		return fmt.Errorf("event: max body text size must be within 1..%d bytes", maxBodyTextSizeLimit)
	case l.MaxFieldSize < 1 || l.MaxFieldSize > maxFieldSizeLimit:
		return fmt.Errorf("event: max field size must be within 1..%d bytes", maxFieldSizeLimit)
	case l.MaxAddressesPerType < 1 || l.MaxAddressesPerType > maxListLimit:
		return fmt.Errorf("event: max addresses per type must be within 1..%d", maxListLimit)
	case l.MaxAttachments < 1 || l.MaxAttachments > maxListLimit:
		return fmt.Errorf("event: max attachments must be within 1..%d", maxListLimit)
	case l.MaxPayloadSize < 1 || l.MaxPayloadSize > maxPayloadSizeLimit:
		return fmt.Errorf("event: max payload size must be within 1..%d bytes", maxPayloadSizeLimit)
	}

	worst, err := l.WorstCaseSize()
	if err != nil {
		return err
	}
	if worst > int64(l.MaxPayloadSize) {
		return fmt.Errorf("event: limits allow a %d-byte payload, above the payload limit of %d bytes", worst, l.MaxPayloadSize)
	}

	return nil
}

// WorstCaseSize adds up the envelope, one worst list item times the list limit
// and the body. Fields are filled with a character that doubles when escaped.
func (l Limits) WorstCaseSize() (int64, error) {
	fill := func(size int) string { return strings.Repeat(`"`, size) }
	maxID := int64(math.MaxInt64)
	latest := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)

	// Lists and the body stay empty here and are added arithmetically below.
	base, err := encode(InboundMessage{
		EventID:       uuid.Max,
		EventType:     TypeInboundMessage,
		SchemaVersion: InboundMessageSchema,
		OccurredAt:    latest,
		DomainID:      maxID,
		ProfileID:     maxID,
		FlowID:        &maxID,
		ThreadID:      maxID,
		MessageID:     maxID,
		Snapshot: InboundMessageSnapshot{
			InternetMessageID: fill(l.MaxFieldSize),
			InReplyTo:         fill(l.MaxFieldSize),
			Subject:           fill(l.MaxFieldSize),
			MessageKind:       model.EmailKindRegular,
			ThreadKind:        model.EmailThreadKindRegular,
			ContactID:         &maxID,
			ReceivedAt:        latest,
			From:              []Address{},
			Sender:            []Address{},
			ReplyTo:           []Address{},
			To:                []Address{},
			Cc:                []Address{},
			Attachments:       []Attachment{},
		},
	})
	if err != nil {
		return 0, err
	}

	address, err := encode(Address{Address: fill(maxAddressSize), Name: fill(maxDisplayNameSize)})
	if err != nil {
		return 0, err
	}

	attachment, err := encode(Attachment{
		ID:      maxID,
		Name:    fill(l.MaxFieldSize),
		Size:    maxID,
		Mime:    fill(maxMimeTypeSize),
		Channel: attachmentChannel,
	})
	if err != nil {
		return 0, err
	}

	// n items in a list add n items and n-1 commas to its empty brackets.
	list := func(items, itemSize int) int64 { return int64(items)*int64(itemSize) + int64(items-1) }

	const addressLists = 5

	return int64(len(base)) +
		2*int64(l.MaxBodyTextSize) +
		addressLists*list(l.MaxAddressesPerType, len(address)) +
		list(l.MaxAttachments, len(attachment)), nil
}

// Address is one participant of the email.
type Address struct {
	Address string `json:"address"`
	Name    string `json:"name"`
}

// Attachment has the JSON shape of model.File in flow_manager; ID is the storage file id.
type Attachment struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Mime     string  `json:"mime"`
	ViewName *string `json:"view_name"`
	Channel  string  `json:"channel"`
}

// InboundMessageSnapshot is the bounded copy of the Message that Flow reads.
type InboundMessageSnapshot struct {
	InternetMessageID string                `json:"internet_message_id"`
	InReplyTo         string                `json:"in_reply_to"`
	Subject           string                `json:"subject"`
	MessageKind       model.EmailKind       `json:"message_kind"`
	ThreadKind        model.EmailThreadKind `json:"thread_kind"`
	ContactID         *int64                `json:"contact_id"`
	ReceivedAt        time.Time             `json:"received_at"`

	From    []Address `json:"from"`
	Sender  []Address `json:"sender"`
	ReplyTo []Address `json:"reply_to"`
	To      []Address `json:"to"`
	Cc      []Address `json:"cc"`

	BodyText    string       `json:"body_text"`
	Attachments []Attachment `json:"attachments"`

	BodyTextTruncated    bool `json:"body_text_truncated"`
	FromTruncated        bool `json:"from_truncated"`
	SenderTruncated      bool `json:"sender_truncated"`
	ReplyToTruncated     bool `json:"reply_to_truncated"`
	ToTruncated          bool `json:"to_truncated"`
	CcTruncated          bool `json:"cc_truncated"`
	AttachmentsTruncated bool `json:"attachments_truncated"`
}

// InboundMessage is the email.message.inbound envelope.
type InboundMessage struct {
	EventID       uuid.UUID `json:"event_id"`
	EventType     string    `json:"event_type"`
	SchemaVersion int       `json:"schema_version"`
	OccurredAt    time.Time `json:"occurred_at"`

	DomainID  int64  `json:"domain_id"`
	ProfileID int64  `json:"profile_id"`
	FlowID    *int64 `json:"flow_id"`
	ThreadID  int64  `json:"thread_id"`
	MessageID int64  `json:"message_id"`

	Snapshot InboundMessageSnapshot `json:"snapshot"`
}

// InboundMessageSource is the stored state an event is built from.
type InboundMessageSource struct {
	// OccurredAt is when the Message became ready.
	OccurredAt  time.Time
	FlowID      *int64
	Thread      *model.EmailThread
	Message     *model.EmailMessage
	Recipients  []*model.EmailRecipient
	Attachments []*model.EmailMessageAttachment
}

// Eligible reports whether a Message starts Flow: auto-replies, bounces and
// service Threads never do (technical design 2.1.17).
func Eligible(message *model.EmailMessage, thread *model.EmailThread) bool {
	return message != nil && thread != nil &&
		message.Direction == model.EmailMessageDirectionInbound &&
		message.Kind == model.EmailKindRegular &&
		thread.Kind == model.EmailThreadKindRegular
}

// InboundMessageDedupKey identifies the one event a Message may have.
func InboundMessageDedupKey(domainID, messageID int64) string {
	return strconv.FormatInt(domainID, 10) + ":" + strconv.FormatInt(messageID, 10)
}

// NewInboundMessage builds the envelope; oversized input is truncated, never rejected.
func NewInboundMessage(eventID uuid.UUID, src InboundMessageSource, limits Limits) (InboundMessage, error) {
	message, thread := src.Message, src.Thread
	if !Eligible(message, thread) {
		return InboundMessage{}, fmt.Errorf("%w: message is not a regular inbound one of a regular thread", errInvalidEvent)
	}
	if message.DomainID != thread.DomainID || message.ThreadID != thread.ID || message.ProfileID != thread.ProfileID {
		return InboundMessage{}, fmt.Errorf("%w: message %d does not belong to thread %d", errInvalidEvent, message.ID, thread.ID)
	}

	body, bodyTruncated := clean(message.TextBody, limits.MaxBodyTextSize, false)
	snapshot := InboundMessageSnapshot{
		InternetMessageID: field(message.MessageID, limits.MaxFieldSize),
		InReplyTo:         field(message.InReplyTo, limits.MaxFieldSize),
		Subject:           field(message.Subject, limits.MaxFieldSize),
		MessageKind:       message.Kind,
		ThreadKind:        thread.Kind,
		ContactID:         copyID(thread.ContactID),
		ReceivedAt:        message.ReceivedAt.UTC(),
		BodyText:          body,
		BodyTextTruncated: bodyTruncated,
	}

	snapshot.From, snapshot.FromTruncated = addresses(src.Recipients, model.EmailRecipientTypeFrom, limits)
	snapshot.Sender, snapshot.SenderTruncated = addresses(src.Recipients, model.EmailRecipientTypeSender, limits)
	snapshot.ReplyTo, snapshot.ReplyToTruncated = addresses(src.Recipients, model.EmailRecipientTypeReplyTo, limits)
	snapshot.To, snapshot.ToTruncated = addresses(src.Recipients, model.EmailRecipientTypeTo, limits)
	snapshot.Cc, snapshot.CcTruncated = addresses(src.Recipients, model.EmailRecipientTypeCc, limits)
	snapshot.Attachments, snapshot.AttachmentsTruncated = attachments(src.Attachments, limits)

	event := InboundMessage{
		EventID:       eventID,
		EventType:     TypeInboundMessage,
		SchemaVersion: InboundMessageSchema,
		OccurredAt:    src.OccurredAt.UTC(),
		DomainID:      message.DomainID,
		ProfileID:     message.ProfileID,
		FlowID:        copyID(src.FlowID),
		ThreadID:      thread.ID,
		MessageID:     message.ID,
		Snapshot:      snapshot,
	}

	return event, event.Validate()
}

// NewInboundMessageOutboxEvent builds the pending Outbox row of a Message.
func NewInboundMessageOutboxEvent(eventID uuid.UUID, src InboundMessageSource, limits Limits) (*model.OutboxEvent, error) {
	event, err := NewInboundMessage(eventID, src, limits)
	if err != nil {
		return nil, err
	}

	payload, err := event.Marshal(limits.MaxPayloadSize)
	if err != nil {
		return nil, err
	}

	return &model.OutboxEvent{
		EventID:       event.EventID,
		DomainID:      event.DomainID,
		EventType:     TypeInboundMessage,
		SchemaVersion: InboundMessageSchema,
		DedupKey:      InboundMessageDedupKey(event.DomainID, event.MessageID),
		Exchange:      Exchange,
		RoutingKey:    InboundMessageRoutingKey,
		Payload:       payload,
	}, nil
}

// Validate reports whether the envelope is complete enough to publish.
func (e InboundMessage) Validate() error {
	s := e.Snapshot

	switch {
	case e.EventID == uuid.Nil:
		return fmt.Errorf("%w: empty event_id", errInvalidEvent)
	case e.EventType != TypeInboundMessage || e.SchemaVersion != InboundMessageSchema:
		return fmt.Errorf("%w: type %q schema %d", errInvalidEvent, e.EventType, e.SchemaVersion)
	case e.OccurredAt.IsZero() || s.ReceivedAt.IsZero():
		return fmt.Errorf("%w: zero occurred_at or received_at", errInvalidEvent)
	case e.DomainID <= 0 || e.ProfileID <= 0 || e.ThreadID <= 0 || e.MessageID <= 0:
		return fmt.Errorf("%w: non-positive id", errInvalidEvent)
	case (e.FlowID != nil && *e.FlowID <= 0) || (s.ContactID != nil && *s.ContactID <= 0):
		return fmt.Errorf("%w: non-positive flow_id or contact_id", errInvalidEvent)
	case s.MessageKind != model.EmailKindRegular || s.ThreadKind != model.EmailThreadKindRegular:
		return fmt.Errorf("%w: message_kind %q thread_kind %q", errInvalidEvent, s.MessageKind, s.ThreadKind)
	case s.InternetMessageID == "":
		return fmt.Errorf("%w: empty internet_message_id", errInvalidEvent)
	}

	return nil
}

// Marshal validates the envelope and renders its wire JSON within maxSize bytes.
func (e InboundMessage) Marshal(maxSize int) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}

	payload, err := encode(e)
	if err != nil {
		return nil, err
	}
	// Unreachable while the limits passed Validate; guards a contract change.
	if len(payload) > maxSize {
		return nil, fmt.Errorf("%w: payload of %d bytes exceeds %d", errInvalidEvent, len(payload), maxSize)
	}

	return payload, nil
}

// UnmarshalInboundMessage parses and validates a wire envelope, for tests and tooling.
func UnmarshalInboundMessage(data []byte) (InboundMessage, error) {
	var e InboundMessage
	if err := json.Unmarshal(data, &e); err != nil {
		return InboundMessage{}, fmt.Errorf("event: parse inbound message: %w", err)
	}

	return e, e.Validate()
}

// encode leaves <, > and & as they are, so no character grows more than twice.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, fmt.Errorf("event: encode: %w", err)
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func addresses(recipients []*model.EmailRecipient, kind model.EmailRecipientType, limits Limits) ([]Address, bool) {
	var matched []*model.EmailRecipient
	for _, recipient := range recipients {
		if recipient != nil && recipient.Type == kind {
			matched = append(matched, recipient)
		}
	}
	slices.SortStableFunc(matched, func(a, b *model.EmailRecipient) int { return cmp.Compare(a.Ordinal, b.Ordinal) })

	truncated := len(matched) > limits.MaxAddressesPerType
	if truncated {
		matched = matched[:limits.MaxAddressesPerType]
	}

	result := make([]Address, 0, len(matched))
	for _, recipient := range matched {
		address, addressCut := line(recipient.Address, maxAddressSize)
		name, nameCut := line(recipient.DisplayName, maxDisplayNameSize)
		truncated = truncated || addressCut || nameCut

		result = append(result, Address{Address: address, Name: name})
	}

	return result, truncated
}

// attachments keeps stored attachment parts only; flow_manager never listed inline parts.
func attachments(parts []*model.EmailMessageAttachment, limits Limits) ([]Attachment, bool) {
	var stored []*model.EmailMessageAttachment
	for _, part := range parts {
		if part != nil && part.State == model.EmailAttachmentStateStored && part.FileID != nil &&
			part.Disposition == model.EmailPartDispositionAttachment {
			stored = append(stored, part)
		}
	}
	slices.SortStableFunc(stored, func(a, b *model.EmailMessageAttachment) int { return cmp.Compare(a.Position, b.Position) })

	truncated := len(stored) > limits.MaxAttachments
	if truncated {
		stored = stored[:limits.MaxAttachments]
	}

	result := make([]Attachment, 0, len(stored))
	for _, part := range stored {
		name, nameCut := line(part.FileName, limits.MaxFieldSize)
		mime, mimeCut := line(part.MimeType, maxMimeTypeSize)
		truncated = truncated || nameCut || mimeCut

		result = append(result, Attachment{
			ID:      *part.FileID,
			Name:    name,
			Size:    part.Size,
			Mime:    mime,
			Channel: attachmentChannel,
		})
	}

	return result, truncated
}

// line cleans a header value; line breaks and other controls become spaces.
func line(value string, maxSize int) (string, bool) {
	return clean(value, maxSize, true)
}

// field cleans a header value whose cut the contract does not flag.
func field(value string, maxSize int) string {
	cleaned, _ := line(value, maxSize)

	return cleaned
}

// clean makes value valid UTF-8 without controls JSON escapes as \u00XX, then
// cuts it to maxSize bytes on a rune boundary; true means it was cut.
func clean(value string, maxSize int, singleLine bool) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(r rune) rune {
		switch {
		case r >= 0x20 || r == '\t':
			return r
		case !singleLine && (r == '\n' || r == '\r'):
			return r
		case singleLine:
			return ' '
		default:
			return -1
		}
	}, value)

	if len(value) <= maxSize {
		return value, false
	}

	cut := maxSize
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}

	return value[:cut], true
}

func copyID(id *int64) *int64 {
	if id == nil {
		return nil
	}

	value := *id

	return &value
}
