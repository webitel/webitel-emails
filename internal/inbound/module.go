package inbound

import (
	"go.uber.org/fx"
)

// Module assembles the inbound pipeline: raw email, MIME parsing, persistence.
// Providing it is mandatory; without it the polling scheduler cannot start.
var Module = fx.Module(
	"inbound",
	fx.Provide(
		fx.Annotate(NewMIMEParser, fx.As(new(Parser))),
		NewPersistenceHandler,
		// Remove the gate with task 6 and annotate NewPersistenceHandler as the
		// ParsedMessageHandler instead.
		fx.Annotate(NewAttachmentGate, fx.As(new(ParsedMessageHandler))),
		NewMIMEHandler,
		fx.Annotate(NewQuarantineHandler, fx.As(new(Handler))),
	),
)
