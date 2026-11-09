package event

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

func snapshot() AttemptSnapshot {
	return AttemptSnapshot{
		EventID: uuid.New(), EventType: TypeAttemptSnapshot, SchemaVersion: AttemptSnapshotSchema,
		OccurredAt: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
		DomainID:   1, ThreadID: 10, ProjectionVersion: 100, Transition: "taken", Source: "engine",
		AttemptID: ptr(int64(5)), AttemptVersion: ptr(int64(2)), AttemptState: ptr(AttemptStateActive),
		QueueID: ptr(int64(3)), AgentID: ptr(int64(7)), AgentUserID: ptr(int64(70)),
	}
}

func marker() AttemptSnapshot {
	return AttemptSnapshot{
		EventID: uuid.New(), EventType: TypeAttemptSnapshot, SchemaVersion: AttemptSnapshotSchema,
		OccurredAt: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
		DomainID:   1, ThreadID: 10, ProjectionVersion: 101, Transition: TransitionInboundReopened, Source: "system",
		CausationID: ptr(uuid.NewString()),
	}
}

func TestAttemptSnapshotValidate(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*AttemptSnapshot)
		valid bool
	}{
		{"active attempt", func(*AttemptSnapshot) {}, true},
		{"postponed attempt", func(e *AttemptSnapshot) {
			e.AttemptState, e.PostponedUntil = ptr(AttemptStatePostponed), ptr(e.OccurredAt.Add(time.Hour))
		}, true},
		{"leaving attempt", func(e *AttemptSnapshot) {
			e.Transition, e.AttemptState, e.Result = "completed", ptr(AttemptStateLeaving), ptr(AttemptResultProcessed)
		}, true},
		{"unknown transition", func(e *AttemptSnapshot) { e.Transition = "parked" }, false},
		{"unknown state", func(e *AttemptSnapshot) { e.AttemptState = ptr("parked") }, false},
		{"unknown result", func(e *AttemptSnapshot) {
			e.AttemptState, e.Result = ptr(AttemptStateLeaving), ptr("lost")
		}, false},
		{"transferred to a queue", transferred(func(e *AttemptSnapshot) {}), true},
		{"transferred to a user", transferred(func(e *AttemptSnapshot) { e.TransferToQueueID, e.TransferToUserID = nil, ptr(int64(80)) }), true},
		{"transferred without a target", transferred(func(e *AttemptSnapshot) { e.TransferToQueueID = nil }), false},
		{"transferred to both", transferred(func(e *AttemptSnapshot) { e.TransferToUserID = ptr(int64(80)) }), false},
		{"transferred not leaving", transferred(func(e *AttemptSnapshot) { e.AttemptState, e.Result = ptr(AttemptStateActive), nil }), false},
		{"transfer result elsewhere", func(e *AttemptSnapshot) {
			e.Transition, e.AttemptState, e.Result = "completed", ptr(AttemptStateLeaving), ptr(AttemptResultTransfer)
		}, false},
		{"target outside a transfer", func(e *AttemptSnapshot) { e.TransferToQueueID = ptr(int64(4)) }, false},
		{"long comment", transferred(func(e *AttemptSnapshot) { e.Comment = ptr(string(make([]rune, MaxCommentLength+1))) }), false},
		{"comment of another transition", func(e *AttemptSnapshot) { e.Comment = ptr("note") }, false},
		{"return with its reason", func(e *AttemptSnapshot) {
			e.Transition, e.AttemptState, e.AgentID, e.AgentUserID = TransitionReturned, ptr(AttemptStateWaiting), nil, nil
			e.ActorUserID, e.Comment = ptr(int64(70)), ptr("busy")
		}, true},
		{"actor of the system", func(e *AttemptSnapshot) { e.Source, e.ActorUserID = "system", ptr(int64(70)) }, false},
		{"created by a transfer", func(e *AttemptSnapshot) {
			e.Transition, e.AttemptState, e.AgentID, e.AgentUserID = TransitionTransferCreated, ptr(AttemptStateWaiting), nil, nil
		}, true},
		{"created by a transfer in work", func(e *AttemptSnapshot) { e.Transition = TransitionTransferCreated }, false},
		{"leaving without result", func(e *AttemptSnapshot) { e.AttemptState = ptr(AttemptStateLeaving) }, false},
		{"result outside leaving", func(e *AttemptSnapshot) { e.Result = ptr(AttemptResultProcessed) }, false},
		{"postponed without deadline", func(e *AttemptSnapshot) { e.AttemptState = ptr(AttemptStatePostponed) }, false},
		{"deadline outside postponed", func(e *AttemptSnapshot) { e.PostponedUntil = ptr(e.OccurredAt) }, false},
		{"waiting with agent", func(e *AttemptSnapshot) { e.AttemptState = ptr(AttemptStateWaiting) }, false},
		{"agent without user", func(e *AttemptSnapshot) { e.AgentUserID = nil }, false},
		{"missing queue", func(e *AttemptSnapshot) { e.QueueID = nil }, false},
		{"missing attempt version", func(e *AttemptSnapshot) { e.AttemptVersion = nil }, false},
		{"unknown source", func(e *AttemptSnapshot) { e.Source = "workspace" }, false},
		{"other schema", func(e *AttemptSnapshot) { e.SchemaVersion = 2 }, false},
		{"zero projection version", func(e *AttemptSnapshot) { e.ProjectionVersion = 0 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := snapshot()
			tc.edit(&e)
			if err := e.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, valid %v", err, tc.valid)
			}
		})
	}

	t.Run("marker", func(t *testing.T) {
		e := marker()
		if err := e.Validate(); err != nil {
			t.Fatalf("marker without queue: %v", err)
		}
		e.QueueID = ptr(int64(3))
		if err := e.Validate(); err != nil {
			t.Fatalf("marker with queue: %v", err)
		}
		e.AttemptID = ptr(int64(5))
		if err := e.Validate(); !errors.Is(err, errInvalidSnapshot) {
			t.Fatalf("marker with an attempt: %v", err)
		}
	})
}

