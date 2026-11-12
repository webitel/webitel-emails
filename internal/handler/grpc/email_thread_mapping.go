package grpc

import (
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
)

var threadStatusToProto = map[model.EmailThreadStatus]emailpb.EmailThreadStatus{
	model.EmailThreadStatusNew:         emailpb.EmailThreadStatus_EMAIL_THREAD_STATUS_NEW,
	model.EmailThreadStatusDistributed: emailpb.EmailThreadStatus_EMAIL_THREAD_STATUS_DISTRIBUTED,
	model.EmailThreadStatusInProgress:  emailpb.EmailThreadStatus_EMAIL_THREAD_STATUS_IN_PROGRESS,
	model.EmailThreadStatusPostponed:   emailpb.EmailThreadStatus_EMAIL_THREAD_STATUS_POSTPONED,
	model.EmailThreadStatusProcessed:   emailpb.EmailThreadStatus_EMAIL_THREAD_STATUS_PROCESSED,
}

var threadStatusFromProto = func() map[emailpb.EmailThreadStatus]model.EmailThreadStatus {
	out := make(map[emailpb.EmailThreadStatus]model.EmailThreadStatus, len(threadStatusToProto))
	for status, value := range threadStatusToProto {
		out[value] = status
	}

	return out
}()

var contactResolutionToProto = map[model.EmailContactResolutionState]emailpb.EmailContactResolutionState{
	model.EmailContactResolutionPending:     emailpb.EmailContactResolutionState_EMAIL_CONTACT_RESOLUTION_STATE_PENDING,
	model.EmailContactResolutionResolved:    emailpb.EmailContactResolutionState_EMAIL_CONTACT_RESOLUTION_STATE_RESOLVED,
	model.EmailContactResolutionNotFound:    emailpb.EmailContactResolutionState_EMAIL_CONTACT_RESOLUTION_STATE_NOT_FOUND,
	model.EmailContactResolutionAmbiguous:   emailpb.EmailContactResolutionState_EMAIL_CONTACT_RESOLUTION_STATE_AMBIGUOUS,
	model.EmailContactResolutionUnavailable: emailpb.EmailContactResolutionState_EMAIL_CONTACT_RESOLUTION_STATE_UNAVAILABLE,
	model.EmailContactResolutionUnlinked:    emailpb.EmailContactResolutionState_EMAIL_CONTACT_RESOLUTION_STATE_UNLINKED,
}

var messageDirectionToProto = map[model.EmailMessageDirection]emailpb.EmailMessageDirection{
	model.EmailMessageDirectionInbound:  emailpb.EmailMessageDirection_EMAIL_MESSAGE_DIRECTION_INBOUND,
	model.EmailMessageDirectionOutbound: emailpb.EmailMessageDirection_EMAIL_MESSAGE_DIRECTION_OUTBOUND,
}

var threadAccessToProto = map[model.EmailThreadAccess]emailpb.EmailThreadAccess{
	model.EmailThreadAccessCurrent: emailpb.EmailThreadAccess_EMAIL_THREAD_ACCESS_CURRENT,
	model.EmailThreadAccessHistory: emailpb.EmailThreadAccess_EMAIL_THREAD_ACCESS_HISTORY,
}

func threadListRequestFromProto(req *emailpb.ListEmailThreadsRequest) (service.EmailThreadListRequest, error) {
	out := service.EmailThreadListRequest{
		Page:       req.GetPage(),
		Size:       req.GetSize(),
		Sort:       req.GetSort(),
		ProfileIDs: req.GetProfileId(),
		ContactID:  req.GetContactId(),
	}

	switch req.GetAssignment() {
	case emailpb.EmailThreadAssignment_EMAIL_THREAD_ASSIGNMENT_UNSPECIFIED,
		emailpb.EmailThreadAssignment_EMAIL_THREAD_ASSIGNMENT_ALL:
		out.Assignment = model.EmailThreadAssignmentAll
	case emailpb.EmailThreadAssignment_EMAIL_THREAD_ASSIGNMENT_CURRENT:
		out.Assignment = model.EmailThreadAssignmentCurrent
	case emailpb.EmailThreadAssignment_EMAIL_THREAD_ASSIGNMENT_HISTORY:
		out.Assignment = model.EmailThreadAssignmentHistory
	default:
		return out, invalidThreadFilter("unknown assignment")
	}

	for _, status := range req.GetStatus() {
		value, ok := threadStatusFromProto[status]
		if !ok {
			return out, invalidThreadFilter("unknown status")
		}
		out.Statuses = append(out.Statuses, value)
	}

	return out, nil
}

