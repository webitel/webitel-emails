package delivery

import (
	"context"
	"log/slog"
	"time"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/store"
)

const recoveryBatch = 100

// Recovery is the leader task that finishes attempts whose worker is gone.
type Recovery struct {
	interval   time.Duration
	deliveries store.EmailDeliveryStore
	log        *slog.Logger
}

// NewRecovery creates the recovery task.
func NewRecovery(cfg *config.Config, deliveries store.EmailDeliveryStore, log *slog.Logger) *Recovery {
	return &Recovery{
		interval: cfg.Outbound.DeliveryRecoveryInterval, deliveries: deliveries,
		log: log.With("component", "outbound_delivery_recovery"),
	}
}

func (r *Recovery) Name() string {
	return "outbound_delivery_recovery"
}

// Run fails expired attempts while this instance leads.
func (r *Recovery) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		r.Recover(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Recover fails every expired attempt in batches.
func (r *Recovery) Recover(ctx context.Context) {
	total := 0
	for ctx.Err() == nil {
		recovered, err := r.deliveries.RecoverExpired(ctx, recoveryBatch)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("recover outbound emails", "err", err)
			}
			break
		}
		total += recovered
		if recovered < recoveryBatch {
			break
		}
	}
	if total > 0 {
		r.log.Warn("abandoned outbound emails failed", "count", total)
	}
}
