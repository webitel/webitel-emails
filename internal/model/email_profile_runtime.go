package model

import "time"

// EmailProfileAssignment is the fencing token of the instance serving a profile.
type EmailProfileAssignment struct {
	ProfileID       int64
	DomainID        int64
	OwnerInstanceID string
	Generation      int64
}

// EmailProfileRuntime is the internal serving state of an Email Profile.
type EmailProfileRuntime struct {
	ProfileID int64
	DomainID  int64

	// Empty when the profile is not assigned.
	OwnerInstanceID      string
	AssignmentGeneration int64

	NextCheckAt    *time.Time
	ProviderCursor *ProviderCursor

	LastPollStartedAt  *time.Time
	LastPollFinishedAt *time.Time
	LastPollError      string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Assignment returns the current owner's token, or false when unassigned.
func (r *EmailProfileRuntime) Assignment() (EmailProfileAssignment, bool) {
	if r == nil || r.OwnerInstanceID == "" {
		return EmailProfileAssignment{}, false
	}

	return EmailProfileAssignment{
		ProfileID:       r.ProfileID,
		DomainID:        r.DomainID,
		OwnerInstanceID: r.OwnerInstanceID,
		Generation:      r.AssignmentGeneration,
	}, true
}

// EmailProfileOwnership is a profile with its current owner, as seen by the leader.
type EmailProfileOwnership struct {
	ProfileID       int64
	Enabled         bool
	OwnerInstanceID string
}

// ProviderCursor is the mailbox sync position of one provider.
type ProviderCursor struct {
	IMAP *IMAPCursor `json:"imap,omitempty"`
}

// IsEmpty reports whether no provider position is stored.
func (c *ProviderCursor) IsEmpty() bool {
	return c == nil || c.IMAP == nil
}

// IMAPCursor is valid only for the same mailbox and UIDVALIDITY.
type IMAPCursor struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	// Highest UID confirmed by the handler.
	LastUID uint32 `json:"last_uid"`
}

// IsValidFor reports whether the cursor can be continued.
func (c *IMAPCursor) IsValidFor(mailbox string, uidValidity uint32) bool {
	return c != nil && c.Mailbox == mailbox && c.UIDValidity == uidValidity
}

// EmailProfilePollResult is recorded when a poll finishes.
type EmailProfilePollResult struct {
	// Nil keeps the stored cursor.
	ProviderCursor *ProviderCursor
	// Nil schedules the next poll after the profile fetch interval.
	NextCheckAt *time.Time
	// Empty on success.
	Error string

	// Empty leaves the profile connection state and error unchanged.
	ConnectionState EmailConnectionState
	ConnectionError string
	// Updates the last successful connection time.
	Connected bool
}
