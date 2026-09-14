package server

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra"
	"github.com/webitel/webitel-emails/infra/logging"
)

func NewApp(cfg *config.Config) *fx.App {
	return fx.New(
		fx.Supply(cfg),
		infra.Module,
		fx.WithLogger(logging.NewFxEventLogger),
	)
}
