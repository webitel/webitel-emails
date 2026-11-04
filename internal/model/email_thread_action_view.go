package model

import "time"

// EmailThreadActionView is one recorded transition of a Thread as the read API shows it.
type EmailThreadActionView struct {
	ProjectionVersion int64
	OccurredAt        time.Time
	Transition        string
	AttemptState      string
	Result            string
	Source            string
	// Actor is the operator who sent the command; Agent and TransferToUser are users of agents.
	Actor             *Lookup
	Agent             *Lookup
	QueueID           *int64
	TransferToQueueID *int64
	TransferToUser    *Lookup
	// Comment is plain text of the operator, never HTML.
	Comment string
}

// EmailThreadActionFilter selects a page of the actions of one Thread the user may see.
type EmailThreadActionFilter struct {
	DomainID int64
	UserID   int64
	ThreadID int64
	// After is the projection_version of the last action of the previous page, 0 for the first.
	After int64
	Size  int
}
