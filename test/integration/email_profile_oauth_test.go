//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func TestSetOAuthRefreshTokenAndStateReturnsStoredProfile(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	ctx := context.Background()

	const domainID = 1
	profileID := testhelpers.SeedProfile(t, db, domainID, "oauth")
	profiles := storepostgres.NewEmailProfileStore(db)

	// A failed connection test and an old updated_at make every returned change visible.
	staleSince := time.Now().Add(-time.Hour)
	markStale := func(t *testing.T) {
		t.Helper()

		if _, err := db.Exec(ctx, `
UPDATE email.profile SET
    auth_type = 'oauth2', oauth_provider = 'google', oauth_client_id = 'client',
    connection_state = 'error', connection_error = 'IMAP: login failed', updated_at = $3
WHERE domain_id = $1 AND id = $2`, domainID, profileID, staleSince); err != nil {
			t.Fatalf("mark profile stale: %v", err)
		}
	}
	requireStored := func(t *testing.T, got *model.EmailProfile, connected bool) {
		t.Helper()

		if got.OAuthConnected != connected || got.ConnectionState != model.EmailConnectionStateIdle ||
			got.ConnectionError != "" || !got.UpdatedAt.After(staleSince) {
			t.Fatalf("returned profile = connected %v, state %q, error %q, updated %v; want connected %v, idle, no error, updated after %v",
				got.OAuthConnected, got.ConnectionState, got.ConnectionError, got.UpdatedAt, connected, staleSince)
		}

		stored, err := profiles.Locate(ctx, domainID, profileID)
		if err != nil {
			t.Fatalf("locate: %v", err)
		}
		if !stored.UpdatedAt.Equal(got.UpdatedAt) || stored.OAuthConnected != got.OAuthConnected {
			t.Fatalf("returned profile differs from the stored one: %+v vs %+v", got, stored)
		}
	}

	t.Run("a new authorization connects the profile", func(t *testing.T) {
		markStale(t)

		got, err := profiles.SetOAuthRefreshTokenAndState(
			ctx, domainID, profileID, []byte("sealed-refresh-token"), model.EmailConnectionStateIdle,
		)
		if err != nil {
			t.Fatalf("set refresh token: %v", err)
		}
		requireStored(t, got, true)
	})

	t.Run("a disconnect clears the authorization", func(t *testing.T) {
		markStale(t)

		got, err := profiles.SetOAuthRefreshTokenAndState(ctx, domainID, profileID, nil, model.EmailConnectionStateIdle)
		if err != nil {
			t.Fatalf("clear refresh token: %v", err)
		}
		requireStored(t, got, false)
	})

	t.Run("another domain sees no profile", func(t *testing.T) {
		_, err := profiles.SetOAuthRefreshTokenAndState(ctx, domainID+1, profileID, nil, model.EmailConnectionStateIdle)
		if kiterrors.ID(err) != "store.email_profile.not_found" {
			t.Fatalf("error = %v, want store.email_profile.not_found", err)
		}
	})
}
