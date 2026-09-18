package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func New(cfg *config.Config, log *slog.Logger, lifecycle fx.Lifecycle) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	cfg.Postgres.ApplyToSQLDB(db)

	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := db.PingContext(ctx); err != nil {
				_ = db.Close()
				return fmt.Errorf("postgres: ping: %w", err)
			}
			log.Info("postgres connection established")

			return nil
		},
		OnStop: func(context.Context) error {
			if err := db.Close(); err != nil {
				return fmt.Errorf("postgres: close: %w", err)
			}
			log.Info("postgres connection closed")

			return nil
		},
	})

	return db, nil
}
