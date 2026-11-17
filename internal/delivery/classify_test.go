package delivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
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
	connectFailed := func(err error) error { return failedWith(connect, err) }
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	certificate := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	unknownHost := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}

	tests := []struct {
		name      string
		err       error
		canceled  bool
		want      model.EmailDeliveryError
		retryable bool
	}{
		{"larger than SIZE", reply(mailinfra.SMTPStageSize, 0, ""), false, model.EmailDeliveryErrorMessageTooLarge, false},
		{"connection refused", connectFailed(refused), false, model.EmailDeliveryErrorConnection, true},
		{"connect timed out", connectFailed(context.DeadlineExceeded), false, model.EmailDeliveryErrorConnection, true},
		{"connect broken", connectFailed(io.EOF), false, model.EmailDeliveryErrorConnection, true},
		{"certificate", connectFailed(certificate), false, model.EmailDeliveryErrorConnection, false},
		{"not TLS", connectFailed(tls.RecordHeaderError{}), false, model.EmailDeliveryErrorConnection, false},
		{"unknown host", connectFailed(unknownHost), false, model.EmailDeliveryErrorConnection, false},
		{"unrecognized connect error", connectFailed(errors.New("smtp: unsupported security")), false, model.EmailDeliveryErrorConnection, false},
		{"no STARTTLS", &mailinfra.SMTPSendError{Stage: connect, Unsupported: true}, false, model.EmailDeliveryErrorConnection, false},
		{"connect canceled", reply(connect, 0, ""), true, model.EmailDeliveryErrorInterrupted, true},
		{"greeting 4xx", reply(connect, 421, "4.3.2"), false, model.EmailDeliveryErrorTemporaryFailure, true},
		{"STARTTLS 4xx", reply(connect, 454, "4.7.0"), false, model.EmailDeliveryErrorTemporaryFailure, true},
		{"greeting 5xx", reply(connect, 554, ""), false, model.EmailDeliveryErrorRejected, false},
		{"auth unsupported", &mailinfra.SMTPSendError{Stage: auth, Unsupported: true}, false, model.EmailDeliveryErrorAuth, false},
		{"auth refused", reply(auth, 535, "5.7.8"), false, model.EmailDeliveryErrorAuth, false},
		{"auth broken", fmt.Errorf("%w: %w", reply(auth, 0, ""), io.EOF), false, model.EmailDeliveryErrorConnection, true},
		{"auth busy", reply(auth, 454, ""), false, model.EmailDeliveryErrorTemporaryFailure, true},
		{"mail 552 without 5.3.4", reply(mail, 552, ""), false, model.EmailDeliveryErrorRejected, false},
		{"mail 5.3.4", reply(mail, 552, "5.3.4"), false, model.EmailDeliveryErrorMessageTooLarge, false},
		{"mail 4xx", reply(mail, 451, ""), false, model.EmailDeliveryErrorTemporaryFailure, true},
		{"rcpt 552", reply(rcpt, 552, ""), false, model.EmailDeliveryErrorRecipientRejected, false},
		{"rcpt 552 mailbox full", reply(rcpt, 552, "5.2.2"), false, model.EmailDeliveryErrorRecipientRejected, false},
		{"rcpt 552 too many recipients", reply(rcpt, 552, "5.5.3"), false, model.EmailDeliveryErrorTemporaryFailure, true},
		{"rcpt 550", reply(rcpt, 550, "5.1.1"), false, model.EmailDeliveryErrorRecipientRejected, false},
		{"rcpt 4xx", reply(rcpt, 452, "4.5.3"), false, model.EmailDeliveryErrorTemporaryFailure, true},
		{"data 5xx", reply(data, 554, ""), false, model.EmailDeliveryErrorRejected, false},
		{"dropped before the dot", failedWith(body, io.ErrUnexpectedEOF), false, model.EmailDeliveryErrorInterrupted, true},
		{"body timed out", reply(body, 0, ""), true, model.EmailDeliveryErrorInterrupted, true},
		{"final 552", reply(final, 552, ""), false, model.EmailDeliveryErrorMessageTooLarge, false},
		{"final 5.3.4", reply(final, 554, "5.3.4"), false, model.EmailDeliveryErrorMessageTooLarge, false},
		{"final 5xx", reply(final, 554, "5.7.1"), false, model.EmailDeliveryErrorRejected, false},
		{"final 4xx", reply(final, 451, ""), false, model.EmailDeliveryErrorTemporaryFailure, true},
		{"final without reply", reply(final, 0, ""), true, model.EmailDeliveryErrorUnknownResult, false},
		{"storage unavailable", fmt.Errorf("%w: x", storageinfra.ErrUnavailable), false, model.EmailDeliveryErrorInterrupted, true},
		{"file cut", storageinfra.ErrFileIncomplete, false, model.EmailDeliveryErrorInterrupted, true},
		{"file rejected", fmt.Errorf("%w: x", storageinfra.ErrFileRejected), false, model.EmailDeliveryErrorInterrupted, false},
		{"file missing", status.Error(codes.NotFound, "no file"), false, model.EmailDeliveryErrorInterrupted, false},
		{"storage busy", status.Error(codes.ResourceExhausted, "busy"), false, model.EmailDeliveryErrorInterrupted, true},
		{"not an SMTP error", errors.New("storage"), false, model.EmailDeliveryErrorInterrupted, false},
		{"not an SMTP error, canceled", errors.New("storage"), true, model.EmailDeliveryErrorInterrupted, true},
	}
	for _, tt := range tests {
		code, retryable := classify(tt.err, tt.canceled)
		if code != tt.want || retryable != tt.retryable {
			t.Errorf("%s: %s %v, want %s %v", tt.name, code, retryable, tt.want, tt.retryable)
		}
	}

	reauthorize := kiterrors.New("not connected", kiterrors.WithID("email.profile.oauth_not_connected"))
	noPassword := kiterrors.New("no password", kiterrors.WithID("email.profile.password_required"))
	refreshFailed := func(cause error) error {
		return kiterrors.Internal("refresh", kiterrors.WithID("email.profile.oauth.refresh_failed"), kiterrors.WithCause(cause))
	}
	provider := func(code int) error {
		return &oauth2.RetrieveError{Response: &http.Response{StatusCode: code}}
	}
	for _, tt := range []struct {
		name      string
		err       error
		canceled  bool
		want      model.EmailDeliveryError
		retryable bool
	}{
		{"reauthorize", reauthorize, false, model.EmailDeliveryErrorProfileUnavailable, false},
		{"no password", noPassword, false, model.EmailDeliveryErrorProfileUnavailable, false},
		{"token endpoint unreachable", refreshFailed(refused), false, model.EmailDeliveryErrorConnection, true},
		{"provider 503", refreshFailed(provider(http.StatusServiceUnavailable)), false, model.EmailDeliveryErrorConnection, true},
		{"provider 400", refreshFailed(provider(http.StatusBadRequest)), false, model.EmailDeliveryErrorConnection, false},
		{"unrecognized", errors.New("token endpoint"), false, model.EmailDeliveryErrorConnection, false},
		{"canceled", errors.New("token endpoint"), true, model.EmailDeliveryErrorInterrupted, true},
	} {
		code, retryable := classifyCredentials(tt.err, tt.canceled)
		if code != tt.want || retryable != tt.retryable {
			t.Errorf("credentials %s: %s %v, want %s %v", tt.name, code, retryable, tt.want, tt.retryable)
		}
	}
}

// failedWith is a failure without a reply and with its transport cause, as the
// sender returns it.
func failedWith(stage mailinfra.SMTPStage, cause error) error {
	return fmt.Errorf("send: %w", &wrapped{&mailinfra.SMTPSendError{Stage: stage}, cause})
}

// wrapped joins an SMTP failure with its cause.
type wrapped struct {
	failure *mailinfra.SMTPSendError
	cause   error
}

func (w *wrapped) Error() string   { return w.failure.Error() }
func (w *wrapped) Unwrap() []error { return []error{w.failure, w.cause} }
