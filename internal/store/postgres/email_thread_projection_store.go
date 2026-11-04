package postgres

import (
	"context"
	stderrors "errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailThreadProjectionStore struct {
	db Database
}

var _ store.EmailThreadProjectionStore = (*emailThreadProjectionStore)(nil)

// NewEmailThreadProjectionStore creates the store the snapshot consumer writes through.
func NewEmailThreadProjectionStore(db Database) *emailThreadProjectionStore {
	return &emailThreadProjectionStore{db: db}
}

// Apply updates the projection and the last handled agent in one transaction;
// each has its own version, so a late final snapshot still records its agent.
func (s *emailThreadProjectionStore) Apply(
	ctx context.Context,
	p *store.EmailThreadProjection,
) (store.EmailThreadProjectionResult, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.EmailThreadProjectionResult{}, fmt.Errorf("store: begin transaction: %w", err)
	}

	result, err := applyProjection(ctx, tx, p)
	if err != nil {
		if rbErr := rollback(ctx, tx); rbErr != nil {
			return result, fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}

		return result, err
	}

	if err := tx.Commit(ctx); err != nil {
		return store.EmailThreadProjectionResult{}, fmt.Errorf("store: commit transaction: %w", err)
	}

	return result, nil
}

func applyProjection(ctx context.Context, tx pgx.Tx, p *store.EmailThreadProjection) (store.EmailThreadProjectionResult, error) {
	var result store.EmailThreadProjectionResult

	// Every field is written, so a value the snapshot no longer carries is cleared;
	// SET reads the old status, so the change time moves only with the status.
	const project = `
UPDATE email.thread SET
    status = $3,
    queue_id = coalesce($4, queue_id),
    current_attempt_id = $5,
    current_attempt_version = $6,
    current_agent_id = $7,
    current_user_id = $8,
    postponed_until = $9,
    completed_at = $10,
    status_changed_at = CASE WHEN status <> $3 THEN $11 ELSE status_changed_at END,
    projection_version = $12,
    updated_at = now()
WHERE domain_id = $1 AND id = $2 AND kind = 'regular' AND coalesce(projection_version, 0) < $12`

	tag, err := tx.Exec(ctx, project,
		p.DomainID, p.ThreadID, string(p.Status), p.QueueID, p.AttemptID, p.AttemptVersion,
		p.AgentID, p.UserID, p.PostponedUntil, p.CompletedAt, p.OccurredAt, p.Version)
	if err != nil {
		return result, projectionError("project thread", err)
	}
	applied := tag.RowsAffected() == 1

	if p.HandledAgentID != nil {
		const handled = `
UPDATE email.thread SET
    last_handled_agent_id = $3,
    last_handled_user_id = $4,
    last_handled_version = $5,
    updated_at = now()
WHERE domain_id = $1 AND id = $2 AND kind = 'regular' AND coalesce(last_handled_version, 0) < $5`

		tag, err := tx.Exec(ctx, handled, p.DomainID, p.ThreadID, p.HandledAgentID, p.HandledUserID, p.Version)
		if err != nil {
			return result, projectionError("record last handled agent", err)
		}
		result.HandledApplied = tag.RowsAffected() == 1
	}

	if p.ParticipantUserID != nil && p.ParticipantAgentID != nil {
		recorded, err := recordParticipant(ctx, tx, p)
		if err != nil {
			return result, err
		}
		result.ParticipantRecorded = recorded
	}

	if applied {
		result.Outcome = store.EmailThreadProjectionApplied

		return result, nil
	}

	// One read tells a repeated or older snapshot from one with no Thread to land on.
	var kind string
	err = tx.QueryRow(ctx, `SELECT kind FROM email.thread WHERE domain_id = $1 AND id = $2`,
		p.DomainID, p.ThreadID).Scan(&kind)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		result.Outcome = store.EmailThreadProjectionNotFound
	case err != nil:
		return result, fmt.Errorf("store: read projected thread: %w", err)
	case kind == string(model.EmailThreadKindService):
		result.Outcome = store.EmailThreadProjectionService
	default:
		result.Outcome = store.EmailThreadProjectionStale
	}

	return result, nil
}

// recordParticipant adds the agent of a Take-proving snapshot to the journal of a
// regular Thread; versions and times only widen, so order does not matter.
func recordParticipant(ctx context.Context, tx pgx.Tx, p *store.EmailThreadProjection) (bool, error) {
	const record = `
INSERT INTO email.thread_participant AS tp (
    domain_id, thread_id, user_id, agent_id,
    first_version, last_version, first_participated_at, last_participated_at)
SELECT t.domain_id, t.id, $3, $4, $5, $5, $6, $6
FROM email.thread t
WHERE t.domain_id = $1 AND t.id = $2 AND t.kind = 'regular'
ON CONFLICT (domain_id, thread_id, user_id) DO UPDATE SET
    agent_id = CASE WHEN EXCLUDED.last_version > coalesce(tp.last_version, 0)
        THEN EXCLUDED.agent_id ELSE tp.agent_id END,
    first_version = LEAST(tp.first_version, EXCLUDED.first_version),
    last_version = GREATEST(tp.last_version, EXCLUDED.last_version),
    first_participated_at = LEAST(tp.first_participated_at, EXCLUDED.first_participated_at),
    last_participated_at = GREATEST(tp.last_participated_at, EXCLUDED.last_participated_at),
    updated_at = now()
WHERE tp.first_version IS NULL
   OR EXCLUDED.first_version < tp.first_version
   OR EXCLUDED.last_version > tp.last_version`

	tag, err := tx.Exec(ctx, record,
		p.DomainID, p.ThreadID, p.ParticipantUserID, p.ParticipantAgentID, p.Version, p.OccurredAt)
	if err != nil {
		return false, projectionError("record participant", err)
	}

	return tag.RowsAffected() == 1, nil
}

// projectionError marks data and constraint errors as permanent; anything else
// is left for a retry.
func projectionError(action string, err error) error {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) && (pgErr.SQLState()[:2] == "22" || pgErr.SQLState()[:2] == "23") {
		return fmt.Errorf("store: %s: %w: %w", action, store.ErrEmailThreadProjectionRejected, err)
	}

	return fmt.Errorf("store: %s: %w", action, err)
}
