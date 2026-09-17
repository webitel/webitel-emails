// Package service contains the application use cases of the email service.
package service

import (
	"context"
	"fmt"
	"net/mail"
	"strings"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const minFetchIntervalSeconds int32 = 5

// EmailProfileService coordinates Email Profile CRUD operations.
type EmailProfileService struct {
	store store.EmailProfileStore
}

// NewEmailProfileService creates an Email Profile service.
func NewEmailProfileService(store store.EmailProfileStore) *EmailProfileService {
	return &EmailProfileService{store: store}
}

// List returns one page of Email Profiles within a domain.
func (s *EmailProfileService) List(ctx context.Context, domainID int64, filter model.EmailProfileFilter) ([]*model.EmailProfile, bool, error) {
	return s.store.List(ctx, domainID, filter)
}

// Locate returns an Email Profile within a domain.
func (s *EmailProfileService) Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error) {
	return s.store.Locate(ctx, domainID, id)
}

// Create stores an Email Profile within a domain.
func (s *EmailProfileService) Create(ctx context.Context, domainID, userID int64, profile *model.EmailProfile) (*model.EmailProfile, error) {
	if err := validateEmailProfile(profile); err != nil {
		return nil, err
	}

	return s.store.Create(ctx, domainID, userID, profile)
}

// Update replaces writable Email Profile settings within a domain.
func (s *EmailProfileService) Update(ctx context.Context, domainID, userID, id int64, profile *model.EmailProfile) (*model.EmailProfile, error) {
	if err := validateEmailProfile(profile); err != nil {
		return nil, err
	}

	return s.store.Update(ctx, domainID, userID, id, profile)
}

// Delete removes an Email Profile within a domain and returns its last state.
func (s *EmailProfileService) Delete(ctx context.Context, domainID, id int64) (*model.EmailProfile, error) {
	return s.store.Delete(ctx, domainID, id)
}

// validateEmailProfile checks persisted profile fields without establishing
// IMAP, SMTP, or OAuth connections.
func validateEmailProfile(profile *model.EmailProfile) error {
	if profile == nil {
		return invalidEmailProfile("input_required", "email profile is required")
	}
	if strings.TrimSpace(profile.Name) == "" {
		return invalidEmailProfile("name_required", "name is required")
	}
	if strings.TrimSpace(profile.EmailAddress) == "" {
		return invalidEmailProfile("email_address_required", "email address is required")
	}
	if !validEmailAddress(profile.EmailAddress) {
		return invalidEmailProfile("email_address_invalid", "email address is invalid")
	}
	if profile.ReplyTo != "" && !validEmailAddress(profile.ReplyTo) {
		return invalidEmailProfile("reply_to_invalid", "reply-to address is invalid")
	}
	if strings.TrimSpace(profile.IMAPHost) == "" {
		return invalidEmailProfile("imap_host_required", "IMAP host is required")
	}
	if err := validateEmailPort("imap", profile.IMAPPort); err != nil {
		return err
	}
	if !validConnectionSecurity(profile.IMAPSecurity) {
		return invalidEmailProfile("imap_security_invalid", "IMAP security is invalid")
	}
	if strings.TrimSpace(profile.SMTPHost) == "" {
		return invalidEmailProfile("smtp_host_required", "SMTP host is required")
	}
	if err := validateEmailPort("smtp", profile.SMTPPort); err != nil {
		return err
	}
	if !validConnectionSecurity(profile.SMTPSecurity) {
		return invalidEmailProfile("smtp_security_invalid", "SMTP security is invalid")
	}
	if !validAuthType(profile.AuthType) {
		return invalidEmailProfile("auth_type_invalid", "authentication type is invalid")
	}
	if profile.AuthType == model.EmailAuthTypeOAuth2 && !validOAuthProvider(profile.OAuthProvider) {
		return invalidEmailProfile("oauth_provider_invalid", "OAuth provider is invalid")
	}
	if profile.FlowID != nil && *profile.FlowID <= 0 {
		return invalidEmailProfile("flow_id_invalid", "flow id must be greater than zero")
	}
	if profile.FetchIntervalSeconds != 0 && profile.FetchIntervalSeconds < minFetchIntervalSeconds {
		return invalidEmailProfile(
			"fetch_interval_invalid",
			fmt.Sprintf("fetch interval must be at least %d seconds", minFetchIntervalSeconds),
		)
	}

	return nil
}

func validEmailAddress(value string) bool {
	value = strings.TrimSpace(value)
	address, err := mail.ParseAddress(value)

	return err == nil && address.Address == value
}

func validateEmailPort(protocol string, port int32) error {
	if port < 1 || port > 65535 {
		return invalidEmailProfile(protocol+"_port_invalid", strings.ToUpper(protocol)+" port is invalid")
	}

	return nil
}

func validConnectionSecurity(value model.EmailConnectionSecurity) bool {
	return value == model.EmailConnectionSecurityTLS || value == model.EmailConnectionSecurityStartTLS
}

func validAuthType(value model.EmailAuthType) bool {
	return value == "" || value == model.EmailAuthTypeBasic || value == model.EmailAuthTypeOAuth2
}

func validOAuthProvider(value model.EmailOAuthProvider) bool {
	return value == model.EmailOAuthProviderGoogle || value == model.EmailOAuthProviderMicrosoft
}

func invalidEmailProfile(id, message string) error {
	return kiterrors.InvalidArgument(message, kiterrors.WithID("email.profile."+id))
}
