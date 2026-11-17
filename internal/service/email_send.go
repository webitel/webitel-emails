package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"strings"
	"time"

	"github.com/google/uuid"
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/outbound"
	"github.com/webitel/webitel-emails/internal/store"
)

// OutboundMessageInput is an email as the client composed it.
type OutboundMessageInput struct {
	To, Cc, Bcc          []model.EmailAddress
	Subject              string
	HTMLBody             string
	TextBody             string
	AttachmentReferences []string
}

// ReplyInput is a reply in a Thread. AttemptID 0 replies from history.
type ReplyInput struct {
	ThreadID               int64
	RequestID              string
	AttemptID              int64
	ExpectedAttemptVersion int64
	InReplyToMessageID     int64
	Message                OutboundMessageInput
}

// ContactMode is the operator's confirmed contact of a new Thread.
type ContactMode int

// Contact modes of a new Thread; an unspecified one is rejected.
const (
	ContactModeUnspecified ContactMode = iota
	ContactModeLink
	ContactModeNone
)

// CreateThreadInput is a new outbound email that starts a Thread.
type CreateThreadInput struct {
	RequestID   string
	ProfileID   int64
	ContactMode ContactMode
	ContactID   int64
	Message     OutboundMessageInput
}

// EmailSenderProfile is a profile a new email may be sent from, with the caller's
// signature.
type EmailSenderProfile struct {
	ID           int64
	Name         string
	EmailAddress string
	SenderName   string
	Signature    string
}

// errRequestStored rolls back a transaction that found its request already stored;
// the stored result is returned instead.
var errRequestStored = errors.New("outbound request already stored")

// EmailSendService accepts outbound emails for SMTP delivery. It never changes the
// attempt: rights come from the latest applied projection.
type EmailSendService struct {
	uow      store.UnitOfWork
	reads    *EmailThreadReadService
	profiles store.EmailProfileStore
	contacts ContactVerifier
	cfg      config.OutboundConfig
	now      func() time.Time
}

// NewEmailSendService creates the send service.
func NewEmailSendService(
	cfg *config.Config,
	uow store.UnitOfWork,
	reads *EmailThreadReadService,
	profiles store.EmailProfileStore,
	contacts ContactVerifier,
) *EmailSendService {
	return &EmailSendService{uow: uow, reads: reads, profiles: profiles, contacts: contacts, cfg: cfg.Outbound, now: time.Now}
}

// ListSenderProfiles returns the profiles a new email may be sent from, by the
// same check as Send; enabled does not apply.
func (s *EmailSendService) ListSenderProfiles(ctx context.Context, session *auth.Session) ([]EmailSenderProfile, error) {
	if !s.cfg.Enabled {
		return nil, errOutboundDisabled()
	}
	profiles, _, err := s.profiles.List(ctx, session.DomainID, model.EmailProfileFilter{Size: -1, Sort: "name"})
	if err != nil {
		return nil, err
	}
	agentName, err := s.reads.threads.UserName(ctx, session.DomainID, session.UserID)
	if err != nil {
		return nil, err
	}

	result := make([]EmailSenderProfile, 0, len(profiles))
	for _, p := range profiles {
		if _, err := newThreadProfile(p); err != nil {
			continue
		}
		result = append(result, EmailSenderProfile{
			ID: p.ID, Name: p.Name, EmailAddress: p.EmailAddress, SenderName: p.SenderName,
			Signature: outbound.Signature(p.Signature, agentName),
		})
	}

	return result, nil
}

