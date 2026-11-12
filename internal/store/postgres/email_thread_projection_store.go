package postgres

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailThreadProjectionStore struct {
	db              Database
	workspaceEvents bool
}

var _ store.EmailThreadProjectionStore = (*emailThreadProjectionStore)(nil)

// NewEmailThreadProjectionStore creates the store the snapshot consumer writes through.
func NewEmailThreadProjectionStore(db Database) *emailThreadProjectionStore {
	return &emailThreadProjectionStore{db: db}
}

// WithWorkspaceEvents turns on writing the Workspace events of applied snapshots.
func (s *emailThreadProjectionStore) WithWorkspaceEvents(enabled bool) *emailThreadProjectionStore {
	s.workspaceEvents = enabled

	return s
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

	result, err := applyProjection(ctx, tx, p, s.workspaceEvents)
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

func applyProjection(
	ctx context.Context,
	tx pgx.Tx,
	p *store.EmailThreadProjection,
	workspaceEvents bool,
) (store.EmailThreadProjectionResult, error) {
	var result store.EmailThreadProjectionResult

	// The row lock comes first even for a stale snapshot: it orders this change
	// against ready Messages and contacts, and gives the user it replaces.
	var (
		kind         string
		previousUser *int64
	)
	err := tx.QueryRow(ctx, `SELECT kind, current_user_id FROM email.thread WHERE domain_id = $1 AND id = $2 FOR UPDATE`,
		p.DomainID, p.ThreadID).Scan(&kind, &previousUser)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		result.Outcome = store.EmailThreadProjectionNotFound

		return result, nil
	case err != nil:
		return result, fmt.Errorf("store: lock projected thread: %w", err)
	case kind == string(model.EmailThreadKindService):
		result.Outcome = store.EmailThreadProjectionService

		return result, nil
	}

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
		recorded, added, err := recordParticipant(ctx, tx, p)
		if err != nil {
			return result, err
		}
		result.ParticipantRecorded, result.ParticipantAdded = recorded, added
	}

	recorded, err := recordAction(ctx, tx, p)
	if err != nil {
		return result, err
	}
	result.ActionRecorded = recorded

	result.Outcome = store.EmailThreadProjectionStale
	if applied {
		result.Outcome = store.EmailThreadProjectionApplied
	}

	// A repeated snapshot that changed nothing visible raises no version.
	var changes []string
	if applied || result.HandledApplied {
		changes = append(changes, event.WorkspaceChangeProjection)
	}
	if result.ParticipantAdded {
		changes = append(changes, event.WorkspaceChangeParticipants)
	}
	if result.ActionRecorded {
		changes = append(changes, event.WorkspaceChangeActions)
	}
	if len(changes) == 0 {
		return result, nil
	}

	result.WorkspaceEvents, err = recordWorkspaceChange(ctx, tx, workspaceEvents, store.EmailWorkspaceChange{
		DomainID: p.DomainID, ThreadID: p.ThreadID, EventType: event.TypeWorkspaceThreadChanged,
		Changes: changes, PreviousUserID: previousUser, OccurredAt: time.Now(),
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

// recordParticipant adds the agent of a Take-proving snapshot to the journal of a
// regular Thread; versions and times only widen, so order does not matter.
// The second result tells a new row from widened versions.
func recordParticipant(ctx context.Context, tx pgx.Tx, p *store.EmailThreadProjection) (bool, bool, error) {
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
   OR EXCLUDED.last_version > tp.last_version
RETURNING xmax = 0`

	var added bool
	err := tx.QueryRow(ctx, record,
		p.DomainID, p.ThreadID, p.ParticipantUserID, p.ParticipantAgentID, p.Version, p.OccurredAt).Scan(&added)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, projectionError("record participant", err)
	}

	return true, added, nil
}

// recordAction keeps one journal row per snapshot of a regular Thread, so a repeated
// or late delivery never adds another.
func recordAction(ctx context.Context, tx pgx.Tx, p *store.EmailThreadProjection) (bool, error) {
	const record = `
INSERT INTO email.thread_action (
    domain_id, thread_id, projection_version, occurred_at, transition, source,
    attempt_id, attempt_state, result, queue_id, agent_id, agent_user_id,
    actor_user_id, transfer_to_queue_id, transfer_to_user_id, comment)
SELECT t.domain_id, t.id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
FROM email.thread t
WHERE t.domain_id = $1 AND t.id = $2 AND t.kind = 'regular'
ON CONFLICT (domain_id, thread_id, projection_version) DO NOTHING`

	a := p.Action
	tag, err := tx.Exec(ctx, record,
		p.DomainID, p.ThreadID, p.Version, p.OccurredAt, a.Transition, a.Source,
		a.AttemptID, a.AttemptState, a.Result, a.QueueID, a.AgentID, a.AgentUserID,
		a.ActorUserID, a.TransferToQueueID, a.TransferToUserID, a.Comment)
	if err != nil {
		return false, projectionError("record action", err)
	}

	return tag.RowsAffected() == 1, nil
}

// DeleteActionsBefore removes up to limit actions older than before, oldest first.
func (s *emailThreadProjectionStore) DeleteActionsBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := s.db.Exec(ctx, `
DELETE FROM email.thread_action
WHERE (domain_id, thread_id, projection_version) IN (
    SELECT domain_id, thread_id, projection_version
    FROM email.thread_action
    WHERE occurred_at < $1
    ORDER BY occurred_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("store: delete thread actions: %w", err)
	}

	return tag.RowsAffected(), nil
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
