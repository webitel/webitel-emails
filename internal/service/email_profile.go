// Package service contains the application use cases of the email service.
package service

import (
	"context"
	"fmt"
	stdmail "net/mail"
	"strings"
	"sync"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/infra/crypto"
	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	oauthinfra "github.com/webitel/webitel-emails/infra/oauth"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const minFetchIntervalSeconds int32 = 5

// EmailProfileService coordinates Email Profile CRUD operations.
type EmailProfileService struct {
	store     store.EmailProfileStore
	encryptor crypto.Encryptor
	imap      mailinfra.IMAPClient
	smtp      mailinfra.SMTPClient
	oauth     oauthinfra.ConfigResolver

	// oauthTokenCache reuses access tokens and serializes refreshes per profile.
	oauthTokenCache sync.Map // emailProfileTokenKey -> *emailProfileTokenCacheEntry
}

// NewEmailProfileService creates an Email Profile service.
func NewEmailProfileService(
	store store.EmailProfileStore,
	encryptor crypto.Encryptor,
	imap mailinfra.IMAPClient,
	smtp mailinfra.SMTPClient,
	oauth oauthinfra.ConfigResolver,
) *EmailProfileService {
	return &EmailProfileService{
		store:     store,
		encryptor: encryptor,
		imap:      imap,
		smtp:      smtp,
		oauth:     oauth,
	}
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
func (s *EmailProfileService) Create(
	ctx context.Context,
	domainID, userID int64,
	profile *model.EmailProfile,
	password string,
	oauthClientSecret string,
) (*model.EmailProfile, error) {
	if err := validateEmailProfile(profile); err != nil {
		return nil, err
	}

	sealedPassword, err := s.sealBasicPassword(ctx, profile, password, true)
	if err != nil {
		return nil, err
	}
	sealedOAuthClientSecret, err := s.sealOAuthClientSecret(ctx, profile, oauthClientSecret, true)
	if err != nil {
		return nil, err
	}

	return s.store.Create(ctx, domainID, userID, profile, sealedPassword, sealedOAuthClientSecret)
}

// Update replaces writable Email Profile settings within a domain.
func (s *EmailProfileService) Update(
	ctx context.Context,
	domainID, userID, id int64,
	profile *model.EmailProfile,
	password string,
	oauthClientSecret string,
) (*model.EmailProfile, error) {
	if err := validateEmailProfile(profile); err != nil {
		return nil, err
	}

	entry := s.oauthTokenEntry(domainID, id)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	current, err := s.store.Locate(ctx, domainID, id)
	if err != nil {
		return nil, err
	}

	sealedPassword, err := s.sealBasicPassword(ctx, profile, password, false)
	if err != nil {
		return nil, err
	}
	requireOAuthClientSecret := profile.AuthType == model.EmailAuthTypeOAuth2 &&
		(current.AuthType != model.EmailAuthTypeOAuth2 ||
			current.OAuthProvider != profile.OAuthProvider ||
			current.OAuthClientID != profile.OAuthClientID)
	sealedOAuthClientSecret, err := s.sealOAuthClientSecret(
		ctx, profile, oauthClientSecret, requireOAuthClientSecret,
	)
	if err != nil {
		return nil, err
	}

	updated, err := s.store.Update(
		ctx, domainID, userID, id, profile, sealedPassword, sealedOAuthClientSecret,
	)
	if err != nil {
		return nil, err
	}
	entry.token = nil

	return updated, nil
}

// Delete removes an Email Profile within a domain and returns its last state.
func (s *EmailProfileService) Delete(ctx context.Context, domainID, id int64) (*model.EmailProfile, error) {
	entry := s.oauthTokenEntry(domainID, id)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	profile, err := s.store.Delete(ctx, domainID, id)
	if err != nil {
		return nil, err
	}
	entry.token = nil

	return profile, nil
}

// Test validates the saved settings against IMAP and SMTP using Basic Auth or OAuth2;
// each protocol's failure is reported in its own result and does not skip the other.
func (s *EmailProfileService) Test(ctx context.Context, domainID, id int64) (*model.EmailProfileTestResult, error) {
	profile, err := s.store.Locate(ctx, domainID, id)
	if err != nil {
		return nil, err
	}

	switch profile.AuthType {
	case model.EmailAuthTypeOAuth2:
		return s.testOAuth2(ctx, domainID, id, profile)
	default:
		return s.testBasic(ctx, domainID, id, profile)
	}
}

// testBasic validates the saved Basic Auth settings against IMAP and SMTP.
func (s *EmailProfileService) testBasic(
	ctx context.Context,
	domainID, id int64,
	profile *model.EmailProfile,
) (*model.EmailProfileTestResult, error) {
	password, err := s.openBasicPassword(ctx, domainID, id)
	if err != nil {
		return nil, err
	}
	if password == "" {
		return nil, invalidEmailProfile("password_required", "password is required")
	}

	imapResult := connectionTestResult(s.imap.Test(ctx, mailinfra.IMAPConnection{
		Host:     profile.IMAPHost,
		Port:     profile.IMAPPort,
		Security: profile.IMAPSecurity,
		Username: profile.Username,
		Password: password,
	}))
	smtpResult := connectionTestResult(s.smtp.Test(ctx, mailinfra.SMTPConnection{
		Host:     profile.SMTPHost,
		Port:     profile.SMTPPort,
		Security: profile.SMTPSecurity,
		Username: profile.Username,
		Password: password,
	}))

	return s.storeTestResult(ctx, domainID, id, imapResult, smtpResult)
}

// testOAuth2 validates the saved OAuth2 settings with a valid access token.
func (s *EmailProfileService) testOAuth2(
	ctx context.Context,
	domainID, id int64,
	profile *model.EmailProfile,
) (*model.EmailProfileTestResult, error) {
	accessToken, err := s.accessToken(ctx, domainID, id)
	if err != nil {
		if kiterrors.ID(err) == "email.profile.oauth_not_connected" {
			return nil, err
		}

		return s.storeOAuthTokenFailure(ctx, domainID, id, err)
	}

	imapResult := connectionTestResult(s.imap.Test(ctx, mailinfra.IMAPConnection{
		Host:        profile.IMAPHost,
		Port:        profile.IMAPPort,
		Security:    profile.IMAPSecurity,
		Username:    profile.Username,
		AuthType:    model.EmailAuthTypeOAuth2,
		AccessToken: accessToken,
	}))
	smtpResult := connectionTestResult(s.smtp.Test(ctx, mailinfra.SMTPConnection{
		Host:        profile.SMTPHost,
		Port:        profile.SMTPPort,
		Security:    profile.SMTPSecurity,
		Username:    profile.Username,
		AuthType:    model.EmailAuthTypeOAuth2,
		AccessToken: accessToken,
	}))

	return s.storeTestResult(ctx, domainID, id, imapResult, smtpResult)
}

// storeOAuthTokenFailure stores a token refresh failure for both protocols
// and marks rejected refresh tokens as requiring reauthorization.
func (s *EmailProfileService) storeOAuthTokenFailure(
	ctx context.Context,
	domainID, id int64,
	tokenErr error,
) (*model.EmailProfileTestResult, error) {
	tokenError := model.EmailConnectionTestResult{Error: tokenErr.Error()}
	result := &model.EmailProfileTestResult{IMAP: tokenError, SMTP: tokenError}

	state := model.EmailConnectionStateError
	if isOAuthReauthorizationRequired(tokenErr) {
		state = model.EmailConnectionStateReauthorizationRequired
	}

	if err := s.store.SetConnectionResult(ctx, domainID, id, state, tokenErr.Error(), false); err != nil {
		return nil, err
	}

	return result, nil
}

// storeTestResult persists the latest connection state derived from an IMAP
// and SMTP check and returns the result to the caller.
func (s *EmailProfileService) storeTestResult(
	ctx context.Context,
	domainID, id int64,
	imapResult, smtpResult model.EmailConnectionTestResult,
) (*model.EmailProfileTestResult, error) {
	result := &model.EmailProfileTestResult{
		IMAP: imapResult,
		SMTP: smtpResult,
	}
	state, connectionError, successful := connectionState(result)
	if err := s.store.SetConnectionResult(
		ctx,
		domainID,
		id,
		state,
		connectionError,
		successful,
	); err != nil {
		return nil, err
	}

	return result, nil
}

func connectionTestResult(err error) model.EmailConnectionTestResult {
	if err == nil {
		return model.EmailConnectionTestResult{Success: true}
	}

	return model.EmailConnectionTestResult{Error: err.Error()}
}

func connectionState(result *model.EmailProfileTestResult) (model.EmailConnectionState, string, bool) {
	if result.IMAP.Success && result.SMTP.Success {
		return model.EmailConnectionStateReady, "", true
	}

	errors := make([]string, 0, 2)
	if !result.IMAP.Success {
		errors = append(errors, "IMAP: "+result.IMAP.Error)
	}
	if !result.SMTP.Success {
		errors = append(errors, "SMTP: "+result.SMTP.Error)
	}

	return model.EmailConnectionStateError, strings.Join(errors, "; "), false
}

// sealBasicPassword encrypts a write-only Basic Auth password. A missing password on update is represented by nil so the store preserves the current ciphertext.
func (s *EmailProfileService) sealBasicPassword(ctx context.Context, profile *model.EmailProfile, password string, required bool) ([]byte, error) {
	authType := profile.AuthType
	if authType == "" {
		authType = model.EmailAuthTypeBasic
	}

	if authType != model.EmailAuthTypeBasic {
		if password != "" {
			return nil, invalidEmailProfile(
				"password_not_allowed",
				"password is available only for Basic authentication",
			)
		}

		return nil, nil
	}

	if password == "" {
		if required {
			return nil, invalidEmailProfile("password_required", "password is required")
		}

		return nil, nil
	}

	return s.encryptor.Encrypt(ctx, []byte(password))
}

// sealOAuthClientSecret encrypts a write-only OAuth client secret. A missing
// secret on update preserves the current value unless OAuth settings changed.
func (s *EmailProfileService) sealOAuthClientSecret(
	ctx context.Context,
	profile *model.EmailProfile,
	clientSecret string,
	required bool,
) ([]byte, error) {
	if profile.AuthType != model.EmailAuthTypeOAuth2 {
		if clientSecret != "" {
			return nil, invalidEmailProfile(
				"oauth_client_secret_not_allowed",
				"OAuth client secret is available only for OAuth2 authentication",
			)
		}

		return nil, nil
	}
	if clientSecret == "" {
		if required {
			return nil, invalidEmailProfile("oauth_client_secret_required", "OAuth client secret is required")
		}

		return nil, nil
	}

	return s.encryptor.Encrypt(ctx, []byte(clientSecret))
}

// openBasicPassword loads and decrypts a profile password for internal use.
func (s *EmailProfileService) openBasicPassword(ctx context.Context, domainID, id int64) (string, error) {
	sealedPassword, err := s.store.GetPassword(ctx, domainID, id)
	if err != nil {
		return "", err
	}

	password, err := s.encryptor.Decrypt(ctx, sealedPassword)
	if err != nil {
		return "", err
	}

	return string(password), nil
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
	if strings.TrimSpace(profile.Username) == "" {
		return invalidEmailProfile("username_required", "username is required")
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
	if profile.AuthType == model.EmailAuthTypeOAuth2 {
		if !validOAuthProvider(profile.OAuthProvider) {
			return invalidEmailProfile("oauth_provider_invalid", "OAuth provider is invalid")
		}
		if strings.TrimSpace(profile.OAuthClientID) == "" {
			return invalidEmailProfile("oauth_client_id_required", "OAuth client ID is required")
		}
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
	address, err := stdmail.ParseAddress(value)

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
