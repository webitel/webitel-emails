package inbound

import (
	"go.uber.org/fx"

	contactsinfra "github.com/webitel/webitel-emails/infra/contacts"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
)

// Module assembles the inbound pipeline: raw email, MIME parsing, persistence.
// Providing it is mandatory; without it the polling scheduler cannot start.
var Module = fx.Module(
	"inbound",
	fx.Provide(
		fx.Annotate(NewMIMEParser, fx.As(new(Parser))),
		// Bound here so infra stays unaware of this package.
		func(client *storageinfra.Client) FileUploader { return client },
		func(client *contactsinfra.Client) ContactResolver { return client },
		NewMessageCompleter,
		fx.Annotate(NewPersistenceHandler, fx.As(new(ParsedMessageHandler))),
		NewMIMEHandler,
		fx.Annotate(NewQuarantineHandler, fx.As(new(Handler))),
	),
)
