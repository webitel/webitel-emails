package oauth

import "go.uber.org/fx"

// Module provides OAuth configuration for supported mailbox providers.
var Module = fx.Module(
	"oauth",
	fx.Provide(fx.Annotate(NewResolver, fx.As(new(ConfigResolver)))),
)
