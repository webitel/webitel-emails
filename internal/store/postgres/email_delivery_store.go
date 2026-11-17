package postgres

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailDeliveryStore struct {
	db              Database
	workspaceEvents bool
}

var _ store.EmailDeliveryStore = (*emailDeliveryStore)(nil)

// NewEmailDeliveryStore creates the store of SMTP attempts; workspaceEvents decides
// only whether events are written.
func NewEmailDeliveryStore(db Database, workspaceEvents bool) *emailDeliveryStore {
	return &emailDeliveryStore{db: db, workspaceEvents: workspaceEvents}
}

func (s *emailDeliveryStore) Claim(ctx context.Context, lease time.Duration, skip []int64) (*store.EmailDelivery, error) {
	if skip == nil {
		skip = []int64{}
	}

	var d store.EmailDelivery
	err := s.db.QueryRow(ctx, `
UPDATE email.message m SET
    delivery_phase = 'sending',
    delivery_attempts = m.delivery_attempts + 1,
    delivery_lease_until = now() + $1::bigint * interval '1 microsecond',
    updated_at = now()
WHERE (m.domain_id, m.id) = (
    SELECT q.domain_id, q.id FROM email.message q
    WHERE q.delivery_status = 'pending' AND q.delivery_phase = 'queued' AND NOT q.profile_id = ANY($2)
    ORDER BY q.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED)
RETURNING m.domain_id, m.id, m.thread_id, m.profile_id, m.delivery_attempts`,
		lease.Microseconds(), skip).Scan(&d.DomainID, &d.MessageID, &d.ThreadID, &d.ProfileID, &d.Token)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: claim delivery: %w", ParseError(err))
	}

	return &d, nil
}

// Commit locks the message first and checks the lease by the clock after the lock:
// now() would be the start of a transaction that may have waited.
func (s *emailDeliveryStore) Commit(ctx context.Context, d store.EmailDelivery) (bool, error) {
	committed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.message WHERE domain_id = $1 AND id = $2 FOR UPDATE`,
			d.DomainID, d.MessageID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
UPDATE email.message SET delivery_phase = 'committed', updated_at = now()
WHERE domain_id = $1 AND id = $2 AND delivery_attempts = $3
    AND delivery_status = 'pending' AND delivery_phase = 'sending'
    AND delivery_lease_until > clock_timestamp()`, d.DomainID, d.MessageID, d.Token)
		committed = err == nil && tag.RowsAffected() == 1

		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: commit delivery: %w", ParseError(err))
	}

	return committed, nil
}

// Finish needs only the token: a worker that outlived its lease may still store a
// result recovery has not written yet.
func (s *emailDeliveryStore) Finish(
	ctx context.Context,
	d store.EmailDelivery,
	status model.EmailDeliveryStatus,
	code model.EmailDeliveryError,
) (bool, error) {
	finished := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE email.message SET
    delivery_status = $4, delivery_phase = NULL, delivery_lease_until = NULL,
    delivery_error_code = NULLIF($5, ''),
    delivered_at = CASE WHEN $4 = 'sent' THEN now() END,
    updated_at = now()
WHERE domain_id = $1 AND id = $2 AND delivery_attempts = $3 AND delivery_status = 'pending'`,
			d.DomainID, d.MessageID, d.Token, string(status), string(code))
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		finished = true

		return s.recordDelivery(ctx, tx, d.DomainID, d.ThreadID, d.MessageID)
	})
	if err != nil {
		return false, fmt.Errorf("store: finish delivery: %w", ParseError(err))
	}

	return finished, nil
}

// RecoverExpired fails abandoned attempts: before the commit the server cannot have
// the email, after it the result is unknown.
func (s *emailDeliveryStore) RecoverExpired(ctx context.Context, limit int) (int, error) {
	recovered := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
WITH expired AS (
    SELECT domain_id, id FROM email.message
    WHERE delivery_status = 'pending' AND delivery_phase IN ('sending', 'committed')
        AND delivery_lease_until < now()
    ORDER BY delivery_lease_until
    LIMIT $1
    FOR UPDATE SKIP LOCKED)
UPDATE email.message m SET
    delivery_status = 'failed',
    delivery_error_code = CASE m.delivery_phase WHEN 'committed' THEN 'unknown_result' ELSE 'interrupted' END,
    delivery_phase = NULL, delivery_lease_until = NULL, updated_at = now()
FROM expired e
WHERE m.domain_id = e.domain_id AND m.id = e.id
RETURNING m.domain_id, m.thread_id, m.id`, limit)
		if err != nil {
			return err
		}
		type row struct{ domainID, threadID, messageID int64 }
		failed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var v row
			err := r.Scan(&v.domainID, &v.threadID, &v.messageID)
			return v, err
		})
		if err != nil {
			return err
		}
		for _, v := range failed {
			if err := s.recordDelivery(ctx, tx, v.domainID, v.threadID, v.messageID); err != nil {
				return err
			}
		}
		recovered = len(failed)

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: recover deliveries: %w", ParseError(err))
	}

	return recovered, nil
}

func (s *emailDeliveryStore) recordDelivery(ctx context.Context, tx pgx.Tx, domainID, threadID, messageID int64) error {
	_, err := recordWorkspaceChange(ctx, tx, s.workspaceEvents, store.EmailWorkspaceChange{
		DomainID: domainID, ThreadID: threadID, EventType: event.TypeWorkspaceMessageDelivery,
		MessageID: &messageID, OccurredAt: time.Now(),
	})

	return err
}

func (s *emailDeliveryStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		if rbErr := rollback(ctx, tx); rbErr != nil {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}
		return err
	}

	return tx.Commit(ctx)
}
