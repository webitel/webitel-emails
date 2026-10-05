// Package outbox publishes the events stored in email.outbox to RabbitMQ. It runs
// only on the leader, so one instance at a time drains the table.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/webitel/webitel-go-kit/infra/pubsub/rabbitmq"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const (
	// backlogInterval is how often the leader reports what it has not delivered.
	backlogInterval = 30 * time.Second
	// releaseTimeout bounds giving rows back once the term is already over.
	releaseTimeout = 5 * time.Second
)

// Publisher is the broker side of the relay.
type Publisher interface {
	DeclareTopology(ctx context.Context) error
	Publish(ctx context.Context, event *model.OutboxEvent) error
}

// Relay is the leader task that moves pending Outbox rows to RabbitMQ.
type Relay struct {
	store          store.OutboxRelayStore
	broker         Publisher
	owner          string
	cfg            config.OutboxConfig
	maxPayloadSize int
	log            *slog.Logger
	terms          atomic.Int64
}

// New creates the relay; its leases carry the instance id.
func New(
	cfg *config.Config,
	relayStore store.OutboxRelayStore,
	broker Publisher,
	instanceID model.InstanceID,
	log *slog.Logger,
) *Relay {
	return &Relay{
		store:          relayStore,
		broker:         broker,
		owner:          string(instanceID),
		cfg:            cfg.Outbox,
		maxPayloadSize: cfg.InboundEvent.MaxPayloadSize,
		log:            log.With("component", "outbox_relay"),
	}
}

// Name identifies the task in logs.
func (r *Relay) Name() string {
	return "outbox_relay"
}

// Run serves one leadership term with fresh state. It never fails on broker or
// database errors: that would end the term for every leader task.
func (r *Relay) Run(ctx context.Context) error {
	t := r.newTerm()
	t.log.Info("outbox relay started")

	var background sync.WaitGroup
	background.Add(2)

	go func() { defer background.Done(); t.cleanupLoop(ctx) }()
	go func() { defer background.Done(); t.backlogLoop(ctx) }()

	for {
		if !sleep(ctx, t.cycle(ctx)) {
			break
		}
	}

	background.Wait()
	t.log.Info("outbox relay stopped")

	return nil
}

func (r *Relay) newTerm() *term {
	id := r.terms.Add(1)

	return &term{Relay: r, log: r.log.With("term", id)}
}

// term is the state of one leadership term; nothing survives into the next.
type term struct {
	*Relay

	log       *slog.Logger
	recovered bool
	declared  bool
	backoff   time.Duration
}

// cycle runs one claim and publish round and returns the pause before the next.
func (t *term) cycle(ctx context.Context) time.Duration {
	// Own leases are freed at the start of a term and after a batch that could not
	// give its rows back; no row of this relay is in flight at those points.
	if _, err := t.store.RecoverLeases(ctx, t.owner, !t.recovered); err != nil {
		return t.pause(ctx, t.log, "database", err)
	}
	t.recovered = true

	// go-kit replays a declaration after reconnect only once it has succeeded.
	if !t.declared {
		if err := t.broker.DeclareTopology(ctx); err != nil {
			return t.pause(ctx, t.log, "declare", err)
		}
		t.declared = true
	}

	deadline := time.Now().Add(t.cfg.LeaseDuration)
	rows, err := t.store.Claim(ctx, t.owner, t.cfg.BatchSize, t.cfg.LeaseDuration)
	if err != nil {
		return t.pause(ctx, t.log, "database", err)
	}

	if wait, done := t.publish(ctx, rows, deadline); !done {
		return wait
	}

	t.backoff = 0
	if len(rows) == t.cfg.BatchSize {
		return 0
	}

	return t.cfg.PollInterval
}

// publish sends the rows in order; false means the batch stopped early and its
// unpublished rows were given back, or will be by the next own-lease recovery.
func (t *term) publish(ctx context.Context, rows []*model.OutboxEvent, deadline time.Time) (time.Duration, bool) {
	for i, row := range rows {
		rest := rows[i:]

		// Checked on the local clock: a row that could outlive its lease is not sent.
		if ctx.Err() != nil || time.Until(deadline) < t.maxPublishDuration() {
			t.release(ctx, rest, time.Now())

			return 0, false
		}

		log := t.log.With(
			"event_id", row.EventID,
			"event_type", row.EventType,
			"domain_id", row.DomainID,
			"attempts", row.Attempts,
		)

		if err := t.check(row); err != nil {
			if wait, done := t.fail(ctx, row, err, rows[i+1:], log); !done {
				return wait, false
			}

			continue
		}

		publishCtx, cancel := context.WithTimeout(ctx, t.cfg.PublishTimeout)
		err := t.broker.Publish(publishCtx, row)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				t.release(ctx, rest, time.Now())

				return 0, false
			}

			// Any broker error is taken as an outage: no attempt is spent and the
			// exchange is declared again before the next claim.
			wait := t.pause(ctx, log, category(err), err)
			t.declared = false
			t.release(ctx, rest, time.Now().Add(wait))

			return wait, false
		}

		// Publishing on without recording the result would only multiply duplicates.
		published, err := t.store.MarkPublished(ctx, t.owner, row.EventID)
		if err != nil || !published {
			return t.stopBatch(ctx, rows[i+1:], err, log)
		}

		log.Debug("outbox event published")
	}

	return 0, true
}

