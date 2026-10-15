// Package webitelapp holds the shared gRPC connection to go.webitel.app, which
// serves both the authorization and the Contacts APIs.
package webitelapp

import (
	"context"
	"fmt"

	kitdiscovery "github.com/webitel/webitel-go-kit/infra/discovery"
	"github.com/webitel/webitel-go-kit/infra/transport/gRPC/resolver/discovery"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/fx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
func New(provider kitdiscovery.DiscoveryProvider, lifecycle fx.Lifecycle) (Conn, error) {
	conn, err := grpc.NewClient(
		fmt.Sprintf("discovery:///%s", ServiceName),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithResolvers(discovery.NewBuilder(provider, discovery.WithInsecure(true))),
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
