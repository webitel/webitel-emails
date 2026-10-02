package service

import "go.uber.org/fx"

// Module provides application services.
var Module = fx.Module(
	"service",
	fx.Provide(NewEmailProfileService),
)
