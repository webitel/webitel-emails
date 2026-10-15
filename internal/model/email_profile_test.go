package model

import "testing"

func TestEmailProfileOAuthClientChangedFrom(t *testing.T) {
	oauth := func(provider EmailOAuthProvider, clientID string) *EmailProfile {
		return &EmailProfile{AuthType: EmailAuthTypeOAuth2, OAuthProvider: provider, OAuthClientID: clientID}
	}
	basic := &EmailProfile{AuthType: EmailAuthTypeBasic}
	unset := &EmailProfile{}

	tests := []struct {
		name    string
		current *EmailProfile
		next    *EmailProfile
		changed bool
	}{
		{"same OAuth2 client", oauth(EmailOAuthProviderGoogle, "a"), oauth(EmailOAuthProviderGoogle, "a"), false},
		{"another client ID", oauth(EmailOAuthProviderGoogle, "a"), oauth(EmailOAuthProviderGoogle, "b"), true},
		{"another provider", oauth(EmailOAuthProviderGoogle, "a"), oauth(EmailOAuthProviderMicrosoft, "a"), true},
		{"Basic to OAuth2", basic, oauth(EmailOAuthProviderGoogle, "a"), true},
		{"default Basic to OAuth2", unset, oauth(EmailOAuthProviderGoogle, "a"), true},
		{"OAuth2 to Basic", oauth(EmailOAuthProviderGoogle, "a"), basic, false},
		{"Basic stays Basic", basic, unset, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.next.OAuthClientChangedFrom(tt.current); got != tt.changed {
				t.Fatalf("OAuthClientChangedFrom() = %v, want %v", got, tt.changed)
			}
		})
	}
}
