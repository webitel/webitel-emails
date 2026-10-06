package postgres

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type outboxStore struct {
	db Querier
}

var _ store.OutboxStore = (*outboxStore)(nil)

// NewOutboxStore creates an Outbox store over the caller's querier.
func NewOutboxStore(db Querier) *outboxStore {
	return &outboxStore{db: db}
}

func (s *outboxStore) Append(ctx context.Context, event *model.OutboxEvent) (bool, error) {
	if _, ok := s.db.(pgx.Tx); !ok {
		return false, store.ErrOutboxNeedsTransaction
	}
	if err := event.Validate(); err != nil {
		return false, fmt.Errorf("store: %w", err)
	}

	// The conflict target repeats the partial index predicate, so only a
	// duplicate business event is skipped and any other conflict still fails.
	const query = `
INSERT INTO email.outbox (
    event_id, domain_id, event_type, schema_version, dedup_key, exchange, routing_key, payload
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (event_type, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING`

	result, err := s.db.Exec(
		ctx,
		query,
		event.EventID.String(),
		event.DomainID,
		event.EventType,
		event.SchemaVersion,
		nullIfEmpty(event.DedupKey),
		event.Exchange,
		event.RoutingKey,
		string(event.Payload),
	)
	if err != nil {
		return false, ParseError(err)
	}

	return result.RowsAffected() == 1, nil
}

type outboxRelayStore struct {
	db Querier
}

var _ store.OutboxRelayStore = (*outboxRelayStore)(nil)

// NewOutboxRelayStore creates the relay side of the Outbox over the pool.
func NewOutboxRelayStore(db Querier) *outboxRelayStore {
	return &outboxRelayStore{db: db}
}

func (s *outboxRelayStore) RecoverLeases(ctx context.Context, owner string, own bool) (int64, error) {
	const query = `
UPDATE email.outbox SET
    state = 'pending', locked_by = NULL, locked_until = NULL, updated_at = now()
WHERE state = 'processing' AND (locked_until < now() OR ($2 AND locked_by = $1))`

	return affectedRows(s.db.Exec(ctx, query, owner, own))
}

func (s *outboxRelayStore) Claim(
	ctx context.Context,
	owner string,
	limit int,
	lease time.Duration,
) ([]*model.OutboxEvent, error) {
	// SKIP LOCKED keeps a concurrent claim from waiting on or taking the same rows.
	const query = `
WITH due AS (
    SELECT event_id FROM email.outbox
    WHERE state = 'pending' AND next_attempt_at <= now()
    ORDER BY next_attempt_at, created_at, event_id
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE email.outbox o SET
    state = 'processing', locked_by = $1,
    locked_until = now() + $3 * interval '1 millisecond', updated_at = now()
FROM due
WHERE o.event_id = due.event_id
RETURNING o.event_id, o.domain_id, o.event_type, o.schema_version, o.dedup_key,
    o.exchange, o.routing_key, o.payload::text, o.state, o.attempts,
    o.next_attempt_at, o.locked_by, o.locked_until, o.created_at, o.updated_at`

	rows, err := s.db.Query(ctx, query, owner, limit, lease.Milliseconds())
	if err != nil {
		return nil, ParseError(err)
	}
	defer rows.Close()

	var events []*model.OutboxEvent
	for rows.Next() {
		var (
			event    model.OutboxEvent
			eventID  string
			dedupKey sql.NullString
			payload  string
			state    string
			lockedBy sql.NullString
			until    sql.NullTime
		)
		if err := rows.Scan(
			&eventID, &event.DomainID, &event.EventType, &event.SchemaVersion, &dedupKey,
			&event.Exchange, &event.RoutingKey, &payload, &state, &event.Attempts,
			&event.NextAttemptAt, &lockedBy, &until, &event.CreatedAt, &event.UpdatedAt,
		); err != nil {
			return nil, ParseError(err)
		}

		if event.EventID, err = uuid.Parse(eventID); err != nil {
			return nil, fmt.Errorf("store: outbox event id: %w", err)
		}
		event.DedupKey = dedupKey.String
		event.Payload = []byte(payload)
		event.State = model.OutboxEventState(state)
		event.LockedBy = lockedBy.String
		event.LockedUntil = nullTimeValue(until)
		events = append(events, &event)
	}
	if err := rows.Err(); err != nil {
		return nil, ParseError(err)
	}

	// RETURNING does not keep the order of the claim.
	slices.SortFunc(events, func(a, b *model.OutboxEvent) int {
		return cmp.Or(
			a.NextAttemptAt.Compare(b.NextAttemptAt),
			a.CreatedAt.Compare(b.CreatedAt),
			bytes.Compare(a.EventID[:], b.EventID[:]),
		)
	})

	return events, nil
}

