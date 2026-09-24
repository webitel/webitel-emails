package server

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra"
	"github.com/webitel/webitel-emails/infra/logging"
	"github.com/webitel/webitel-emails/internal/distribution"
	grpchandler "github.com/webitel/webitel-emails/internal/handler/grpc"
	"github.com/webitel/webitel-emails/internal/polling"
	"github.com/webitel/webitel-emails/internal/service"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
)

func NewApp(cfg *config.Config) *fx.App {
	return fx.New(
		fx.Supply(cfg),
		infra.Module,
		storepostgres.Module,
		service.Module,
		grpchandler.Module,
		distribution.Module,
		polling.Module,
		fx.WithLogger(logging.NewFxEventLogger),
	)
}
