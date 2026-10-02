package postgres

import (
	"database/sql"

	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/internal/store"
)

// Module provides PostgreSQL-backed stores.
var Module = fx.Module(
	"store",
	fx.Provide(
		fx.Annotate(
			NewUnitOfWork,
			// The constructor takes the Database seam; the graph supplies *sql.DB.
			fx.From(new(*sql.DB)),
			fx.As(new(store.UnitOfWork)),
		),
		fx.Annotate(
			NewEmailProfileStore,
			fx.As(new(store.EmailProfileStore)),
		),
		fx.Annotate(
			NewEmailProfileRuntimeStore,
			fx.As(new(store.EmailProfileRuntimeStore)),
		),
		fx.Annotate(
			NewEmailMessageStore,
			fx.From(new(*sql.DB)),
			fx.As(new(store.EmailMessageStore)),
		),
		fx.Annotate(
			NewInboundFailureStore,
			fx.From(new(*sql.DB)),
			fx.As(new(store.InboundFailureStore)),
		),
	),
)
