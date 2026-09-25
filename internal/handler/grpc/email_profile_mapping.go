// Package grpc implements the Email Profile gRPC transport layer.
package grpc

import (
	"time"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/model"
)

func emailProfileFromProto(input *emailpb.InputEmailProfile) *model.EmailProfile {
	if input == nil {
		return nil
	}

	profile := &model.EmailProfile{
		Name:                 input.GetName(),
		Description:          input.GetDescription(),
		Enabled:              input.GetEnabled(),
		EmailAddress:         input.GetEmailAddress(),
		SenderName:           input.GetSenderName(),
		ReplyTo:              input.GetReplyTo(),
		Signature:            input.GetSignature(),
		IMAPHost:             input.GetImapHost(),
		IMAPPort:             input.GetImapPort(),
		IMAPSecurity:         connectionSecurityFromProto(input.GetImapSecurity()),
		SMTPHost:             input.GetSmtpHost(),
		SMTPPort:             input.GetSmtpPort(),
		SMTPSecurity:         connectionSecurityFromProto(input.GetSmtpSecurity()),
		Username:             input.GetUsername(),
		Mailbox:              input.GetMailbox(),
		FetchIntervalSeconds: input.GetFetchIntervalSeconds(),
		AuthType:             authTypeFromProto(input.GetAuthType()),
		OAuthProvider:        oauthProviderFromProto(input.GetOauthProvider()),
		OAuthClientID:        input.GetOauthClientId(),
	}

	if flowID := input.GetFlowId(); flowID != 0 {
		profile.FlowID = &flowID
	}

	return profile
}

func emailProfileToProto(profile *model.EmailProfile) *emailpb.EmailProfile {
	return &emailpb.EmailProfile{
		Id:                         profile.ID,
		DomainId:                   profile.DomainID,
		Name:                       profile.Name,
		Description:                profile.Description,
		Enabled:                    profile.Enabled,
		EmailAddress:               profile.EmailAddress,
		SenderName:                 profile.SenderName,
		ReplyTo:                    profile.ReplyTo,
		Signature:                  profile.Signature,
		ImapHost:                   profile.IMAPHost,
		ImapPort:                   profile.IMAPPort,
		ImapSecurity:               connectionSecurityToProto(profile.IMAPSecurity),
		SmtpHost:                   profile.SMTPHost,
		SmtpPort:                   profile.SMTPPort,
		SmtpSecurity:               connectionSecurityToProto(profile.SMTPSecurity),
		Username:                   profile.Username,
		Mailbox:                    profile.Mailbox,
		FetchIntervalSeconds:       profile.FetchIntervalSeconds,
		FlowId:                     int64Value(profile.FlowID),
		AuthType:                   authTypeToProto(profile.AuthType),
		OauthProvider:              oauthProviderToProto(profile.OAuthProvider),
		OauthClientId:              profile.OAuthClientID,
		OauthConnected:             profile.OAuthConnected,
		ConnectionState:            connectionStateToProto(profile.ConnectionState),
		LastSuccessfulConnectionAt: unixMilliValue(profile.LastSuccessfulConnectionAt),
		ConnectionError:            profile.ConnectionError,
		CreatedAt:                  unixMilli(profile.CreatedAt),
		CreatedBy:                  lookupToProto(profile.CreatedBy),
		UpdatedAt:                  unixMilli(profile.UpdatedAt),
		UpdatedBy:                  lookupToProto(profile.UpdatedBy),
	}
}

func connectionTestResultToProto(result model.EmailConnectionTestResult) *emailpb.EmailConnectionTestResult {
	return &emailpb.EmailConnectionTestResult{
		Success: result.Success,
		Error:   result.Error,
	}
}

func authTypeToProto(value model.EmailAuthType) emailpb.EmailAuthType {
	switch value {
	case model.EmailAuthTypeBasic:
		return emailpb.EmailAuthType_EMAIL_AUTH_TYPE_BASIC
	case model.EmailAuthTypeOAuth2:
		return emailpb.EmailAuthType_EMAIL_AUTH_TYPE_OAUTH2
	default:
		return emailpb.EmailAuthType_EMAIL_AUTH_TYPE_UNSPECIFIED
	}
}

