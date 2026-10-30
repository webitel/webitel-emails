package projection

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/webitel/webitel-emails/internal/event"
)

// declare creates the topology of the snapshots before every subscription; the
// parameters match docs/events.md, since a redeclaration with others fails.
func declare(ch *amqp.Channel, deliveryLimit int) error {
	for _, exchange := range []struct{ name, kind string }{
		{event.AttemptExchange, event.AttemptExchangeType},
		{event.ProjectionDeadLetterExchange, event.ProjectionDeadLetterExchangeType},
	} {
		if err := ch.ExchangeDeclare(exchange.name, exchange.kind, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare exchange %s: %w", exchange.name, err)
		}
	}

	queues := []struct {
		name, exchange, key string
		args                amqp.Table
	}{
		{event.ProjectionQueue, event.AttemptExchange, event.AttemptSnapshotRoutingKey, amqp.Table{
			"x-queue-type":           event.ProjectionQueueType,
			"x-dead-letter-exchange": event.ProjectionDeadLetterExchange,
			"x-delivery-limit":       deliveryLimit,
		}},
		// The fanout exchange ignores the routing key.
		{event.ProjectionDeadLetterQueue, event.ProjectionDeadLetterExchange, "", amqp.Table{
			"x-queue-type": event.ProjectionQueueType,
		}},
	}
	for _, q := range queues {
		if _, err := ch.QueueDeclare(q.name, true, false, false, false, q.args); err != nil {
			return fmt.Errorf("declare queue %s: %w", q.name, err)
		}
		if err := ch.QueueBind(q.name, q.key, q.exchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %s: %w", q.name, err)
		}
	}

	return nil
}
