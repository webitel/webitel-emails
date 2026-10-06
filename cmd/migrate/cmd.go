// Package migrate implements the standalone database migration command.
package migrate

import (
	"context"
	"database/sql"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra/logging"
	"github.com/webitel/webitel-emails/infra/postgres"
)

func CMD() *cli.Command {
	return &cli.Command{
		Name:    "migrate",
		Aliases: []string{"m"},
		Usage:   "Apply all pending database migrations",
		// appconfig uses pflag, so the command passes all arguments to the same
		// configuration loader as the server command.
		SkipFlagParsing: true,
		Action: func(c *cli.Context) error {
			args := c.Args().Slice()
			if slices.Contains(args, "--help") || slices.Contains(args, "-h") {
				return cli.ShowSubcommandHelp(c)
			}

			cfg, err := config.LoadMigrateConfig(args)
			if err != nil {
				return err
			}

			return run(c.Context, cfg)
		},
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	app := fx.New(
		modules(cfg),
		fx.NopLogger,
	)

	if err := app.Start(ctx); err != nil {
		return err
	}

	return app.Stop(ctx)
}

func modules(cfg *config.Config) fx.Option {
	return fx.Options(
		fx.Supply(cfg),
		logging.Module,
		postgres.Module,
		fx.Provide(newMigrationDB, NewMigrator),
		fx.Invoke(func(lifecycle fx.Lifecycle, migrator *Migrator) {
			lifecycle.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					return migrator.Run(ctx)
				},
			})
		}),
	)
}

// newMigrationDB adapts the pgx pool for Goose, which requires database/sql.
func newMigrationDB(pool *pgxpool.Pool, lifecycle fx.Lifecycle) *sql.DB {
	db := stdlib.OpenDBFromPool(pool)
	lifecycle.Append(fx.StopHook(db.Close))

	return db
}
