package store

import (
	"context"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
)

// EmailDelivery is an outbound message claimed for one SMTP attempt.
type EmailDelivery struct {
	DomainID  int64
	MessageID int64
	ThreadID  int64
	ProfileID int64
	// Token is the number of the attempt; every later write of the worker requires it.
	Token int32
}

// EmailDeliveryStore moves outbound messages through their SMTP attempt.
type EmailDeliveryStore interface {
	// Claim takes the oldest queued message of a profile not in skip; nil means none.
	Claim(ctx context.Context, lease time.Duration, skip []int64) (*EmailDelivery, error)
	// Commit marks that the server may accept the email from now on. False means the
	// attempt is no longer the worker's, so the email must not be finished.
	Commit(ctx context.Context, d EmailDelivery) (bool, error)
	// Finish stores the result of the attempt with its Workspace event; false means
	// the attempt was already finished or taken over.
	Finish(ctx context.Context, d EmailDelivery, status model.EmailDeliveryStatus, code model.EmailDeliveryError) (bool, error)
	// RecoverExpired fails up to limit attempts whose lease expired and returns how many.
	RecoverExpired(ctx context.Context, limit int) (int, error)
}
