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
	// retryDelays are the pauses before the automatic retries of a series.
	retryDelays []time.Duration
}

var _ store.EmailDeliveryStore = (*emailDeliveryStore)(nil)

// NewEmailDeliveryStore creates the store of SMTP attempts; workspaceEvents decides
// only whether events are written, retryDelays is the schedule of a series.
func NewEmailDeliveryStore(db Database, workspaceEvents bool, retryDelays []time.Duration) *emailDeliveryStore {
	return &emailDeliveryStore{db: db, workspaceEvents: workspaceEvents, retryDelays: retryDelays}
}

// Claim merges the initial queue and due retries by time and rechecks the locked row.
// Keep delivery_retry_at visible until the next public transition.
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
    SELECT q.domain_id, q.id FROM (
        (SELECT domain_id, id, received_at AS ready_at FROM email.message
         WHERE delivery_status = 'pending' AND delivery_phase = 'queued' AND delivery_retry_at IS NULL
         ORDER BY received_at, id)
        UNION ALL
        (SELECT domain_id, id, delivery_retry_at FROM email.message
         WHERE delivery_status = 'pending' AND delivery_phase = 'queued' AND delivery_retry_at <= now()
         ORDER BY delivery_retry_at, id)
    ) r
    JOIN email.message q ON q.id = r.id AND q.domain_id = r.domain_id
    WHERE q.delivery_status = 'pending' AND q.delivery_phase = 'queued'
        AND (q.delivery_retry_at IS NULL OR q.delivery_retry_at <= now())
        AND NOT q.profile_id = ANY($2)
    ORDER BY r.ready_at, r.id
    LIMIT 1
    FOR UPDATE OF q SKIP LOCKED)
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

// Finish checks the token and running phase, even after lease expiry.
// A result can win until recovery settles that attempt.
func (s *emailDeliveryStore) Finish(
	ctx context.Context,
	d store.EmailDelivery,
	outcome store.EmailDeliveryOutcome,
) (applied, requeued bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockThread(ctx, tx, d.DomainID, d.ThreadID, false); err != nil {
			return err
		}
		var a attempt
		err := tx.QueryRow(ctx, `
SELECT delivery_phase, delivery_attempts, delivery_series_start_attempt FROM email.message
WHERE domain_id = $1 AND id = $2 AND thread_id = $3 AND delivery_attempts = $4
    AND delivery_status = 'pending' AND delivery_phase IN ('sending', 'committed')
FOR UPDATE`, d.DomainID, d.MessageID, d.ThreadID, d.Token).Scan(&a.phase, &a.token, &a.seriesStart)
		switch {
		case stderrors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		case outcome.Status == model.EmailDeliveryStatusSent && a.phase != "committed":
			// Only a committed attempt can have reached the server.
			return nil
		}
		applied = true
		requeued, err = s.settle(ctx, tx, d.DomainID, d.ThreadID, d.MessageID, a, outcome)

		return err
	})
	if err != nil {
		return false, false, fmt.Errorf("store: finish delivery: %w", ParseError(err))
	}

	return applied, requeued, nil
}

