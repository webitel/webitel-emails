package postgres

import "github.com/webitel/webitel-emails/internal/model"

// The visibility boundary of the read API, on Thread "t" with @domain_id and @user_id
// from the session. A Thread id only narrows a query that carries it.
const (
	// threadReadable: a regular Thread of the caller's domain with at least one
	// ready message; last_message_at moves only when a message becomes ready.
	threadReadable = `t.domain_id = @domain_id AND t.kind = 'regular' AND t.last_message_at IS NOT NULL`

	// threadCurrent: the caller is the current agent by the latest applied
	// projection, which may lag behind call_center.
	threadCurrent = `t.current_user_id = @user_id`

	// threadParticipant: the caller took the Thread into work at some point.
	threadParticipant = `EXISTS (
    SELECT 1 FROM email.thread_participant tp
    WHERE tp.domain_id = t.domain_id AND tp.thread_id = t.id AND tp.user_id = @user_id)`

	// threadAssigned: the caller may change the Thread, being its current agent
	// after Take. This is the projection's view, not a real-time check.
	threadAssigned = threadCurrent + ` AND t.status IN ('in_progress', 'postponed')`

	// threadAccessExpr tells the caller's relation to a visible Thread.
	threadAccessExpr = `CASE WHEN t.current_user_id = @user_id THEN 'current' ELSE 'history' END`
)

// visibleThread is the complete visibility condition for one assignment view.
func visibleThread(assignment model.EmailThreadAssignment) string {
	var relation string
	switch assignment {
	case model.EmailThreadAssignmentCurrent:
		relation = threadCurrent
	case model.EmailThreadAssignmentHistory:
		relation = threadParticipant + ` AND t.current_user_id IS DISTINCT FROM @user_id`
	default:
		relation = threadCurrent + ` OR ` + threadParticipant
	}

	return threadReadable + ` AND (` + relation + `)`
}
