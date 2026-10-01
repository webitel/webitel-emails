package inbound

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/config"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// FileUploader stores one attachment file outside the database transaction.
type FileUploader interface {
	UploadFile(ctx context.Context, req storageinfra.UploadRequest) (storageinfra.UploadResult, error)
}

// storedMessage is the stored email whose attachment processing is being finished.
type storedMessage struct {
	ID       int64
	ThreadID int64
}

// MessageCompleter finishes a processing Message: it uploads attachment parts
// that have no file yet and makes the Message visible only once none remain.
// Every write is fenced by the assignment of the poll that read the email.
type MessageCompleter struct {
	uow         store.UnitOfWork
	files       FileUploader
	maxAttempts int32
	log         *slog.Logger
}

// NewMessageCompleter creates the use case that owns the work after the first
// transaction has committed.
func NewMessageCompleter(
	uow store.UnitOfWork,
	files FileUploader,
	cfg *config.Config,
	log *slog.Logger,
) *MessageCompleter {
	return &MessageCompleter{
		uow:         uow,
		files:       files,
		maxAttempts: cfg.Storage.MaxAttachmentAttempts,
		log:         log.With("component", "inbound_completion"),
	}
}

// Complete uploads the pending parts and moves the Message to ready. Returning
// nil lets the IMAP cursor pass the email: either it is complete, or it failed
// permanently and its quarantine record is committed.
func (u *MessageCompleter) Complete(
	ctx context.Context,
	parsed *model.ParsedEmail,
	message storedMessage,
) error {
	log := u.log.With(
		"domain_id", parsed.DomainID,
		"profile_id", parsed.ProfileID,
		"message_id", message.ID,
	)

	attachments, err := u.uow.EmailMessageAttachmentStore().List(ctx, parsed.DomainID, message.ID)
	if err != nil {
		return err
	}

	pending := 0
	for _, attachment := range attachments {
		if attachment.State == model.EmailAttachmentStatePending {
			pending++
		}
	}
	log.Debug("attachments of the email", "parts", len(attachments), "pending", pending)

	for _, attachment := range attachments {
		if attachment.State != model.EmailAttachmentStatePending {
			continue
		}

		// Checked before the upload as well, so a worker that lost the profile
		// stops before spending it.
		if err := u.uow.EnsureEmailProfileAssignment(ctx, parsed.Assignment); err != nil {
			return err
		}

		content, ok := partContent(parsed, attachment.Position)
		if !ok {
			log.Error("attachment part is no longer available", "position", attachment.Position)

			return u.fail(ctx, parsed, message, "attachment part is no longer available in the email")
		}

		result, err := u.files.UploadFile(ctx, storageinfra.UploadRequest{
			DomainID:    parsed.DomainID,
			Name:        attachment.FileName,
			MimeType:    attachment.MimeType,
			ReferenceID: referenceID(message.ID, attachment.Position),
			Content:     content,
		})
		if err != nil {
			return u.uploadFailed(ctx, parsed, message, attachment, err, log)
		}

		if err := u.markStored(ctx, parsed, message, attachment.Position, result.FileID); err != nil {
			return err
		}

		log.Debug("attachment stored",
			"position", attachment.Position,
			"file_id", result.FileID,
			"disposition", attachment.Disposition,
		)
	}

	return u.markReady(ctx, parsed, message, log)
}

// markStored records the file id in its own short transaction, so a failure
// later leaves at most one unbound file instead of all of them.
func (u *MessageCompleter) markStored(
	ctx context.Context,
	parsed *model.ParsedEmail,
	message storedMessage,
	position int32,
	fileID int64,
) error {
	return u.uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
		if err := uow.EnsureEmailProfileAssignment(ctx, parsed.Assignment); err != nil {
			return err
		}

		stored, err := uow.EmailMessageAttachmentStore().MarkStored(
			ctx, parsed.DomainID, message.ID, position, fileID,
		)
		if err != nil {
			return err
		}
		if !stored {
			// The part was finished by someone else or is not meant for upload.
			u.log.Warn("attachment part was already finished",
				"message_id", message.ID, "position", position, "file_id", fileID)
		}

		return nil
	})
}

// markReady completes the Message and advances the Thread in one transaction,
// so the email becomes visible with every attachment it owns.
func (u *MessageCompleter) markReady(
	ctx context.Context,
	parsed *model.ParsedEmail,
	message storedMessage,
	log *slog.Logger,
) error {
	return u.uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
		if err := uow.EnsureEmailProfileAssignment(ctx, parsed.Assignment); err != nil {
			return err
		}

		state, err := uow.EmailMessageStore().MarkReady(ctx, parsed.DomainID, message.ID)
		if err != nil {
			return err
		}
		if state == model.EmailMessageStateProcessing {
			return kiterrors.New(
				"email message still has pending attachments",
				kiterrors.WithID("inbound.completion.incomplete"),
			)
		}
		// A concurrent delivery of the same email may have completed it already,
		// which is an outcome rather than a failure.
		if state != model.EmailMessageStateReady {
			log.Debug("email message was already finished elsewhere", "state", state)

			return nil
		}

		if err := uow.EmailThreadStore().AdvanceLastMessage(
			ctx, parsed.DomainID, parsed.ProfileID, message.ThreadID, parsed.ReceivedAt,
		); err != nil {
			return err
		}

		log.Info("email message completed with attachments")

		return nil
	})
}

