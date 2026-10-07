package mail

import "go.uber.org/fx"

// Module provides IMAP and SMTP clients.
var Module = fx.Module(
	"mail",
	fx.Provide(
		NewIMAPClient,
		NewSMTPClient,
	),
)
