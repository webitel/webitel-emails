package model

import "time"

// EmailThreadKind separates operator conversations from service-only threads.
type EmailThreadKind string

// Email Thread kinds.
const (
	EmailThreadKindRegular EmailThreadKind = "regular"
	EmailThreadKindService EmailThreadKind = "service"
)

// EmailThreadStatus is the business status of a conversation owned by this service.
type EmailThreadStatus string

// Email Thread statuses.
const (
	EmailThreadStatusNew         EmailThreadStatus = "new"
	EmailThreadStatusDistributed EmailThreadStatus = "distributed"
	EmailThreadStatusInProgress  EmailThreadStatus = "in_progress"
	EmailThreadStatusProcessed   EmailThreadStatus = "processed"
)

// EmailThread is one conversation within a single Email Profile.
// A service thread holds auto-replies and bounces with no known original message.
type EmailThread struct {
	ID        int64
	DomainID  int64
	ProfileID int64
	// Resolved by contact detection; nil until then.
	ContactID *int64

	Kind    EmailThreadKind
	Subject string
	Status  EmailThreadStatus

	// Advanced only when a message becomes ready.
	LastMessageAt *time.Time
	CompletedAt   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}