// CreateThread starts a processed Thread with an outbound email of its author. A
// repeated request returns the stored email; the Thread is returned only while the
// author still sees it, nil otherwise.
func (s *EmailSendService) CreateThread(
	ctx context.Context,
	session *auth.Session,
	in CreateThreadInput,
) (*model.EmailThreadView, *model.EmailMessageView, error) {
	if !s.cfg.Enabled {
		return nil, nil, errOutboundDisabled()
	}
	requestID, err := parseRequestID(in.RequestID)
	if err != nil {
		return nil, nil, err
	}
	if in.ProfileID <= 0 {
		return nil, nil, invalidEmailProfile("id_required", "email profile id is required")
	}
	if !(in.ContactMode == ContactModeLink && in.ContactID > 0 || in.ContactMode == ContactModeNone && in.ContactID == 0) {
		return nil, nil, kiterrors.InvalidArgument("contact mode is required: a contact id to link, or none",
			kiterrors.WithID("email.message.invalid_contact_mode"))
	}
	fingerprint := createFingerprint(in)

	message, err := s.result(ctx, session, requestID, fingerprint)
	if err != nil {
		return nil, nil, err
	}
	if message == nil {
		// Applied after the request lock, so a concurrent first request is returned
		// instead of this failure.
		var contactErr error
		if in.ContactMode == ContactModeLink {
			contactErr = s.verifyContact(ctx, session.DomainID, in.ContactID)
		}
		message, err = s.accept(ctx, session, requestID, fingerprint, func(ctx context.Context, uow store.UnitOfWork) error {
			if contactErr != nil {
				return contactErr
			}
			return s.acceptThread(ctx, uow, session, in, requestID, fingerprint)
		})
		if err != nil {
			return nil, nil, err
		}
	}

	thread, err := s.reads.threads.LocateThread(ctx, session.DomainID, session.UserID, message.ThreadID)
	if err != nil {
		return nil, nil, err
	}
	if thread != nil {
		normalizePreview(thread)
	}

	return thread, message, nil
}

func (s *EmailSendService) acceptThread(
	ctx context.Context,
	uow store.UnitOfWork,
	session *auth.Session,
	in CreateThreadInput,
	requestID uuid.UUID,
	fingerprint []byte,
) error {
	out := uow.EmailOutboundStore()
	profile, err := out.SenderProfile(ctx, session.DomainID, in.ProfileID)
	if err != nil {
		return err
	}
	sender, err := newThreadProfile(profile)
	if err != nil {
		return err
	}
	references, files, acceptedAt, err := s.lockFiles(ctx, out, session, in.Message.AttachmentReferences)
	if err != nil {
		return err
	}
	prepared, err := outbound.Prepare(s.cfg, draftOf(in.Message, files))
	if err != nil {
		return err
	}

	thread := &model.EmailThread{
		DomainID: session.DomainID, ProfileID: in.ProfileID, Kind: model.EmailThreadKindRegular,
		Subject: prepared.Subject, Status: model.EmailThreadStatusProcessed, CompletedAt: &acceptedAt,
		ContactResolutionState: model.EmailContactResolutionUnlinked,
	}
	if in.ContactMode == ContactModeLink {
		thread.ContactID, thread.ContactResolutionState = &in.ContactID, model.EmailContactResolutionResolved
	}
	thread, err = uow.EmailThreadStore().Create(ctx, thread)
	if err != nil {
		return err
	}
	if err := out.AddAuthor(ctx, session.DomainID, thread.ID, session.UserID, acceptedAt); err != nil {
		return err
	}

	message := &model.EmailMessage{
		DomainID: session.DomainID, ThreadID: thread.ID, ProfileID: in.ProfileID,
		MessageID: outbound.NewMessageID(sender.Address),
		Subject:   prepared.Subject, TextBody: prepared.TextBody, HTMLBody: prepared.HTMLBody,
		SentAt: &acceptedAt, ReceivedAt: acceptedAt,
		AuthorUserID: session.UserID, RequestID: requestID, RequestFingerprint: fingerprint,
	}

	return storeOutbound(ctx, uow, message, sender, prepared, references, files)
}

func (s *EmailSendService) verifyContact(ctx context.Context, domainID, contactID int64) error {
	own, err := s.contacts.VerifyContact(ctx, domainID, contactID)
	switch {
	case err != nil:
		return err
	case !own:
		return errContactNotFound()
	}

	return nil
}

