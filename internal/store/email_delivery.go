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

// EmailDeliveryOutcome is how one SMTP attempt ended.
type EmailDeliveryOutcome struct {
	// Status is sent or failed.
	Status model.EmailDeliveryStatus
	Code   model.EmailDeliveryError
	// Retryable tells that the server certainly did not accept the email and the
	// cause may pass, so the series may try again.
	Retryable bool
}

// EmailDeliveryStore moves outbound messages through their SMTP attempts.
type EmailDeliveryStore interface {
	// Claim takes the queued message of a profile not in skip that became due
	// first; nil means none.
	Claim(ctx context.Context, lease time.Duration, skip []int64) (*EmailDelivery, error)
	// Commit marks that the server may accept the email from now on. False means the
	// attempt is no longer the worker's, so the email must not be finished.
	Commit(ctx context.Context, d EmailDelivery) (bool, error)
	// Finish atomically writes Sent, Failed or a scheduled retry and its event.
	// applied is false if the attempt already finished or lost ownership.
	Finish(ctx context.Context, d EmailDelivery, outcome EmailDeliveryOutcome) (applied, requeued bool, err error)
	// RecoverExpired finishes up to limit attempts whose lease expired, after the
	// cursor when one is given. A busy or failing one is left for a later run.
	RecoverExpired(ctx context.Context, after *EmailDeliveryCursor, limit int) (EmailDeliveryRecovery, error)
	// ChangeDelivery locks the visible Thread then the message and writes the change with its event.
	// decide sees nil for an invisible resource and can reject the change.
	ChangeDelivery(ctx context.Context, domainID, userID, threadID, messageID int64,
		decide func(*EmailReplyThread, *EmailDeliveryMessage) (EmailDeliveryChange, error)) (*EmailDeliveryMessage, error)
}

// EmailDeliveryMessage is a message as a delivery command sees it, locked.
type EmailDeliveryMessage struct {
	ID       int64
	Outbound bool
	// AuthorUserID is 0 for an inbound message.
	AuthorUserID  int64
	Status        model.EmailDeliveryStatus
	Phase         string
	Code          model.EmailDeliveryError
	Version       int64
	ManualRetries int
	// Age is the time since the email was accepted, by the database clock.
	Age time.Duration
}

// EmailDeliveryChange is what a command does to a delivery.
type EmailDeliveryChange int

// Changes of a delivery by its author.
const (
	// EmailDeliveryRetry queues a failed email again and starts a new series.
	EmailDeliveryRetry EmailDeliveryChange = iota + 1
	// EmailDeliveryCancel stops an email before the server may have it.
	EmailDeliveryCancel
)

// EmailDeliveryCursor is the place of an expired attempt in the order of recovery.
type EmailDeliveryCursor struct {
	LeaseUntil time.Time
	MessageID  int64
}

// EmailDeliveryRecovery is one batch of recovery.
type EmailDeliveryRecovery struct {
	Recovered int
	Scanned   int
	// Next is the last attempt looked at; the following batch starts after it.
	Next EmailDeliveryCursor
	// Failed are the errors of single attempts, each rolled back on its own.
	Failed []error
}
