package postgres

import (
	"context"
	stderrors "errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailThreadReadStore struct {
	db Querier
}

var _ store.EmailThreadReadStore = (*emailThreadReadStore)(nil)

// NewEmailThreadReadStore creates the store of the read API.
func NewEmailThreadReadStore(q Querier) *emailThreadReadStore {
	return &emailThreadReadStore{db: q}
}

// Rights reads the Thread only through the visibility boundary, so a Thread the
// user may not see is indistinguishable from a missing one.
func (s *emailThreadReadStore) Rights(
	ctx context.Context,
	domainID, userID, threadID int64,
) (store.EmailThreadRights, error) {
	query := `SELECT (` + threadAssigned + `) FROM email.thread t
WHERE t.id = @thread_id AND ` + visibleThread(model.EmailThreadAssignmentAll)

	var assigned bool
	err := s.db.QueryRow(ctx, query, pgx.NamedArgs{
		"domain_id": domainID,
		"user_id":   userID,
		"thread_id": threadID,
	}).Scan(&assigned)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return store.EmailThreadRights{}, nil
	case err != nil:
		return store.EmailThreadRights{}, ParseError(err)
	}

	return store.EmailThreadRights{Visible: true, Assigned: assigned}, nil
}

// emailThreadViewSelect reads everything the read API shows; the caller appends a
// WHERE with the visibility boundary. Names are joined within the domain.
const emailThreadViewSelect = `
SELECT t.id, t.profile_id, p.name AS profile_name, t.subject, t.status, t.status_changed_at,
    t.created_at, t.updated_at, t.last_message_at, t.completed_at,
    t.contact_id, t.contact_resolution_state, t.queue_id, t.postponed_until,
    t.current_user_id, coalesce(nullif(cu.name, ''), cu.username::text, '') AS current_user_name,
    t.last_handled_user_id, coalesce(nullif(hu.name, ''), hu.username::text, '') AS last_handled_user_name,
    ` + threadAccessExpr + ` AS access, t.current_attempt_id, t.current_attempt_version,
    t.projection_version, t.workspace_version,
    pv.id AS preview_id, pv.direction AS preview_direction, pv.received_at AS preview_received_at,
    pv.text AS preview_text, pv.from_address AS preview_from_address, pv.from_name AS preview_from_name
FROM email.thread t
JOIN email.profile p ON p.domain_id = t.domain_id AND p.id = t.profile_id
LEFT JOIN directory.wbt_user cu ON cu.id = t.current_user_id AND cu.dc = t.domain_id
LEFT JOIN directory.wbt_user hu ON hu.id = t.last_handled_user_id AND hu.dc = t.domain_id
LEFT JOIN LATERAL (
    SELECT m.id, m.direction, m.received_at, left(m.body_text, 1000) AS text,
        r.address AS from_address, r.display_name AS from_name
    FROM email.message m
    LEFT JOIN email.recipient r
        ON r.domain_id = m.domain_id AND r.message_id = m.id AND r.type = 'from' AND r.ordinal = 0
    WHERE m.domain_id = t.domain_id AND m.thread_id = t.id AND m.state = 'ready'
    ORDER BY m.received_at DESC, m.id DESC
    LIMIT 1
) pv ON true
WHERE `

var emailThreadSortColumns = map[model.EmailThreadSortField]string{
	model.EmailThreadSortLastMessageAt:   "t.last_message_at",
	model.EmailThreadSortCreatedAt:       "t.created_at",
	model.EmailThreadSortUpdatedAt:       "t.updated_at",
	model.EmailThreadSortStatusChangedAt: "t.status_changed_at",
}

// ListThreads applies the filters on top of the visibility boundary of the
// requested assignment view.
func (s *emailThreadReadStore) ListThreads(
	ctx context.Context,
	filter model.EmailThreadFilter,
) ([]*model.EmailThreadView, bool, error) {
	args := pgx.NamedArgs{
		"domain_id": filter.DomainID,
		"user_id":   filter.UserID,
		"limit":     filter.Size + 1,
		"offset":    (filter.Page - 1) * filter.Size,
	}

	where := []string{visibleThread(filter.Assignment)}
	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			statuses = append(statuses, string(status))
		}
		where = append(where, "t.status = ANY(@statuses)")
		args["statuses"] = statuses
	}
	if len(filter.ProfileIDs) > 0 {
		where = append(where, "t.profile_id = ANY(@profile_ids)")
		args["profile_ids"] = filter.ProfileIDs
	}
	if filter.ContactID > 0 {
		where = append(where, "t.contact_id = @contact_id")
		args["contact_id"] = filter.ContactID
	}

	query := emailThreadViewSelect + strings.Join(where, " AND ") +
		"\nORDER BY " + emailThreadOrderBy(filter.Sort) + "\nLIMIT @limit OFFSET @offset"

	rows, err := s.db.Query(ctx, query, args)
	if err != nil {
		return nil, false, ParseError(err)
	}
	views, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByName[emailThreadViewRecord])
	if err != nil {
		return nil, false, ParseError(err)
	}

	next := len(views) > filter.Size
	if next {
		views = views[:filter.Size]
	}
	out := make([]*model.EmailThreadView, 0, len(views))
	for _, view := range views {
		out = append(out, view.toModel())
	}

	return out, next, nil
}

