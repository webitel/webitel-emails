// Package store declares storage contracts used by the service layer.
package store

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/internal/model"
)

// ErrEmailProfileHasHistory means the profile still owns Threads or Messages.
var ErrEmailProfileHasHistory = kiterrors.New(
	"email profile contains email history and cannot be deleted; disable it instead",
	kiterrors.WithID("store.email_profile.has_history"),
	kiterrors.WithCode(codes.FailedPrecondition),
)

// EmailProfileOAuthCredentials contains encrypted OAuth values loaded only for
// internal authorization flows.
type EmailProfileOAuthCredentials struct {
	ClientSecret []byte
	RefreshToken []byte
}

// EmailProfileStore persists Email Profiles within a domain.
type EmailProfileStore interface {
	// List returns one page of profiles and reports whether another page exists.
	List(ctx context.Context, domainID int64, filter model.EmailProfileFilter) ([]*model.EmailProfile, bool, error)
	// Locate returns a profile only when it belongs to the requested domain.
	Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error)
	// Create stores a profile owned by domainID and records userID as its author.
	Create(ctx context.Context, domainID, userID int64, profile *model.EmailProfile, password, oauthClientSecret []byte) (*model.EmailProfile, error)
	// Update replaces writable profile settings within a domain.
	Update(ctx context.Context, domainID, userID, id int64, profile *model.EmailProfile, password, oauthClientSecret []byte) (*model.EmailProfile, error)
	// GetPassword returns the stored encrypted Basic Auth password within a domain.
	GetPassword(ctx context.Context, domainID, id int64) ([]byte, error)
	// GetOAuthCredentials returns encrypted OAuth values within a domain.
	GetOAuthCredentials(ctx context.Context, domainID, id int64) (*EmailProfileOAuthCredentials, error)
	// SetOAuthRefreshToken stores an encrypted refresh token. A nil value clears it.
	SetOAuthRefreshToken(ctx context.Context, domainID, id int64, refreshToken []byte) error
	// SetOAuthRefreshTokenAndState atomically stores a refresh token and state.
	SetOAuthRefreshTokenAndState(ctx context.Context, domainID, id int64, refreshToken []byte, state model.EmailConnectionState) error
	// SetConnectionResult stores the latest connection state and error. A
	// successful result also updates the last successful connection time.
	SetConnectionResult(
		ctx context.Context,
		domainID, id int64,
		state model.EmailConnectionState,
		connectionError string,
		successful bool,
	) error
	// Delete removes a profile within a domain and returns its last state.
	Delete(ctx context.Context, domainID, id int64) (*model.EmailProfile, error)
}
