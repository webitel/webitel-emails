package grpc

import (
	"go.uber.org/fx"

	emailpb "github.com/webitel/webitel-emails/api/email"
	grpcserver "github.com/webitel/webitel-emails/infra/server/grpc"
)

// Module provides and registers public gRPC handlers.
var Module = fx.Module(
	"grpc_handler",
	fx.Provide(NewEmailProfilesServer),
	fx.Invoke(RegisterEmailProfilesServer),
)

// RegisterEmailProfilesServer registers the Email Profile API before the gRPC server starts.
func RegisterEmailProfilesServer(server *grpcserver.Server, service *EmailProfilesServer) {
	emailpb.RegisterEmailProfilesServer(server.Server, service)
}
