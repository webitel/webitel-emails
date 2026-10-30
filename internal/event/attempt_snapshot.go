package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Contract of the Email attempt snapshot call_center publishes after every
// transition. Adding a field keeps the schema version; changing or removing one bumps it.
const (
	TypeAttemptSnapshot       = "cc.email.attempt.snapshot"
	AttemptSnapshotSchema     = 1
	AttemptSnapshotRoutingKey = "cc.email.attempt.snapshot.v1"
)

// TransitionInboundReopened is the marker of a new email in a Thread without an
// active attempt; it carries no attempt.
const TransitionInboundReopened = "inbound_reopened"

// Attempt states of call_center.
const (
	AttemptStateWaiting    = "waiting"
	AttemptStateOffering   = "offering"
	AttemptStateActive     = "active"
	AttemptStatePostponed  = "postponed"
	AttemptStateProcessing = "processing"
	AttemptStateLeaving    = "leaving"
)

// Results of a leaving attempt.
const (
	AttemptResultProcessed         = "processed"
	AttemptResultProcessingTimeout = "processing_timeout"
)

var errInvalidSnapshot = errors.New("event: invalid attempt snapshot")

var (
	attemptTransitions = set("created", "assigned", "taken", "postponed", "resumed", "woken", "queued", "reopened",
		"deadline_woken", "deadline_queued", "returned", "auto_released", "logged_out", "processing_started",
		"renewed", "completed", "processing_timeout")
	attemptStates = set(AttemptStateWaiting, AttemptStateOffering, AttemptStateActive, AttemptStatePostponed,
		AttemptStateProcessing, AttemptStateLeaving)
	attemptResults = set(AttemptResultProcessed, AttemptResultProcessingTimeout)
	attemptSources = set("engine", "flow", "system")
)

// AttemptSnapshot is the cc.email.attempt.snapshot envelope: the whole attempt
// after a transition. Attempt fields are null only in the inbound_reopened marker.
type AttemptSnapshot struct {
	EventID       uuid.UUID `json:"event_id"`
	EventType     string    `json:"event_type"`
	SchemaVersion int       `json:"schema_version"`
	OccurredAt    time.Time `json:"occurred_at"`

	DomainID          int64   `json:"domain_id"`
	ThreadID          int64   `json:"thread_id"`
	ProjectionVersion int64   `json:"projection_version"`
	Transition        string  `json:"transition"`
	Source            string  `json:"source"`
	CausationID       *string `json:"causation_id"`

	AttemptID      *int64     `json:"attempt_id"`
	AttemptVersion *int64     `json:"attempt_version"`
	AttemptState   *string    `json:"attempt_state"`
	Result         *string    `json:"result"`
	QueueID        *int64     `json:"queue_id"`
	AgentID        *int64     `json:"agent_id"`
	AgentUserID    *int64     `json:"agent_user_id"`
	PostponedUntil *time.Time `json:"postponed_until"`
}

// IsMarker reports whether the snapshot opens a Thread without an attempt.
func (e AttemptSnapshot) IsMarker() bool {
	return e.Transition == TransitionInboundReopened
}

// Validate rejects an envelope the projection cannot map; unknown values are never guessed.
func (e AttemptSnapshot) Validate() error {
	switch {
	case e.EventID == uuid.Nil:
		return fmt.Errorf("%w: empty event_id", errInvalidSnapshot)
	case e.EventType != TypeAttemptSnapshot || e.SchemaVersion != AttemptSnapshotSchema:
		return fmt.Errorf("%w: type %q schema %d", errInvalidSnapshot, e.EventType, e.SchemaVersion)
	case e.OccurredAt.IsZero():
		return fmt.Errorf("%w: zero occurred_at", errInvalidSnapshot)
	case e.DomainID <= 0 || e.ThreadID <= 0 || e.ProjectionVersion <= 0:
		return fmt.Errorf("%w: non-positive domain_id, thread_id or projection_version", errInvalidSnapshot)
	case !attemptSources[e.Source]:
		return fmt.Errorf("%w: unknown source %q", errInvalidSnapshot, e.Source)
	case e.QueueID != nil && *e.QueueID <= 0:
		return fmt.Errorf("%w: non-positive queue_id", errInvalidSnapshot)
	case (e.AgentID == nil) != (e.AgentUserID == nil):
		return fmt.Errorf("%w: agent_id and agent_user_id must be set together", errInvalidSnapshot)
	case e.AgentID != nil && (*e.AgentID <= 0 || *e.AgentUserID <= 0):
		return fmt.Errorf("%w: non-positive agent_id or agent_user_id", errInvalidSnapshot)
	}

	if e.IsMarker() {
		if e.AttemptID != nil || e.AttemptVersion != nil || e.AttemptState != nil || e.Result != nil ||
			e.AgentID != nil || e.PostponedUntil != nil {
			return fmt.Errorf("%w: %s carries an attempt", errInvalidSnapshot, TransitionInboundReopened)
		}

		return nil
	}

	switch {
	case !attemptTransitions[e.Transition]:
		return fmt.Errorf("%w: unknown transition %q", errInvalidSnapshot, e.Transition)
	case e.AttemptID == nil || *e.AttemptID <= 0 || e.QueueID == nil:
		return fmt.Errorf("%w: missing attempt_id or queue_id", errInvalidSnapshot)
	case e.AttemptVersion == nil || *e.AttemptVersion < 0:
		return fmt.Errorf("%w: missing or negative attempt_version", errInvalidSnapshot)
	case e.AttemptState == nil || !attemptStates[*e.AttemptState]:
		return fmt.Errorf("%w: unknown attempt_state", errInvalidSnapshot)
	}

	state := *e.AttemptState
	switch {
	case (state == AttemptStateLeaving) != (e.Result != nil):
		return fmt.Errorf("%w: result must be set only for %s", errInvalidSnapshot, AttemptStateLeaving)
	case e.Result != nil && !attemptResults[*e.Result]:
		return fmt.Errorf("%w: unknown result %q", errInvalidSnapshot, *e.Result)
	case (state == AttemptStatePostponed) != (e.PostponedUntil != nil):
		return fmt.Errorf("%w: postponed_until must be set only for %s", errInvalidSnapshot, AttemptStatePostponed)
	case state == AttemptStateWaiting && e.AgentID != nil:
		return fmt.Errorf("%w: %s attempt has an agent", errInvalidSnapshot, AttemptStateWaiting)
	}

	return nil
}

// UnmarshalAttemptSnapshot parses and validates a delivery; the x-event-id
// header is required and must repeat event_id.
func UnmarshalAttemptSnapshot(data []byte, headerEventID string) (AttemptSnapshot, error) {
	var e AttemptSnapshot
	if err := json.Unmarshal(data, &e); err != nil {
		return AttemptSnapshot{}, fmt.Errorf("%w: parse: %w", errInvalidSnapshot, err)
	}
	if headerEventID != e.EventID.String() {
		return AttemptSnapshot{}, fmt.Errorf("%w: header %s %q differs from event_id %q",
			errInvalidSnapshot, HeaderEventID, headerEventID, e.EventID)
	}

	return e, e.Validate()
}

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}

	return m
}
