package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
)

const oauthStateTTL = 10 * time.Minute

// emailProfileTokenKey identifies a profile's cached OAuth2 token.
type emailProfileTokenKey struct {
	domainID, id int64
}

type emailProfileTokenCacheEntry struct {
	mu    sync.Mutex
	token *oauth2.Token
}

type emailProfileOAuthState struct {
	DomainID  int64                    `json:"domain_id"`
	UserID    int64                    `json:"user_id"`
	ProfileID int64                    `json:"profile_id"`
	Provider  model.EmailOAuthProvider `json:"provider"`
	ExpiresAt int64                    `json:"expires_at"`
}

// BeginOAuth creates the provider authorization URL for an Email Profile.
func (s *EmailProfileService) BeginOAuth(ctx context.Context, domainID, userID, id int64) (*model.EmailProfileOAuthStart, error) {
	profile, err := s.store.Locate(ctx, domainID, id)
	if err != nil {
		return nil, err
	}
	config, err := s.oauthConfig(ctx, domainID, profile)
	if err != nil {
		return nil, err
	}
	state, err := s.newOAuthState(ctx, domainID, userID, profile)
	if err != nil {
		return nil, err
	}

	return &model.EmailProfileOAuthStart{
		// prompt=consent forces the provider's consent screen even when the
		// user already authorized this profile before, which is required to
		// reliably get a refresh_token back from Google on a repeat
		// authorization (e.g. after Disconnect or reauthorization_required).
		AuthURL: config.AuthCodeURL(
			state,
			oauth2.AccessTypeOffline,
			oauth2.SetAuthURLParam("prompt", "consent"),
		),
		State: state,
	}, nil
}

// CompleteOAuth exchanges the provider callback code and stores the refresh token.
func (s *EmailProfileService) CompleteOAuth(ctx context.Context, domainID, userID, id int64, code, encodedState string) (*model.EmailProfile, error) {
	if strings.TrimSpace(code) == "" {
		return nil, invalidEmailProfile("oauth_code_required", "OAuth authorization code is required")
	}

	state, err := s.openOAuthState(ctx, encodedState)
	if err != nil || state.DomainID != domainID || state.UserID != userID || state.ProfileID != id {
		return nil, invalidOAuthState()
	}

	entry := s.oauthTokenEntry(domainID, id)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	profile, err := s.store.Locate(ctx, domainID, id)
	if err != nil {
		return nil, err
	}
	if state.Provider != profile.OAuthProvider {
		return nil, invalidOAuthState()
	}

	config, err := s.oauthConfig(ctx, domainID, profile)
	if err != nil {
		return nil, err
	}
	token, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, invalidEmailProfile(
			"oauth_code_exchange_failed",
			"OAuth authorization code could not be exchanged",
		)
	}
	if token.RefreshToken == "" {
		return nil, invalidEmailProfile(
			"oauth_refresh_token_missing",
			"OAuth provider did not return a refresh token",
		)
	}

	sealedRefreshToken, err := s.encryptor.Encrypt(ctx, []byte(token.RefreshToken))
	if err != nil {
		return nil, err
	}

	// A new authorization remains idle until its IMAP/SMTP connection is tested.
	if err := s.store.SetOAuthRefreshTokenAndState(
		ctx, domainID, id, sealedRefreshToken, model.EmailConnectionStateIdle,
	); err != nil {
		return nil, err
	}
	entry.token = token

	return s.store.Locate(ctx, domainID, id)
}

// accessToken returns a valid cached or refreshed token for an OAuth2 profile.
func (s *EmailProfileService) accessToken(ctx context.Context, domainID, id int64) (string, error) {
	entry := s.oauthTokenEntry(domainID, id)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	profile, err := s.store.Locate(ctx, domainID, id)
	if err != nil {
		return "", err
	}

	config, err := s.oauthConfig(ctx, domainID, profile)
	if err != nil {
		return "", err
	}

	refreshToken, err := s.openOAuthRefreshToken(ctx, domainID, id)
	if err != nil {
		return "", err
	}
	if refreshToken == "" {
		return "", invalidEmailProfile(
			"oauth_not_connected",
			"email profile is not connected to an OAuth provider",
		)
	}

	baseToken := &oauth2.Token{RefreshToken: refreshToken}
	if entry.token != nil && entry.token.RefreshToken == refreshToken {
		baseToken = entry.token
	}

	token, err := config.TokenSource(ctx, baseToken).Token()
	if err != nil {
		return "", oauthRefreshError(err)
	}

	// The oauth2 package backfills Token.RefreshToken with the original
	// value when the provider does not rotate it, so this only persists a
	// write when the provider actually issued a new refresh token.
	if token.RefreshToken != refreshToken {
		sealedRefreshToken, err := s.encryptor.Encrypt(ctx, []byte(token.RefreshToken))
		if err != nil {
			return "", err
		}
		if err := s.store.SetOAuthRefreshToken(ctx, domainID, id, sealedRefreshToken); err != nil {
			return "", err
		}
	}
	entry.token = token

	return token.AccessToken, nil
}

