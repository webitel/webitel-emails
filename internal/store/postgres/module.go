package postgres

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/store"
)

// Module provides PostgreSQL-backed stores.
var Module = fx.Module(
	"store",
	fx.Provide(
		fx.Annotate(
			func(pool *pgxpool.Pool, cfg *config.Config) *unitOfWork {
				return NewUnitOfWork(pool).WithWorkspaceEvents(cfg.WorkspaceEvents.Enabled)
			},
			fx.As(new(store.UnitOfWork)),
		),
		fx.Annotate(
			NewEmailProfileStore,
			fx.From(new(*pgxpool.Pool)),
			fx.As(new(store.EmailProfileStore)),
		),
		fx.Annotate(
			NewEmailProfileRuntimeStore,
			fx.From(new(*pgxpool.Pool)),
			fx.As(new(store.EmailProfileRuntimeStore)),
		),
		fx.Annotate(
			NewEmailThreadStore,
			fx.From(new(*pgxpool.Pool)),
			fx.As(new(store.EmailThreadStore)),
		),
		fx.Annotate(
			func(pool *pgxpool.Pool, cfg *config.Config) *emailThreadContactStore {
				return NewEmailThreadContactStore(pool, cfg.WorkspaceEvents.Enabled)
			},
			fx.As(new(store.EmailThreadContactStore)),
		),
		fx.Annotate(
			NewEmailThreadReadStore,
			fx.From(new(*pgxpool.Pool)),
			fx.As(new(store.EmailThreadReadStore)),
		),
		fx.Annotate(
			NewEmailMessageStore,
			fx.From(new(*pgxpool.Pool)),
			fx.As(new(store.EmailMessageStore)),
		),
		fx.Annotate(
			func(pool *pgxpool.Pool, cfg *config.Config) *emailThreadProjectionStore {
				return NewEmailThreadProjectionStore(pool).WithWorkspaceEvents(cfg.WorkspaceEvents.Enabled)
			},
			fx.As(new(store.EmailThreadProjectionStore)),
			fx.As(new(store.EmailThreadActionCleaner)),
		),
		fx.Annotate(
			NewOutboxRelayStore,
			fx.From(new(*pgxpool.Pool)),
			fx.As(new(store.OutboxRelayStore)),
		),
		fx.Annotate(
			NewInboundFailureStore,
			fx.From(new(*pgxpool.Pool)),
			fx.As(new(store.InboundFailureStore)),
		),
	),
)
