package service

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/model"
)

// IMAPConnection returns the IMAP settings of a profile with decrypted credentials for background polling.
func (s *EmailProfileService) IMAPConnection(ctx context.Context, profile *model.EmailProfile) (mailinfra.IMAPConnection, error) {
	connection := mailinfra.IMAPConnection{
		Host:     profile.IMAPHost,
		Port:     profile.IMAPPort,
		Security: profile.IMAPSecurity,
		Username: profile.Username,
		AuthType: profile.AuthType,
	}

	if profile.AuthType == model.EmailAuthTypeOAuth2 {
		accessToken, err := s.accessToken(ctx, profile.DomainID, profile.ID)
		if err != nil {
			return mailinfra.IMAPConnection{}, err
		}

		connection.AccessToken = accessToken

		return connection, nil
	}

	password, err := s.openBasicPassword(ctx, profile.DomainID, profile.ID)
	if err != nil {
		return mailinfra.IMAPConnection{}, err
	}
	if password == "" {
		return mailinfra.IMAPConnection{}, invalidEmailProfile("password_required", "password is required")
	}

	connection.Password = password

	return connection, nil
}

// RequiresReauthorization reports whether a credentials error can be fixed only by authorizing the profile again.
func RequiresReauthorization(err error) bool {
	return isOAuthReauthorizationRequired(err) || kiterrors.ID(err) == "email.profile.oauth_not_connected"
}
