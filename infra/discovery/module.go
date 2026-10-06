// Package servicediscovery registers the service in Consul and elects its leader.
package servicediscovery

import "go.uber.org/fx"

var Module = fx.Module(
	"service_discovery",
	fx.Provide(
		NewInstanceID,
		NewProvider,
		NewLeaderElector,
		Register,
	),
	fx.Invoke(func(Registration) {}),
)
