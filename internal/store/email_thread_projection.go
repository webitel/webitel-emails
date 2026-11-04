package store

import (
	"context"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
)

// ErrEmailThreadProjectionRejected means the database refused the projected
// values, so applying the snapshot again can never succeed.
var ErrEmailThreadProjectionRejected = kiterrors.InvalidArgument(
	"attempt snapshot cannot be stored on the thread",
	kiterrors.WithID("store.email_thread_projection.rejected"),
)

// EmailThreadProjection is the state one attempt snapshot gives a Thread. Nil
// fields are stored as NULL, except QueueID, whose nil keeps the stored queue.
type EmailThreadProjection struct {
	DomainID   int64
	ThreadID   int64
	Version    int64
	OccurredAt time.Time

	Status         model.EmailThreadStatus
	QueueID        *int64
	AttemptID      *int64
	AttemptVersion *int64
	AgentID        *int64
	UserID         *int64
	PostponedUntil *time.Time
	CompletedAt    *time.Time

	// HandledAgentID and HandledUserID are set only by a finished attempt with an agent.
	HandledAgentID *int64
	HandledUserID  *int64

	// ParticipantAgentID and ParticipantUserID are set by a snapshot that proves Take
	// (active, postponed, processing or finished with an agent), never by an offer.
	ParticipantAgentID *int64
	ParticipantUserID  *int64

	// Action is the journal row of the snapshot, written whether the Thread changes or not.
	Action EmailThreadAction
}

// EmailThreadAction is one transition of a Thread as its action journal keeps it.
type EmailThreadAction struct {
	Transition        string
	Source            string
	AttemptID         *int64
	AttemptState      *string
	Result            *string
	QueueID           *int64
	AgentID           *int64
	AgentUserID       *int64
	ActorUserID       *int64
	TransferToQueueID *int64
	TransferToUserID  *int64
	Comment           *string
}

// EmailThreadProjectionOutcome tells what applying a snapshot did.
type EmailThreadProjectionOutcome string

// Outcomes of applying a snapshot.
const (
	EmailThreadProjectionApplied EmailThreadProjectionOutcome = "applied"
	// EmailThreadProjectionStale means the Thread already holds this or a newer snapshot.
	EmailThreadProjectionStale EmailThreadProjectionOutcome = "stale"
	// EmailThreadProjectionNotFound covers a missing Thread and one of another domain.
	EmailThreadProjectionNotFound EmailThreadProjectionOutcome = "not_found"
	// EmailThreadProjectionService means the Thread is a service one and is never projected.
	EmailThreadProjectionService EmailThreadProjectionOutcome = "service"
)

// EmailThreadProjectionResult is the outcome of one snapshot.
type EmailThreadProjectionResult struct {
	Outcome EmailThreadProjectionOutcome
	// HandledApplied reports that the last handled agent changed, even for a stale snapshot.
	HandledApplied bool
	// ParticipantRecorded reports that the participant journal gained or refined
	// a row, even for a stale snapshot.
	ParticipantRecorded bool
	// ActionRecorded reports that the action journal gained the row of this snapshot.
	ActionRecorded bool
}

// EmailThreadProjectionStore applies attempt snapshots to Threads. Only a
// snapshot newer than the stored one changes the Thread, so the order of
// deliveries never matters.
type EmailThreadProjectionStore interface {
	Apply(ctx context.Context, projection *EmailThreadProjection) (EmailThreadProjectionResult, error)
}

// EmailThreadActionCleaner deletes old rows of the action journal.
type EmailThreadActionCleaner interface {
	// DeleteActionsBefore removes up to limit actions that occurred before the time.
	DeleteActionsBefore(ctx context.Context, before time.Time, limit int) (int64, error)
}
