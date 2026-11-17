package service

import (
	"context"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// ManualRetryWindow is how long after its acceptance an email may be retried by
// hand: a later email would carry an old Date (MVP, П26).
const ManualRetryWindow = 24 * time.Hour

// manualRetryCodes are the failures whose cause an operator may remove (П27). The
// server may have an email with unknown_result, so it is never sent again.
var manualRetryCodes = map[model.EmailDeliveryError]bool{
	model.EmailDeliveryErrorTemporaryFailure:   true,
	model.EmailDeliveryErrorConnection:         true,
	model.EmailDeliveryErrorInterrupted:        true,
	model.EmailDeliveryErrorAuth:               true,
	model.EmailDeliveryErrorProfileUnavailable: true,
	model.EmailDeliveryErrorRejected:           true,
}

// DeliveryCommandInput selects an email and the delivery version its author saw.
// The attempt applies to a retry only; AttemptID 0 retries from history.
type DeliveryCommandInput struct {
	ThreadID                int64
	MessageID               int64
	ExpectedDeliveryVersion *int64
	AttemptID               int64
	ExpectedAttemptVersion  int64
}

// DeliveryReceipt is the accepted change; the email itself is read again.
type DeliveryReceipt struct {
	MessageID int64
	Status    model.EmailDeliveryStatus
	Version   int64
}

// EmailDeliveryService lets authors retry or cancel delivery.
// Both commands compare the delivery version to reject stale requests.
type EmailDeliveryService struct {
	deliveries store.EmailDeliveryStore
	cfg        config.OutboundConfig
	now        func() time.Time
}

// NewEmailDeliveryService creates the delivery command service.
func NewEmailDeliveryService(cfg *config.Config, deliveries store.EmailDeliveryStore) *EmailDeliveryService {
	return &EmailDeliveryService{deliveries: deliveries, cfg: cfg.Outbound, now: time.Now}
}

// MarkManualRetry computes eligibility, including update access and reply rights.
// Retry checks eligibility again under lock.
func (s *EmailDeliveryService) MarkManualRetry(session *auth.Session, messages ...*model.EmailMessageView) {
	now := s.now()
	allowed := s.cfg.Enabled && session.CheckOBACAccess(emailThreadObjClass, auth.Edit)
	for _, m := range messages {
		m.ManualRetryAvailable = allowed && m.Author != nil && m.Author.ID == session.UserID &&
			m.DeliveryStatus == model.EmailDeliveryStatusFailed && manualRetryCodes[m.DeliveryErrorCode] &&
			m.DeliveryManualRetries < s.cfg.DeliveryManualRetries && now.Sub(m.ReceivedAt) < ManualRetryWindow &&
			m.ReplyAllowed
	}
}

// Retry queues a failed email of the caller again, within its right to reply.
func (s *EmailDeliveryService) Retry(ctx context.Context, session *auth.Session, in DeliveryCommandInput) (*DeliveryReceipt, error) {
	if err := validateDeliveryCommand(session, in); err != nil {
		return nil, err
	}
	if !s.cfg.Enabled {
		return nil, errOutboundDisabled()
	}

	return s.change(ctx, session, in, func(thread *store.EmailReplyThread, m *store.EmailDeliveryMessage) error {
		// The version first: a repeated request tells that the delivery moved on,
		// whatever happened to the Thread since.
		if err := checkDeliveryVersion(m, in); err != nil {
			return err
		}
		if err := checkReplyRights(thread, session.UserID, in.AttemptID, in.ExpectedAttemptVersion); err != nil {
			return err
		}
		switch {
		case m.Status != model.EmailDeliveryStatusFailed || !manualRetryCodes[m.Code]:
			return errDeliveryRefused("email.message.retry_not_allowed", "this email cannot be sent again")
		case m.ManualRetries >= s.cfg.DeliveryManualRetries:
			return errDeliveryRefused("email.message.retry_limit_reached", "this email was sent again too many times")
		case m.Age >= ManualRetryWindow:
			return errDeliveryRefused("email.message.retry_expired", "this email is too old to be sent again")
		}
		return nil
	}, store.EmailDeliveryRetry)
}

// Cancel stops the caller's visible email before the server may have accepted it.
// It also works with sending disabled and while another agent handles the Thread.
func (s *EmailDeliveryService) Cancel(ctx context.Context, session *auth.Session, in DeliveryCommandInput) (*DeliveryReceipt, error) {
	if err := validateDeliveryCommand(session, in); err != nil {
		return nil, err
	}

	return s.change(ctx, session, in, func(_ *store.EmailReplyThread, m *store.EmailDeliveryMessage) error {
		if err := checkDeliveryVersion(m, in); err != nil {
			return err
		}
		switch {
		case m.Status == model.EmailDeliveryStatusPending && m.Phase == "committed":
			return errDeliveryRefused("email.message.delivery_committed", "the server may already have this email")
		case m.Status != model.EmailDeliveryStatusPending:
			return errDeliveryRefused("email.message.cancel_not_allowed", "this email is no longer being sent")
		}
		return nil
	}, store.EmailDeliveryCancel)
}

// change runs a command under the locks: the Thread and the email must be visible
// and the email the caller's own; check decides the rest.
func (s *EmailDeliveryService) change(
	ctx context.Context,
	session *auth.Session,
	in DeliveryCommandInput,
	check func(*store.EmailReplyThread, *store.EmailDeliveryMessage) error,
	change store.EmailDeliveryChange,
) (*DeliveryReceipt, error) {
	m, err := s.deliveries.ChangeDelivery(ctx, session.DomainID, session.UserID, in.ThreadID, in.MessageID,
		func(thread *store.EmailReplyThread, m *store.EmailDeliveryMessage) (store.EmailDeliveryChange, error) {
			switch {
			case thread == nil:
				return 0, errThreadNotFound()
			case m == nil:
				return 0, kiterrors.NotFound("email message not found", kiterrors.WithID("email.message.not_found"))
			case !m.Outbound || m.AuthorUserID != session.UserID:
				return 0, kiterrors.New("only the author may change the delivery of this email",
					kiterrors.WithID("email.message.not_author"), kiterrors.WithCode(codes.PermissionDenied))
			}
			if err := check(thread, m); err != nil {
				return 0, err
			}
			return change, nil
		})
	if err != nil {
		return nil, err
	}

	return &DeliveryReceipt{MessageID: m.ID, Status: m.Status, Version: m.Version}, nil
}

// validateDeliveryCommand rejects a missing version before any change, including when Gateway loses the body.
// The interceptor checks update. The service also requires read.
func validateDeliveryCommand(session *auth.Session, in DeliveryCommandInput) error {
	switch {
	case in.ExpectedDeliveryVersion == nil || *in.ExpectedDeliveryVersion < 0:
		return kiterrors.InvalidArgument("expected delivery version is required",
			kiterrors.WithID("email.message.delivery_version_invalid"))
	case in.ThreadID <= 0:
		return errThreadIDRequired()
	case in.MessageID <= 0:
		return kiterrors.InvalidArgument("email message id is required", kiterrors.WithID("email.message.id_required"))
	case !session.CheckOBACAccess(emailThreadObjClass, auth.Read):
		return kiterrors.Forbidden("missing required permissions: "+emailThreadObjClass,
			kiterrors.WithID("auth.interceptor.permission"))
	}

	return nil
}

func checkDeliveryVersion(m *store.EmailDeliveryMessage, in DeliveryCommandInput) error {
	if m.Version != *in.ExpectedDeliveryVersion {
		return kiterrors.New("the delivery of this email changed",
			kiterrors.WithID("email.message.delivery_version_conflict"), kiterrors.WithCode(codes.Aborted))
	}

	return nil
}

func errDeliveryRefused(id, message string) error {
	return kiterrors.New(message, kiterrors.WithID(id), kiterrors.WithCode(codes.FailedPrecondition))
}