// SendReply stores a reply as pending delivery. A repeated request returns the
// stored message whatever happened to the Thread since.
func (s *EmailSendService) SendReply(ctx context.Context, session *auth.Session, in ReplyInput) (*model.EmailMessageView, error) {
	if !s.cfg.Enabled {
		return nil, errOutboundDisabled()
	}
	if in.ThreadID <= 0 {
		return nil, errThreadIDRequired()
	}
	requestID, err := parseRequestID(in.RequestID)
	if err != nil {
		return nil, err
	}
	fingerprint := replyFingerprint(in)

	if view, err := s.result(ctx, session, requestID, fingerprint); view != nil || err != nil {
		return view, err
	}

	return s.accept(ctx, session, requestID, fingerprint, func(ctx context.Context, uow store.UnitOfWork) error {
		return s.acceptReply(ctx, uow, session, in, requestID, fingerprint)
	})
}

// accept runs body under the request lock unless the request is already stored,
// then reads the stored result.
func (s *EmailSendService) accept(
	ctx context.Context,
	session *auth.Session,
	requestID uuid.UUID,
	fingerprint []byte,
	body func(context.Context, store.UnitOfWork) error,
) (*model.EmailMessageView, error) {
	err := s.uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
		out := uow.EmailOutboundStore()
		// Equal request ids are serialized whatever their target, so a concurrent one
		// is seen here as stored.
		if err := out.LockRequest(ctx, session.DomainID, session.UserID, requestID); err != nil {
			return err
		}
		prior, err := out.LocateRequest(ctx, session.DomainID, session.UserID, requestID)
		if err != nil || prior != nil {
			return storedRequest(prior, fingerprint, err)
		}

		return body(ctx, uow)
	})
	if err != nil && !errors.Is(err, errRequestStored) {
		return nil, err
	}

	view, err := s.result(ctx, session, requestID, fingerprint)
	if err == nil && view == nil {
		err = kiterrors.Internal("accepted email not found", kiterrors.WithID("email.message.not_found"))
	}

	return view, err
}

func (s *EmailSendService) acceptReply(
	ctx context.Context,
	uow store.UnitOfWork,
	session *auth.Session,
	in ReplyInput,
	requestID uuid.UUID,
	fingerprint []byte,
) error {
	out := uow.EmailOutboundStore()
	thread, err := out.LockReplyThread(ctx, session.DomainID, session.UserID, in.ThreadID)
	if err != nil {
		return err
	}
	if thread == nil {
		return errThreadNotFound()
	}
	if err := checkReplyRights(thread, session.UserID, in.AttemptID, in.ExpectedAttemptVersion); err != nil {
		return err
	}

	profile, err := out.SenderProfile(ctx, session.DomainID, thread.ProfileID)
	if err != nil {
		return err
	}
	sender, err := sendingProfile(profile)
	if err != nil {
		return err
	}

	references, files, acceptedAt, err := s.lockFiles(ctx, out, session, in.Message.AttachmentReferences)
	if err != nil {
		return err
	}

	parent, err := out.ReplyParent(ctx, session.DomainID, in.ThreadID, in.InReplyToMessageID)
	if err != nil {
		return err
	}
	if parent == nil {
		return errReplyParentNotFound()
	}
	inReplyTo, refs, err := outbound.ReplyHeaders(parent.MessageID, parent.References)
	if err != nil {
		return err
	}

	draft := draftOf(in.Message, files)
	draft.Reply, draft.ThreadSubject = true, thread.Subject
	prepared, err := outbound.Prepare(s.cfg, draft)
	if err != nil {
		return err
	}

	message := &model.EmailMessage{
		DomainID: session.DomainID, ThreadID: in.ThreadID, ProfileID: thread.ProfileID,
		MessageID: outbound.NewMessageID(sender.Address), InReplyTo: inReplyTo, References: refs,
		Subject: prepared.Subject, TextBody: prepared.TextBody, HTMLBody: prepared.HTMLBody,
		SentAt: &acceptedAt, ReceivedAt: acceptedAt,
		AuthorUserID: session.UserID, RequestID: requestID, RequestFingerprint: fingerprint,
	}

	return storeOutbound(ctx, uow, message, sender, prepared, references, files)
}

