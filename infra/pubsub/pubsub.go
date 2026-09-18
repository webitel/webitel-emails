// Package pubsub provides the RabbitMQ publisher used by the service.
package pubsub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.uber.org/fx"

	"github.com/webitel/webitel-go-kit/infra/pubsub/rabbitmq"
	rabbitslog "github.com/webitel/webitel-go-kit/infra/pubsub/rabbitmq/pkg/adapter/slog"

	"github.com/webitel/webitel-emails/config"
)

var Module = fx.Module(
	"pubsub",
	fx.Provide(New),
	fx.Invoke(func(rabbitmq.Publisher) {}),
)

func New(cfg *config.Config, log *slog.Logger, lifecycle fx.Lifecycle) (rabbitmq.Publisher, error) {
	logger := rabbitslog.NewSlogLogger(log.With("component", "rabbitmq"))

	connectionConfig, err := rabbitmq.NewConfig(cfg.Pubsub.URL)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: create connection config: %w", err)
	}

	connection, err := rabbitmq.NewConnection(connectionConfig, logger)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: create connection: %w", err)
	}

	publisherConfig, err := rabbitmq.NewPublisherConfig(
		rabbitmq.WithConfirmation(true),
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

	lifecycle.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return errors.Join(
				publisher.Close(),
				connection.Close(),
			)
		},
	})

	return publisher, nil
}
