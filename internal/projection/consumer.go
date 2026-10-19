package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// Params are the consumer dependencies.
type Params struct {
	fx.In

	Config     *config.Config
	InstanceID model.InstanceID
	Store      store.EmailThreadProjectionStore
	Log        *slog.Logger
	Lifecycle  fx.Lifecycle
}

// Consumer moves snapshots from RabbitMQ to the Threads; a delivery is
// acknowledged only after its transaction commits.
type Consumer struct {
	url   string
	name  string
	tag   string
	cfg   config.ThreadProjectionConfig
	store store.EmailThreadProjectionStore
	log   *slog.Logger
	stats stats

	stop context.CancelFunc
	done sync.WaitGroup
}

// New creates the consumer and ties it to the application lifecycle.
func New(p Params) *Consumer {
	c := &Consumer{
		url:   p.Config.Pubsub.URL,
		name:  "webitel-emails thread projection " + string(p.InstanceID),
		tag:   string(p.InstanceID),
		cfg:   p.Config.ThreadProjection,
		store: p.Store,
		log:   p.Log.With("component", "thread_projection"),
	}

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			c.start()

			return nil
		},
		OnStop: c.shutdown,
	})

	return c
}

func (c *Consumer) start() {
	ctx, stop := context.WithCancel(context.Background())
	c.stop = stop

	c.done.Add(2)
	go func() { defer c.done.Done(); c.run(ctx) }()
	go func() { defer c.done.Done(); c.reportLoop(ctx) }()
}

// shutdown stops consuming; a delivery still being applied returns to the queue.
func (c *Consumer) shutdown(ctx context.Context) error {
	if c.stop == nil {
		return nil
	}
	c.stop()

	done := make(chan struct{})
	go func() { c.done.Wait(); close(done) }()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("thread projection: shutdown: %w", ctx.Err())
	}
}

// run reconnects until ctx is done; an unavailable broker never fails it.
func (c *Consumer) run(ctx context.Context) {
	var backoff time.Duration

	for ctx.Err() == nil {
		consumed, err := c.generation(ctx)
		if ctx.Err() != nil {
			break
		}
		if consumed {
			backoff = 0
		}

		backoff = c.next(backoff)
		c.log.Warn("thread projection consumer paused", "retry_in", backoff, "err", err)
		sleep(ctx, backoff)
	}

	c.log.Info("thread projection consumer stopped")
}

// generation serves one connection and channel and declares the topology
// before subscribing; consumed reports it reached Consume. Unacknowledged
// deliveries return to the queue when the channel closes.
func (c *Consumer) generation(ctx context.Context) (consumed bool, err error) {
	props := amqp.NewConnectionProperties()
	props.SetClientConnectionName(c.name)

	conn, err := amqp.DialConfig(c.url, amqp.Config{Locale: "en_US", Properties: props})
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return false, fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()

	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return false, fmt.Errorf("set prefetch: %w", err)
	}
	if err := declare(ch, c.cfg.DeliveryLimit); err != nil {
		return false, err
	}

	deliveries, err := ch.Consume(event.ProjectionQueue, c.tag, false, false, false, false, nil)
	if err != nil {
		return false, fmt.Errorf("consume: %w", err)
	}

	// Cancelled first among the defers, so a retrying write stops before Close.
	genCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))
	chClosed := ch.NotifyClose(make(chan *amqp.Error, 1))
	go func() {
		select {
		case e := <-connClosed:
			cancel(fmt.Errorf("connection closed: %v", e))
		case e := <-chClosed:
			cancel(fmt.Errorf("channel closed: %v", e))
		case <-genCtx.Done():
		}
	}()

	c.log.Info("thread projection consumer started", "queue", event.ProjectionQueue, "consumer_tag", c.tag)

	for {
		select {
		case <-genCtx.Done():
			return true, context.Cause(genCtx)
		case d, ok := <-deliveries:
			if !ok {
				return true, errors.New("deliveries closed")
			}
			if err := c.handle(genCtx, d); err != nil {
				return true, err
			}
		}
	}
}

