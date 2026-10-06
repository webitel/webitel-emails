package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"
	otelpgx "github.com/webitel/webitel-go-kit/infra/otel/instrumentation/pgx"
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
)

func New(cfg *config.Config, log *slog.Logger, lifecycle fx.Lifecycle) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.Postgres.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}
	if cfg.Postgres.MaxOpenConns > math.MaxInt32 {
		return nil, fmt.Errorf("postgres: max open connections exceeds %d", math.MaxInt32)
	}
	if cfg.Postgres.MaxOpenConns > 0 {
		poolConfig.MaxConns = int32(cfg.Postgres.MaxOpenConns)
	}
	if cfg.Postgres.ConnMaxIdleTime > 0 {
		poolConfig.MaxConnIdleTime = cfg.Postgres.ConnMaxIdleTime
	}
	if cfg.Postgres.ConnMaxLifetime > 0 {
		poolConfig.MaxConnLifetime = cfg.Postgres.ConnMaxLifetime
	}
	if cfg.Postgres.MaxIdleConns > 0 {
		log.Warn("postgres.max_idle_conns is not supported by pgxpool; use conn_max_idle_time")
	}
	if cfg.Log.Otel {
		poolConfig.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTrimSQLInSpanName())
	}

	db, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := db.Ping(ctx); err != nil {
				db.Close()
				return fmt.Errorf("postgres: ping: %w", err)
			}
			log.Info("postgres connection established")

			return nil
		},
		OnStop: func(context.Context) error {
			db.Close()
			log.Info("postgres connection closed")

			return nil
		},
	})

	return db, nil
}
