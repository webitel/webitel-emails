package service

import (
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
)

func TestEmailProfileOAuthStateValidAt(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	valid := emailProfileOAuthState{
		DomainID:  1,
		UserID:    2,
		ProfileID: 3,
		Provider:  model.EmailOAuthProviderGoogle,
		ExpiresAt: now.Unix() + 1,
	}

	tests := []struct {
		name   string
		mutate func(*emailProfileOAuthState)
		want   bool
	}{
		{"valid", func(*emailProfileOAuthState) {}, true},
		{"expires exactly now", func(s *emailProfileOAuthState) { s.ExpiresAt = now.Unix() }, false},
		{"expired", func(s *emailProfileOAuthState) { s.ExpiresAt = now.Unix() - 1 }, false},
		{"missing domain", func(s *emailProfileOAuthState) { s.DomainID = 0 }, false},
		{"missing user", func(s *emailProfileOAuthState) { s.UserID = 0 }, false},
		{"missing profile", func(s *emailProfileOAuthState) { s.ProfileID = 0 }, false},
		{"unknown provider", func(s *emailProfileOAuthState) { s.Provider = "github" }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := valid
			tt.mutate(&state)

			if got := state.validAt(now); got != tt.want {
				t.Fatalf("validAt() = %v, want %v", got, tt.want)
			}
		})
	}
}
