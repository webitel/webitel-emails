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

	// messageReadable, on message "m" for @user_id, hides a report that could name
	// a blind copy of an outbound email from everyone but its author: a bounce on an
	// email with Bcc, or an auto-reply from an address only in its Bcc. The email is
	// found as threading links it: the bounced message, In-Reply-To, then References
	// from the last. The Thread still changes for everyone; only the content is hidden.
	messageReadable = `(m.kind = 'regular' OR NOT EXISTS (
    SELECT 1
    FROM (
        SELECT o.id, o.direction, o.author_user_id
        FROM unnest(ARRAY[m.bounce_original_message_id, m.in_reply_to] || m.message_references)
            WITH ORDINALITY AS ref(message_id, n)
        JOIN email.message o
            ON o.domain_id = m.domain_id AND o.profile_id = m.profile_id AND o.message_id = ref.message_id
        ORDER BY CASE WHEN ref.n <= 2 THEN ref.n ELSE 1000000 - ref.n END
        LIMIT 1
    ) rel
    WHERE rel.direction = 'outbound' AND rel.author_user_id IS DISTINCT FROM @user_id
        AND CASE WHEN m.kind = 'bounce' THEN EXISTS (
                SELECT 1 FROM email.recipient b
                WHERE b.domain_id = m.domain_id AND b.message_id = rel.id AND b.type = 'bcc')
            ELSE EXISTS (
                SELECT 1 FROM email.recipient f
                JOIN email.recipient b ON b.domain_id = f.domain_id AND b.message_id = rel.id
                    AND b.type = 'bcc' AND b.normalized_address = f.normalized_address
                WHERE f.domain_id = m.domain_id AND f.message_id = m.id AND f.type = 'from'
                    AND NOT EXISTS (
                        SELECT 1 FROM email.recipient v
                        WHERE v.domain_id = m.domain_id AND v.message_id = rel.id
                            AND v.type IN ('to', 'cc') AND v.normalized_address = f.normalized_address))
        END))`

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
