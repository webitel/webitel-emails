package delivery

import (
	"context"
	"log/slog"
	"time"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/store"
)

const recoveryBatch = 100

// Recovery is the leader task that finishes attempts whose worker is gone: before
// the commit the series may retry, after it the result is unknown.
type Recovery struct {
	interval   time.Duration
	batch      int
	deliveries store.EmailDeliveryStore
	log        *slog.Logger
}

// NewRecovery creates the recovery task.
func NewRecovery(cfg *config.Config, deliveries store.EmailDeliveryStore, log *slog.Logger) *Recovery {
	return &Recovery{
		interval: cfg.Outbound.DeliveryRecoveryInterval, batch: recoveryBatch, deliveries: deliveries,
		log: log.With("component", "outbound_delivery_recovery"),
	}
}

func (r *Recovery) Name() string {
	return "outbound_delivery_recovery"
}

// Run recovers expired attempts while this instance leads.
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

// Recover goes once through the attempts expired by now, in batches after a
// cursor. One it cannot take now, busy or failing, is left for the next run.
func (r *Recovery) Recover(ctx context.Context) {
	total := 0
	var after *store.EmailDeliveryCursor
	for ctx.Err() == nil {
		batch, err := r.deliveries.RecoverExpired(ctx, after, r.batch)
		total += batch.Recovered
		for _, failed := range batch.Failed {
			r.log.Error("recover outbound email", "err", failed)
		}
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("recover outbound emails", "err", err)
			}
			break
		}
		if batch.Scanned < r.batch {
			break
		}
		after = &batch.Next
	}
	if total > 0 {
		r.log.Warn("abandoned outbound emails recovered", "count", total)
	}
}
