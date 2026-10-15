package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"go.uber.org/fx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	kitinterceptors "github.com/webitel/webitel-go-kit/pkg/interceptors"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra/server/grpc/interceptors"
	infratls "github.com/webitel/webitel-emails/infra/tls"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/model"
)

// Server owns the internal gRPC server, listener, and health service.
type Server struct {
	*grpc.Server

	listener net.Listener
	health   *health.Server
	log      *slog.Logger
}

// New creates the gRPC server and binds its start and graceful stop to Fx.
func New(
	cfg *config.Config,
	log *slog.Logger,
	tlsConfig *infratls.Config,
	authManager auth.Manager,
	lifecycle fx.Lifecycle,
) (*Server, error) {
	listener, err := net.Listen("tcp", cfg.Service.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpc server: listen on %s: %w", cfg.Service.Addr, err)
	}

	options := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			kitinterceptors.UnaryServerErrorInterceptor(),
			interceptors.NewUnaryAuthInterceptor(authManager),
		),
	}
	if tlsConfig.Server != nil {
		options = append(options, grpc.Creds(credentials.NewTLS(tlsConfig.Server.Clone())))
	}

	grpcServer := grpc.NewServer(options...)
	healthServer := health.NewServer()
	healthgrpc.RegisterHealthServer(grpcServer, healthServer)
	setHealthStatus(healthServer, healthgrpc.HealthCheckResponse_NOT_SERVING)

	server := &Server{
		Server:   grpcServer,
		listener: listener,
		health:   healthServer,
		log:      log,
	}
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			setHealthStatus(server.health, healthgrpc.HealthCheckResponse_SERVING)
			server.log.Info("gRPC server started", "address", server.listener.Addr().String())

			go func() {
				if err := server.Serve(server.listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
					server.log.Error("gRPC server stopped with error", "error", err)
				}
			}()

			return nil
		},
		OnStop: func(ctx context.Context) error {
			server.health.Shutdown()

			stopped := make(chan struct{})
			go func() {
				server.GracefulStop()
				close(stopped)
			}()

			select {
			case <-stopped:
				server.log.Info("gRPC server stopped")
				return nil
			case <-ctx.Done():
				server.Stop()
				server.log.Warn("gRPC graceful shutdown timed out", "error", ctx.Err())

				return fmt.Errorf("grpc server: graceful shutdown: %w", ctx.Err())
			}
		},
	})

	return server, nil
}

func setHealthStatus(server *health.Server, status healthgrpc.HealthCheckResponse_ServingStatus) {
	server.SetServingStatus("", status)
	server.SetServingStatus(model.ServiceName, status)
}
