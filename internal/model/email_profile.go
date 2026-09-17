package model

import "time"

// EmailAuthType identifies the authentication mechanism used by a profile.
type EmailAuthType string

// Supported Email Profile authentication types.
const (
	EmailAuthTypeBasic  EmailAuthType = "basic"
	EmailAuthTypeOAuth2 EmailAuthType = "oauth2"
)

// EmailOAuthProvider identifies a supported OAuth provider.
type EmailOAuthProvider string

// Supported OAuth providers.
const (
	EmailOAuthProviderGoogle    EmailOAuthProvider = "google"
	EmailOAuthProviderMicrosoft EmailOAuthProvider = "microsoft"
)

// EmailConnectionSecurity identifies the transport security mode for IMAP or SMTP.
type EmailConnectionSecurity string

// Supported IMAP and SMTP transport security modes.
const (
	EmailConnectionSecurityTLS      EmailConnectionSecurity = "tls"
	EmailConnectionSecurityStartTLS EmailConnectionSecurity = "starttls"
)

// EmailConnectionState describes the latest known connection status of a profile.
type EmailConnectionState string

// Email Profile connection states.
const (
	EmailConnectionStateIdle                    EmailConnectionState = "idle"
	EmailConnectionStateReady                   EmailConnectionState = "ready"
	EmailConnectionStateError                   EmailConnectionState = "error"
	EmailConnectionStateReauthorizationRequired EmailConnectionState = "reauthorization_required"
)

// EmailProfile is the safe internal representation of a mailbox profile.
// Stored credentials are deliberately kept outside this read model.
type EmailProfile struct {
	ID       int64
	DomainID int64

	Name        string
	Description string
	Enabled     bool

	EmailAddress string
	SenderName   string
	ReplyTo      string
	Signature    string

	IMAPHost             string
	IMAPPort             int32
	IMAPSecurity         EmailConnectionSecurity
	SMTPHost             string
	SMTPPort             int32
	SMTPSecurity         EmailConnectionSecurity
	Username             string
	Mailbox              string
	FetchIntervalSeconds int32

	FlowID        *int64
	AuthType      EmailAuthType
	OAuthProvider EmailOAuthProvider
	OAuthClientID string
	// OAuthConnected is derived from the presence of a stored refresh token.
	OAuthConnected bool

	ConnectionState            EmailConnectionState
	LastSuccessfulConnectionAt *time.Time
	ConnectionError            string

	CreatedAt time.Time
	CreatedBy *Lookup
	UpdatedAt time.Time
	UpdatedBy *Lookup
}

// EmailProfileFilter contains list parameters accepted by the Email Profile API.
type EmailProfileFilter struct {
	Page   int32
	Size   int32
	Query  string
	Sort   string
	Fields []string
}
