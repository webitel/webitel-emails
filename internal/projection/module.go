package projection

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/infra/leader"
)

// Module consumes the attempt snapshots on every instance; the leader trims the action journal.
var Module = fx.Module(
	"projection",
	fx.Provide(
		New,
		fx.Annotate(
			NewActionCleanup,
			fx.As(new(leader.Task)),
			fx.ResultTags(leader.TaskGroup),
		),
	),
	fx.Invoke(func(*Consumer) {}),
)
