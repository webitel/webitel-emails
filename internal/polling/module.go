package polling

import "go.uber.org/fx"

// Module polls the mailboxes assigned to this instance.
var Module = fx.Module(
	"polling",
	fx.Provide(New),
	fx.Invoke(func(*Scheduler) {}),
)
