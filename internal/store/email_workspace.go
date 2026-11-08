package store

import (
	"context"
	"time"
)

// EmailWorkspaceChange is one visible change of a Thread, told to Workspace.
type EmailWorkspaceChange struct {
	DomainID  int64
	ThreadID  int64
	EventType string
	// MessageID is set for message events only.
	MessageID *int64
	// Changes name what changed, for thread events only.
	Changes []string
	// PreviousUserID is the current user before the change, when it may have moved.
	PreviousUserID *int64
	OccurredAt     time.Time
}

// EmailWorkspaceEventStore records Workspace changes in the caller's transaction.
type EmailWorkspaceEventStore interface {
	// Record raises the workspace version of a regular Thread and, when the events
	// are enabled and the Thread is readable, writes one event per recipient. It is
	// called only for a real change; it returns the events written.
	Record(ctx context.Context, change EmailWorkspaceChange) (int, error)
}
