package service

import (
	"context"
	"errors"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/internal/model"
)

// AttachmentLink returns a short-lived link to a stored part of a visible message;
// the file id comes only from the database.
func (s *EmailThreadReadService) AttachmentLink(
	ctx context.Context,
	domainID, userID, threadID, messageID, attachmentID int64,
) (string, time.Time, error) {
	switch {
	case threadID <= 0:
		return "", time.Time{}, errThreadIDRequired()
	case messageID <= 0:
		return "", time.Time{}, kiterrors.InvalidArgument("email message id is required",
			kiterrors.WithID("email.message.id_required"))
	case attachmentID <= 0:
		return "", time.Time{}, kiterrors.InvalidArgument("email attachment id is required",
			kiterrors.WithID("email.attachment.id_required"))
	}
	if err := s.requireVisible(ctx, domainID, userID, threadID); err != nil {
		return "", time.Time{}, err
	}

	part, err := s.threads.LocateAttachment(ctx, domainID, userID, threadID, messageID, attachmentID)
	if err != nil {
		return "", time.Time{}, err
	}
	if part == nil {
		return "", time.Time{}, kiterrors.NotFound("email attachment not found",
			kiterrors.WithID("email.attachment.not_found"))
	}
	switch {
	case part.State == model.EmailAttachmentStateSkipped:
		return "", time.Time{}, kiterrors.New("email attachment was not stored",
			kiterrors.WithID("email.attachment.not_stored"),
			kiterrors.WithCode(codes.FailedPrecondition))
	case part.State != model.EmailAttachmentStateStored || part.FileID == nil:
		// A ready message never has such a part; it is a data error.
		s.log.Warn("ready email message has an attachment without a stored file",
			"message_id", messageID, "attachment_id", attachmentID, "state", part.State)
		return "", time.Time{}, kiterrors.Internal("email attachment has no stored file",
			kiterrors.WithID("email.attachment.no_file"))
	}

	link, expiresAt, err := s.links.Link(ctx, domainID, *part.FileID)
	switch {
	case err == nil:
		return link, expiresAt, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
		return "", time.Time{}, err
	case errors.Is(err, ErrFileLinksUnavailable):
		return "", time.Time{}, kiterrors.Unavailable("storage is unavailable",
			kiterrors.WithID("email.storage.unavailable"), kiterrors.WithCause(err))
	default:
		return "", time.Time{}, kiterrors.Internal("email attachment link failed",
			kiterrors.WithID("email.storage.link_failed"), kiterrors.WithCause(err))
	}
}
