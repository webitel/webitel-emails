package contacts

import "go.uber.org/fx"

// Module provides the Contacts client.
var Module = fx.Module("contacts", fx.Provide(NewClient))
