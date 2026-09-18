package mail

import "go.uber.org/fx"

// Module provides clients used to validate mailbox connections.
var Module = fx.Module(
	"mail",
	fx.Provide(
		NewIMAPClient,
		NewSMTPClient,
	),
)
