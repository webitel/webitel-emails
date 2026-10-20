package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-emails/config"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
)

type fakeIssuer struct {
	mu        sync.Mutex
	ttl       time.Duration
	domain    int64
	active    atomic.Int32
	maxActive atomic.Int32
	delay     time.Duration
	err       error
	failOn    int64
	// doneErr replaces ctx.Err() when the delay is cut short, as a gRPC client does.
	doneErr error
}

func (f *fakeIssuer) DownloadLink(ctx context.Context, domainID, fileID int64, ttl time.Duration) (string, error) {
	n := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		m := f.maxActive.Load()
		if n <= m || f.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	f.ttl, f.domain = ttl, domainID
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			if f.doneErr != nil {
				return "", f.doneErr
			}
			return "", ctx.Err()
		}
	}
	if f.err != nil && (f.failOn == 0 || f.failOn == fileID) {
		return "", f.err
	}
	return fmt.Sprintf("https://storage.example/f/%d", fileID), nil
}

func newLinks(issuer *fakeIssuer, timeout time.Duration) *StorageAttachmentLinks {
	return NewStorageAttachmentLinks(issuer, config.StorageConfig{
		LinkTTL: 5 * time.Minute, LinkTimeout: timeout, MaxConcurrentLinks: 2,
	})
}

func TestInlineLinksAreBoundedAndComplete(t *testing.T) {
	issuer := &fakeIssuer{delay: 5 * time.Millisecond}
	links, err := newLinks(issuer, time.Second).InlineLinks(context.Background(), 3, []int64{1, 2, 3, 4, 5})
	if err != nil || len(links) != 5 || links[4] != "https://storage.example/f/4" {
		t.Fatalf("links %v, %v", links, err)
	}
	if issuer.maxActive.Load() > 2 || issuer.ttl != 5*time.Minute || issuer.domain != 3 {
		t.Fatalf("parallel %d, ttl %s, domain %d", issuer.maxActive.Load(), issuer.ttl, issuer.domain)
	}
}

func TestLinkErrorsAreClassified(t *testing.T) {
	cases := map[string]struct {
		issuer  *fakeIssuer
		timeout time.Duration
		cancel  bool
		want    error
	}{
		"unreachable storage": {issuer: &fakeIssuer{err: fmt.Errorf("%w: down", storageinfra.ErrUnavailable), failOn: 2},
			timeout: time.Second, want: ErrFileLinksUnavailable},
		"own time limit": {issuer: &fakeIssuer{delay: time.Second}, timeout: 10 * time.Millisecond,
			want: ErrFileLinksUnavailable},
		"own time limit as gRPC status": {issuer: &fakeIssuer{delay: time.Second,
			doneErr: status.Error(codes.DeadlineExceeded, "context deadline exceeded")},
			timeout: 10 * time.Millisecond, want: ErrFileLinksUnavailable},
		"caller cancelled": {issuer: &fakeIssuer{delay: time.Second}, timeout: time.Second, cancel: true,
			want: context.Canceled},
		"other error": {issuer: &fakeIssuer{err: storageinfra.ErrFileRejected}, timeout: time.Second,
			want: storageinfra.ErrFileRejected},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancel {
				time.AfterFunc(10*time.Millisecond, cancel)
			}
			links := newLinks(c.issuer, c.timeout)

			if _, err := links.InlineLinks(ctx, 1, []int64{1, 2}); !errors.Is(err, c.want) {
				t.Errorf("inline: %v, want %v", err, c.want)
			}
			if c.cancel {
				return
			}
			if _, _, err := links.Link(ctx, 1, 2); !errors.Is(err, c.want) {
				t.Errorf("link: %v, want %v", err, c.want)
			}
			if c.want != ErrFileLinksUnavailable {
				if _, err := links.InlineLinks(ctx, 1, []int64{1, 2}); errors.Is(err, ErrFileLinksUnavailable) {
					t.Errorf("%v was reported as unavailable", err)
				}
			}
		})
	}
}

func TestLinkReportsExpiryTakenBeforeTheRequest(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	links := newLinks(&fakeIssuer{}, time.Second)
	links.now = func() time.Time { return now }
	link, expiresAt, err := links.Link(context.Background(), 1, 9)
	if err != nil || link != "https://storage.example/f/9" || !expiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("link %q expires %s, %v", link, expiresAt, err)
	}
}
