package inbound

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// QuarantineHandler records an email that can never be processed as it is and
// lets the cursor move past it, so one broken message cannot block a mailbox.
// The message itself is left in the mailbox and is not copied here.
type QuarantineHandler struct {
	next     Handler
	failures store.InboundFailureStore
	log      *slog.Logger
}

var _ Handler = (*QuarantineHandler)(nil)

// NewQuarantineHandler puts quarantine in front of the rest of the pipeline.
func NewQuarantineHandler(next *MIMEHandler, failures store.InboundFailureStore, log *slog.Logger) *QuarantineHandler {
	return &QuarantineHandler{
		next:     next,
		failures: failures,
		log:      log.With("component", "inbound_quarantine"),
	}
}

func (h *QuarantineHandler) Handle(ctx context.Context, message *Message) error {
	if message == nil {
		return fmt.Errorf("quarantine: message is required")
	}

	// An oversized email never reaches the parser: only its prefix was fetched.
	if message.TooLarge {
		return h.quarantine(ctx, message, model.InboundFailureMessageTooLarge,
			fmt.Sprintf("message size %d exceeds the configured limit", message.Size))
	}

	err := h.next.Handle(ctx, message)

	var permanent *PermanentError
	if err == nil || !stderrors.As(err, &permanent) {
		return err
	}
	// Cancellation and deadlines are never permanent, whatever wrapped them.
	if ctx.Err() != nil {
		return err
	}

	// The parser error can quote a malformed header, so only a fixed reason is
	// stored and logged; the email itself stays in the mailbox for diagnosis.
	return h.quarantine(ctx, message, permanent.Category, categoryReason(permanent.Category))
}

// categoryReason is the stable text stored for a category, so nothing that a
// sender controls reaches the database or the log.
func categoryReason(category model.InboundFailureCategory) string {
	switch category {
	case model.InboundFailureMIMEParse:
		return "failed to parse MIME message"
	case model.InboundFailureAttachmentUpload:
		return "failed to store attachments"
	default:
		return string(category)
	}
}

// quarantine returns nil once the record is committed, which is what allows the
// cursor to pass the email. A failed write keeps the email for the next poll.
func (h *QuarantineHandler) quarantine(
	ctx context.Context,
	message *Message,
	category model.InboundFailureCategory,
	reason string,
) error {
	failure := &model.InboundFailure{
		DomainID:    message.DomainID,
		ProfileID:   message.ProfileID,
		Mailbox:     message.Mailbox,
		UIDValidity: message.UIDValidity,
		UID:         message.UID,
		Category:    category,
		Reason:      reason,
		Size:        message.Size,
	}
	failure.MessageID, failure.Sender, failure.Subject = headerHints(message.Raw)

	if err := h.failures.Record(ctx, failure); err != nil {
		return err
	}

	h.log.Warn("inbound email quarantined",
		"profile_id", message.ProfileID,
		"mailbox", message.Mailbox,
		"uid", message.UID,
		"category", category,
		"reason", reason,
	)

	return nil
}

// headerHints reads what it can from the headers so the record identifies the
// email in the mailbox. A truncated or broken message simply yields less.
func headerHints(raw []byte) (messageID, sender, subject string) {
	entity, err := gomessage.ReadWithOptions(bytes.NewReader(raw), &gomessage.ReadOptions{MaxHeaderBytes: -1})
	if entity == nil {
		return "", "", ""
	}
	_ = err

	header := mail.Header{Header: entity.Header}
	if value, err := header.MessageID(); err == nil {
		messageID = truncateRunes(cleanMessageID(value), maxHeaderTextRunes)
	}
	if addresses := parseAddressList(&header, "From"); len(addresses) > 0 {
		sender = truncateRunes(cleanHeaderText(addresses[0].Address), maxHeaderTextRunes)
	}
	if value, err := header.Subject(); err == nil {
		subject = truncateRunes(cleanHeaderText(value), maxHeaderTextRunes)
	}

	return messageID, sender, subject
}
