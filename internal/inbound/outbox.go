package inbound

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// EventWriter records the Flow event of a Message in the transaction that makes
// it ready, so neither can commit without the other.
type EventWriter struct {
	limits event.Limits
	now    func() time.Time
	newID  func() (uuid.UUID, error)
	log    *slog.Logger
}

// NewEventWriter creates the writer with the snapshot limits of the service.
func NewEventWriter(cfg *config.Config, log *slog.Logger) *EventWriter {
	return &EventWriter{
		limits: cfg.InboundEvent.Limits(),
		now:    time.Now,
		newID:  uuid.NewV7,
		log:    log.With("component", "inbound_event"),
	}
}

// Write builds the event from the stored state. A Message that never starts Flow
// writes nothing; any error rolls the caller's transaction back.
func (w *EventWriter) Write(ctx context.Context, uow store.UnitOfWork, domainID, messageID int64) error {
	message, err := uow.EmailMessageStore().Get(ctx, domainID, messageID)
	if err != nil {
		return err
	}

	thread, err := uow.EmailThreadStore().Get(ctx, domainID, message.ThreadID)
	if err != nil {
		return err
	}
	if !event.Eligible(message, thread) {
		return nil
	}
	if message.State != model.EmailMessageStateReady {
		return fmt.Errorf("inbound event: message %d is %s, not ready", message.ID, message.State)
	}

	recipients, err := uow.EmailRecipientStore().List(ctx, domainID, message.ID)
	if err != nil {
		return err
	}

	attachments, err := uow.EmailMessageAttachmentStore().List(ctx, domainID, message.ID)
	if err != nil {
		return err
	}

	flowID, err := uow.EmailProfileRoutingStore().FlowID(ctx, domainID, message.ProfileID)
	if err != nil {
		return err
	}

	eventID, err := w.newID()
	if err != nil {
		return fmt.Errorf("inbound event: generate id: %w", err)
	}

	row, err := event.NewInboundMessageOutboxEvent(eventID, event.InboundMessageSource{
		OccurredAt:  w.now(),
		FlowID:      flowID,
		Thread:      thread,
		Message:     message,
		Recipients:  recipients,
		Attachments: attachments,
	}, w.limits)
	if err != nil {
		return err
	}

	inserted, err := uow.OutboxStore().Append(ctx, row)
	if err != nil {
		return err
	}

	log := w.log.With(
		"domain_id", domainID,
		"profile_id", message.ProfileID,
		"thread_id", thread.ID,
		"message_id", message.ID,
	)
	if flowID != nil {
		log = log.With("flow_id", *flowID)
	}
	// A concurrent or repeated completion of the same Message already wrote it.
	if !inserted {
		log.Debug("inbound event already recorded")

		return nil
	}

	log.Info("inbound event recorded", "event_id", eventID)

	return nil
}
