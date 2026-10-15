package postgres

import (
	"context"
	"database/sql"

	"github.com/webitel/webitel-emails/internal/store"
)

type emailProfileRoutingStore struct {
	db Querier
}

var _ store.EmailProfileRoutingStore = (*emailProfileRoutingStore)(nil)

// NewEmailProfileRoutingStore creates a profile routing store over q.
func NewEmailProfileRoutingStore(q Querier) *emailProfileRoutingStore {
	return &emailProfileRoutingStore{db: q}
}

func (s *emailProfileRoutingStore) FlowID(ctx context.Context, domainID, profileID int64) (*int64, error) {
	const query = `SELECT flow_id FROM email.profile WHERE domain_id = $1 AND id = $2`

	var flowID sql.NullInt64
	if err := s.db.QueryRow(ctx, query, domainID, profileID).Scan(&flowID); err != nil {
		return nil, ParseError(err)
	}
	if !flowID.Valid {
		return nil, nil
	}

	return &flowID.Int64, nil
}
