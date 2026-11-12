package event

import (
	"encoding/json"
	"testing"
	"time"
)

// The ids are part of the contract in docs/events.md.
func TestWorkspaceEventIDExamples(t *testing.T) {
	for _, c := range []struct {
		eventType string
		version   int64
		want      string
	}{
		{TypeWorkspaceThreadChanged, 17, "5b247aa1-9433-53ce-b37d-346cec14bff5"},
		{TypeWorkspaceMessageCreated, 19, "2c68a74d-7da5-58dd-b6c0-7a9de072241b"},
	} {
		if got := WorkspaceEventID(c.eventType, 1, 1024, c.version, 42).String(); got != c.want {
			t.Errorf("%s: id = %s, want %s", c.eventType, got, c.want)
		}
	}
}

func TestWorkspaceOutboxEvent(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	row, err := NewWorkspaceOutboxEvent(WorkspaceEvent{
		EventType: TypeWorkspaceThreadChanged, OccurredAt: at, DomainID: 1, UserID: 42, ThreadID: 1024,
		Access: WorkspaceAccessNone, WorkspaceVersion: 17, ProjectionVersion: ptr(int64(905)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.RoutingKey != "email.workspace.thread.changed.v1.1.42" || row.DedupKey != row.EventID.String() ||
		!IsWorkspaceEvent(row) {
		t.Fatalf("row = %+v", row)
	}

	var got map[string]any
	if err := json.Unmarshal(row.Payload, &got); err != nil {
		t.Fatal(err)
	}
	want := `{"access":"none","changes":[],"domain_id":1,"event_id":"5b247aa1-9433-53ce-b37d-346cec14bff5",` +
		`"event_type":"email.workspace.thread.changed","message_id":null,"occurred_at":"2026-11-06T09:00:00Z",` +
		`"projection_version":905,"schema_version":1,"thread_id":1024,"user_id":42,"workspace_version":17}`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Fatalf("payload = %s", b)
	}

	for name, e := range map[string]WorkspaceEvent{
		"none with changes": {EventType: TypeWorkspaceThreadChanged, Access: WorkspaceAccessNone,
			Changes: []string{WorkspaceChangeActions}},
		"message without id": {EventType: TypeWorkspaceMessageCreated, Access: WorkspaceAccessCurrent},
		"thread with message": {EventType: TypeWorkspaceThreadChanged, Access: WorkspaceAccessCurrent,
			MessageID: ptr(int64(5))},
		"unknown change": {EventType: TypeWorkspaceThreadChanged, Access: WorkspaceAccessCurrent,
			Changes: []string{"status"}},
		"zero version": {EventType: TypeWorkspaceThreadChanged, Access: WorkspaceAccessCurrent},
	} {
		e.DomainID, e.UserID, e.ThreadID = 1, 42, 1024
		if name != "zero version" {
			e.WorkspaceVersion = 3
		}
		if _, err := NewWorkspaceOutboxEvent(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
