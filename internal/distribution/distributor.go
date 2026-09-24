// Package distribution assigns Email Profiles to healthy email-service instances.
package distribution

import (
	"context"
	"log/slog"
	"time"

	kitdiscovery "github.com/webitel/webitel-go-kit/infra/discovery"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// Distributor is a leader task that keeps every enabled profile owned by one healthy instance.
type Distributor struct {
	store     store.EmailProfileRuntimeStore
	discovery kitdiscovery.Discovery
	interval  time.Duration
	log       *slog.Logger
}

// New creates the profile distributor.
func New(
	cfg *config.Config,
	runtimeStore store.EmailProfileRuntimeStore,
	discovery kitdiscovery.DiscoveryProvider,
	log *slog.Logger,
) *Distributor {
	return &Distributor{
		store:     runtimeStore,
		discovery: discovery,
		interval:  cfg.ProfileDistribution.ReconcileInterval,
		log:       log.With("component", "profile_distribution"),
	}
}

// Name identifies the task in logs.
func (d *Distributor) Name() string {
	return "profile_distribution"
}

// Run rebalances on instance changes and periodically, to pick up profile changes.
func (d *Distributor) Run(ctx context.Context) error {
	changes := d.watchInstances(ctx)

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		d.reconcile(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-changes:
		}
	}
}

// watchInstances signals when the set of healthy instances changes; the ticker covers watch failures.
func (d *Distributor) watchInstances(ctx context.Context) <-chan struct{} {
	changes := make(chan struct{}, 1)

	watcher, err := d.discovery.GetWatcher(ctx, model.ServiceName)
	if err != nil {
		d.log.Warn("watch email-service instances", "err", err)

		return changes
	}

	go func() {
		defer watcher.Stop()

		for {
			if _, err := watcher.Next(); err != nil {
				return
			}

			select {
			case changes <- struct{}{}:
			default:
			}
		}
	}()

	return changes
}

// reconcile applies one distribution plan; failed changes are retried on the next run.
func (d *Distributor) reconcile(ctx context.Context) {
	services, err := d.discovery.GetService(ctx, model.ServiceName)
	if err != nil {
		d.log.Warn("list healthy email-service instances", "err", err)

		return
	}

	instances := make([]string, 0, len(services))
	for _, service := range services {
		instances = append(instances, service.Id)
	}

	// Keeps assignments when Consul briefly reports no instances.
	if len(instances) == 0 {
		d.log.Warn("no healthy email-service instances, assignments are kept")

		return
	}

	profiles, err := d.store.ListOwnership(ctx)
	if err != nil {
		d.log.Error("list email profile ownership", "err", err)

		return
	}

	changes := planDistribution(instances, profiles)

	for _, profileID := range changes.unassign {
		if err := d.store.Unassign(ctx, profileID); err != nil {
			d.log.Error("unassign email profile", "profile_id", profileID, "err", err)
		}
	}

	for _, change := range changes.assign {
		assigned, err := d.store.Assign(ctx, change.profileID, change.instanceID)
		if err != nil {
			d.log.Error("assign email profile", "profile_id", change.profileID, "instance_id", change.instanceID, "err", err)

			continue
		}

		d.log.Info(
			"email profile assigned",
			"profile_id", assigned.ProfileID,
			"instance_id", assigned.OwnerInstanceID,
			"generation", assigned.Generation,
		)
	}

	if len(changes.assign) > 0 || len(changes.unassign) > 0 {
		d.log.Info(
			"email profiles rebalanced",
			"instances", len(instances),
			"assigned", len(changes.assign),
			"unassigned", len(changes.unassign),
		)
	}
}
