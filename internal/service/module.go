package service

import (
	"go.uber.org/fx"

	contactsinfra "github.com/webitel/webitel-emails/infra/contacts"
)

// Module provides application services.
var Module = fx.Module(
	"service",
	fx.Provide(
		NewEmailProfileService,
		// Bound here so infra stays unaware of this package.
		func(client *contactsinfra.Client) ContactVerifier { return client },
		NewEmailThreadService,
	),
)