// lockFiles locks the uploads of a request and takes the acceptance time after the
// last lock, so a reference cannot expire while the request waits.
func (s *EmailSendService) lockFiles(
	ctx context.Context,
	out store.EmailOutboundStore,
	session *auth.Session,
	values []string,
) ([]uuid.UUID, []*model.EmailAttachmentUpload, time.Time, error) {
	references, err := parseReferences(values)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	uploads, err := out.LockUploads(ctx, session.DomainID, session.UserID, references)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	acceptedAt := s.now().UTC().Truncate(time.Microsecond)
	files, err := orderUploads(references, uploads, acceptedAt)

	return references, files, acceptedAt, err
}

func draftOf(m OutboundMessageInput, files []*model.EmailAttachmentUpload) outbound.Draft {
	draft := outbound.Draft{
		To: m.To, Cc: m.Cc, Bcc: m.Bcc, Subject: m.Subject, HTMLBody: m.HTMLBody, TextBody: m.TextBody,
	}
	for _, f := range files {
		draft.Attachments = append(draft.Attachments, outbound.AttachmentMeta{FileName: f.FileName, Size: f.Size})
	}

	return draft
}

// storeOutbound writes the accepted message with its recipients and files and tells
// Workspace, all in the caller's transaction.
func storeOutbound(
	ctx context.Context,
	uow store.UnitOfWork,
	message *model.EmailMessage,
	sender outbound.Sender,
	prepared *outbound.Prepared,
	references []uuid.UUID,
	files []*model.EmailAttachmentUpload,
) error {
	out := uow.EmailOutboundStore()
	inserted, err := out.CreateMessage(ctx, message)
	if err != nil {
		return err
	}
	if !inserted {
		// Rolls back what the transaction wrote before, such as a new Thread.
		prior, err := out.LocateRequest(ctx, message.DomainID, message.AuthorUserID, message.RequestID)

		return storedRequest(prior, message.RequestFingerprint, err)
	}

	recipients := []*model.EmailRecipient{{
		Type: model.EmailRecipientTypeFrom, Address: sender.Address,
		NormalizedAddress: strings.ToLower(sender.Address), DisplayName: sender.Name,
	}}
	if sender.ReplyTo != "" {
		recipients = append(recipients, &model.EmailRecipient{
			Type: model.EmailRecipientTypeReplyTo, Address: sender.ReplyTo, NormalizedAddress: strings.ToLower(sender.ReplyTo),
		})
	}
	for _, list := range []struct {
		kind      model.EmailRecipientType
		addresses []model.EmailAddress
	}{
		{model.EmailRecipientTypeTo, prepared.To},
		{model.EmailRecipientTypeCc, prepared.Cc},
		{model.EmailRecipientTypeBcc, prepared.Bcc},
	} {
		for i, a := range list.addresses {
			recipients = append(recipients, &model.EmailRecipient{
				Type: list.kind, Address: a.Address, NormalizedAddress: a.NormalizedAddress,
				DisplayName: a.Name, Ordinal: int32(i),
			})
		}
	}
	if err := uow.EmailRecipientStore().CreateBulk(ctx, message.DomainID, message.ID, recipients); err != nil {
		return err
	}

	if err := out.UseUploads(ctx, message.DomainID, references, message.ID); err != nil {
		return err
	}
	parts := make([]*model.EmailMessageAttachment, 0, len(files))
	for i, f := range files {
		parts = append(parts, &model.EmailMessageAttachment{
			FileID: &f.FileID, Disposition: model.EmailPartDispositionAttachment, FileName: f.FileName,
			MimeType: f.MimeType, Size: f.Size, Position: int32(i), State: model.EmailAttachmentStateStored,
		})
	}
	if err := uow.EmailMessageAttachmentStore().CreateBulk(ctx, message.DomainID, message.ID, parts); err != nil {
		return err
	}

	if err := uow.EmailThreadStore().AdvanceLastMessage(
		ctx, message.DomainID, message.ProfileID, message.ThreadID, message.ReceivedAt,
	); err != nil {
		return err
	}
	_, err = uow.EmailWorkspaceEventStore().Record(ctx, store.EmailWorkspaceChange{
		DomainID: message.DomainID, ThreadID: message.ThreadID, EventType: event.TypeWorkspaceMessageCreated,
		MessageID: &message.ID, OccurredAt: message.ReceivedAt,
	})

	return err
}