// uploadFailed decides what one failed upload means: a refused file ends the
// Message at once, a canceled context costs nothing, and anything else spends
// one attempt of the whole Message.
func (u *MessageCompleter) uploadFailed(
	ctx context.Context,
	parsed *model.ParsedEmail,
	message storedMessage,
	attachment *model.EmailMessageAttachment,
	uploadErr error,
	log *slog.Logger,
) error {
	// Draining or shutdown is not a failure of this email, so it spends nothing.
	if ctx.Err() != nil {
		log.Debug("attachment upload interrupted", "position", attachment.Position, "err", uploadErr)

		return uploadErr
	}

	category := uploadErrorCategory(uploadErr)

	if stderrors.Is(uploadErr, storageinfra.ErrFileRejected) {
		log.Error("storage refused the attachment",
			"position", attachment.Position, "category", category, "err", uploadErr)

		return u.fail(ctx, parsed, message, "storage refused an attachment of this email")
	}

	// The attempt and the decision it leads to are one transaction, so a crash
	// can neither lose the attempt nor leave the Message retried past its budget.
	var exhausted bool
	err := u.uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
		if err := uow.EnsureEmailProfileAssignment(ctx, parsed.Assignment); err != nil {
			return err
		}

		attempts, err := uow.EmailMessageStore().IncrementAttachmentAttempts(ctx, parsed.DomainID, message.ID)
		if err != nil {
			return err
		}

		log.Warn("attachment upload failed",
			"position", attachment.Position,
			"category", category,
			"attempt", attempts,
			"max_attempts", u.maxAttempts,
			"err", uploadErr,
		)

		if attempts < u.maxAttempts {
			return nil
		}

		exhausted = true

		return quarantine(ctx, uow, parsed, message,
			"attachment upload did not succeed within the allowed attempts")
	})
	if err != nil {
		return err
	}

	if exhausted {
		u.logFailed(parsed, message, "attachment upload did not succeed within the allowed attempts")

		return nil
	}

	return uploadErr
}

// fail gives up on the Message and records why, both in one transaction, then
// confirms the email so it stops blocking the mailbox. Already stored file ids
// stay in the manifest: storage offers no service-side delete.
func (u *MessageCompleter) fail(
	ctx context.Context,
	parsed *model.ParsedEmail,
	message storedMessage,
	reason string,
) error {
	err := u.uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
		if err := uow.EnsureEmailProfileAssignment(ctx, parsed.Assignment); err != nil {
			return err
		}

		return quarantine(ctx, uow, parsed, message, reason)
	})
	if err != nil {
		return err
	}

	u.logFailed(parsed, message, reason)

	return nil
}

// quarantine moves the Message to failed and records the reason in the caller's
// transaction. A Message that is no longer processing gets no record: its
// outcome is already decided.
func quarantine(
	ctx context.Context,
	uow store.UnitOfWork,
	parsed *model.ParsedEmail,
	message storedMessage,
	reason string,
) error {
	failed, err := uow.EmailMessageStore().MarkFailed(ctx, parsed.DomainID, message.ID)
	if err != nil || !failed {
		return err
	}

	return uow.InboundFailureStore().Record(ctx, &model.InboundFailure{
		DomainID:    parsed.DomainID,
		ProfileID:   parsed.ProfileID,
		Mailbox:     parsed.Mailbox,
		UIDValidity: parsed.UIDValidity,
		UID:         parsed.UID,
		Category:    model.InboundFailureAttachmentUpload,
		Reason:      reason,
		MessageID:   parsed.MessageID,
		Sender:      firstAddress(parsed.From),
		Subject:     parsed.Subject,
	})
}

func (u *MessageCompleter) logFailed(parsed *model.ParsedEmail, message storedMessage, reason string) {
	u.log.Error("email message failed on attachments",
		"domain_id", parsed.DomainID,
		"profile_id", parsed.ProfileID,
		"message_id", message.ID,
		"reason", reason,
	)
}

// uploadErrorCategory names the failure for the logs without quoting the error.
func uploadErrorCategory(err error) string {
	switch {
	case stderrors.Is(err, storageinfra.ErrFileRejected):
		return "file_rejected"
	case stderrors.Is(err, storageinfra.ErrUnavailable):
		return "unavailable"
	default:
		return "other"
	}
}

func firstAddress(addresses []model.EmailAddress) string {
	if len(addresses) == 0 {
		return ""
	}

	return addresses[0].Address
}

// partContent returns the decoded bytes of a manifest position. A pending part
// without content means the email no longer yields it, which retrying cannot fix.
func partContent(parsed *model.ParsedEmail, position int32) ([]byte, bool) {
	if position < 0 || int(position) >= len(parsed.Parts) {
		return nil, false
	}

	part := parsed.Parts[int(position)]
	if part.SkippedReason != "" || part.Content == nil {
		return nil, false
	}

	return part.Content, true
}

// referenceID identifies a part in storage; it is stable across redeliveries,
// so the same part is never stored under two references.
func referenceID(messageID int64, position int32) string {
	return fmt.Sprintf("email-%d-%d", messageID, position)
}
