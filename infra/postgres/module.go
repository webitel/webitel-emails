// Package postgres manages the service PostgreSQL connection.
package postgres

import (
	"database/sql"

	"go.uber.org/fx"
)

var Module = fx.Module(
	"postgres",
	fx.Provide(New),
	fx.Invoke(func(*sql.DB) {}),
)
