// Package auth provides the connection to the Webitel authorization service.
package auth

import (
	"context"
	"fmt"

	"go.uber.org/fx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	_ "github.com/mbobakov/grpc-consul-resolver"

	"github.com/webitel/webitel-emails/config"
	internalauth "github.com/webitel/webitel-emails/internal/auth"
	webitelapp "github.com/webitel/webitel-emails/internal/auth/webitel_app"
)

// Module provides the authorized caller session manager.
var Module = fx.Module("auth", fx.Provide(NewManager))

// NewManager connects to go.webitel.app through Consul.
func NewManager(cfg *config.Config, lifecycle fx.Lifecycle) (internalauth.Manager, error) {
	conn, err := grpc.NewClient(
		fmt.Sprintf("consul://%s/go.webitel.app?wait=14s", cfg.Consul.Addr),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}

	lifecycle.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return conn.Close()
		},
	})

	return webitelapp.New(conn), nil
}