func emailThreadToProto(view *model.EmailThreadView) *emailpb.EmailThread {
	out := &emailpb.EmailThread{
		Id:                     view.ID,
		Profile:                lookupToProto(&view.Profile),
		Subject:                view.Subject,
		Status:                 threadStatusToProto[view.Status],
		StatusChangedAt:        unixMilliValue(view.StatusChangedAt),
		CreatedAt:              unixMilli(view.CreatedAt),
		UpdatedAt:              unixMilli(view.UpdatedAt),
		LastMessageAt:          unixMilliValue(view.LastMessageAt),
		CompletedAt:            unixMilliValue(view.CompletedAt),
		ContactId:              int64Value(view.ContactID),
		ContactResolutionState: contactResolutionToProto[view.ContactResolutionState],
		QueueId:                int64Value(view.QueueID),
		PostponedUntil:         unixMilliValue(view.PostponedUntil),
		CurrentUser:            lookupToProto(view.CurrentUser),
		LastHandledUser:        lookupToProto(view.LastHandledUser),
		Access:                 threadAccessToProto[view.Access],
		CurrentAttemptId:       int64Value(view.CurrentAttemptID),
		CurrentAttemptVersion:  int64Value(view.CurrentAttemptVersion),
		ProjectionVersion:      int64Value(view.ProjectionVersion),
		WorkspaceVersion:       view.WorkspaceVersion,
	}
	if preview := view.Preview; preview != nil {
		out.Preview = &emailpb.EmailThreadPreview{
			MessageId:  preview.MessageID,
			Direction:  messageDirectionToProto[preview.Direction],
			Text:       preview.Text,
			ReceivedAt: unixMilli(preview.ReceivedAt),
		}
		if preview.From != nil {
			out.Preview.From = &emailpb.EmailAddress{Address: preview.From.Address, Name: preview.From.Name}
		}
	}

	return out
}

func emailThreadActionToProto(action *model.EmailThreadActionView) *emailpb.EmailThreadAction {
	return &emailpb.EmailThreadAction{
		ProjectionVersion: action.ProjectionVersion,
		OccurredAt:        unixMilli(action.OccurredAt),
		Transition:        action.Transition,
		AttemptState:      action.AttemptState,
		Result:            action.Result,
		Source:            action.Source,
		Actor:             lookupToProto(action.Actor),
		Agent:             lookupToProto(action.Agent),
		QueueId:           int64Value(action.QueueID),
		TransferToQueueId: int64Value(action.TransferToQueueID),
		TransferToUser:    lookupToProto(action.TransferToUser),
		Comment:           action.Comment,
	}
}

func invalidThreadFilter(message string) error {
	return kiterrors.InvalidArgument(message, kiterrors.WithID("email.thread.invalid_filter"))
}

var messageKindToProto = map[model.EmailKind]emailpb.EmailMessageKind{
	model.EmailKindRegular:   emailpb.EmailMessageKind_EMAIL_MESSAGE_KIND_REGULAR,
	model.EmailKindAutoReply: emailpb.EmailMessageKind_EMAIL_MESSAGE_KIND_AUTO_REPLY,
	model.EmailKindBounce:    emailpb.EmailMessageKind_EMAIL_MESSAGE_KIND_BOUNCE,
}

var inlineContentToProto = map[model.EmailInlineContentStatus]emailpb.EmailInlineContentStatus{
	model.EmailInlineContentNone:        emailpb.EmailInlineContentStatus_EMAIL_INLINE_CONTENT_STATUS_NONE,
	model.EmailInlineContentOK:          emailpb.EmailInlineContentStatus_EMAIL_INLINE_CONTENT_STATUS_OK,
	model.EmailInlineContentUnavailable: emailpb.EmailInlineContentStatus_EMAIL_INLINE_CONTENT_STATUS_UNAVAILABLE,
}

var dispositionToProto = map[model.EmailPartDisposition]emailpb.EmailAttachmentDisposition{
	model.EmailPartDispositionAttachment: emailpb.EmailAttachmentDisposition_EMAIL_ATTACHMENT_DISPOSITION_ATTACHMENT,
	model.EmailPartDispositionInline:     emailpb.EmailAttachmentDisposition_EMAIL_ATTACHMENT_DISPOSITION_INLINE,
}

