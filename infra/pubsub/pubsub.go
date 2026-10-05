// Package pubsub provides the RabbitMQ broker the Outbox relay publishes through.
package pubsub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/fx"

	"github.com/webitel/webitel-go-kit/infra/pubsub/rabbitmq"
	rabbitslog "github.com/webitel/webitel-go-kit/infra/pubsub/rabbitmq/pkg/adapter/slog"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
)

var Module = fx.Module(
	"pubsub",
	fx.Provide(New),
	fx.Invoke(func(*Broker) {}),
)

// Broker declares the producer topology and publishes stored Outbox events.
type Broker struct {
	conn      *rabbitmq.Connection
	publisher *rabbitmq.MessagePublisher
}

// New connects lazily: an unavailable RabbitMQ neither fails startup nor blocks
// the inbound pipeline, whose events wait in the Outbox.
func New(cfg *config.Config, log *slog.Logger, lifecycle fx.Lifecycle) (*Broker, error) {
	logger := rabbitslog.NewSlogLogger(log.With("component", "rabbitmq"))

	connectionConfig, err := rabbitmq.NewConfig(
		cfg.Pubsub.URL,
		// Bounds a write blocked by TCP pushback, which the publish context cannot.
		rabbitmq.WithWriteTimeout(cfg.Outbox.PublishTimeout),
		rabbitmq.WithLazyConnect(true),
	)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: create connection config: %w", err)
	}

	connection, err := rabbitmq.NewConnection(connectionConfig, logger)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: create connection: %w", err)
	}

	// One attempt per call: retries and pauses belong to the Outbox relay.
	publisherConfig, err := rabbitmq.NewPublisherConfig(
		rabbitmq.WithConfirmation(true),
		rabbitmq.WithPublisherConfirmationTimeout(cfg.Outbox.PublishTimeout),
		rabbitmq.WithPublisherMaxRetries(1),
		rabbitmq.WithPersistentDelivery(true),
		rabbitmq.WithMandatory(true),
	)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("rabbitmq: create publisher config: %w", err)
	}

	publisher, err := rabbitmq.NewPublisher(connection, publisherConfig, logger)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("rabbitmq: create publisher: %w", err)
	}

	broker := &Broker{conn: connection, publisher: publisher}

	lifecycle.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return broker.Close()
		},
	})

	return broker, nil
}

// DeclareTopology declares the exchange events are published to. go-kit replays
// it after a reconnect only once it succeeded, so callers retry a failure.
func (b *Broker) DeclareTopology(ctx context.Context) error {
	exchange, err := rabbitmq.NewExchangeConfig(event.Exchange, rabbitmq.MQExchangeType(event.ExchangeType))
	if err != nil {
		return fmt.Errorf("rabbitmq: exchange config: %w", err)
	}

	if err := b.conn.DeclareExchange(ctx, exchange); err != nil {
		return fmt.Errorf("rabbitmq: declare %s: %w", exchange.Name, err)
	}

	return nil
}

// Publish sends the stored payload untouched; success means the broker confirmed
// it and routed it to a queue.
func (b *Broker) Publish(ctx context.Context, outboxEvent *model.OutboxEvent) error {
	headers := amqp.Table{
		event.HeaderEventID:       outboxEvent.EventID.String(),
		event.HeaderEventType:     outboxEvent.EventType,
		event.HeaderSchemaVersion: outboxEvent.SchemaVersion,
	}

	return b.publisher.Publish(ctx, outboxEvent.Exchange, outboxEvent.RoutingKey, outboxEvent.Payload, headers)
}

// Close stops the publisher before the connection it runs on.
func (b *Broker) Close() error {
	return errors.Join(b.publisher.Close(), b.conn.Close())
}
