// Package oauth resolves provider-specific OAuth2 configuration.
package oauth

import (
	"fmt"

	"golang.org/x/oauth2"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/model"
)

const (
	googleAuthorizationURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL         = "https://oauth2.googleapis.com/token"
	googleMailScope        = "https://mail.google.com/"

	microsoftAuthorizationURL = "https://login.microsoftonline.com/organizations/oauth2/v2.0/authorize"
	microsoftTokenURL         = "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"
	microsoftIMAPScope        = "https://outlook.office.com/IMAP.AccessAsUser.All"
	microsoftSMTPScope        = "https://outlook.office.com/SMTP.Send"
	offlineAccessScope        = "offline_access"
)

// Resolver builds OAuth2 configuration for supported Email Profile providers.
type Resolver struct {
	redirectURL string
}

// ConfigResolver resolves OAuth2 settings for a mailbox provider.
type ConfigResolver interface {
	Config(provider model.EmailOAuthProvider, clientID, clientSecret string) (*oauth2.Config, error)
}

var _ ConfigResolver = (*Resolver)(nil)

// NewResolver creates a provider resolver with the public API Gateway callback.
func NewResolver(cfg *config.Config) *Resolver {
	return &Resolver{redirectURL: cfg.OAuth.RedirectURL}
}

// Config returns the provider endpoints and scopes with profile credentials.
func (r *Resolver) Config(
	provider model.EmailOAuthProvider,
	clientID, clientSecret string,
) (*oauth2.Config, error) {
	result := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  r.redirectURL,
	}

	switch provider {
	case model.EmailOAuthProviderGoogle:
		result.Endpoint = oauth2.Endpoint{
			AuthURL:   googleAuthorizationURL,
			TokenURL:  googleTokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		}
		result.Scopes = []string{googleMailScope}
	case model.EmailOAuthProviderMicrosoft:
		result.Endpoint = oauth2.Endpoint{
			AuthURL:   microsoftAuthorizationURL,
			TokenURL:  microsoftTokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		}
		result.Scopes = []string{
			microsoftIMAPScope,
			microsoftSMTPScope,
			offlineAccessScope,
		}
	default:
		return nil, fmt.Errorf("oauth: unsupported provider %q", provider)
	}
	if r.redirectURL == "" {
		return nil, fmt.Errorf("oauth: redirect URL is not configured")
	}

	return result, nil
}
