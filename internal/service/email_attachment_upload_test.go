package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/config"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/model"
)

type fakeUploader struct {
	req     storageinfra.UploadRequest
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *fakeUploader) Upload(ctx context.Context, req storageinfra.UploadRequest) (storageinfra.UploadResult, error) {
	f.req = req
	if f.started != nil {
		f.started <- struct{}{}
		<-f.release
	}
	if f.err != nil {
		return storageinfra.UploadResult{}, f.err
	}

	return storageinfra.UploadResult{FileID: 501, Size: int64(len(req.Content))}, nil
}

type fakeUploadStore struct {
	created *model.EmailAttachmentUpload
	err     error
}

func (f *fakeUploadStore) Create(_ context.Context, u *model.EmailAttachmentUpload) error {
	f.created = u
	return f.err
}

func (f *fakeUploadStore) DeleteExpiredUnused(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func newUploadService(files AttachmentUploader, uploads *fakeUploadStore, enabled bool) *EmailAttachmentUploadService {
	cfg := &config.Config{Outbound: config.OutboundConfig{
		Enabled: enabled, MaxAttachmentSize: 8, MaxConcurrentUploads: 1, AttachmentReferenceTTL: time.Hour,
	}}
	s := NewEmailAttachmentUploadService(cfg, files, uploads)
	s.now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }

	return s
}

func uploadSession(access string) *auth.Session {
	return &auth.Session{UserID: 70, DomainID: 7,
		Scopes: map[string]auth.Scope{"email_thread": {Access: access, OBAC: true}}}
}

func TestUploadAttachmentStoresFileAndReference(t *testing.T) {
	files, uploads := &fakeUploader{}, &fakeUploadStore{}
	s := newUploadService(files, uploads, true)

	for _, access := range []string{"rx", "rw"} {
		got, err := s.Upload(context.Background(), uploadSession(access),
			UploadAttachmentInput{FileName: "  re/port.pdf", Content: []byte("pdf")})
		if err != nil {
			t.Fatalf("access %q: %v", access, err)
		}
		want := model.EmailAttachmentUpload{
			DomainID: 7, Reference: got.Reference, AuthorUserID: 70, FileID: 501,
			FileName: "re_port.pdf", MimeType: "application/octet-stream", Size: 3,
			ExpiresAt: time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC),
		}
		if *got != want || uploads.created != got {
			t.Fatalf("upload = %+v, want %+v", *got, want)
		}
		if files.req.ReferenceID != got.Reference.String() || files.req.UploadedBy != 70 || files.req.DomainID != 7 {
			t.Fatalf("storage request = %+v", files.req)
		}
	}
}

func TestUploadAttachmentRefusals(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		access  string
		in      UploadAttachmentInput
		want    string
	}{
		{"disabled", false, "rw", UploadAttachmentInput{FileName: "a", Content: []byte("x")}, "email.outbound.disabled"},
		{"read only", true, "r", UploadAttachmentInput{FileName: "a", Content: []byte("x")}, "auth.interceptor.permission"},
		{"no name", true, "rw", UploadAttachmentInput{FileName: " ", Content: []byte("x")}, "email.attachment.invalid"},
		{"bad mime", true, "rw", UploadAttachmentInput{FileName: "a", MimeType: "text", Content: []byte("x")}, "email.attachment.invalid"},
		{"empty", true, "rw", UploadAttachmentInput{FileName: "a"}, "email.attachment.invalid"},
		{"too large", true, "rw", UploadAttachmentInput{FileName: "a", Content: []byte("123456789")}, "email.attachment.too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := &fakeUploader{}
			_, err := newUploadService(files, &fakeUploadStore{}, tt.enabled).Upload(context.Background(), uploadSession(tt.access), tt.in)
			if kiterrors.ID(err) != tt.want || files.req.Content != nil {
				t.Fatalf("error = %v, want %s without an upload", err, tt.want)
			}
		})
	}
}

func TestUploadAttachmentMimeType(t *testing.T) {
	uploads := &fakeUploadStore{}
	got, err := newUploadService(&fakeUploader{}, uploads, true).Upload(context.Background(), uploadSession("rw"),
		UploadAttachmentInput{FileName: "a", MimeType: "Text/Plain; Charset=UTF-8", Content: []byte("x")})
	if err != nil || got.MimeType != "text/plain; charset=UTF-8" {
		t.Fatalf("mime = %q, %v", got.MimeType, err)
	}
}

func TestUploadAttachmentBusyAndErrors(t *testing.T) {
	t.Run("a full set of slots refuses instead of queueing", func(t *testing.T) {
		files := &fakeUploader{started: make(chan struct{}), release: make(chan struct{})}
		s := newUploadService(files, &fakeUploadStore{}, true)
		in := UploadAttachmentInput{FileName: "a", Content: []byte("x")}
		done := make(chan error)
		go func() { _, err := s.Upload(context.Background(), uploadSession("rw"), in); done <- err }()
		<-files.started

		if _, err := s.Upload(context.Background(), uploadSession("rw"), in); kiterrors.ID(err) != "email.attachment.upload_busy" {
			t.Fatalf("second upload = %v", err)
		}
		close(files.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		files.started = nil
		if _, err := s.Upload(context.Background(), uploadSession("rw"), in); err != nil {
			t.Fatalf("slot not released: %v", err)
		}
	})

	tests := []struct {
		name     string
		storage  error
		store    error
		canceled bool
		want     string
	}{
		{"policy refusal", fmt.Errorf("%w: policy", storageinfra.ErrFileRejected), nil, false, "email.attachment.rejected"},
		{"storage down", fmt.Errorf("%w: down", storageinfra.ErrUnavailable), nil, false, "email.storage.unavailable"},
		{"canceled request", errors.New("stream closed"), nil, true, ""},
		{"reference not saved", nil, errors.New("db down"), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tt.canceled {
				cancel()
			}
			defer cancel()
			_, err := newUploadService(&fakeUploader{err: tt.storage}, &fakeUploadStore{err: tt.store}, true).
				Upload(ctx, uploadSession("rw"), UploadAttachmentInput{FileName: "a", Content: []byte("x")})
			switch {
			case tt.canceled && !errors.Is(err, context.Canceled):
				t.Fatalf("canceled request error = %v", err)
			case tt.store != nil && !errors.Is(err, tt.store):
				t.Fatalf("store error = %v", err)
			case tt.want != "" && kiterrors.ID(err) != tt.want:
				t.Fatalf("error = %v, want %s", err, tt.want)
			}
		})
	}
}
