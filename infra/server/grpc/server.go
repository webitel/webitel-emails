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

	"github.com/webitel/webitel-emails/config"
	infratls "github.com/webitel/webitel-emails/infra/tls"
	"github.com/webitel/webitel-emails/internal/model"
)

type Server struct {
	*grpc.Server

	listener net.Listener
	health   *health.Server
	log      *slog.Logger
}

func New(cfg *config.Config, log *slog.Logger, tlsConfig *infratls.Config, lifecycle fx.Lifecycle) (*Server, error) {
	listener, err := net.Listen("tcp", cfg.Service.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpc server: listen on %s: %w", cfg.Service.Addr, err)
	}

	options := make([]grpc.ServerOption, 0, 1)
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
