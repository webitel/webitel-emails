// Package projection applies the Email attempt snapshots of call_center to
// Threads. It runs on every instance, apart from leadership and profile assignment.
package projection

import (
	"fmt"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// Project maps a validated snapshot to the Thread state, as docs/events.md describes.
func Project(e event.AttemptSnapshot) (*store.EmailThreadProjection, error) {
	p := &store.EmailThreadProjection{
		DomainID:   e.DomainID,
		ThreadID:   e.ThreadID,
		Version:    e.ProjectionVersion,
		OccurredAt: e.OccurredAt,
		QueueID:    e.QueueID,
	}

	// The marker opens the Thread without an attempt and keeps the last handled agent.
	if e.IsMarker() {
		p.Status = model.EmailThreadStatusNew

		return p, nil
	}
	if e.AttemptState == nil {
		return nil, fmt.Errorf("projection: snapshot %d has no attempt state", e.ProjectionVersion)
	}

	switch *e.AttemptState {
	case event.AttemptStateLeaving:
		completedAt := e.OccurredAt
		p.Status = model.EmailThreadStatusProcessed
		p.CompletedAt = &completedAt
		p.HandledAgentID, p.HandledUserID = e.AgentID, e.AgentUserID
		p.ParticipantAgentID, p.ParticipantUserID = e.AgentID, e.AgentUserID

		return p, nil
	case event.AttemptStateWaiting:
		p.Status = model.EmailThreadStatusNew
	case event.AttemptStateOffering:
		p.Status = model.EmailThreadStatusDistributed
	case event.AttemptStateActive, event.AttemptStateProcessing:
		p.Status = model.EmailThreadStatusInProgress
	case event.AttemptStatePostponed:
		p.Status = model.EmailThreadStatusPostponed
		p.PostponedUntil = e.PostponedUntil
	default:
		return nil, fmt.Errorf("projection: unknown attempt state %q", *e.AttemptState)
	}

	p.AttemptID, p.AttemptVersion = e.AttemptID, e.AttemptVersion
	p.AgentID, p.UserID = e.AgentID, e.AgentUserID
	// Only a state reached by Take proves participation; an offer does not.
	switch *e.AttemptState {
	case event.AttemptStateActive, event.AttemptStatePostponed, event.AttemptStateProcessing:
		p.ParticipantAgentID, p.ParticipantUserID = e.AgentID, e.AgentUserID
	}

	return p, nil
}
