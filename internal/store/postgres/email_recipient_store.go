package postgres

import (
	"context"

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

	_, err := s.db.Exec(
		ctx,
		query,
		domainID,
		messageID,
		types,
		addresses,
		normalized,
		displayName,
		ordinals,
	)

	return ParseError(err)
}

func (s *emailRecipientStore) List(
	ctx context.Context,
	domainID, messageID int64,
) ([]*model.EmailRecipient, error) {
	const query = `
SELECT id, domain_id, message_id, type, address, normalized_address, display_name, ordinal
FROM email.recipient
WHERE domain_id = $1 AND message_id = $2
ORDER BY type, ordinal`

	rows, err := s.db.Query(ctx, query, domainID, messageID)
	if err != nil {
		return nil, ParseError(err)
	}
	defer rows.Close()

	var recipients []*model.EmailRecipient
	for rows.Next() {
		var (
			recipient     model.EmailRecipient
			recipientType string
		)
		if err := rows.Scan(
			&recipient.ID,
			&recipient.DomainID,
			&recipient.MessageID,
			&recipientType,
			&recipient.Address,
			&recipient.NormalizedAddress,
			&recipient.DisplayName,
			&recipient.Ordinal,
		); err != nil {
			return nil, ParseError(err)
		}

		recipient.Type = model.EmailRecipientType(recipientType)
		recipients = append(recipients, &recipient)
	}

	if err := rows.Err(); err != nil {
		return nil, ParseError(err)
	}

	return recipients, nil
}