// result reads the message of a request without the visibility boundary and
// presents it as the read API does; nil means the request has none.
func (s *EmailSendService) result(
	ctx context.Context,
	session *auth.Session,
	requestID uuid.UUID,
	fingerprint []byte,
) (*model.EmailMessageView, error) {
	view, stored, err := s.reads.threads.LocateMessageByRequest(ctx, session.DomainID, session.UserID, requestID)
	if err != nil || view == nil {
		return nil, err
	}
	if !bytes.Equal(stored, fingerprint) {
		return nil, errRequestConflict()
	}
	if err := s.reads.presentPage(ctx, session.DomainID, []*model.EmailMessageView{view}); err != nil {
		return nil, err
	}

	return view, nil
}

// storedRequest ends the transaction of a request already stored: errRequestStored
// for the same fingerprint, whose message is read after the rollback.
func storedRequest(prior *store.EmailOutboundRequest, fingerprint []byte, err error) error {
	switch {
	case err != nil:
		return err
	case prior == nil:
		return kiterrors.Internal("outbound request not found", kiterrors.WithID("email.message.not_found"))
	case !bytes.Equal(prior.Fingerprint, fingerprint):
		return errRequestConflict()
	}

	return errRequestStored
}

// checkReplyRights: the current agent replies within the attempt it sees, a
// participant replies from history only when the Thread has no agent.
func checkReplyRights(t *store.EmailReplyThread, userID, attemptID, version int64) error {
	current := t.CurrentUserID != nil && *t.CurrentUserID == userID
	if attemptID != 0 {
		switch {
		case !current || t.Status != model.EmailThreadStatusInProgress:
			return errReplyNotAllowed()
		case t.CurrentAttemptID == nil || *t.CurrentAttemptID != attemptID || *t.CurrentAttemptVersion != version:
			return kiterrors.New("email thread attempt changed",
				kiterrors.WithID("email.thread.attempt_version_conflict"), kiterrors.WithCode(codes.Aborted))
		}

		return nil
	}
	if t.Status == model.EmailThreadStatusProcessed && t.CurrentUserID == nil && t.Participant {
		return nil
	}

	return errReplyNotAllowed()
}

// sendingProfile requires SMTP settings, a usable OAuth2 connection and valid
// sender headers; enabled and allow_send do not apply to a reply.
func sendingProfile(p *model.EmailProfile) (outbound.Sender, error) {
	sender := outbound.Sender{Name: p.SenderName, Address: p.EmailAddress, ReplyTo: p.ReplyTo}
	usable := p.SMTPHost != "" && p.SMTPPort > 0
	if p.AuthType == model.EmailAuthTypeOAuth2 {
		usable = usable && p.OAuthConnected && p.ConnectionState != model.EmailConnectionStateReauthorizationRequired
	}
	if !usable {
		return sender, errSendUnavailable()
	}

	return sender, outbound.CheckSender(sender)
}

// newThreadProfile also requires allow_send, which only a new email needs.
func newThreadProfile(p *model.EmailProfile) (outbound.Sender, error) {
	if !p.AllowSend {
		return outbound.Sender{}, errSendUnavailable()
	}

	return sendingProfile(p)
}

func parseRequestID(value string) (uuid.UUID, error) {
	requestID, err := uuid.Parse(value)
	if err != nil || requestID == uuid.Nil {
		return uuid.Nil, kiterrors.InvalidArgument("request id must be a UUID", kiterrors.WithID("email.message.request_id_invalid"))
	}

	return requestID, nil
}

