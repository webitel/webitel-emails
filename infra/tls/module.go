// Package tls provides TLS configuration for the internal gRPC server.
package tls

import "go.uber.org/fx"

var Module = fx.Module(
	"tls",
	fx.Provide(New),
)
