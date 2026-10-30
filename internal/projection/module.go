package projection

import "go.uber.org/fx"

// Module consumes the attempt snapshots on every instance.
var Module = fx.Module(
	"projection",
	fx.Provide(New),
	fx.Invoke(func(*Consumer) {}),
)
