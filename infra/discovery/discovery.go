package servicediscovery

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	kitdiscovery "github.com/webitel/webitel-go-kit/infra/discovery"
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	grpcserver "github.com/webitel/webitel-emails/infra/server/grpc"
	"github.com/webitel/webitel-emails/internal/model"

	_ "github.com/webitel/webitel-go-kit/infra/discovery/consul"
)

// NewInstanceID returns the ID shared by Consul registration, leader election and profile ownership.
func NewInstanceID() model.InstanceID {
	return model.InstanceID(kitdiscovery.GenerateInstanceID(model.ServiceName))
}

func New(
	cfg *config.Config,
	log *slog.Logger,
	instanceID model.InstanceID,
	_ *grpcserver.Server,
	lifecycle fx.Lifecycle,
) (kitdiscovery.DiscoveryProvider, error) {
	provider, err := kitdiscovery.DefaultFactory.CreateProvider(
		kitdiscovery.ProviderConsul,
		log,
		cfg.Consul.Addr,
		kitdiscovery.WithHeartbeat[kitdiscovery.DiscoveryProvider](true),
		kitdiscovery.WithTimeout[kitdiscovery.DiscoveryProvider](30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("service discovery: create Consul provider: %w", err)
	}

	instance := &kitdiscovery.ServiceInstance{
		Id:      string(instanceID),
		Name:    model.ServiceName,
		Version: model.Version,
		Metadata: map[string]string{
			"commit":         model.Commit,
			"commitDate":     model.CommitDate,
			"branch":         model.Branch,
			"buildTimestamp": model.BuildTimestamp,
		},
		Endpoints: []string{(&url.URL{Scheme: "grpc", Host: cfg.Service.Addr}).String()},
	}
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := provider.Register(ctx, instance); err != nil {
				return fmt.Errorf("service discovery: register %s: %w", instance.Name, err)
			}
			log.Info(
				"service registered in Consul",
				"service", instance.Name,
				"instance_id", instance.Id,
				"endpoints", instance.Endpoints,
			)

			return nil
		},
		OnStop: func(ctx context.Context) error {
			if err := provider.Deregister(ctx, instance); err != nil {
				return fmt.Errorf("service discovery: deregister %s: %w", instance.Name, err)
			}
			log.Info(
				"service deregistered from Consul",
				"service", instance.Name,
				"instance_id", instance.Id,
			)

			return nil
		},
	})

	return provider, nil
}
