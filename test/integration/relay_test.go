//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/webitel/webitel-go-kit/infra/pubsub/rabbitmq"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/outbox"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// fakeBroker records what the relay sends; errors switch it into an outage.
type fakeBroker struct {
	mu         sync.Mutex
	declareErr error
	publishErr error
	declares   int
	published  []uuid.UUID
}

func (b *fakeBroker) DeclareTopology(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.declares++

	return b.declareErr
}

func (b *fakeBroker) Publish(_ context.Context, event *model.OutboxEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.publishErr != nil {
		return b.publishErr
	}
	b.published = append(b.published, event.EventID)

	return nil
}

func (b *fakeBroker) set(declareErr, publishErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.declareErr, b.publishErr = declareErr, publishErr
}

func (b *fakeBroker) counts() (declares, published int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.declares, len(b.published)
}

// publishedTimes counts the publications of the given events.
func (b *fakeBroker) publishedTimes(ids []uuid.UUID) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	total := 0
	for _, published := range b.published {
		if slices.Contains(ids, published) {
			total++
		}
	}

	return total
}

// flakyStore fails the next Release, or the next MarkPublished with an error or
// a lost lease, once each; the failure leaves the database untouched.
type flakyStore struct {
	store.OutboxRelayStore

	failRelease    atomic.Int32
	failedReleases atomic.Int32
	failMark       atomic.Int32
	loseMark       atomic.Int32
	failFailure    atomic.Int32
	loseFailure    atomic.Int32
}

func (s *flakyStore) Release(ctx context.Context, owner string, ids []uuid.UUID, next time.Time) error {
	if s.failRelease.Add(-1) >= 0 {
		s.failedReleases.Add(1)

		return errors.New("database unavailable")
	}

	return s.OutboxRelayStore.Release(ctx, owner, ids, next)
}

func (s *flakyStore) MarkPublished(ctx context.Context, owner string, id uuid.UUID) (bool, error) {
	if s.failMark.Add(-1) >= 0 {
		return false, errors.New("database unavailable")
	}
	if s.loseMark.Add(-1) >= 0 {
		return false, nil
	}

	return s.OutboxRelayStore.MarkPublished(ctx, owner, id)
}

func (s *flakyStore) MarkFailed(ctx context.Context, owner string, id uuid.UUID, reason string) (bool, error) {
	if s.failFailure.Add(-1) >= 0 {
		return false, errors.New("database unavailable")
	}
	if s.loseFailure.Add(-1) >= 0 {
		return false, nil
	}

	return s.OutboxRelayStore.MarkFailed(ctx, owner, id, reason)
}

// insertOutboxEvents adds rows in one statement, so the relay claims them
// together and in the given order.
func insertOutboxEvents(t *testing.T, db *pgxpool.Pool, payloads ...string) []uuid.UUID {
	t.Helper()

	ids := make([]string, len(payloads))
	result := make([]uuid.UUID, len(payloads))
	for i := range payloads {
		result[i] = uuid.Must(uuid.NewV7())
		ids[i] = result[i].String()
	}

	if _, err := db.Exec(context.Background(), `
INSERT INTO email.outbox (event_id, domain_id, event_type, schema_version, exchange, routing_key, payload, created_at)
SELECT e.id::uuid, 1, 'email.message.inbound', 1, 'email.events', 'email.message.inbound.v1', e.payload::json,
    now() + e.position * interval '1 millisecond'
FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS e(id, payload, position)`,
		ids, payloads); err != nil {
		t.Fatalf("insert outbox events: %v", err)
	}

	return result
}

func insertOutboxEvent(t *testing.T, db *pgxpool.Pool, payload string) uuid.UUID {
	t.Helper()

	id := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(context.Background(), `
INSERT INTO email.outbox (event_id, domain_id, event_type, schema_version, exchange, routing_key, payload)
VALUES ($1, 1, 'email.message.inbound', 1, 'email.events', 'email.message.inbound.v1', $2)`,
		id.String(), payload); err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}

	return id
}

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("timed out waiting until %s", what)
}

