package delivery

import (
	"errors"
	"io"
	"net"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
)

// classify turns a failed send into its delivery code. canceled tells that the
// worker gave up, which is never the server's fault.
func classify(err error, canceled bool) model.EmailDeliveryError {
	var failure *mailinfra.SMTPSendError
	if !errors.As(err, &failure) {
		return model.EmailDeliveryErrorInterrupted
	}

	if failure.Stage == mailinfra.SMTPStageFinal {
		switch code := failure.Code; {
		case code == 552 || failure.Enhanced == "5.3.4":
			return model.EmailDeliveryErrorMessageTooLarge
		case code >= 500:
			return model.EmailDeliveryErrorRejected
		case code >= 400:
			return model.EmailDeliveryErrorTemporaryFailure
		default:
			// No reply after the final dot: the server may have the email.
			return model.EmailDeliveryErrorUnknownResult
		}
	}

	if failure.Code == 0 {
		switch {
		case failure.Stage == mailinfra.SMTPStageSize:
			return model.EmailDeliveryErrorMessageTooLarge
		case canceled:
			return model.EmailDeliveryErrorInterrupted
		case failure.Stage == mailinfra.SMTPStageConnect:
			return model.EmailDeliveryErrorConnection
		case failure.Stage == mailinfra.SMTPStageAuth && (failure.Unsupported || !transport(err)):
			return model.EmailDeliveryErrorAuth
		case failure.Stage == mailinfra.SMTPStageAuth:
			return model.EmailDeliveryErrorConnection
		default:
			return model.EmailDeliveryErrorInterrupted
		}
	}

	if failure.Code < 500 {
		return model.EmailDeliveryErrorTemporaryFailure
	}
	switch failure.Stage {
	case mailinfra.SMTPStageAuth:
		return model.EmailDeliveryErrorAuth
	case mailinfra.SMTPStageMail:
		// Without SIZE= on MAIL FROM, only 5.3.4 tells the size (MVP rule).
		if failure.Enhanced == "5.3.4" {
			return model.EmailDeliveryErrorMessageTooLarge
		}
		return model.EmailDeliveryErrorRejected
	case mailinfra.SMTPStageRcpt:
		return model.EmailDeliveryErrorRecipientRejected
	default:
		return model.EmailDeliveryErrorRejected
	}
}

// transport tells a broken connection from a refusal of the client itself.
func transport(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// classifyCredentials: only the profile can fix a missing authorization or password.
func classifyCredentials(err error, canceled bool) model.EmailDeliveryError {
	switch {
	case service.RequiresReauthorization(err) || kiterrors.ID(err) == "email.profile.password_required":
		return model.EmailDeliveryErrorProfileUnavailable
	case canceled:
		return model.EmailDeliveryErrorInterrupted
	default:
		return model.EmailDeliveryErrorConnection
	}
}
