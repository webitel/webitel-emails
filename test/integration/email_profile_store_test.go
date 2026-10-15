//go:build integration

package integration_test

import (
	"context"
	"slices"
	"testing"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func TestEmailProfileStoreSearchAndFields(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	ctx := context.Background()
	profiles := storepostgres.NewEmailProfileStore(db)

	const domainID = 1
	create := func(t *testing.T, name string) *model.EmailProfile {
		t.Helper()

		profile := &model.EmailProfile{
			Name: name, EmailAddress: "box@example.org", Username: "box",
			IMAPHost: "imap.example.org", IMAPPort: 993, IMAPSecurity: model.EmailConnectionSecurityTLS,
			SMTPHost: "smtp.example.org", SMTPPort: 465, SMTPSecurity: model.EmailConnectionSecurityTLS,
			AuthType: model.EmailAuthTypeBasic,
		}
		profile.ApplyDefaults()

		created, err := profiles.Create(ctx, store.CreateEmailProfileParams{
			DomainID: domainID, Profile: profile, Password: []byte("sealed"),
		})
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}

		return created
	}
	for _, name := range []string{"Sales 50% off", "Sales 500", "sales_team", "salesXteam"} {
		create(t, name)
	}

	names := func(t *testing.T, filter model.EmailProfileFilter) []string {
		t.Helper()

		filter.Sort = "name"
		found, _, err := profiles.List(ctx, domainID, filter)
		if err != nil {
			t.Fatalf("list %+v: %v", filter, err)
		}

		result := make([]string, 0, len(found))
		for _, profile := range found {
			result = append(result, profile.Name)
		}

		return result
	}

	t.Run("search treats only * as a wildcard", func(t *testing.T) {
		tests := []struct {
			query string
			want  []string
		}{
			{"*50%*", []string{"Sales 50% off"}},
			{"sales_team", []string{"sales_team"}},
			{"SALES 500", []string{"Sales 500"}},
			{"sales*", []string{"Sales 50% off", "Sales 500", "sales_team", "salesXteam"}},
		}

		for _, tt := range tests {
			got := names(t, model.EmailProfileFilter{Query: tt.query})
			slices.Sort(got)
			slices.Sort(tt.want)
			if !slices.Equal(got, tt.want) {
				t.Errorf("search %q = %v, want %v", tt.query, got, tt.want)
			}
		}
	})

	t.Run("fields select known columns and always the id", func(t *testing.T) {
		found, _, err := profiles.List(ctx, domainID, model.EmailProfileFilter{
			Query:  "sales_team",
			Fields: []string{"name", "oauth_client_secret", "name", "password"},
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(found) != 1 {
			t.Fatalf("found %d profiles, want 1", len(found))
		}

		got := found[0]
		if got.ID == 0 || got.Name != "sales_team" || got.EmailAddress != "" || !got.CreatedAt.IsZero() {
			t.Fatalf("projection = id %d, name %q, email %q, created %v; want only id and name",
				got.ID, got.Name, got.EmailAddress, got.CreatedAt)
		}
	})

	t.Run("only the id can be requested", func(t *testing.T) {
		found, _, err := profiles.List(ctx, domainID, model.EmailProfileFilter{
			Query:  "sales_team",
			Fields: []string{"id"},
		})
		if err != nil || len(found) != 1 {
			t.Fatalf("list = %d profiles, %v", len(found), err)
		}
		if got := found[0]; got.ID == 0 || got.Name != "" || got.EmailAddress != "" {
			t.Fatalf("projection = id %d, name %q, email %q; want only id", got.ID, got.Name, got.EmailAddress)
		}
	})

	t.Run("no known field returns the full projection", func(t *testing.T) {
		found, _, err := profiles.List(ctx, domainID, model.EmailProfileFilter{
			Query:  "sales_team",
			Fields: []string{"password"},
		})
		if err != nil || len(found) != 1 {
			t.Fatalf("list = %d profiles, %v", len(found), err)
		}
		if found[0].EmailAddress == "" || found[0].Mailbox != model.DefaultEmailProfileMailbox {
			t.Fatalf("full projection missing settings: %+v", found[0])
		}
	})

	t.Run("update writes the named settings", func(t *testing.T) {
		current := create(t, "Before")
		changed := *current
		changed.Name = "After"
		changed.FetchIntervalSeconds = 60

		updated, err := profiles.Update(ctx, store.UpdateEmailProfileParams{
			DomainID: domainID, ID: current.ID, Profile: &changed,
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Name != "After" || updated.FetchIntervalSeconds != 60 || updated.EmailAddress != current.EmailAddress {
			t.Fatalf("updated profile = %+v", updated)
		}

		password, err := profiles.GetPassword(ctx, domainID, current.ID)
		if err != nil || string(password) != "sealed" {
			t.Fatalf("password after update without one = %q, %v; want kept", password, err)
		}
	})
}