// transferred is a valid transfer of the attempt to queue 4, changed by edit.
func transferred(edit func(*AttemptSnapshot)) func(*AttemptSnapshot) {
	return func(e *AttemptSnapshot) {
		e.Transition, e.AttemptState, e.Result = TransitionTransferred, ptr(AttemptStateLeaving), ptr(AttemptResultTransfer)
		e.ActorUserID, e.Comment, e.TransferToQueueID = ptr(int64(70)), ptr("to sales"), ptr(int64(4))
		edit(e)
	}
}

func TestUnmarshalAttemptSnapshot(t *testing.T) {
	e := marker()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	got, err := UnmarshalAttemptSnapshot(data, e.EventID.String())
	if err != nil || got.EventID != e.EventID || got.AttemptID != nil {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if _, err := UnmarshalAttemptSnapshot(data, ""); !errors.Is(err, errInvalidSnapshot) {
		t.Fatalf("without header: %v", err)
	}
	if _, err := UnmarshalAttemptSnapshot(data, uuid.NewString()); !errors.Is(err, errInvalidSnapshot) {
		t.Fatalf("other header: %v", err)
	}
	if _, err := UnmarshalAttemptSnapshot([]byte("{"), ""); !errors.Is(err, errInvalidSnapshot) {
		t.Fatalf("broken JSON: %v", err)
	}
}

func TestAttemptSnapshotClosedContract(t *testing.T) {
	for _, transition := range []string{
		"created", "assigned", "taken", "postponed", "resumed", "woken", "queued", "reopened",
		"deadline_woken", "deadline_queued", "returned", "auto_released", "logged_out",
		"processing_started", "renewed", "completed", "processing_timeout",
	} {
		t.Run(transition, func(t *testing.T) {
			e := snapshot()
			e.Transition = transition
			if err := e.Validate(); err != nil {
				t.Fatalf("known transition rejected: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name string
		edit func(*AttemptSnapshot)
	}{
		{"attempt version", func(e *AttemptSnapshot) { e.AttemptVersion = ptr(int64(0)) }},
		{"agent", func(e *AttemptSnapshot) { e.AgentID, e.AgentUserID = ptr(int64(7)), ptr(int64(70)) }},
		{"result", func(e *AttemptSnapshot) { e.Result = ptr(AttemptResultProcessed) }},
		{"deadline", func(e *AttemptSnapshot) { e.PostponedUntil = ptr(e.OccurredAt.Add(time.Hour)) }},
	} {
		t.Run("marker rejects "+tc.name, func(t *testing.T) {
			e := marker()
			tc.edit(&e)
			if err := e.Validate(); !errors.Is(err, errInvalidSnapshot) {
				t.Fatalf("invalid marker accepted: %v", err)
			}
		})
	}
}