func (s *outboxRelayStore) MarkPublished(ctx context.Context, owner string, eventID uuid.UUID) (bool, error) {
	const query = `
UPDATE email.outbox SET
    state = 'published', published_at = now(), locked_by = NULL, locked_until = NULL,
    last_error = NULL, updated_at = now()
WHERE event_id = $1 AND state = 'processing' AND locked_by = $2`

	affected, err := affectedRows(s.db.Exec(ctx, query, eventID.String(), owner))

	return affected == 1, err
}

func (s *outboxRelayStore) Release(
	ctx context.Context,
	owner string,
	eventIDs []uuid.UUID,
	nextAttemptAt time.Time,
) error {
	if len(eventIDs) == 0 {
		return nil
	}

	ids := make([]string, 0, len(eventIDs))
	for _, id := range eventIDs {
		ids = append(ids, id.String())
	}

	const query = `
UPDATE email.outbox SET
    state = 'pending', locked_by = NULL, locked_until = NULL,
    next_attempt_at = $3, updated_at = now()
WHERE event_id = ANY($2::uuid[]) AND state = 'processing' AND locked_by = $1`

	_, err := s.db.Exec(ctx, query, owner, ids, nextAttemptAt)

	return ParseError(err)
}

func (s *outboxRelayStore) MarkFailed(
	ctx context.Context,
	owner string,
	eventID uuid.UUID,
	reason string,
) (bool, error) {
	const query = `
UPDATE email.outbox SET
    state = 'failed', attempts = attempts + 1, last_error = left($3, 1024),
    locked_by = NULL, locked_until = NULL, updated_at = now()
WHERE event_id = $1 AND state = 'processing' AND locked_by = $2`

	affected, err := affectedRows(s.db.Exec(ctx, query, eventID.String(), owner, reason))

	return affected == 1, err
}

func (s *outboxRelayStore) DeletePublishedBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	const query = `
DELETE FROM email.outbox
WHERE event_id IN (
    SELECT event_id FROM email.outbox
    WHERE state = 'published' AND published_at < $1
    ORDER BY published_at
    LIMIT $2
)`

	return affectedRows(s.db.Exec(ctx, query, before, limit))
}

func (s *outboxRelayStore) Backlog(ctx context.Context) (store.OutboxBacklog, error) {
	// One branch per state, so each count reads only its partial index.
	const query = `
SELECT
    (SELECT count(*) FROM email.outbox WHERE state = 'pending'),
    (SELECT count(*) FROM email.outbox WHERE state = 'processing'),
    (SELECT count(*) FROM email.outbox WHERE state = 'failed'),
    COALESCE(EXTRACT(EPOCH FROM now() - LEAST(
        (SELECT min(created_at) FROM email.outbox WHERE state = 'pending'),
        (SELECT min(created_at) FROM email.outbox WHERE state = 'processing')
    )), 0)`

	var (
		backlog store.OutboxBacklog
		seconds float64
	)
	if err := s.db.QueryRow(ctx, query).Scan(
		&backlog.Pending, &backlog.Processing, &backlog.Failed, &seconds,
	); err != nil {
		return store.OutboxBacklog{}, ParseError(err)
	}
	backlog.OldestUndelivered = time.Duration(seconds * float64(time.Second))

	return backlog, nil
}

func affectedRows(result pgconn.CommandTag, err error) (int64, error) {
	if err != nil {
		return 0, ParseError(err)
	}

	return result.RowsAffected(), nil
}
