package service

import (
	"context"
	"errors"
	"mime"
	"strings"
	"time"

	"github.com/google/uuid"
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/config"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const (
	emailThreadObjClass = "email_thread"
	defaultMimeType     = "application/octet-stream"
	maxMimeTypeLength   = 255
)

// AttachmentUploader stores an outbound attachment outside the inbound upload slots.
type AttachmentUploader interface {
	Upload(ctx context.Context, req storageinfra.UploadRequest) (storageinfra.UploadResult, error)
}

// UploadAttachmentInput is a file an operator uploads for an outbound email.
type UploadAttachmentInput struct {
	FileName string
	MimeType string
	Content  []byte
}

// EmailAttachmentUploadService stores outbound attachments and issues their references.
type EmailAttachmentUploadService struct {
	files   AttachmentUploader
	uploads store.EmailAttachmentUploadStore
	cfg     config.OutboundConfig
	// slots bound the uploads of this instance; a full set is refused, not queued,
	// because each waiting request already holds its file in memory.
	slots chan struct{}
	now   func() time.Time
}

// NewEmailAttachmentUploadService creates the upload service.
func NewEmailAttachmentUploadService(
	cfg *config.Config,
	files AttachmentUploader,
	uploads store.EmailAttachmentUploadStore,
) *EmailAttachmentUploadService {
	return &EmailAttachmentUploadService{
		files:   files,
		uploads: uploads,
		cfg:     cfg.Outbound,
		slots:   make(chan struct{}, cfg.Outbound.MaxConcurrentUploads),
		now:     time.Now,
	}
}

// Upload stores the file and returns the reference its author may attach once.
func (s *EmailAttachmentUploadService) Upload(
	ctx context.Context,
	session *auth.Session,
	in UploadAttachmentInput,
) (*model.EmailAttachmentUpload, error) {
	if !s.cfg.Enabled {
		return nil, errOutboundDisabled()
	}
	// The interceptor checked read; uploading belongs to those who may send.
	if !session.CheckOBACAccess(emailThreadObjClass, auth.Add) && !session.CheckOBACAccess(emailThreadObjClass, auth.Edit) {
		return nil, kiterrors.Forbidden("missing required permissions: "+emailThreadObjClass,
			kiterrors.WithID("auth.interceptor.permission"))
	}

	if strings.TrimSpace(in.FileName) == "" {
		return nil, errAttachmentInvalid("file name is required")
	}
	mimeType, ok := normalizeMimeType(in.MimeType)
	if !ok {
		return nil, errAttachmentInvalid("mime type is invalid")
	}
	switch size := int64(len(in.Content)); {
	case size == 0:
		return nil, errAttachmentInvalid("file is empty")
	case size > s.cfg.MaxAttachmentSize:
		return nil, kiterrors.InvalidArgument("file is larger than the outbound attachment limit",
			kiterrors.WithID("email.attachment.too_large"))
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return nil, kiterrors.New("too many attachment uploads; retry later",
			kiterrors.WithID("email.attachment.upload_busy"), kiterrors.WithCode(codes.ResourceExhausted))
	}

	upload := &model.EmailAttachmentUpload{
		DomainID:     session.DomainID,
		Reference:    uuid.New(),
		AuthorUserID: session.UserID,
		FileName:     storageinfra.SanitizeFileName(in.FileName),
		MimeType:     mimeType,
		Size:         int64(len(in.Content)),
	}
	result, err := s.files.Upload(ctx, storageinfra.UploadRequest{
		DomainID:    upload.DomainID,
		Name:        upload.FileName,
		MimeType:    upload.MimeType,
		ReferenceID: upload.Reference.String(),
		UploadedBy:  upload.AuthorUserID,
		Content:     in.Content,
	})
	if err != nil {
		return nil, uploadError(ctx, err)
	}
	upload.FileID = result.FileID
	upload.ExpiresAt = s.now().Add(s.cfg.AttachmentReferenceTTL)

	// A failure here leaves the file in storage without a reference; storage
	// has no service deletion yet.
	if err := s.uploads.Create(ctx, upload); err != nil {
		return nil, err
	}

	return upload, nil
}

// normalizeMimeType lowercases a valid media type; empty is a generic file.
func normalizeMimeType(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultMimeType, true
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", false
	}
	// ParseMediaType also accepts a bare type such as "text".
	if kind, subtype, ok := strings.Cut(mediaType, "/"); !ok || kind == "" || subtype == "" {
		return "", false
	}
	normalized := mime.FormatMediaType(strings.ToLower(mediaType), params)

	return normalized, normalized != "" && len(normalized) <= maxMimeTypeLength
}

func uploadError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, storageinfra.ErrFileRejected):
		return kiterrors.InvalidArgument("storage rejected the file",
			kiterrors.WithID("email.attachment.rejected"), kiterrors.WithCause(err))
	case errors.Is(err, storageinfra.ErrUnavailable):
		return kiterrors.Unavailable("storage is unavailable",
			kiterrors.WithID("email.storage.unavailable"), kiterrors.WithCause(err))
	default:
		return kiterrors.Internal("attachment upload failed",
			kiterrors.WithID("email.attachment.upload_failed"), kiterrors.WithCause(err))
	}
}

func errOutboundDisabled() error {
	return kiterrors.New("outbound email is disabled",
		kiterrors.WithID("email.outbound.disabled"), kiterrors.WithCode(codes.FailedPrecondition))
}

func errAttachmentInvalid(message string) error {
	return kiterrors.InvalidArgument(message, kiterrors.WithID("email.attachment.invalid"))
}