// LocateThread reads one Thread through the visibility boundary.
func (s *emailThreadReadStore) LocateThread(
	ctx context.Context,
	domainID, userID, threadID int64,
) (*model.EmailThreadView, error) {
	query := emailThreadViewSelect + "t.id = @thread_id AND " + visibleThread(model.EmailThreadAssignmentAll)

	rows, err := s.db.Query(ctx, query, pgx.NamedArgs{
		"domain_id": domainID,
		"user_id":   userID,
		"thread_id": threadID,
	})
	if err != nil {
		return nil, ParseError(err)
	}
	view, err := pgx.CollectOneRow(rows, pgx.RowToAddrOfStructByName[emailThreadViewRecord])
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, ParseError(err)
	}

	return view.toModel(), nil
}

// emailThreadOrderBy builds ORDER BY from criteria the service already
// validated; the id breaks ties in the direction of the first criterion.
func emailThreadOrderBy(sort []model.EmailThreadSort) string {
	if len(sort) == 0 {
		sort = []model.EmailThreadSort{{Field: model.EmailThreadSortLastMessageAt, Desc: true}}
	}

	parts := make([]string, 0, len(sort)+1)
	for _, criterion := range sort {
		column, ok := emailThreadSortColumns[criterion.Field]
		if !ok {
			continue
		}
		direction := " ASC"
		if criterion.Desc {
			direction = " DESC"
		}
		parts = append(parts, column+direction+" NULLS LAST")
	}

	tieBreak := "t.id ASC"
	if sort[0].Desc {
		tieBreak = "t.id DESC"
	}

	return strings.Join(append(parts, tieBreak), ", ")
}

type emailThreadViewRecord struct {
	ID                     int64      `db:"id"`
	ProfileID              int64      `db:"profile_id"`
	ProfileName            string     `db:"profile_name"`
	Subject                string     `db:"subject"`
	Status                 string     `db:"status"`
	StatusChangedAt        *time.Time `db:"status_changed_at"`
	CreatedAt              time.Time  `db:"created_at"`
	UpdatedAt              time.Time  `db:"updated_at"`
	LastMessageAt          *time.Time `db:"last_message_at"`
	CompletedAt            *time.Time `db:"completed_at"`
	ContactID              *int64     `db:"contact_id"`
	ContactResolutionState string     `db:"contact_resolution_state"`
	QueueID                *int64     `db:"queue_id"`
	PostponedUntil         *time.Time `db:"postponed_until"`
	CurrentUserID          *int64     `db:"current_user_id"`
	CurrentUserName        string     `db:"current_user_name"`
	LastHandledUserID      *int64     `db:"last_handled_user_id"`
	LastHandledUserName    string     `db:"last_handled_user_name"`
	Access                 string     `db:"access"`
	CurrentAttemptID       *int64     `db:"current_attempt_id"`
	CurrentAttemptVersion  *int64     `db:"current_attempt_version"`
	ProjectionVersion      *int64     `db:"projection_version"`
	WorkspaceVersion       int64      `db:"workspace_version"`
	PreviewID              *int64     `db:"preview_id"`
	PreviewDirection       *string    `db:"preview_direction"`
	PreviewReceivedAt      *time.Time `db:"preview_received_at"`
	PreviewText            *string    `db:"preview_text"`
	PreviewFromAddress     *string    `db:"preview_from_address"`
	PreviewFromName        *string    `db:"preview_from_name"`
}

func (r *emailThreadViewRecord) toModel() *model.EmailThreadView {
	view := &model.EmailThreadView{
		ID:                     r.ID,
		Profile:                model.Lookup{ID: r.ProfileID, Name: r.ProfileName},
		Subject:                r.Subject,
		Status:                 model.EmailThreadStatus(r.Status),
		StatusChangedAt:        r.StatusChangedAt,
		CreatedAt:              r.CreatedAt,
		UpdatedAt:              r.UpdatedAt,
		LastMessageAt:          r.LastMessageAt,
		CompletedAt:            r.CompletedAt,
		ContactID:              r.ContactID,
		ContactResolutionState: model.EmailContactResolutionState(r.ContactResolutionState),
		QueueID:                r.QueueID,
		PostponedUntil:         r.PostponedUntil,
		CurrentUser:            userLookup(r.CurrentUserID, r.CurrentUserName),
		LastHandledUser:        userLookup(r.LastHandledUserID, r.LastHandledUserName),
		Access:                 model.EmailThreadAccess(r.Access),
		ProjectionVersion:      r.ProjectionVersion,
		WorkspaceVersion:       r.WorkspaceVersion,
	}
	// The attempt matters only to the agent who may act on it.
	if view.Access == model.EmailThreadAccessCurrent {
		view.CurrentAttemptID, view.CurrentAttemptVersion = r.CurrentAttemptID, r.CurrentAttemptVersion
	}
	if r.PreviewID != nil {
		preview := &model.EmailThreadPreview{
			MessageID: *r.PreviewID,
			Direction: model.EmailMessageDirection(derefString(r.PreviewDirection)),
			Text:      derefString(r.PreviewText),
		}
		if r.PreviewReceivedAt != nil {
			preview.ReceivedAt = *r.PreviewReceivedAt
		}
		if r.PreviewFromAddress != nil {
			preview.From = &model.EmailAddress{Address: *r.PreviewFromAddress, Name: derefString(r.PreviewFromName)}
		}
		view.Preview = preview
	}

	return view
}

func userLookup(id *int64, name string) *model.Lookup {
	if id == nil {
		return nil
	}

	return &model.Lookup{ID: *id, Name: name}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}
