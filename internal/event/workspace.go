package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/webitel/webitel-emails/internal/model"
)

// Contract of the events that tell Workspace, through engine, that a Thread changed.
// Adding a field keeps the schema version; changing or removing one bumps it.
const (
	TypeWorkspaceThreadChanged   = "email.workspace.thread.changed"
	TypeWorkspaceMessageCreated  = "email.workspace.message.created"
	TypeWorkspaceMessageDelivery = "email.workspace.message.delivery"
	WorkspaceSchema              = 1

	// MaxWorkspacePayloadSize bounds one event; it holds ids and versions only.
	MaxWorkspacePayloadSize = 4 << 10
)

// Values of WorkspaceEvent.Access, by the read API rules.
const (
	WorkspaceAccessCurrent = "current"
	WorkspaceAccessHistory = "history"
	WorkspaceAccessNone    = "none"
)

// Values of WorkspaceEvent.Changes.
const (
	WorkspaceChangeProjection   = "projection"
	WorkspaceChangeActions      = "actions"
	WorkspaceChangeParticipants = "participants"
	WorkspaceChangeContact      = "contact"
)

// workspaceNamespace is the UUIDv5 namespace of the Workspace event ids.
var workspaceNamespace = uuid.MustParse("38ad4b08-3ead-470a-ba68-4b72ffd03723")

var errInvalidWorkspaceEvent = errors.New("event: invalid workspace event")

// WorkspaceRoutingKey names the recipient, as the keys of calls and chats do: the
// queue of every engine node with a socket of the user is bound to it.
func WorkspaceRoutingKey(eventType string, domainID, userID int64) string {
	return eventType + ".v1." + strconv.FormatInt(domainID, 10) + "." + strconv.FormatInt(userID, 10)
}

// IsWorkspaceEvent tells a Workspace event by its type, exchange and routing key.
func IsWorkspaceEvent(e *model.OutboxEvent) bool {
	switch e.EventType {
	case TypeWorkspaceThreadChanged, TypeWorkspaceMessageCreated, TypeWorkspaceMessageDelivery:
		return e.Exchange == Exchange && strings.HasPrefix(e.RoutingKey, e.EventType+".v1.")
	}

	return false
}

// WorkspaceEventID is the UUIDv5 of the event type, domain, Thread, version and recipient.
func WorkspaceEventID(eventType string, domainID, threadID, workspaceVersion, userID int64) uuid.UUID {
	name := eventType + ":" + strconv.FormatInt(domainID, 10) + ":" + strconv.FormatInt(threadID, 10) + ":" +
		strconv.FormatInt(workspaceVersion, 10) + ":" + strconv.FormatInt(userID, 10)

	return uuid.NewSHA1(workspaceNamespace, []byte(name))
}

// WorkspaceEvent is the envelope for one recipient; every field is sent, a missing value as null.
type WorkspaceEvent struct {
	EventID           uuid.UUID `json:"event_id"`
	EventType         string    `json:"event_type"`
	SchemaVersion     int       `json:"schema_version"`
	OccurredAt        time.Time `json:"occurred_at"`
	DomainID          int64     `json:"domain_id"`
	UserID            int64     `json:"user_id"`
	ThreadID          int64     `json:"thread_id"`
	Access            string    `json:"access"`
	WorkspaceVersion  int64     `json:"workspace_version"`
	ProjectionVersion *int64    `json:"projection_version"`
	MessageID         *int64    `json:"message_id"`
	Changes           []string  `json:"changes"`
}

// Validate reports whether the envelope follows the contract.
func (e *WorkspaceEvent) Validate() error {
	switch {
	case e.EventType != TypeWorkspaceThreadChanged && e.EventType != TypeWorkspaceMessageCreated &&
		e.EventType != TypeWorkspaceMessageDelivery:
		return fmt.Errorf("%w: unknown type %q", errInvalidWorkspaceEvent, e.EventType)
	case e.DomainID <= 0 || e.UserID <= 0 || e.ThreadID <= 0 || e.WorkspaceVersion <= 0:
		return fmt.Errorf("%w: ids and the version must be positive", errInvalidWorkspaceEvent)
	case e.ProjectionVersion != nil && *e.ProjectionVersion <= 0:
		return fmt.Errorf("%w: projection version must be positive", errInvalidWorkspaceEvent)
	case e.Access != WorkspaceAccessCurrent && e.Access != WorkspaceAccessHistory && e.Access != WorkspaceAccessNone:
		return fmt.Errorf("%w: unknown access %q", errInvalidWorkspaceEvent, e.Access)
	case (e.EventType == TypeWorkspaceThreadChanged) == (e.MessageID != nil):
		return fmt.Errorf("%w: a message id belongs to message events only", errInvalidWorkspaceEvent)
	case e.MessageID != nil && *e.MessageID <= 0:
		return fmt.Errorf("%w: message id must be positive", errInvalidWorkspaceEvent)
	case e.Access == WorkspaceAccessNone && (e.EventType != TypeWorkspaceThreadChanged || len(e.Changes) > 0):
		return fmt.Errorf("%w: access none is a bare thread invalidation", errInvalidWorkspaceEvent)
	case e.EventType != TypeWorkspaceThreadChanged && len(e.Changes) > 0:
		return fmt.Errorf("%w: changes belong to thread events only", errInvalidWorkspaceEvent)
	}

	for _, c := range e.Changes {
		switch c {
		case WorkspaceChangeProjection, WorkspaceChangeActions, WorkspaceChangeParticipants, WorkspaceChangeContact:
		default:
			return fmt.Errorf("%w: unknown change %q", errInvalidWorkspaceEvent, c)
		}
	}

	return nil
}

// NewWorkspaceOutboxEvent sets the id and the schema and wraps the envelope as an Outbox row.
func NewWorkspaceOutboxEvent(e WorkspaceEvent) (*model.OutboxEvent, error) {
	e.EventID = WorkspaceEventID(e.EventType, e.DomainID, e.ThreadID, e.WorkspaceVersion, e.UserID)
	e.SchemaVersion = WorkspaceSchema
	e.OccurredAt = e.OccurredAt.UTC()
	if e.Changes == nil {
		e.Changes = []string{}
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(&e)
	if err != nil {
		return nil, fmt.Errorf("event: encode workspace event: %w", err)
	}
	if len(payload) > MaxWorkspacePayloadSize {
		return nil, fmt.Errorf("%w: payload of %d bytes", errInvalidWorkspaceEvent, len(payload))
	}

	return &model.OutboxEvent{
		EventID:       e.EventID,
		DomainID:      e.DomainID,
		EventType:     e.EventType,
		SchemaVersion: WorkspaceSchema,
		DedupKey:      e.EventID.String(),
		Exchange:      Exchange,
		RoutingKey:    WorkspaceRoutingKey(e.EventType, e.DomainID, e.UserID),
		Payload:       payload,
	}, nil
}
