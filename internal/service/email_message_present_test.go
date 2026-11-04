package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type fakeLinker struct {
	calls   int
	domain  int64
	fileIDs []int64
	err     error
}

func (f *fakeLinker) Link(context.Context, int64, int64) (string, time.Time, error) {
	return "", time.Time{}, f.err
}

func (f *fakeLinker) InlineLinks(_ context.Context, domainID int64, fileIDs []int64) (map[int64]string, error) {
	f.calls++
	f.domain, f.fileIDs = domainID, fileIDs
	if f.err != nil {
		return nil, f.err
	}
	links := map[int64]string{}
	for _, id := range fileIDs {
		links[id] = "https://storage.example/f" + string(rune('0'+id))
	}
	return links, nil
}

func inline(id, fileID int64, contentID, mime string) model.EmailAttachmentView {
	return model.EmailAttachmentView{
		ID: id, FileID: &fileID, ContentID: contentID, MimeType: mime,
		Disposition: model.EmailPartDispositionInline, State: model.EmailAttachmentStateStored,
	}
}

func presentWith(t *testing.T, linker AttachmentLinker, messages ...*model.EmailMessageView) error {
	t.Helper()
	s := NewEmailThreadReadService(&fakeReadStore{rights: store.EmailThreadRights{Visible: true}}, linker,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s.presentPage(context.Background(), 7, messages)
}

func TestPresentLinksInlineImagesOnceForThePage(t *testing.T) {
	first := &model.EmailMessageView{ID: 1,
		RawHTML:     `<p>hi</p><img src="cid:logo@x" alt="logo"><img src="https://tracker.example/p.gif">`,
		Attachments: []model.EmailAttachmentView{inline(1, 1, "logo@x", "image/PNG")}}
	second := &model.EmailMessageView{ID: 2,
		RawHTML:     `<img src="cid:logo%40x"><img src="cid:photo@x">`,
		Attachments: []model.EmailAttachmentView{inline(2, 1, "logo@x", "image/png"), inline(3, 2, "photo@x", "image/jpeg")}}
	linker := &fakeLinker{}
	if err := presentWith(t, linker, first, second); err != nil {
		t.Fatal(err)
	}

	if linker.calls != 1 || linker.domain != 7 || len(linker.fileIDs) != 2 {
		t.Fatalf("linker asked %d times for %v", linker.calls, linker.fileIDs)
	}
	if first.InlineContentStatus != model.EmailInlineContentOK || !first.RemoteContentBlocked ||
		!strings.Contains(first.SanitizedHTML, `src="https://storage.example/f1"`) ||
		strings.Contains(first.SanitizedHTML, "tracker.example") || first.RawHTML != "" {
		t.Fatalf("first message %+v", first)
	}
	if second.InlineContentStatus != model.EmailInlineContentOK ||
		!strings.Contains(second.SanitizedHTML, "f1") || !strings.Contains(second.SanitizedHTML, "f2") {
		t.Fatalf("second message %+v", second)
	}
}

func TestPresentLinksOnlyOneStoredRasterImage(t *testing.T) {
	svg := &model.EmailMessageView{ID: 1, RawHTML: `<img src="cid:v@x" alt="vector">`,
		Attachments: []model.EmailAttachmentView{inline(1, 1, "v@x", "image/svg+xml")}}
	duplicate := &model.EmailMessageView{ID: 2, RawHTML: `<img src="cid:d@x">`,
		Attachments: []model.EmailAttachmentView{inline(2, 2, "d@x", "image/png"), inline(3, 3, "d@x", "image/png")}}
	attachment := inline(4, 4, "a@x", "image/png")
	attachment.Disposition = model.EmailPartDispositionAttachment
	notInline := &model.EmailMessageView{ID: 3, RawHTML: `<img src="cid:a@x">`, Attachments: []model.EmailAttachmentView{attachment}}

	linker := &fakeLinker{}
	if err := presentWith(t, linker, svg, duplicate, notInline); err != nil {
		t.Fatal(err)
	}
	if linker.calls != 0 {
		t.Fatalf("linker asked for %v", linker.fileIDs)
	}
	for _, m := range []*model.EmailMessageView{svg, duplicate, notInline} {
		if m.InlineContentStatus != model.EmailInlineContentNone || strings.Contains(m.SanitizedHTML, "src=") {
			t.Errorf("message %d: %s, %s", m.ID, m.InlineContentStatus, m.SanitizedHTML)
		}
	}
	if !strings.Contains(svg.SanitizedHTML, `alt="vector"`) {
		t.Errorf("alternative text lost: %s", svg.SanitizedHTML)
	}
}

func TestPresentDegradesOnlyWhenStorageIsUnavailable(t *testing.T) {
	message := func() *model.EmailMessageView {
		return &model.EmailMessageView{ID: 1, BodyText: "text", RawHTML: `<p>body</p><img src="cid:logo@x">`,
			Attachments: []model.EmailAttachmentView{inline(1, 1, "logo@x", "image/png")}}
	}

	degraded := message()
	if err := presentWith(t, &fakeLinker{err: ErrFileLinksUnavailable}, degraded); err != nil {
		t.Fatalf("unavailable storage failed the request: %v", err)
	}
	if degraded.InlineContentStatus != model.EmailInlineContentUnavailable || degraded.BodyText != "text" ||
		!strings.Contains(degraded.SanitizedHTML, "<p>body</p>") || strings.Contains(degraded.SanitizedHTML, "cid:") ||
		len(degraded.Attachments) != 1 {
		t.Fatalf("degraded message %+v", degraded)
	}

	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("storage refused the request")} {
		if got := presentWith(t, &fakeLinker{err: err}, message()); !errors.Is(got, err) {
			t.Errorf("%v was masked: %v", err, got)
		}
	}
}

func TestPresentWithoutHTML(t *testing.T) {
	message := &model.EmailMessageView{ID: 1, BodyText: "plain"}
	if err := presentWith(t, nil, message); err != nil {
		t.Fatal(err)
	}
	if message.SanitizedHTML != "" || message.InlineContentStatus != model.EmailInlineContentNone {
		t.Fatalf("plain message %+v", message)
	}
}
