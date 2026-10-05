package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
)

// ErrOutboxNeedsTransaction means an event was written outside the transaction of
// the change it describes, which would let one commit without the other.
var ErrOutboxNeedsTransaction = kiterrors.Internal(
	"an outbox event needs the transaction of the change it describes",
	kiterrors.WithID("store.outbox.no_transaction"),
)

// OutboxStore writes broker events in the caller's transaction.
type OutboxStore interface {
	// Append stores a pending event. False means an event with the same type and
	// dedup key is already stored, which is an outcome rather than a failure.
	Append(ctx context.Context, event *model.OutboxEvent) (bool, error)
}

// OutboxBacklog is what the relay has not delivered yet.
type OutboxBacklog struct {
	Pending    int64
	Processing int64
	Failed     int64
	// OldestUndelivered is the age of the oldest pending or processing row.
	OldestUndelivered time.Duration
}

// OutboxRelayStore moves Outbox rows through publication. Every write to a held
// row is conditional on the lease of owner, so a relay never overwrites another.
type OutboxRelayStore interface {
	// RecoverLeases returns expired leases to pending; own also returns every
	// lease of owner, which is safe only before owner claims anything.
	RecoverLeases(ctx context.Context, owner string, own bool) (int64, error)
	// Claim leases up to limit due rows to owner in publication order.
	Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]*model.OutboxEvent, error)
	// MarkPublished completes a row; false means owner no longer holds it.
	MarkPublished(ctx context.Context, owner string, eventID uuid.UUID) (bool, error)
	// Release returns rows to pending without spending an attempt.
	Release(ctx context.Context, owner string, eventIDs []uuid.UUID, nextAttemptAt time.Time) error
	// MarkFailed gives up on a row that can never be published; false means owner
	// no longer holds it.
	MarkFailed(ctx context.Context, owner string, eventID uuid.UUID, reason string) (bool, error)
	// DeletePublishedBefore removes up to limit rows published before the time.
	DeletePublishedBefore(ctx context.Context, before time.Time, limit int) (int64, error)
	// Backlog reports the undelivered and failed rows.
	Backlog(ctx context.Context) (OutboxBacklog, error)
}
