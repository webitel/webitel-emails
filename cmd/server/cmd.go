package server

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/webitel/webitel-emails/config"
)

func CMD() *cli.Command {
	return &cli.Command{
		Name:    "server",
		Aliases: []string{"s"},
		Usage:   "Run the email service",
		// appconfig uses pflag so it can consistently resolve flags, environment,
		// config files, and defaults. Pass command arguments to that parser.
		SkipFlagParsing: true,
		Action: func(c *cli.Context) error {
			args := c.Args().Slice()
			if slices.Contains(args, "--help") || slices.Contains(args, "-h") {
				return cli.ShowCommandHelp(c, c.Command.Name)
			}

			cfg, err := config.LoadServerConfig(args)
			if err != nil {
				return err
			}

			app := NewApp(cfg)
			if err := app.Start(c.Context); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(c.Context, os.Interrupt, syscall.SIGTERM)
			defer stop()

			<-ctx.Done()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), app.StopTimeout())
			defer cancel()

			return app.Stop(shutdownCtx)
		},
	}
}
