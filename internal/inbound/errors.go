package inbound

import (
	"github.com/webitel/webitel-emails/internal/model"
)

// PermanentError marks an email that fails the same way on every retry. Such an
// email goes to quarantine instead of blocking the rest of the mailbox.
type PermanentError struct {
	Category model.InboundFailureCategory
	Err      error
}

func (e *PermanentError) Error() string { return e.Err.Error() }

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a failure that retrying cannot fix.
func Permanent(category model.InboundFailureCategory, err error) error {
	return &PermanentError{Category: category, Err: err}
}
