package service

import (
	"context"
	"errors"
	"testing"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type fakeAttachmentStore struct {
	fakeReadStore
	part  *model.EmailAttachmentView
	asked bool
}

func (f *fakeAttachmentStore) LocateAttachment(context.Context, int64, int64, int64, int64, int64) (*model.EmailAttachmentView, error) {
	f.asked = true
	return f.part, nil
}

type fakeSingleLinker struct {
	noAttachmentLinker
	link      string
	expiresAt time.Time
	err       error
	fileID    int64
}

func (f *fakeSingleLinker) Link(_ context.Context, _ int64, fileID int64) (string, time.Time, error) {
	f.fileID = fileID
	return f.link, f.expiresAt, f.err
}

func TestAttachmentLink(t *testing.T) {
	fileID := int64(777)
	stored := &model.EmailAttachmentView{ID: 3, FileID: &fileID, State: model.EmailAttachmentStateStored}
	skipped := &model.EmailAttachmentView{ID: 3, State: model.EmailAttachmentStateSkipped}
	pending := &model.EmailAttachmentView{ID: 3, State: model.EmailAttachmentStatePending}
	visible := store.EmailThreadRights{Visible: true}
	expires := time.Date(2026, 10, 20, 9, 5, 0, 0, time.UTC)

	cases := []struct {
		name    string
		ids     [3]int64
		rights  store.EmailThreadRights
		part    *model.EmailAttachmentView
		linkErr error
		wantID  string
		wantErr error
	}{
		{name: "missing thread id", ids: [3]int64{0, 2, 3}, rights: visible, wantID: "email.thread.id_required"},
		{name: "missing message id", ids: [3]int64{1, 0, 3}, rights: visible, wantID: "email.message.id_required"},
		{name: "missing attachment id", ids: [3]int64{1, 2, 0}, rights: visible, wantID: "email.attachment.id_required"},
		{name: "invisible thread", ids: [3]int64{1, 2, 3}, part: stored, wantID: "email.thread.not_found"},
		{name: "no such attachment", ids: [3]int64{1, 2, 3}, rights: visible, wantID: "email.attachment.not_found"},
		{name: "skipped part", ids: [3]int64{1, 2, 3}, rights: visible, part: skipped, wantID: "email.attachment.not_stored"},
		{name: "unfinished part", ids: [3]int64{1, 2, 3}, rights: visible, part: pending, wantID: "email.attachment.no_file"},
		{name: "storage unavailable", ids: [3]int64{1, 2, 3}, rights: visible, part: stored,
			linkErr: ErrFileLinksUnavailable, wantID: "email.storage.unavailable"},
		{name: "caller cancelled", ids: [3]int64{1, 2, 3}, rights: visible, part: stored,
			linkErr: context.Canceled, wantErr: context.Canceled},
		{name: "storage refused", ids: [3]int64{1, 2, 3}, rights: visible, part: stored,
			linkErr: errors.New("refused"), wantID: "email.storage.link_failed"},
		{name: "link", ids: [3]int64{1, 2, 3}, rights: visible, part: stored},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &fakeAttachmentStore{fakeReadStore: fakeReadStore{rights: c.rights}, part: c.part}
			linker := &fakeSingleLinker{link: "https://storage.example/f/777", expiresAt: expires, err: c.linkErr}
			svc := newReadService(&s.fakeReadStore)
			svc.threads, svc.links = s, linker

			link, at, err := svc.AttachmentLink(context.Background(), 1, 70, c.ids[0], c.ids[1], c.ids[2])
			switch {
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("error %v, want %v", err, c.wantErr)
				}
			case c.wantID != "":
				if kiterrors.ID(err) != c.wantID {
					t.Fatalf("error %v, want %s", err, c.wantID)
				}
				if c.name == "invisible thread" && s.asked {
					t.Fatal("the attachment was read for an invisible thread")
				}
			default:
				if err != nil || link != "https://storage.example/f/777" || !at.Equal(expires) || linker.fileID != 777 {
					t.Fatalf("link %q at %s for file %d: %v", link, at, linker.fileID, err)
				}
			}
		})
	}
}