// The relay must never lose an event or spend attempts on a broker outage, and
// one bad row must not hold back the rest.
func TestOutboxRelay(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)

	cfg := &config.Config{
		InboundEvent: config.InboundEventConfig{MaxPayloadSize: 64},
		Outbox: config.OutboxConfig{
			PublishTimeout: time.Second, PollInterval: 10 * time.Millisecond, BatchSize: 10,
			LeaseDuration: time.Minute, RetryInitialInterval: 20 * time.Millisecond, RetryMaxInterval: 40 * time.Millisecond,
			Retention: time.Hour, CleanupInterval: time.Hour, CleanupBatch: 10,
		},
	}
	broker := &fakeBroker{}
	flaky := &flakyStore{OutboxRelayStore: storepostgres.NewOutboxRelayStore(db)}
	relay := outbox.New(cfg, flaky, broker, "relay-1",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	states := func(state string) int {
		return count(t, db, `SELECT count(*) FROM email.outbox WHERE state = $1`, state)
	}

	// Seeded before the relay starts, so the outage below sees them first.
	first := insertOutboxEvent(t, db, `{"n":1}`)
	insertOutboxEvent(t, db, `{"n":2}`)
	broker.set(rabbitmq.ErrConnectionNotAvailable, nil)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = relay.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })

	t.Run("nothing is claimed until the exchange is declared", func(t *testing.T) {
		eventually(t, "the declaration is retried", func() bool { d, _ := broker.counts(); return d >= 3 })
		if _, published := broker.counts(); published != 0 || states("pending") != 2 {
			t.Fatalf("published %d, pending %d", published, states("pending"))
		}
	})

	t.Run("a broker outage releases the batch without spending attempts", func(t *testing.T) {
		broker.set(nil, rabbitmq.ErrPublishTimeout)
		declares, _ := broker.counts()

		eventually(t, "the exchange is declared again after the failure", func() bool {
			d, _ := broker.counts()
			return d >= declares+2
		})
		if spent := count(t, db, `SELECT count(*) FROM email.outbox WHERE attempts > 0`); spent != 0 {
			t.Fatalf("%d rows spent an attempt on an outage", spent)
		}

		broker.set(nil, nil)
		eventually(t, "both events are published", func() bool { return states("published") == 2 })
	})

	t.Run("an unpublishable row fails without blocking the next", func(t *testing.T) {
		oversized := insertOutboxEvent(t, db, `{"n":"`+strings.Repeat("x", 100)+`"}`)
		next := insertOutboxEvent(t, db, `{"n":3}`)

		eventually(t, "the next row is published", func() bool {
			return count(t, db, `SELECT count(*) FROM email.outbox WHERE event_id = $1 AND state = 'published'`, next.String()) == 1
		})
		if count(t, db, `
SELECT count(*) FROM email.outbox
WHERE event_id = $1 AND state = 'failed' AND attempts = 1 AND last_error LIKE 'payload of%'`, oversized.String()) != 1 {
			t.Fatal("the oversized row was not marked failed")
		}
	})

	t.Run("an expired lease of another relay is taken over", func(t *testing.T) {
		if _, err := db.Exec(context.Background(), `
UPDATE email.outbox SET state = 'processing', published_at = NULL,
    locked_by = 'gone', locked_until = now() - interval '1 second'
WHERE event_id = $1`, first.String()); err != nil {
			t.Fatalf("expire lease: %v", err)
		}

		eventually(t, "the abandoned row is published again", func() bool {
			return count(t, db, `SELECT count(*) FROM email.outbox WHERE event_id = $1 AND state = 'published'`, first.String()) == 1
		})
	})

	// The lease is a minute, so each case below passes only through recovery.
	t.Run("a failed release does not keep the batch locked", func(t *testing.T) {
		// Failures are armed before the rows exist, so the relay cannot get there first.
		flaky.failRelease.Store(1)
		broker.set(nil, rabbitmq.ErrConnectionNotAvailable)
		ids := insertOutboxEvents(t, db, "{}", "{}")

		eventually(t, "the release fails", func() bool { return flaky.failedReleases.Load() == 1 })
		broker.set(nil, nil)
		eventually(t, "the batch is published without waiting for its lease", func() bool {
			return broker.publishedTimes(ids) == 2
		})
	})

	for name, fail := range map[string]*atomic.Int32{"failed": &flaky.failMark, "lost": &flaky.loseMark} {
		t.Run("a "+name+" publish mark stops the batch", func(t *testing.T) {
			fail.Store(1)
			ids := insertOutboxEvents(t, db, "{}", "{}", "{}")

			eventually(t, "every event is published", func() bool {
				return count(t, db, `SELECT count(*) FROM email.outbox WHERE event_id = ANY($1::uuid[]) AND state = 'published'`,
					uuidStrings(ids)) == 3
			})
			// Only the event whose mark was missed goes out twice.
			if times := broker.publishedTimes(ids); times != 4 {
				t.Fatalf("events published %d times, want 4", times)
			}
		})
	}

	for name, fail := range map[string]*atomic.Int32{"failed": &flaky.failFailure, "lost": &flaky.loseFailure} {
		t.Run("a "+name+" failure mark stops the batch", func(t *testing.T) {
			fail.Store(1)
			ids := insertOutboxEvents(t, db, `{"n":"`+strings.Repeat("x", 100)+`"}`, "{}", "{}")

			eventually(t, "the oversized row fails and the rest are published", func() bool {
				return count(t, db, `SELECT count(*) FROM email.outbox WHERE event_id = $1 AND state = 'failed'`, ids[0].String()) == 1 &&
					count(t, db, `SELECT count(*) FROM email.outbox WHERE event_id = ANY($1::uuid[]) AND state = 'published'`,
						uuidStrings(ids[1:])) == 2
			})
			if times := broker.publishedTimes(ids[1:]); times != 2 {
				t.Fatalf("events published %d times, want once each", times)
			}
		})
	}
}

func uuidStrings(ids []uuid.UUID) []string {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		result = append(result, id.String())
	}

	return result
}
