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

// EmailContactResolutionState tells apart the reasons a Thread has no contact.
type EmailContactResolutionState string

// Contact resolution states of a Thread.
const (
	// EmailContactResolutionPending means the contact was not searched yet.
	EmailContactResolutionPending EmailContactResolutionState = "pending"
	// EmailContactResolutionResolved means exactly one contact matched.
	EmailContactResolutionResolved EmailContactResolutionState = "resolved"
	// EmailContactResolutionNotFound means no contact holds that address.
	EmailContactResolutionNotFound EmailContactResolutionState = "not_found"
	// EmailContactResolutionAmbiguous means several contacts hold that address.
	EmailContactResolutionAmbiguous EmailContactResolutionState = "ambiguous"
	// EmailContactResolutionUnavailable means Contacts could not be reached, so
	// a later message of the Thread may try again.
	EmailContactResolutionUnavailable EmailContactResolutionState = "unavailable"
	// EmailContactResolutionUnlinked means an operator removed the contact.
	EmailContactResolutionUnlinked EmailContactResolutionState = "unlinked"
	// EmailContactResolutionNotApplicable is for service Threads, which never
	// resolve a contact.
	EmailContactResolutionNotApplicable EmailContactResolutionState = "not_applicable"
)

// EmailThread is one conversation within a single Email Profile.
// A service thread holds auto-replies and bounces with no known original message.
type EmailThread struct {
	ID        int64
	DomainID  int64
	ProfileID int64
	// Resolved by contact detection; nil unless ContactResolutionState is resolved.
	ContactID *int64
	// ContactResolutionState is why the Thread has or has no contact.
	ContactResolutionState EmailContactResolutionState

	Kind    EmailThreadKind
	Subject string
	Status  EmailThreadStatus

	// Advanced only when a message becomes ready.
	LastMessageAt *time.Time
	CompletedAt   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}
