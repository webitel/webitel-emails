package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/webitel/webitel-emails/config"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
)

// FileLinkIssuer signs a download link to one stored file.
type FileLinkIssuer interface {
	DownloadLink(ctx context.Context, domainID, fileID int64, ttl time.Duration) (string, error)
}

// StorageAttachmentLinks issues links through storage with the configured life,
// time limit and parallelism. Links are never logged or stored.
type StorageAttachmentLinks struct {
	files   FileLinkIssuer
	ttl     time.Duration
	timeout time.Duration
	slots   int
	now     func() time.Time
}

var _ AttachmentLinker = (*StorageAttachmentLinks)(nil)

// NewStorageAttachmentLinks creates the storage-backed linker.
func NewStorageAttachmentLinks(files FileLinkIssuer, cfg config.StorageConfig) *StorageAttachmentLinks {
	return &StorageAttachmentLinks{
		files:   files,
		ttl:     cfg.LinkTTL,
		timeout: cfg.LinkTimeout,
		slots:   cfg.MaxConcurrentLinks,
		now:     time.Now,
	}
}

// InlineLinks gets every link of a page within one time limit; the first
// failure stops the rest, so a page is linked completely or not at all.
func (l *StorageAttachmentLinks) InlineLinks(
	ctx context.Context,
	domainID int64,
	fileIDs []int64,
) (map[int64]string, error) {
	linkCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	var mu sync.Mutex
	links := make(map[int64]string, len(fileIDs))
	group, groupCtx := errgroup.WithContext(linkCtx)
	group.SetLimit(l.slots)
	for _, fileID := range fileIDs {
		group.Go(func() error {
			link, err := l.files.DownloadLink(groupCtx, domainID, fileID, l.ttl)
			if err != nil {
				return err
			}
			mu.Lock()
			links[fileID] = link
			mu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, l.classify(ctx, linkCtx, err)
	}

	return links, nil
}

// Link gets one link. The expiry is taken before the request, so the link
// stops working at that time or later, never earlier than reported.
func (l *StorageAttachmentLinks) Link(ctx context.Context, domainID, fileID int64) (string, time.Time, error) {
	expiresAt := l.now().Add(l.ttl)

	linkCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	link, err := l.files.DownloadLink(linkCtx, domainID, fileID, l.ttl)
	if err != nil {
		return "", time.Time{}, l.classify(ctx, linkCtx, err)
	}

	return link, expiresAt, nil
}

// classify returns the caller's own cancellation as is; storage down or the linker's
// time limit (read from linkCtx, a gRPC status otherwise) means unavailable.
func (l *StorageAttachmentLinks) classify(ctx, linkCtx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if linkCtx.Err() != nil || errors.Is(err, storageinfra.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrFileLinksUnavailable, err)
	}

	return err
}