func authTypeFromProto(value emailpb.EmailAuthType) model.EmailAuthType {
	switch value {
	case emailpb.EmailAuthType_EMAIL_AUTH_TYPE_UNSPECIFIED:
		return ""
	case emailpb.EmailAuthType_EMAIL_AUTH_TYPE_BASIC:
		return model.EmailAuthTypeBasic
	case emailpb.EmailAuthType_EMAIL_AUTH_TYPE_OAUTH2:
		return model.EmailAuthTypeOAuth2
	default:
		return model.EmailAuthType(value.String())
	}
}

func oauthProviderToProto(value model.EmailOAuthProvider) emailpb.EmailOAuthProvider {
	switch value {
	case model.EmailOAuthProviderGoogle:
		return emailpb.EmailOAuthProvider_EMAIL_OAUTH_PROVIDER_GOOGLE
	case model.EmailOAuthProviderMicrosoft:
		return emailpb.EmailOAuthProvider_EMAIL_OAUTH_PROVIDER_MICROSOFT
	default:
		return emailpb.EmailOAuthProvider_EMAIL_OAUTH_PROVIDER_UNSPECIFIED
	}
}

func oauthProviderFromProto(value emailpb.EmailOAuthProvider) model.EmailOAuthProvider {
	switch value {
	case emailpb.EmailOAuthProvider_EMAIL_OAUTH_PROVIDER_GOOGLE:
		return model.EmailOAuthProviderGoogle
	case emailpb.EmailOAuthProvider_EMAIL_OAUTH_PROVIDER_MICROSOFT:
		return model.EmailOAuthProviderMicrosoft
	default:
		return ""
	}
}

func connectionSecurityToProto(value model.EmailConnectionSecurity) emailpb.EmailConnectionSecurity {
	switch value {
	case model.EmailConnectionSecurityTLS:
		return emailpb.EmailConnectionSecurity_EMAIL_CONNECTION_SECURITY_TLS
	case model.EmailConnectionSecurityStartTLS:
		return emailpb.EmailConnectionSecurity_EMAIL_CONNECTION_SECURITY_STARTTLS
	default:
		return emailpb.EmailConnectionSecurity_EMAIL_CONNECTION_SECURITY_UNSPECIFIED
	}
}

func connectionSecurityFromProto(value emailpb.EmailConnectionSecurity) model.EmailConnectionSecurity {
	switch value {
	case emailpb.EmailConnectionSecurity_EMAIL_CONNECTION_SECURITY_TLS:
		return model.EmailConnectionSecurityTLS
	case emailpb.EmailConnectionSecurity_EMAIL_CONNECTION_SECURITY_STARTTLS:
		return model.EmailConnectionSecurityStartTLS
	default:
		return ""
	}
}

func connectionStateToProto(value model.EmailConnectionState) emailpb.EmailConnectionState {
	switch value {
	case model.EmailConnectionStateIdle:
		return emailpb.EmailConnectionState_EMAIL_CONNECTION_STATE_IDLE
	case model.EmailConnectionStateReady:
		return emailpb.EmailConnectionState_EMAIL_CONNECTION_STATE_READY
	case model.EmailConnectionStateError:
		return emailpb.EmailConnectionState_EMAIL_CONNECTION_STATE_ERROR
	case model.EmailConnectionStateReauthorizationRequired:
		return emailpb.EmailConnectionState_EMAIL_CONNECTION_STATE_REAUTHORIZATION_REQUIRED
	default:
		return emailpb.EmailConnectionState_EMAIL_CONNECTION_STATE_UNSPECIFIED
	}
}

func lookupToProto(value *model.Lookup) *emailpb.Lookup {
	if value == nil {
		return nil
	}

	return &emailpb.Lookup{Id: value.ID, Name: value.Name}
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}

	return *value
}

func unixMilli(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}

	return value.UnixMilli()
}

func unixMilliValue(value *time.Time) int64 {
	if value == nil {
		return 0
	}

	return unixMilli(*value)
}