// DisconnectOAuth clears a profile's stored OAuth refresh token, letting the
// user run BeginOAuth again. Mirrors engine's LogoutEmailProfile (which just
// clears the stored token, with no provider-side revocation call); this also
// resets connection_state, a concept LogoutEmailProfile predates.
func (s *EmailProfileService) DisconnectOAuth(ctx context.Context, domainID, id int64) (*model.EmailProfile, error) {
	entry := s.oauthTokenEntry(domainID, id)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	profile, err := s.store.Locate(ctx, domainID, id)
	if err != nil {
		return nil, err
	}
	if profile.AuthType != model.EmailAuthTypeOAuth2 {
		return nil, invalidEmailProfile(
			"oauth2_required",
			"OAuth2 authentication is required for this profile",
		)
	}

	if err := s.store.SetOAuthRefreshTokenAndState(
		ctx, domainID, id, nil, model.EmailConnectionStateIdle,
	); err != nil {
		return nil, err
	}
	entry.token = nil

	return s.store.Locate(ctx, domainID, id)
}

// isOAuthReauthorizationRequired reports whether err indicates that the
// OAuth provider rejected the stored refresh token (invalid_grant), meaning
// the profile needs the user to authorize it again.
func isOAuthReauthorizationRequired(err error) bool {
	return errors.Is(err, errOAuthReauthorizationRequired)
}

var errOAuthReauthorizationRequired = errors.New("oauth: refresh token rejected by provider")

// oauthRefreshError distinguishes a rejected refresh token (the provider
// revoked or expired the authorization) from a transport or provider-side
// failure, so a caller can react to a revoked authorization without
// inspecting the provider-specific error shape itself.
func oauthRefreshError(cause error) error {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(cause, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant" {
		return kiterrors.Unauthenticated(
			"OAuth refresh token was rejected by the provider",
			kiterrors.WithID("email.profile.oauth.invalid_grant"),
			// WithCause carries the sentinel so callers can detect this case
			// with isOAuthReauthorizationRequired; AppendMessagef keeps the
			// provider's own error text for logs and diagnostics.
			kiterrors.WithCause(errOAuthReauthorizationRequired),
			kiterrors.AppendMessagef("%v", cause),
		)
	}

	return kiterrors.Internal(
		"unable to refresh OAuth access token",
		kiterrors.WithID("email.profile.oauth.refresh_failed"),
		kiterrors.WithCause(cause),
	)
}

func (s *EmailProfileService) oauthTokenEntry(domainID, id int64) *emailProfileTokenCacheEntry {
	key := emailProfileTokenKey{domainID: domainID, id: id}
	entry, _ := s.oauthTokenCache.LoadOrStore(key, new(emailProfileTokenCacheEntry))

	return entry.(*emailProfileTokenCacheEntry)
}

func (s *EmailProfileService) oauthConfig(ctx context.Context, domainID int64, profile *model.EmailProfile) (*oauth2.Config, error) {
	if profile.AuthType != model.EmailAuthTypeOAuth2 {
		return nil, invalidEmailProfile(
			"oauth2_required",
			"OAuth2 authentication is required for this profile",
		)
	}
	if profile.OAuthClientID == "" {
		return nil, invalidEmailProfile("oauth_client_id_required", "OAuth client ID is required")
	}

	clientSecret, err := s.openOAuthClientSecret(ctx, domainID, profile.ID)
	if err != nil {
		return nil, err
	}
	if clientSecret == "" {
		return nil, invalidEmailProfile("oauth_client_secret_required", "OAuth client secret is required")
	}

	return s.oauth.Config(profile.OAuthProvider, profile.OAuthClientID, clientSecret)
}

func (s *EmailProfileService) openOAuthClientSecret(
	ctx context.Context,
	domainID, id int64,
) (string, error) {
	credentials, err := s.store.GetOAuthCredentials(ctx, domainID, id)
	if err != nil {
		return "", err
	}

	clientSecret, err := s.encryptor.Decrypt(ctx, credentials.ClientSecret)
	if err != nil {
		return "", err
	}

	return string(clientSecret), nil
}

func (s *EmailProfileService) openOAuthRefreshToken(
	ctx context.Context,
	domainID, id int64,
) (string, error) {
	credentials, err := s.store.GetOAuthCredentials(ctx, domainID, id)
	if err != nil {
		return "", err
	}

	refreshToken, err := s.encryptor.Decrypt(ctx, credentials.RefreshToken)
	if err != nil {
		return "", err
	}

	return string(refreshToken), nil
}

func (s *EmailProfileService) newOAuthState(
	ctx context.Context,
	domainID, userID int64,
	profile *model.EmailProfile,
) (string, error) {
	payload, err := json.Marshal(emailProfileOAuthState{
		DomainID:  domainID,
		UserID:    userID,
		ProfileID: profile.ID,
		Provider:  profile.OAuthProvider,
		ExpiresAt: time.Now().Add(oauthStateTTL).Unix(),
	})
	if err != nil {
		return "", err
	}

	sealed, err := s.encryptor.Encrypt(ctx, payload)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *EmailProfileService) openOAuthState(ctx context.Context, encodedState string) (*emailProfileOAuthState, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(encodedState)
	if err != nil {
		return nil, err
	}
	payload, err := s.encryptor.Decrypt(ctx, sealed)
	if err != nil {
		return nil, err
	}

	state := new(emailProfileOAuthState)
	if err := json.Unmarshal(payload, state); err != nil {
		return nil, err
	}
	if state.DomainID <= 0 || state.UserID <= 0 || state.ProfileID <= 0 ||
		!validOAuthProvider(state.Provider) || time.Now().Unix() >= state.ExpiresAt {
		return nil, invalidOAuthState()
	}

	return state, nil
}

func invalidOAuthState() error {
	return invalidEmailProfile("oauth_state_invalid", "OAuth state is invalid or expired")
}
