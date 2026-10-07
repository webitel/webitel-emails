// Package webitelapp holds the shared gRPC connection to go.webitel.app, which
// serves both the authorization and the Contacts APIs.
package webitelapp

import (
	"context"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/fx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	_ "github.com/mbobakov/grpc-consul-resolver"

	"github.com/webitel/webitel-emails/config"
)

// ServiceName is how go.webitel.app registers itself in Consul.
const ServiceName = "go.webitel.app"

// Conn is the shared connection. It is a named type so the graph cannot confuse
// it with a connection to another service, and so a second one can be added.
type Conn struct {
	*grpc.ClientConn
}

// Module provides the connection to go.webitel.app.
var Module = fx.Module("webitelapp", fx.Provide(New))

// New opens one connection for every go.webitel.app client of this service.
// A single HTTP/2 connection carries concurrent unary calls, so these short
// requests need no connection pool; only streaming clients, such as storage, do.
func New(cfg *config.Config, lifecycle fx.Lifecycle) (Conn, error) {
	conn, err := grpc.NewClient(
		fmt.Sprintf("consul://%s/%s?wait=14s", cfg.Consul.Addr, ServiceName),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return Conn{}, fmt.Errorf("webitelapp: create client: %w", err)
	}

	lifecycle.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return conn.Close()
		},
	})

	return Conn{ClientConn: conn}, nil
}
