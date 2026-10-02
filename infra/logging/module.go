// Package logging configures structured logging for the service.
package logging

import "go.uber.org/fx"

var Module = fx.Module(
	"logging",
	fx.Provide(New),
)
