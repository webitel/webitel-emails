package postgres

import (
	"context"
	"database/sql"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailWorkspaceEventStore struct {
	db      Querier
	enabled bool
}

var _ store.EmailWorkspaceEventStore = (*emailWorkspaceEventStore)(nil)

// NewEmailWorkspaceEventStore creates the writer over the caller's transaction;
// enabled decides only whether events are written.
func NewEmailWorkspaceEventStore(db Querier, enabled bool) *emailWorkspaceEventStore {
	return &emailWorkspaceEventStore{db: db, enabled: enabled}
}

func (s *emailWorkspaceEventStore) Record(ctx context.Context, change store.EmailWorkspaceChange) (int, error) {
	if _, ok := s.db.(pgx.Tx); !ok {
		return 0, store.ErrOutboxNeedsTransaction
	}

	return recordWorkspaceChange(ctx, s.db, s.enabled, change)
}

// recordWorkspaceChange raises the version under the row lock of the Thread, so
// changes of one Thread are serialized and each recipient set is complete.
func recordWorkspaceChange(ctx context.Context, q Querier, enabled bool, c store.EmailWorkspaceChange) (int, error) {
	const bump = `
UPDATE email.thread SET workspace_version = workspace_version + 1
WHERE domain_id = $1 AND id = $2 AND kind = 'regular'
RETURNING workspace_version, projection_version, last_message_at IS NOT NULL`

	var (
		version    int64
		projection sql.NullInt64
		readable   bool
	)
	err := q.QueryRow(ctx, bump, c.DomainID, c.ThreadID).Scan(&version, &projection, &readable)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		// A service Thread has no version and no events.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("store: raise workspace version: %w", ParseError(err))
	case !enabled || !readable:
		return 0, nil
	}

	// The access of each recipient after the change, by the read API rules.
	const recipients = `
SELECT r.user_id,
    CASE WHEN r.user_id = t.current_user_id THEN 'current'
         WHEN EXISTS (
             SELECT 1 FROM email.thread_participant tp
             WHERE tp.domain_id = t.domain_id AND tp.thread_id = t.id AND tp.user_id = r.user_id)
         THEN 'history'
         ELSE 'none' END
FROM email.thread t
CROSS JOIN LATERAL (
    SELECT t.current_user_id AS user_id WHERE t.current_user_id IS NOT NULL
    UNION
    SELECT $3::bigint WHERE $3::bigint IS NOT NULL
    UNION
    SELECT tp.user_id FROM email.thread_participant tp
    WHERE tp.domain_id = t.domain_id AND tp.thread_id = t.id
) r
WHERE t.domain_id = $1 AND t.id = $2
ORDER BY r.user_id`

	rows, err := q.Query(ctx, recipients, c.DomainID, c.ThreadID, c.PreviousUserID)
	if err != nil {
		return 0, fmt.Errorf("store: read workspace recipients: %w", ParseError(err))
	}

	occurredAt := c.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	var events []*model.OutboxEvent
	for rows.Next() {
		var (
			userID int64
			access string
		)
		if err := rows.Scan(&userID, &access); err != nil {
			rows.Close()

			return 0, fmt.Errorf("store: read workspace recipient: %w", err)
		}

		e := event.WorkspaceEvent{
			EventType: c.EventType, OccurredAt: occurredAt, DomainID: c.DomainID, UserID: userID,
			ThreadID: c.ThreadID, Access: access, WorkspaceVersion: version, MessageID: c.MessageID,
			Changes: c.Changes,
		}
		if projection.Valid {
			e.ProjectionVersion = &projection.Int64
		}
		if access == event.WorkspaceAccessNone {
			e.Changes = nil
		}

		row, err := event.NewWorkspaceOutboxEvent(e)
		if err != nil {
			rows.Close()

			return 0, err
		}
		events = append(events, row)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: read workspace recipients: %w", ParseError(err))
	}
	if len(events) == 0 {
		return 0, nil
	}

	return appendOutboxBatch(ctx, q, events)
}

// appendOutboxBatch stores rows of one domain and type in one statement, each with
// the routing key of its recipient; a row with a stored dedup key is skipped, as in
// Append.
func appendOutboxBatch(ctx context.Context, q Querier, events []*model.OutboxEvent) (int, error) {
	ids := make([]string, len(events))
	keys := make([]string, len(events))
	routes := make([]string, len(events))
	payloads := make([]string, len(events))
	for i, e := range events {
		ids[i], keys[i], routes[i], payloads[i] = e.EventID.String(), e.DedupKey, e.RoutingKey, string(e.Payload)
	}
	first := events[0]

	const query = `
INSERT INTO email.outbox (
    event_id, domain_id, event_type, schema_version, dedup_key, exchange, routing_key, payload
)
SELECT r.event_id, $5, $6, $7, r.dedup_key, $8, r.routing_key, r.payload::json
FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[]) AS r(event_id, dedup_key, routing_key, payload)
ON CONFLICT (event_type, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING`

	tag, err := q.Exec(ctx, query, ids, keys, routes, payloads,
		first.DomainID, first.EventType, first.SchemaVersion, first.Exchange)
	if err != nil {
		return 0, fmt.Errorf("store: append workspace events: %w", ParseError(err))
	}

	return int(tag.RowsAffected()), nil
}