var attachmentStateToProto = map[model.EmailAttachmentState]emailpb.EmailAttachmentState{
	model.EmailAttachmentStateStored:  emailpb.EmailAttachmentState_EMAIL_ATTACHMENT_STATE_STORED,
	model.EmailAttachmentStateSkipped: emailpb.EmailAttachmentState_EMAIL_ATTACHMENT_STATE_SKIPPED,
}

var skippedReasonToProto = map[model.EmailPartSkippedReason]emailpb.EmailAttachmentSkippedReason{
	model.EmailPartSkippedCountLimit: emailpb.EmailAttachmentSkippedReason_EMAIL_ATTACHMENT_SKIPPED_REASON_COUNT_LIMIT_EXCEEDED,
	model.EmailPartSkippedSizeLimit:  emailpb.EmailAttachmentSkippedReason_EMAIL_ATTACHMENT_SKIPPED_REASON_SIZE_LIMIT_EXCEEDED,
	model.EmailPartSkippedTotalLimit: emailpb.EmailAttachmentSkippedReason_EMAIL_ATTACHMENT_SKIPPED_REASON_TOTAL_SIZE_LIMIT_EXCEEDED,
	model.EmailPartSkippedReadError:  emailpb.EmailAttachmentSkippedReason_EMAIL_ATTACHMENT_SKIPPED_REASON_READ_ERROR,
}

func messageOrderFromProto(order emailpb.EmailMessageOrder) (bool, error) {
	switch order {
	case emailpb.EmailMessageOrder_EMAIL_MESSAGE_ORDER_UNSPECIFIED, emailpb.EmailMessageOrder_EMAIL_MESSAGE_ORDER_ASC:
		return false, nil
	case emailpb.EmailMessageOrder_EMAIL_MESSAGE_ORDER_DESC:
		return true, nil
	default:
		return false, invalidThreadFilter("unknown order")
	}
}

// emailMessageToProto maps only the sanitized HTML body; the stored original has
// no field to go to.
func emailMessageToProto(message *model.EmailMessageView) *emailpb.EmailMessage {
	out := &emailpb.EmailMessage{
		Id:                   message.ID,
		ThreadId:             message.ThreadID,
		ProfileId:            message.ProfileID,
		Direction:            messageDirectionToProto[message.Direction],
		Kind:                 messageKindToProto[message.Kind],
		InternetMessageId:    message.InternetMessageID,
		InReplyTo:            message.InReplyTo,
		Subject:              message.Subject,
		From:                 addressesToProto(message.From),
		Sender:               addressesToProto(message.Sender),
		ReplyTo:              addressesToProto(message.ReplyTo),
		To:                   addressesToProto(message.To),
		Cc:                   addressesToProto(message.Cc),
		SentAt:               unixMilliValue(message.SentAt),
		ReceivedAt:           unixMilli(message.ReceivedAt),
		BodyText:             message.BodyText,
		BodyHtml:             message.SanitizedHTML,
		RemoteContentBlocked: message.RemoteContentBlocked,
		InlineContentStatus:  inlineContentToProto[message.InlineContentStatus],
	}
	if bounce := message.Bounce; bounce != nil {
		out.Bounce = &emailpb.EmailBounce{
			OriginalMessageId: bounce.OriginalMessageID,
			Recipient:         bounce.Recipient,
			Status:            bounce.Status,
			DiagnosticCode:    bounce.DiagnosticCode,
		}
	}
	for _, part := range message.Attachments {
		attachment := &emailpb.EmailAttachment{
			Id:            part.ID,
			FileName:      part.FileName,
			MimeType:      part.MimeType,
			Size:          part.Size,
			Disposition:   dispositionToProto[part.Disposition],
			State:         attachmentStateToProto[part.State],
			SkippedReason: skippedReasonToProto[part.SkippedReason],
		}
		if part.Disposition == model.EmailPartDispositionInline {
			attachment.ContentId = part.ContentID
		}
		out.Attachments = append(out.Attachments, attachment)
	}

	return out
}

func addressesToProto(addresses []model.EmailAddress) []*emailpb.EmailAddress {
	out := make([]*emailpb.EmailAddress, 0, len(addresses))
	for _, address := range addresses {
		out = append(out, &emailpb.EmailAddress{Address: address.Address, Name: address.Name})
	}

	return out
}