// handle settles one delivery; an error ends the generation. A cancelled ctx
// leaves the delivery unsettled for redelivery.
func (c *Consumer) handle(ctx context.Context, d amqp.Delivery) error {
	snapshot, err := event.UnmarshalAttemptSnapshot(d.Body, headerString(d.Headers[event.HeaderEventID]))
	log := c.log.With(
		"email_event_id", headerString(d.Headers[event.HeaderEventID]),
		"domain_id", snapshot.DomainID,
		"email_thread_id", snapshot.ThreadID,
		"projection_version", snapshot.ProjectionVersion,
		"transition", snapshot.Transition,
	)
	if err != nil {
		return c.reject(d, log, "invalid_event", err)
	}

	projection, err := Project(snapshot)
	if err != nil {
		return c.reject(d, log, "invalid_event", err)
	}

	// A database outage keeps the delivery instead of spending its delivery limit.
	var backoff time.Duration
	for {
		result, err := c.store.Apply(ctx, projection)
		switch {
		case err == nil:
			return c.settle(d, log, snapshot, result)
		case errors.Is(err, store.ErrEmailThreadProjectionRejected):
			return c.reject(d, log, "invalid_data", err)
		case ctx.Err() != nil:
			return nil
		}

		backoff = c.next(backoff)
		log.Warn("attempt snapshot apply failed", "retry_in", backoff, "err", err)
		if !sleep(ctx, backoff) {
			return nil
		}
	}
}

// settle acknowledges a committed snapshot or rejects one without a Thread to land on.
func (c *Consumer) settle(
	d amqp.Delivery,
	log *slog.Logger,
	snapshot event.AttemptSnapshot,
	result store.EmailThreadProjectionResult,
) error {
	switch result.Outcome {
	case store.EmailThreadProjectionNotFound:
		return c.reject(d, log, "thread_not_found", errors.New("no regular thread of the domain"))
	case store.EmailThreadProjectionService:
		return c.reject(d, log, "service_thread", errors.New("a service thread is never projected"))
	case store.EmailThreadProjectionApplied:
		c.stats.applied(time.Since(snapshot.OccurredAt))
		log.Debug("attempt snapshot applied", "last_handled_applied", result.HandledApplied)
	default:
		c.stats.stale()
		log.Debug("attempt snapshot is not newer than the thread", "last_handled_applied", result.HandledApplied)
	}

	if err := d.Ack(false); err != nil {
		return fmt.Errorf("ack: %w", err)
	}

	return nil
}

// reject sends a delivery that no retry can apply to the dead letter queue; the
// payload is never logged.
func (c *Consumer) reject(d amqp.Delivery, log *slog.Logger, category string, reason error) error {
	c.stats.rejected()
	log.Warn("attempt snapshot sent to the dead letter queue",
		"category", category, "routing_key", d.RoutingKey, "err", reason)

	if err := d.Nack(false, false); err != nil {
		return fmt.Errorf("nack: %w", err)
	}

	return nil
}

// reportLoop logs the counts and the lag of the projection since the last report.
func (c *Consumer) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.ReportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		r := c.stats.take()
		if r.applied+r.stale+r.rejected == 0 {
			continue
		}
		c.log.Info("thread projection",
			"applied", r.applied,
			"stale", r.stale,
			"rejected", r.rejected,
			"last_lag", r.lastLag.Round(time.Millisecond),
			"max_lag", r.maxLag.Round(time.Millisecond),
		)
	}
}

func (c *Consumer) next(backoff time.Duration) time.Duration {
	return min(max(2*backoff, c.cfg.RetryInitialInterval), c.cfg.RetryMaxInterval)
}

// stats are the counts since the last report; lag is now minus occurred_at of an applied snapshot.
type stats struct {
	mu     sync.Mutex
	report report
}

type report struct {
	applied, stale, rejected int64
	lastLag, maxLag          time.Duration
}

func (s *stats) applied(lag time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.applied++
	s.report.lastLag = lag
	s.report.maxLag = max(s.report.maxLag, lag)
}

func (s *stats) stale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.stale++
}

func (s *stats) rejected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.rejected++
}

func (s *stats) take() report {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.report
	s.report = report{}

	return r
}

func headerString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// sleep waits for d and reports whether ctx is still running.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
