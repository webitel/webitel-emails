package delivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
)

// classify permits retry only for a transient failure with confirmed non-acceptance.
// canceled means the worker stopped or timed out.
func classify(err error, canceled bool) (model.EmailDeliveryError, bool) {
	var failure *mailinfra.SMTPSendError
	if !errors.As(err, &failure) {
		return model.EmailDeliveryErrorInterrupted, canceled || transient(err)
	}

	if failure.Stage == mailinfra.SMTPStageFinal {
		switch code := failure.Code; {
		case code == 552 || failure.Enhanced == "5.3.4":
			return model.EmailDeliveryErrorMessageTooLarge, false
		case code >= 500:
			return model.EmailDeliveryErrorRejected, false
		case code >= 400:
			// An explicit refusal after the dot: the server does not have the email.
			return model.EmailDeliveryErrorTemporaryFailure, true
		default:
			// No reply after the final dot: the server may have the email.
			return model.EmailDeliveryErrorUnknownResult, false
		}
	}

	if failure.Code == 0 {
		switch {
		case failure.Stage == mailinfra.SMTPStageSize:
			return model.EmailDeliveryErrorMessageTooLarge, false
		case canceled:
			return model.EmailDeliveryErrorInterrupted, true
		case failure.Stage == mailinfra.SMTPStageConnect:
			// A certificate, a missing STARTTLS or an unknown host waits for the profile.
			return model.EmailDeliveryErrorConnection, !failure.Unsupported && transient(err)
		case failure.Stage == mailinfra.SMTPStageAuth && (failure.Unsupported || !transport(err)):
			return model.EmailDeliveryErrorAuth, false
		case failure.Stage == mailinfra.SMTPStageAuth:
			return model.EmailDeliveryErrorConnection, true
		default:
			// The connection broke before the dot.
			return model.EmailDeliveryErrorInterrupted, transport(err)
		}
	}

	if failure.Code < 500 {
		return model.EmailDeliveryErrorTemporaryFailure, true
	}
	switch failure.Stage {
	case mailinfra.SMTPStageAuth:
		return model.EmailDeliveryErrorAuth, false
	case mailinfra.SMTPStageMail:
		// Without SIZE= on MAIL FROM, only 5.3.4 tells the size (MVP rule).
		if failure.Enhanced == "5.3.4" {
			return model.EmailDeliveryErrorMessageTooLarge, false
		}
		return model.EmailDeliveryErrorRejected, false
	case mailinfra.SMTPStageRcpt:
		// Too many recipients is temporary (RFC 5321 4.5.3.1.10); only 5.5.3 tells it
		// from other 552 replies, such as a full mailbox.
		if failure.Code == 552 && failure.Enhanced == "5.5.3" {
			return model.EmailDeliveryErrorTemporaryFailure, true
		}
		return model.EmailDeliveryErrorRecipientRejected, false
	default:
		return model.EmailDeliveryErrorRejected, false
	}
}

// classifyCredentials: only the profile can fix a missing authorization or password.
func classifyCredentials(err error, canceled bool) (model.EmailDeliveryError, bool) {
	switch {
	case service.RequiresReauthorization(err) || kiterrors.ID(err) == "email.profile.password_required":
		return model.EmailDeliveryErrorProfileUnavailable, false
	case canceled:
		return model.EmailDeliveryErrorInterrupted, true
	default:
		return model.EmailDeliveryErrorConnection, transient(err)
	}
}

// transport tells a broken connection from a refusal of the client itself.
func transport(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// transient reports recoverable transport errors, timeouts and dependency outages.
// Certificate errors, unknown hosts and unrecognized errors do not trigger auto-retry.
func transient(err error) bool {
	if err == nil || certificate(err) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return true
	case errors.Is(err, storageinfra.ErrUnavailable), errors.Is(err, storageinfra.ErrFileIncomplete):
		return true
	case errors.Is(err, storageinfra.ErrFileRejected):
		return false
	case transport(err):
		return true
	}

	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) || pgconn.SafeToRetry(err) {
		return true
	}
	var grpcErr interface{ GRPCStatus() *status.Status }
	if errors.As(err, &grpcErr) {
		switch grpcErr.GRPCStatus().Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
			return true
		}
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) && retrieveErr.Response != nil {
		code := retrieveErr.Response.StatusCode
		return code >= http.StatusInternalServerError || code == http.StatusTooManyRequests
	}

	return false
}

// certificate tells a TLS failure that only the profile or the server can fix.
func certificate(err error) bool {
	var (
		verify    *tls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
		header    tls.RecordHeaderError
		alert     tls.AlertError
	)

	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &header) || errors.As(err, &alert)
}