func parseReferences(values []string) ([]uuid.UUID, error) {
	references := make([]uuid.UUID, 0, len(values))
	seen := make(map[uuid.UUID]bool, len(values))
	for _, value := range values {
		reference, err := uuid.Parse(value)
		if err != nil || seen[reference] {
			return nil, kiterrors.InvalidArgument("attachment reference is invalid or repeated",
				kiterrors.WithID("email.attachment.reference_invalid"))
		}
		seen[reference] = true
		references = append(references, reference)
	}

	return references, nil
}

// orderUploads returns the uploads in the client's order; a missing or expired one
// is reported without the reason.
func orderUploads(references []uuid.UUID, uploads []*model.EmailAttachmentUpload, at time.Time) ([]*model.EmailAttachmentUpload, error) {
	byReference := make(map[uuid.UUID]*model.EmailAttachmentUpload, len(uploads))
	for _, u := range uploads {
		byReference[u.Reference] = u
	}
	files := make([]*model.EmailAttachmentUpload, 0, len(references))
	for _, reference := range references {
		u := byReference[reference]
		if u == nil || !u.ExpiresAt.After(at) {
			return nil, kiterrors.New("attachment is unavailable; upload it again",
				kiterrors.WithID("email.attachment.reference_unavailable"), kiterrors.WithCode(codes.FailedPrecondition))
		}
		files = append(files, u)
	}

	return files, nil
}

// replyFingerprint hashes the request as the client sent it, length-prefixed so no
// two requests encode alike; the attempt is left out, so a retry after a version
// conflict keeps its id.
func replyFingerprint(in ReplyInput) []byte {
	f := fingerprint{sha256.New()}
	f.text("reply")
	f.number(in.ThreadID)
	f.number(in.InReplyToMessageID)
	f.message(in.Message)

	return f.h.Sum(nil)
}

func createFingerprint(in CreateThreadInput) []byte {
	f := fingerprint{sha256.New()}
	f.text("create")
	f.number(in.ProfileID)
	f.number(int64(in.ContactMode))
	f.number(in.ContactID)
	f.message(in.Message)

	return f.h.Sum(nil)
}

type fingerprint struct{ h hash.Hash }

func (f fingerprint) number(v int64) {
	_ = binary.Write(f.h, binary.BigEndian, v)
}

func (f fingerprint) text(v string) {
	f.number(int64(len(v)))
	_, _ = f.h.Write([]byte(v))
}

func (f fingerprint) message(m OutboundMessageInput) {
	for _, list := range [][]model.EmailAddress{m.To, m.Cc, m.Bcc} {
		f.number(int64(len(list)))
		for _, a := range list {
			f.text(a.Address)
			f.text(a.Name)
		}
	}
	f.text(m.Subject)
	f.text(m.HTMLBody)
	f.text(m.TextBody)
	f.number(int64(len(m.AttachmentReferences)))
	for _, r := range m.AttachmentReferences {
		f.text(r)
	}
}

func errSendUnavailable() error {
	return kiterrors.New("email profile cannot send email",
		kiterrors.WithID("email.profile.send_unavailable"), kiterrors.WithCode(codes.FailedPrecondition))
}

func errReplyNotAllowed() error {
	return kiterrors.New("reply is not allowed in this email thread",
		kiterrors.WithID("email.thread.reply_not_allowed"), kiterrors.WithCode(codes.FailedPrecondition))
}

func errReplyParentNotFound() error {
	return kiterrors.New("no email to reply to",
		kiterrors.WithID("email.thread.reply_parent_not_found"), kiterrors.WithCode(codes.FailedPrecondition))
}

func errRequestConflict() error {
	return kiterrors.New("request id is already used for another email",
		kiterrors.WithID("email.message.request_conflict"), kiterrors.WithCode(codes.AlreadyExists))
}