// maxPublishDuration bounds one publish: the write timeout of the connection,
// then the confirmation and go-kit's retry pause within the publish context.
func (t *term) maxPublishDuration() time.Duration {
	return 2 * t.cfg.PublishTimeout
}

// check rejects a row that no retry can publish, so it cannot block the rest.
func (t *term) check(row *model.OutboxEvent) error {
	if err := row.Validate(); err != nil {
		return err
	}
	if len(row.Payload) > t.maxPayloadSize {
		return fmt.Errorf("payload of %d bytes exceeds the limit of %d", len(row.Payload), t.maxPayloadSize)
	}

	return nil
}

// fail gives up on a row; false means it could not be recorded and the batch stopped.
func (t *term) fail(
	ctx context.Context,
	row *model.OutboxEvent,
	reason error,
	rest []*model.OutboxEvent,
	log *slog.Logger,
) (time.Duration, bool) {
	failed, err := t.store.MarkFailed(ctx, t.owner, row.EventID, reason.Error())
	if err != nil || !failed {
		return t.stopBatch(ctx, rest, err, log)
	}

	log.Error("outbox event cannot be published", "category", "invalid_event", "attempts", row.Attempts+1, "err", reason)

	return 0, true
}

// stopBatch ends a batch whose result could not be recorded: err for the database,
// nil for a lost lease. The current row returns through own-lease recovery.
func (t *term) stopBatch(
	ctx context.Context,
	rest []*model.OutboxEvent,
	err error,
	log *slog.Logger,
) (time.Duration, bool) {
	t.recovered = false

	if err == nil {
		log.Warn("outbox event lease was lost")
		t.release(ctx, rest, time.Now())

		return 0, false
	}

	wait := t.pause(ctx, log, "database", err)
	t.release(ctx, rest, time.Now().Add(wait))

	return wait, false
}

// release gives rows back even after the term ended, so the next leader does
// not wait for their leases. On failure the next cycle recovers own leases first.
func (t *term) release(ctx context.Context, rows []*model.OutboxEvent, nextAttemptAt time.Time) {
	if len(rows) == 0 {
		return
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.EventID)
	}

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	if err := t.store.Release(releaseCtx, t.owner, ids, nextAttemptAt); err != nil {
		t.recovered = false
		t.log.Error("release outbox events", "events", len(ids), "err", err)
	}
}

// pause logs a failure and returns the shared backoff, which grows until a
// round succeeds.
func (t *term) pause(ctx context.Context, log *slog.Logger, category string, err error) time.Duration {
	if ctx.Err() != nil {
		return 0
	}

	t.backoff = min(max(2*t.backoff, t.cfg.RetryInitialInterval), t.cfg.RetryMaxInterval)
	log.Warn("outbox relay paused", "category", category, "retry_in", t.backoff, "err", err)

	return t.backoff
}

func (t *term) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(t.cfg.CleanupInterval)
	defer ticker.Stop()

	for {
		t.cleanup(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// cleanup deletes in batches, so one statement never holds many row locks.
func (t *term) cleanup(ctx context.Context) {
	before := time.Now().Add(-t.cfg.Retention)

	var total int64
	for ctx.Err() == nil {
		deleted, err := t.store.DeletePublishedBefore(ctx, before, t.cfg.CleanupBatch)
		if err != nil {
			if ctx.Err() == nil {
				t.log.Error("outbox cleanup failed", "err", err)
			}

			break
		}

		total += deleted
		if deleted < int64(t.cfg.CleanupBatch) {
			break
		}
	}

	if total > 0 {
		t.log.Info("published outbox events removed", "deleted", total)
	}
}

func (t *term) backlogLoop(ctx context.Context) {
	ticker := time.NewTicker(backlogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		backlog, err := t.store.Backlog(ctx)
		if err != nil {
			if ctx.Err() == nil {
				t.log.Error("outbox backlog unavailable", "err", err)
			}

			continue
		}

		if backlog.Pending+backlog.Processing > 0 {
			t.log.Info("outbox backlog",
				"pending", backlog.Pending,
				"processing", backlog.Processing,
				"oldest_age", backlog.OldestUndelivered.Round(time.Second),
			)
		}
		if backlog.Failed > 0 {
			t.log.Warn("outbox has events that cannot be published", "failed", backlog.Failed)
		}
	}
}

// category names a broker failure for logs; all of them pause the whole batch.
func category(err error) string {
	switch {
	case errors.Is(err, rabbitmq.ErrConnectionNotAvailable):
		return "broker_unavailable"
	case errors.Is(err, rabbitmq.ErrPublishTimeout):
		return "confirm_timeout"
	case errors.Is(err, rabbitmq.ErrMessageUnroutable):
		return "unroutable"
	case errors.Is(err, rabbitmq.ErrMessageNacked):
		return "nacked"
	default:
		return "publish_failed"
	}
}

// sleep waits for d and reports whether the term is still running.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
