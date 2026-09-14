// Package servicediscovery registers the service in Consul.
package servicediscovery

import (
	kitdiscovery "github.com/webitel/webitel-go-kit/infra/discovery"
	"go.uber.org/fx"
)

var Module = fx.Module(
	"service_discovery",
	fx.Provide(New),
	fx.Invoke(func(kitdiscovery.DiscoveryProvider) {}),
)