// RecoverExpired settles each candidate separately and moves past busy or failed ones.
// Pre-commit attempts may retry. Post-commit attempts end as unknown_result.
func (s *emailDeliveryStore) RecoverExpired(
	ctx context.Context,
	after *store.EmailDeliveryCursor,
	limit int,
) (store.EmailDeliveryRecovery, error) {
	var batch store.EmailDeliveryRecovery
	cursorAt, cursorID := time.Time{}, int64(0)
	if after != nil {
		cursorAt, cursorID = after.LeaseUntil, after.MessageID
	}
	rows, err := s.db.Query(ctx, `
SELECT domain_id, thread_id, id, delivery_attempts, delivery_phase, delivery_lease_until FROM email.message
WHERE delivery_status = 'pending' AND delivery_phase IN ('sending', 'committed')
    AND delivery_lease_until < now()
    AND ($1::boolean OR (delivery_lease_until, id) > ($2::timestamptz, $3::bigint))
ORDER BY delivery_lease_until, id
LIMIT $4`, after == nil, cursorAt, cursorID, limit)
	if err != nil {
		return batch, fmt.Errorf("store: find expired deliveries: %w", ParseError(err))
	}
	type candidate struct {
		domainID, threadID, messageID int64
		token                         int32
		phase                         string
		leaseUntil                    time.Time
	}
	candidates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := r.Scan(&c.domainID, &c.threadID, &c.messageID, &c.token, &c.phase, &c.leaseUntil)
		return c, err
	})
	if err != nil {
		return batch, fmt.Errorf("store: find expired deliveries: %w", ParseError(err))
	}

	for _, c := range candidates {
		if ctx.Err() != nil {
			return batch, ctx.Err()
		}
		batch.Scanned++
		batch.Next = store.EmailDeliveryCursor{LeaseUntil: c.leaseUntil, MessageID: c.messageID}
		done := false
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			if err := lockThread(ctx, tx, c.domainID, c.threadID, true); err != nil {
				if stderrors.Is(err, errBusy) {
					return nil
				}
				return err
			}
			// The candidate again, after the locks and by the clock: a new claim or
			// a late result of the worker leaves nothing to recover.
			a := attempt{phase: c.phase, token: c.token}
			err := tx.QueryRow(ctx, `
SELECT delivery_series_start_attempt FROM email.message
WHERE domain_id = $1 AND id = $2 AND thread_id = $3 AND delivery_attempts = $4
    AND delivery_status = 'pending' AND delivery_phase = $5
    AND delivery_lease_until < clock_timestamp()
FOR UPDATE SKIP LOCKED`, c.domainID, c.messageID, c.threadID, c.token, c.phase).Scan(&a.seriesStart)
			switch {
			case stderrors.Is(err, pgx.ErrNoRows):
				return nil
			case err != nil:
				return err
			}

			outcome := store.EmailDeliveryOutcome{Status: model.EmailDeliveryStatusFailed,
				Code: model.EmailDeliveryErrorInterrupted, Retryable: true}
			if c.phase == "committed" {
				outcome = store.EmailDeliveryOutcome{Status: model.EmailDeliveryStatusFailed,
					Code: model.EmailDeliveryErrorUnknownResult}
			}
			done = true
			_, err = s.settle(ctx, tx, c.domainID, c.threadID, c.messageID, a, outcome)

			return err
		})
		switch {
		case err != nil:
			batch.Failed = append(batch.Failed,
				fmt.Errorf("store: recover delivery of message %d: %w", c.messageID, ParseError(err)))
		case done:
			batch.Recovered++
		}
	}

	return batch, nil
}

// ChangeDelivery locks Thread then Message, matching Finish.
// decide sees the caller's visible state under both locks, with age from the DB clock.
func (s *emailDeliveryStore) ChangeDelivery(
	ctx context.Context,
	domainID, userID, threadID, messageID int64,
	decide func(*store.EmailReplyThread, *store.EmailDeliveryMessage) (store.EmailDeliveryChange, error),
) (*store.EmailDeliveryMessage, error) {
	var (
		changed *store.EmailDeliveryMessage
		// refused is the error of decide, returned as it is.
		refused error
	)
	refuse := func(thread *store.EmailReplyThread, message *store.EmailDeliveryMessage) (store.EmailDeliveryChange, error) {
		change, err := decide(thread, message)
		refused = err
		return change, err
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		thread, err := NewEmailOutboundStore(tx).LockReplyThread(ctx, domainID, userID, threadID)
		if err != nil || thread == nil {
			if err == nil {
				_, err = refuse(nil, nil)
			}
			return err
		}

		message := &store.EmailDeliveryMessage{ID: messageID}
		var (
			direction       string
			author          *int64
			status, phase   *string
			code            *string
			ageMicroseconds int64
		)
		// The age is taken in the outer SELECT: within the locking one PostgreSQL may
		// read the clock before it waits for the row.
		err = tx.QueryRow(ctx, `
WITH locked AS MATERIALIZED (
    SELECT m.direction, m.author_user_id, m.delivery_status, m.delivery_phase, m.delivery_error_code,
        m.delivery_version, m.delivery_manual_retry_count, m.received_at
    FROM email.message m
    WHERE m.domain_id = @domain_id AND m.thread_id = @thread_id AND m.id = @message_id AND m.state = 'ready'
        AND `+messageReadable+`
    FOR UPDATE OF m)
SELECT direction, author_user_id, delivery_status, delivery_phase, delivery_error_code,
    delivery_version, delivery_manual_retry_count,
    (extract(epoch FROM clock_timestamp() - received_at) * 1000000)::bigint
FROM locked`, pgx.NamedArgs{
			"domain_id": domainID, "user_id": userID, "thread_id": threadID, "message_id": messageID,
		}).Scan(&direction, &author, &status, &phase, &code, &message.Version, &message.ManualRetries, &ageMicroseconds)
		switch {
		case stderrors.Is(err, pgx.ErrNoRows):
			_, err = refuse(thread, nil)
			return err
		case err != nil:
			return err
		}
		message.Outbound = direction == string(model.EmailMessageDirectionOutbound)
		if author != nil {
			message.AuthorUserID = *author
		}
		if status != nil {
			message.Status = model.EmailDeliveryStatus(*status)
		}
		if phase != nil {
			message.Phase = *phase
		}
		if code != nil {
			message.Code = model.EmailDeliveryError(*code)
		}
		message.Age = time.Duration(ageMicroseconds) * time.Microsecond

		change, err := refuse(thread, message)
		if err != nil {
			return err
		}
		var query string
		switch change {
		case store.EmailDeliveryRetry:
			// A new series: the next claim is its first attempt.
			query = `
UPDATE email.message SET
    delivery_status = 'pending', delivery_phase = 'queued', delivery_lease_until = NULL,
    delivery_error_code = NULL, delivery_retry_at = NULL, delivered_at = NULL,
    delivery_manual_retry_count = delivery_manual_retry_count + 1,
    delivery_series_start_attempt = delivery_attempts,
    delivery_version = delivery_version + 1, updated_at = now()
WHERE domain_id = $1 AND id = $2
RETURNING delivery_status, delivery_version, delivery_manual_retry_count`
		case store.EmailDeliveryCancel:
			// A running attempt can no longer commit: the status is not pending.
			query = `
UPDATE email.message SET
    delivery_status = 'cancelled', delivery_phase = NULL, delivery_lease_until = NULL,
    delivery_retry_at = NULL, delivery_version = delivery_version + 1, updated_at = now()
WHERE domain_id = $1 AND id = $2
RETURNING delivery_status, delivery_version, delivery_manual_retry_count`
		default:
			return fmt.Errorf("store: unknown delivery change %d", change)
		}
		var newStatus string
		if err := tx.QueryRow(ctx, query, domainID, messageID).Scan(&newStatus, &message.Version, &message.ManualRetries); err != nil {
			return err
		}
		message.Status, message.Phase, message.Code = model.EmailDeliveryStatus(newStatus), "", ""
		if change == store.EmailDeliveryRetry {
			message.Phase = "queued"
		}
		changed = message

		return s.recordDelivery(ctx, tx, domainID, threadID, messageID)
	})
	switch {
	case refused != nil:
		return nil, refused
	case err != nil:
		return nil, fmt.Errorf("store: change delivery: %w", ParseError(err))
	}

	return changed, nil
}

