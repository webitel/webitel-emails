package leader

import "go.uber.org/fx"

// Module runs the leader election together with the registered leader tasks.
var Module = fx.Module(
	"leader",
	fx.Provide(New),
	fx.Invoke(func(*Runner) {}),
)
