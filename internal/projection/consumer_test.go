package projection

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/store"
)

type projectionStoreStub struct {
	calls int
	apply func(context.Context) (store.EmailThreadProjectionResult, error)
}

func (s *projectionStoreStub) Apply(ctx context.Context, _ *store.EmailThreadProjection) (store.EmailThreadProjectionResult, error) {
	s.calls++
	return s.apply(ctx)
}

type projectionAckStub struct {
	acked, nacked int
	requeue       bool
}

func (a *projectionAckStub) Ack(uint64, bool) error { a.acked++; return nil }
func (a *projectionAckStub) Nack(_ uint64, _ bool, requeue bool) error {
	a.nacked++
	a.requeue = requeue
	return nil
}
func (a *projectionAckStub) Reject(uint64, bool) error { return nil }

func projectionDelivery(t *testing.T, ack amqp.Acknowledger) amqp.Delivery {
	t.Helper()
	id := uuid.New()
	e := event.AttemptSnapshot{
		EventID: id, EventType: event.TypeAttemptSnapshot, SchemaVersion: event.AttemptSnapshotSchema,
		OccurredAt: time.Now().UTC(), DomainID: 1, ThreadID: 7, ProjectionVersion: 10,
		Transition: event.TransitionInboundReopened, Source: "system",
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return amqp.Delivery{
		Acknowledger: ack, DeliveryTag: 1, Headers: amqp.Table{event.HeaderEventID: id.String()}, Body: data,
	}
}

func TestConsumerSettlesOnlyAfterProjectionResult(t *testing.T) {
	newConsumer := func(s store.EmailThreadProjectionStore) *Consumer {
		return &Consumer{
			store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			cfg: config.ThreadProjectionConfig{RetryInitialInterval: time.Millisecond, RetryMaxInterval: time.Millisecond},
		}
	}

	t.Run("temporary database failure retries before ACK", func(t *testing.T) {
		ack := &projectionAckStub{}
		s := &projectionStoreStub{}
		s.apply = func(context.Context) (store.EmailThreadProjectionResult, error) {
			if ack.acked != 0 {
				t.Fatal("delivery was acknowledged before the retry committed")
			}
			if s.calls == 1 {
				return store.EmailThreadProjectionResult{}, errors.New("database unavailable")
			}
			return store.EmailThreadProjectionResult{Outcome: store.EmailThreadProjectionApplied}, nil
		}
		if err := newConsumer(s).handle(context.Background(), projectionDelivery(t, ack)); err != nil {
			t.Fatal(err)
		}
		if s.calls != 2 || ack.acked != 1 || ack.nacked != 0 {
			t.Fatalf("calls=%d ACK=%d NACK=%d", s.calls, ack.acked, ack.nacked)
		}
	})

	for _, tc := range []struct {
		name    string
		outcome store.EmailThreadProjectionOutcome
	}{
		{"missing thread", store.EmailThreadProjectionNotFound},
		{"service thread", store.EmailThreadProjectionService},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ack := &projectionAckStub{}
			s := &projectionStoreStub{apply: func(context.Context) (store.EmailThreadProjectionResult, error) {
				return store.EmailThreadProjectionResult{Outcome: tc.outcome}, nil
			}}
			if err := newConsumer(s).handle(context.Background(), projectionDelivery(t, ack)); err != nil {
				t.Fatal(err)
			}
			if ack.acked != 0 || ack.nacked != 1 || ack.requeue {
				t.Fatalf("ACK=%d NACK=%d requeue=%v", ack.acked, ack.nacked, ack.requeue)
			}
		})
	}

	t.Run("permanent database rejection goes to DLQ", func(t *testing.T) {
		ack := &projectionAckStub{}
		s := &projectionStoreStub{apply: func(context.Context) (store.EmailThreadProjectionResult, error) {
			return store.EmailThreadProjectionResult{}, store.ErrEmailThreadProjectionRejected
		}}
		if err := newConsumer(s).handle(context.Background(), projectionDelivery(t, ack)); err != nil {
			t.Fatal(err)
		}
		if s.calls != 1 || ack.acked != 0 || ack.nacked != 1 || ack.requeue {
			t.Fatalf("calls=%d ACK=%d NACK=%d requeue=%v", s.calls, ack.acked, ack.nacked, ack.requeue)
		}
	})

	t.Run("invalid contract goes to DLQ without a store call", func(t *testing.T) {
		ack := &projectionAckStub{}
		d := projectionDelivery(t, ack)
		d.Body = []byte(`{"schema_version":2}`)
		s := &projectionStoreStub{apply: func(context.Context) (store.EmailThreadProjectionResult, error) {
			t.Fatal("invalid event reached the store")
			return store.EmailThreadProjectionResult{}, nil
		}}
		if err := newConsumer(s).handle(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		if s.calls != 0 || ack.acked != 0 || ack.nacked != 1 || ack.requeue {
			t.Fatalf("calls=%d ACK=%d NACK=%d requeue=%v", s.calls, ack.acked, ack.nacked, ack.requeue)
		}
	})

	t.Run("cancelled database retry leaves delivery unsettled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ack := &projectionAckStub{}
		s := &projectionStoreStub{apply: func(context.Context) (store.EmailThreadProjectionResult, error) {
			cancel()
			return store.EmailThreadProjectionResult{}, errors.New("database unavailable")
		}}
		if err := newConsumer(s).handle(ctx, projectionDelivery(t, ack)); err != nil {
			t.Fatal(err)
		}
		if s.calls != 1 || ack.acked != 0 || ack.nacked != 0 {
			t.Fatalf("calls=%d ACK=%d NACK=%d", s.calls, ack.acked, ack.nacked)
		}
	})
}
