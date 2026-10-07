package postgres

import (
	"context"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type inboundFailureStore struct {
	db Querier
}

var _ store.InboundFailureStore = (*inboundFailureStore)(nil)

// NewInboundFailureStore creates a quarantine store over the shared connection.
func NewInboundFailureStore(db Querier) *inboundFailureStore {
	return &inboundFailureStore{db: db}
}

func (s *inboundFailureStore) Record(ctx context.Context, failure *model.InboundFailure) error {
	// The same broken email may be delivered again before the cursor moves, so
	// the write is an upsert keyed by its position in the mailbox.
	const query = `
INSERT INTO email.inbound_failure (
    domain_id, profile_id, mailbox, uid_validity, uid,
    category, reason, message_size, message_id, sender, subject
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (domain_id, profile_id, mailbox, uid_validity, uid) DO UPDATE SET
    category = EXCLUDED.category,
    reason = EXCLUDED.reason,
    message_size = EXCLUDED.message_size,
    message_id = EXCLUDED.message_id,
    sender = EXCLUDED.sender,
    subject = EXCLUDED.subject`

	var size any
	if failure.Size > 0 {
		size = failure.Size
	}

	_, err := s.db.ExecContext(
		ctx,
		query,
		failure.DomainID,
		failure.ProfileID,
		failure.Mailbox,
		int64(failure.UIDValidity),
		int64(failure.UID),
		failure.Category,
		failure.Reason,
		size,
		nullableText(failure.MessageID),
		nullableText(failure.Sender),
		nullableText(failure.Subject),
	)

	return ParseError(err)
}
