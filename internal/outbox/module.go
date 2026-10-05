package outbox

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/infra/leader"
	"github.com/webitel/webitel-emails/infra/pubsub"
)

// Module registers the relay as a leader task.
var Module = fx.Module(
	"outbox",
	fx.Provide(
		// Bound here so infra stays unaware of this package.
		func(broker *pubsub.Broker) Publisher { return broker },
		fx.Annotate(
			New,
			fx.As(new(leader.Task)),
			fx.ResultTags(leader.TaskGroup),
		),
	),
)
