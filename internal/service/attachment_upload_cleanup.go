package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/store"
)

// AttachmentUploadCleanup is the leader task that removes expired unused uploads.
type AttachmentUploadCleanup struct {
	interval time.Duration
	store    store.EmailAttachmentUploadStore
	log      *slog.Logger
}

const attachmentUploadCleanupBatch = 1000

// NewAttachmentUploadCleanup creates the cleanup task.
func NewAttachmentUploadCleanup(
	cfg *config.Config,
	s store.EmailAttachmentUploadStore,
	log *slog.Logger,
) *AttachmentUploadCleanup {
	return &AttachmentUploadCleanup{
		interval: cfg.Outbound.ReferenceCleanupInterval,
		store:    s,
		log:      log.With("component", "attachment_upload_cleanup"),
	}
}

func (c *AttachmentUploadCleanup) Name() string {
	return "attachment_upload_cleanup"
}

// Run removes expired unused uploads while this instance leads. Their files
// stay in storage until its own retention.
func (c *AttachmentUploadCleanup) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
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

func (c *AttachmentUploadCleanup) cleanup(ctx context.Context) {
	var total int64
	for ctx.Err() == nil {
		deleted, err := c.store.DeleteExpiredUnused(ctx, time.Now(), attachmentUploadCleanupBatch)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Error("attachment upload cleanup failed", "err", err)
			}

			break
		}

		total += deleted
		if deleted < attachmentUploadCleanupBatch {
			break
		}
	}

	if total > 0 {
		c.log.Info("expired attachment uploads removed", "deleted", total)
	}
}
