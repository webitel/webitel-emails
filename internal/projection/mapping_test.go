package projection

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
)

func ptr[T any](v T) *T { return &v }

func snapshot(state string) event.AttemptSnapshot {
	e := event.AttemptSnapshot{
		EventID: uuid.New(), EventType: event.TypeAttemptSnapshot, SchemaVersion: event.AttemptSnapshotSchema,
		OccurredAt: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
		DomainID:   1, ThreadID: 10, ProjectionVersion: 100, Transition: "taken", Source: "engine",
		AttemptID: ptr(int64(5)), AttemptVersion: ptr(int64(2)), AttemptState: ptr(state),
		QueueID: ptr(int64(3)), AgentID: ptr(int64(7)), AgentUserID: ptr(int64(70)),
	}
	switch state {
	case event.AttemptStateWaiting:
		e.AgentID, e.AgentUserID = nil, nil
	case event.AttemptStatePostponed:
		e.PostponedUntil = ptr(e.OccurredAt.Add(time.Hour))
	case event.AttemptStateLeaving:
		e.Result = ptr(event.AttemptResultProcessed)
	}

	return e
}

func TestProject(t *testing.T) {
	statuses := map[string]model.EmailThreadStatus{
		event.AttemptStateWaiting:    model.EmailThreadStatusNew,
		event.AttemptStateOffering:   model.EmailThreadStatusDistributed,
		event.AttemptStateActive:     model.EmailThreadStatusInProgress,
		event.AttemptStatePostponed:  model.EmailThreadStatusPostponed,
		event.AttemptStateProcessing: model.EmailThreadStatusInProgress,
	}
	for state, status := range statuses {
		e := snapshot(state)
		if err := e.Validate(); err != nil {
			t.Fatalf("%s: invalid fixture: %v", state, err)
		}
		p, err := Project(e)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if p.Status != status || p.AttemptID == nil || p.CompletedAt != nil || p.HandledAgentID != nil ||
			(p.PostponedUntil != nil) != (state == event.AttemptStatePostponed) {
			t.Errorf("%s: projection %+v", state, p)
		}
	}

	t.Run("leaving closes the thread and records the agent", func(t *testing.T) {
		e := snapshot(event.AttemptStateLeaving)
		p, err := Project(e)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != model.EmailThreadStatusProcessed || p.CompletedAt == nil || !p.CompletedAt.Equal(e.OccurredAt) ||
			p.AttemptID != nil || p.AgentID != nil || *p.HandledAgentID != 7 || *p.HandledUserID != 70 || *p.QueueID != 3 {
			t.Errorf("projection %+v", p)
		}
	})

	t.Run("processing timeout also closes the thread", func(t *testing.T) {
		e := snapshot(event.AttemptStateLeaving)
		e.Transition, e.Result = "processing_timeout", ptr(event.AttemptResultProcessingTimeout)
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		p, err := Project(e)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != model.EmailThreadStatusProcessed || p.CompletedAt == nil || p.AgentID != nil || p.AttemptID != nil ||
			p.HandledAgentID == nil || *p.HandledAgentID != 7 {
			t.Fatalf("projection %+v", p)
		}
	})

	t.Run("the marker opens the thread without an attempt", func(t *testing.T) {
		e := snapshot(event.AttemptStateWaiting)
		e.Transition, e.Source, e.AttemptID, e.AttemptVersion, e.AttemptState, e.QueueID =
			event.TransitionInboundReopened, "system", nil, nil, nil, nil
		if err := e.Validate(); err != nil {
			t.Fatalf("invalid fixture: %v", err)
		}
		p, err := Project(e)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != model.EmailThreadStatusNew || p.QueueID != nil || p.AttemptID != nil || p.HandledAgentID != nil {
			t.Errorf("projection %+v", p)
		}
	})
}

func TestProjectParticipant(t *testing.T) {
	proves := map[string]bool{
		event.AttemptStateWaiting:    false,
		event.AttemptStateOffering:   false,
		event.AttemptStateActive:     true,
		event.AttemptStatePostponed:  true,
		event.AttemptStateProcessing: true,
		event.AttemptStateLeaving:    true,
	}
	for state, want := range proves {
		e := snapshot(state)
		if err := e.Validate(); err != nil {
			t.Fatalf("%s: invalid fixture: %v", state, err)
		}
		p, err := Project(e)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		got := p.ParticipantUserID != nil && p.ParticipantAgentID != nil
		if got != want {
			t.Errorf("%s: participant recorded = %v, want %v", state, got, want)
		}
		if got && (*p.ParticipantUserID != 70 || *p.ParticipantAgentID != 7) {
			t.Errorf("%s: participant %d/%d", state, *p.ParticipantAgentID, *p.ParticipantUserID)
		}
	}

	t.Run("the marker records no participant", func(t *testing.T) {
		e := snapshot(event.AttemptStateWaiting)
		e.Transition, e.Source, e.AttemptID, e.AttemptVersion, e.AttemptState, e.QueueID =
			event.TransitionInboundReopened, "system", nil, nil, nil, nil
		p, err := Project(e)
		if err != nil {
			t.Fatal(err)
		}
		if p.ParticipantUserID != nil || p.ParticipantAgentID != nil {
			t.Errorf("projection %+v", p)
		}
	})
}
