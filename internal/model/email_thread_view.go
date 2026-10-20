package model

import "time"

// EmailThreadSortField is a Thread field the read API may sort by.
type EmailThreadSortField string

// Sortable Thread fields.
const (
	EmailThreadSortLastMessageAt   EmailThreadSortField = "last_message_at"
	EmailThreadSortCreatedAt       EmailThreadSortField = "created_at"
	EmailThreadSortUpdatedAt       EmailThreadSortField = "updated_at"
	EmailThreadSortStatusChangedAt EmailThreadSortField = "status_changed_at"
)

// EmailThreadSort is one validated sort criterion.
type EmailThreadSort struct {
	Field EmailThreadSortField
	Desc  bool
}

// EmailThreadFilter selects a page of one user's Threads. The domain and the
// user come from the caller's session; the other fields only narrow the result.
type EmailThreadFilter struct {
	DomainID   int64
	UserID     int64
	Assignment EmailThreadAssignment
	Statuses   []EmailThreadStatus
	ProfileIDs []int64
	// ContactID zero means any contact.
	ContactID int64

	Page int
	Size int
	Sort []EmailThreadSort
}

// EmailThreadPreview summarizes the newest ready message of a Thread.
type EmailThreadPreview struct {
	MessageID  int64
	Direction  EmailMessageDirection
	From       *EmailAddress
	Text       string
	ReceivedAt time.Time
}

// EmailThreadView is a Thread as the read API shows it to one user.
type EmailThreadView struct {
	ID              int64
	Profile         Lookup
	Subject         string
	Status          EmailThreadStatus
	StatusChangedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastMessageAt   *time.Time
	CompletedAt     *time.Time

	ContactID              *int64
	ContactResolutionState EmailContactResolutionState

	QueueID         *int64
	PostponedUntil  *time.Time
	CurrentUser     *Lookup
	LastHandledUser *Lookup

	Access EmailThreadAccess
	// Set only for EmailThreadAccessCurrent.
	CurrentAttemptID      *int64
	CurrentAttemptVersion *int64

	Preview *EmailThreadPreview
}
