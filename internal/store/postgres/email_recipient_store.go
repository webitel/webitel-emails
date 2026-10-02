package postgres

import (
	"context"

	"github.com/lib/pq"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type emailRecipientStore struct {
	db Querier
}

var _ store.EmailRecipientStore = (*emailRecipientStore)(nil)

// NewEmailRecipientStore creates a recipient store over q.
func NewEmailRecipientStore(q Querier) *emailRecipientStore {
	return &emailRecipientStore{db: q}
}

func (s *emailRecipientStore) CreateBulk(
	ctx context.Context,
	domainID, messageID int64,
	recipients []*model.EmailRecipient,
) error {
	if len(recipients) == 0 {
		return nil
	}

	// unnest keeps the statement at a fixed number of parameters whatever the
	// number of addresses, so a long recipient list cannot exhaust them.
	const query = `
INSERT INTO email.recipient (
    domain_id, message_id, type, address, normalized_address, display_name, ordinal
)
SELECT $1, $2, r.type, r.address, r.normalized_address, r.display_name, r.ordinal
FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::int[])
    AS r(type, address, normalized_address, display_name, ordinal)`

	var (
		types       = make([]string, 0, len(recipients))
		addresses   = make([]string, 0, len(recipients))
		normalized  = make([]string, 0, len(recipients))
		displayName = make([]string, 0, len(recipients))
		ordinals    = make([]int32, 0, len(recipients))
	)
	for _, recipient := range recipients {
		types = append(types, string(recipient.Type))
		addresses = append(addresses, recipient.Address)
		normalized = append(normalized, recipient.NormalizedAddress)
		displayName = append(displayName, recipient.DisplayName)
		ordinals = append(ordinals, recipient.Ordinal)
	}

	_, err := s.db.ExecContext(
		ctx,
		query,
		domainID,
		messageID,
		pq.Array(types),
		pq.Array(addresses),
		pq.Array(normalized),
		pq.Array(displayName),
		pq.Array(ordinals),
	)

	return ParseError(err)
}
