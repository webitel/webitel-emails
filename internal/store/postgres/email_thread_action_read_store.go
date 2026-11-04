package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/webitel/webitel-emails/internal/model"
)

// emailThreadActionSelect reads the journal of a visible Thread; names are joined within the domain.
var emailThreadActionSelect = `
SELECT a.projection_version, a.occurred_at, a.transition, coalesce(a.attempt_state, ''), coalesce(a.result, ''),
    a.source, a.actor_user_id, coalesce(nullif(au.name, ''), au.username::text, ''),
    a.agent_user_id, coalesce(nullif(gu.name, ''), gu.username::text, ''),
    a.queue_id, a.transfer_to_queue_id,
    a.transfer_to_user_id, coalesce(nullif(tu.name, ''), tu.username::text, ''), coalesce(a.comment, '')
FROM email.thread_action a
JOIN email.thread t ON t.domain_id = a.domain_id AND t.id = a.thread_id
LEFT JOIN directory.wbt_user au ON au.id = a.actor_user_id AND au.dc = a.domain_id
LEFT JOIN directory.wbt_user gu ON gu.id = a.agent_user_id AND gu.dc = a.domain_id
LEFT JOIN directory.wbt_user tu ON tu.id = a.transfer_to_user_id AND tu.dc = a.domain_id
WHERE a.domain_id = @domain_id AND a.thread_id = @thread_id AND a.projection_version > @after
    AND ` + visibleThread(model.EmailThreadAssignmentAll) + `
ORDER BY a.projection_version
LIMIT @limit`

// ListActions pages the journal by projection_version; an invisible Thread yields an empty page.
func (s *emailThreadReadStore) ListActions(
	ctx context.Context,
	filter model.EmailThreadActionFilter,
) ([]*model.EmailThreadActionView, bool, error) {
	rows, err := s.db.Query(ctx, emailThreadActionSelect, pgx.NamedArgs{
		"domain_id": filter.DomainID,
		"user_id":   filter.UserID,
		"thread_id": filter.ThreadID,
		"after":     filter.After,
		"limit":     filter.Size + 1,
	})
	if err != nil {
		return nil, false, ParseError(err)
	}
	defer rows.Close()

	var actions []*model.EmailThreadActionView
	for rows.Next() {
		var (
			a                                  model.EmailThreadActionView
			occurredAt                         time.Time
			actorID, agentUserID, transferToID *int64
			actorName, agentName, transferName string
		)
		if err := rows.Scan(&a.ProjectionVersion, &occurredAt, &a.Transition, &a.AttemptState, &a.Result,
			&a.Source, &actorID, &actorName, &agentUserID, &agentName, &a.QueueID, &a.TransferToQueueID,
			&transferToID, &transferName, &a.Comment); err != nil {
			return nil, false, ParseError(err)
		}
		a.OccurredAt = occurredAt.UTC()
		a.Actor = userLookup(actorID, actorName)
		a.Agent = userLookup(agentUserID, agentName)
		a.TransferToUser = userLookup(transferToID, transferName)
		actions = append(actions, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, ParseError(err)
	}

	next := len(actions) > filter.Size
	if next {
		actions = actions[:filter.Size]
	}

	return actions, next, nil
}
