// Package postgres manages the service PostgreSQL connection.
package postgres

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

var Module = fx.Module(
	"postgres",
	fx.Provide(New),
	fx.Invoke(func(*pgxpool.Pool) {}),
)
