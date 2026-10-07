package distribution

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/infra/leader"
)

// Module registers the profile distributor as a leader task.
var Module = fx.Module(
	"distribution",
	fx.Provide(
		fx.Annotate(
			New,
			fx.As(new(leader.Task)),
			fx.ResultTags(leader.TaskGroup),
		),
	),
)
