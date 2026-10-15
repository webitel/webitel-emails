// Package grpcserver provides the internal gRPC server.
package grpcserver

import "go.uber.org/fx"

var Module = fx.Module(
	"grpc_server",
	fx.Provide(New),
	fx.Invoke(func(*Server) {}),
)
