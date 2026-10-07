// Package servicediscovery registers the service in Consul and elects its leader.
package servicediscovery

import (
	kitdiscovery "github.com/webitel/webitel-go-kit/infra/discovery"
	"go.uber.org/fx"
)

var Module = fx.Module(
	"service_discovery",
	fx.Provide(
		NewInstanceID,
		New,
		NewLeaderElector,
	),
	fx.Invoke(func(kitdiscovery.DiscoveryProvider) {}),
)
