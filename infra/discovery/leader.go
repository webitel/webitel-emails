package servicediscovery

import (
	"log/slog"

	"github.com/webitel/webitel-go-kit/infra/discovery/consul"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"
)

// NewLeaderElector elects one email-service instance through the Consul key service/email-service/leader.
func NewLeaderElector(cfg *config.Config, log *slog.Logger, instanceID model.InstanceID) (consul.LeadershipElector, error) {
	election := cfg.LeaderElection

	return consul.NewLeaderElector(
		cfg.Consul.Addr,
		model.ServiceName,
		string(instanceID),
		log,
		consul.WithSessionTTL(election.SessionTTL),
		consul.WithLockDelay(election.LockDelay),
		consul.WithRetryInterval(election.RetryInterval),
		consul.WithErrorCooldown(election.ErrorCooldown),
		consul.WithMonitorInterval(election.MonitorInterval),
	)
}