// attempt is a running SMTP attempt as stored.
type attempt struct {
	phase       string
	token       int32
	seriesStart int32
}

// settle requeues a retryable failure while delays remain, or writes the final result.
// The caller holds the attempt lock. Both outcomes write a Workspace event when enabled.
func (s *emailDeliveryStore) settle(
	ctx context.Context,
	tx pgx.Tx,
	domainID, threadID, messageID int64,
	a attempt,
	outcome store.EmailDeliveryOutcome,
) (requeued bool, err error) {
	// The number of this attempt within its series, from 1.
	number := int(a.token - a.seriesStart)
	if outcome.Status == model.EmailDeliveryStatusFailed && outcome.Retryable &&
		number >= 1 && number <= len(s.retryDelays) {
		_, err = tx.Exec(ctx, `
UPDATE email.message SET
    delivery_phase = 'queued', delivery_lease_until = NULL, delivery_error_code = NULL,
    delivered_at = NULL,
    delivery_retry_at = clock_timestamp() + $3::bigint * interval '1 microsecond',
    delivery_version = delivery_version + 1, updated_at = now()
WHERE domain_id = $1 AND id = $2`, domainID, messageID, s.retryDelays[number-1].Microseconds())
		requeued = true
	} else {
		_, err = tx.Exec(ctx, `
UPDATE email.message SET
    delivery_status = $3, delivery_phase = NULL, delivery_lease_until = NULL,
    delivery_retry_at = NULL, delivery_error_code = NULLIF($4, ''),
    delivered_at = CASE WHEN $3 = 'sent' THEN clock_timestamp() END,
    delivery_version = delivery_version + 1, updated_at = now()
WHERE domain_id = $1 AND id = $2`, domainID, messageID, string(outcome.Status), string(outcome.Code))
	}
	if err != nil {
		return false, err
	}

	return requeued, s.recordDelivery(ctx, tx, domainID, threadID, messageID)
}

var errBusy = stderrors.New("store: row is locked")

// lockThread enforces Thread before Message, as Send and the projection do.
// With skip, a busy Thread returns errBusy.
func lockThread(ctx context.Context, tx pgx.Tx, domainID, threadID int64, skip bool) error {
	query := `SELECT 1 FROM email.thread WHERE domain_id = $1 AND id = $2 FOR NO KEY UPDATE`
	if skip {
		query += ` SKIP LOCKED`
	}
	var one int
	err := tx.QueryRow(ctx, query, domainID, threadID).Scan(&one)
	if stderrors.Is(err, pgx.ErrNoRows) {
		if skip {
			return errBusy
		}
		return fmt.Errorf("store: thread %d of an outbound email not found", threadID)
	}

	return err
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
