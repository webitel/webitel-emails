package cmd

import (
	"os"

	"github.com/urfave/cli/v2"

	"github.com/webitel/webitel-emails/cmd/migrate"
	"github.com/webitel/webitel-emails/cmd/server"
	"github.com/webitel/webitel-emails/internal/model"
)

func Run() error {
	app := &cli.App{
		Name:  model.ServiceName,
		Usage: "Email service for the Webitel platform",
		Commands: []*cli.Command{
			migrate.CMD(),
			server.CMD(),
		},
	}

	return app.Run(os.Args)
}
