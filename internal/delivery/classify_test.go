package delivery

import (
	"errors"
	"fmt"
	"io"
	"testing"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/model"
)

func reply(stage mailinfra.SMTPStage, code int, enhanced string) error {
	return &mailinfra.SMTPSendError{Stage: stage, Code: code, Enhanced: enhanced}
}

func TestClassify(t *testing.T) {
	const (
		connect = mailinfra.SMTPStageConnect
		auth    = mailinfra.SMTPStageAuth
		mail    = mailinfra.SMTPStageMail
		rcpt    = mailinfra.SMTPStageRcpt
		data    = mailinfra.SMTPStageData
		body    = mailinfra.SMTPStageBody
		final   = mailinfra.SMTPStageFinal
	)
	tests := []struct {
		name     string
		err      error
		canceled bool
		want     model.EmailDeliveryError
	}{
		{"larger than SIZE", reply(mailinfra.SMTPStageSize, 0, ""), false, model.EmailDeliveryErrorMessageTooLarge},
		{"connect", reply(connect, 0, ""), false, model.EmailDeliveryErrorConnection},
		{"connect canceled", reply(connect, 0, ""), true, model.EmailDeliveryErrorInterrupted},
		{"auth unsupported", &mailinfra.SMTPSendError{Stage: auth, Unsupported: true}, false, model.EmailDeliveryErrorAuth},
		{"auth refused", reply(auth, 535, "5.7.8"), false, model.EmailDeliveryErrorAuth},
		{"auth broken", fmt.Errorf("%w: %w", reply(auth, 0, ""), io.EOF), false, model.EmailDeliveryErrorConnection},
		{"auth busy", reply(auth, 454, ""), false, model.EmailDeliveryErrorTemporaryFailure},
		{"mail 552 without 5.3.4", reply(mail, 552, ""), false, model.EmailDeliveryErrorRejected},
		{"mail 5.3.4", reply(mail, 552, "5.3.4"), false, model.EmailDeliveryErrorMessageTooLarge},
		{"mail 4xx", reply(mail, 451, ""), false, model.EmailDeliveryErrorTemporaryFailure},
		{"rcpt 552", reply(rcpt, 552, ""), false, model.EmailDeliveryErrorRecipientRejected},
		{"rcpt 550", reply(rcpt, 550, "5.1.1"), false, model.EmailDeliveryErrorRecipientRejected},
		{"rcpt 4xx", reply(rcpt, 450, ""), false, model.EmailDeliveryErrorTemporaryFailure},
		{"data 5xx", reply(data, 554, ""), false, model.EmailDeliveryErrorRejected},
		{"dropped before the dot", reply(body, 0, ""), false, model.EmailDeliveryErrorInterrupted},
		{"final 552", reply(final, 552, ""), false, model.EmailDeliveryErrorMessageTooLarge},
		{"final 5.3.4", reply(final, 554, "5.3.4"), false, model.EmailDeliveryErrorMessageTooLarge},
		{"final 5xx", reply(final, 554, "5.7.1"), false, model.EmailDeliveryErrorRejected},
		{"final 4xx", reply(final, 451, ""), false, model.EmailDeliveryErrorTemporaryFailure},
		{"final without reply", reply(final, 0, ""), true, model.EmailDeliveryErrorUnknownResult},
		{"not an SMTP error", errors.New("storage"), false, model.EmailDeliveryErrorInterrupted},
	}
	for _, tt := range tests {
		if got := classify(tt.err, tt.canceled); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}

	reauthorize := kiterrors.New("not connected", kiterrors.WithID("email.profile.oauth_not_connected"))
	noPassword := kiterrors.New("no password", kiterrors.WithID("email.profile.password_required"))
	for _, tt := range []struct {
		err      error
		canceled bool
		want     model.EmailDeliveryError
	}{
		{reauthorize, false, model.EmailDeliveryErrorProfileUnavailable},
		{noPassword, false, model.EmailDeliveryErrorProfileUnavailable},
		{errors.New("token endpoint"), false, model.EmailDeliveryErrorConnection},
		{errors.New("token endpoint"), true, model.EmailDeliveryErrorInterrupted},
	} {
		if got := classifyCredentials(tt.err, tt.canceled); got != tt.want {
			t.Errorf("credentials %v: %s, want %s", tt.err, got, tt.want)
		}
	}
}
