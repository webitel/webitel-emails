package postgres

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/internal/store"
)

// Module provides PostgreSQL-backed stores.
var Module = fx.Module(
	"store",
	fx.Provide(
		fx.Annotate(
			NewEmailProfileStore,
			fx.As(new(store.EmailProfileStore)),
		),
		fx.Annotate(
			NewEmailProfileRuntimeStore,
			fx.As(new(store.EmailProfileRuntimeStore)),
		),
	),
)
