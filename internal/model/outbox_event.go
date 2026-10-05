package model

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// OutboxEventState is the publication state of one Outbox row.
type OutboxEventState string

// Outbox publication states.
const (
	OutboxEventStatePending    OutboxEventState = "pending"
	OutboxEventStateProcessing OutboxEventState = "processing"
	OutboxEventStatePublished  OutboxEventState = "published"
	OutboxEventStateFailed     OutboxEventState = "failed"
)

// OutboxEvent is a broker message stored with the change it describes.
// Payload is the validated wire JSON; the relay never decodes it.
type OutboxEvent struct {
	EventID   uuid.UUID
	DomainID  int64
	EventType string
	// SchemaVersion of the payload, published as a header.
	SchemaVersion int32
	// DedupKey makes a repeated write of one business event a no-op; empty disables it.
	DedupKey string

	Exchange   string
	RoutingKey string
	Payload    []byte

	State         OutboxEventState
	Attempts      int32
	NextAttemptAt time.Time
	LockedBy      string
	LockedUntil   *time.Time
	PublishedAt   *time.Time
	LastError     string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate rejects a row the relay could not publish, before it commits with
// the business change.
func (e *OutboxEvent) Validate() error {
	switch {
	case e == nil:
		return errors.New("outbox event is required")
	case e.EventID == uuid.Nil || e.DomainID <= 0 || e.SchemaVersion < 1:
		return errors.New("outbox event needs an event id, a domain and a schema version")
	case e.EventType == "" || e.Exchange == "" || e.RoutingKey == "":
		return errors.New("outbox event needs a type, an exchange and a routing key")
	case len(e.Payload) == 0:
		return errors.New("outbox event needs a payload")
	}

	return nil
}
