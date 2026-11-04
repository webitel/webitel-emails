package projection

import (
	"context"
	"log/slog"
	"time"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/store"
)

const (
	actionCleanupInterval = time.Hour
	actionCleanupBatch    = 1000
)

// ActionCleanup is the leader task that deletes Thread actions older than their retention.
type ActionCleanup struct {
	retention time.Duration
	store     store.EmailThreadActionCleaner
	log       *slog.Logger
}

// NewActionCleanup creates the task; a zero retention keeps every action.
func NewActionCleanup(cfg *config.Config, s store.EmailThreadActionCleaner, log *slog.Logger) *ActionCleanup {
	return &ActionCleanup{
		retention: cfg.ThreadAction.Retention,
		store:     s,
		log:       log.With("component", "thread_action_cleanup"),
	}
}

func (c *ActionCleanup) Name() string {
	return "thread_action_cleanup"
}

// Run deletes old actions once an hour while this instance leads.
func (c *ActionCleanup) Run(ctx context.Context) error {
	if c.retention == 0 {
		return nil
	}

	ticker := time.NewTicker(actionCleanupInterval)
	defer ticker.Stop()
	for {
		c.cleanup(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// cleanup deletes in batches, so one statement never holds many row locks.
func (c *ActionCleanup) cleanup(ctx context.Context) {
	before := time.Now().Add(-c.retention)

	var total int64
	for ctx.Err() == nil {
		deleted, err := c.store.DeleteActionsBefore(ctx, before, actionCleanupBatch)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Error("thread action cleanup failed", "err", err)
			}

			break
		}

		total += deleted
		if deleted < actionCleanupBatch {
			break
		}
	}

	if total > 0 {
		c.log.Info("old thread actions removed", "deleted", total)
	}
}
