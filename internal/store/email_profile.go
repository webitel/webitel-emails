// Package store declares storage contracts used by the service layer.
package store

import (
	"context"

	"github.com/webitel/webitel-emails/internal/model"
)

// EmailProfileStore persists Email Profiles within a domain.
type EmailProfileStore interface {
	// List returns one page of profiles and reports whether another page exists.
	List(ctx context.Context, domainID int64, filter model.EmailProfileFilter) ([]*model.EmailProfile, bool, error)
	// Locate returns a profile only when it belongs to the requested domain.
	Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error)
	// Create stores a profile owned by domainID and records userID as its author.
	Create(ctx context.Context, domainID, userID int64, profile *model.EmailProfile) (*model.EmailProfile, error)
	// Update replaces writable profile settings within a domain.
	Update(ctx context.Context, domainID, userID, id int64, profile *model.EmailProfile) (*model.EmailProfile, error)
	// Delete removes a profile within a domain and returns its last state.
	Delete(ctx context.Context, domainID, id int64) (*model.EmailProfile, error)
}
