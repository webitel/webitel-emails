package inbound

import (
	"context"
	"testing"

	"github.com/webitel/webitel-emails/internal/model"
)

// TestManifestKeepsPartClassification checks that persistence stores the parts
// exactly as the MIME parser classified them, so the read API can replace cid:
// references without parsing the email again.
func TestManifestKeepsPartClassification(t *testing.T) {
	const html = `<p>text <img src="cid:logo@x"></p>`

	email := testEmail("<m1@x>", false, "raw-1", 10, 5)
	email.HTMLBody = html
	email.Parts = []model.EmailPart{
		{Name: "logo.png", ContentType: "image/png", ContentID: "logo@x",
			Disposition: model.EmailPartDispositionInline, Size: 5, Content: []byte("image")},
		// An image the HTML never references; the parser already downgraded it.
		{Name: "unused.png", ContentType: "image/png", ContentID: "unused@x",
			Disposition: model.EmailPartDispositionAttachment, Size: 5, Content: []byte("other")},
		{Name: "report.txt", ContentType: "text/plain",
			Disposition: model.EmailPartDispositionAttachment, Size: 6, Content: []byte("report")},
		// Skipped inline part: it keeps its Content-ID but is never uploaded.
		{Name: "big.png", ContentType: "image/png", ContentID: "big@x",
			Disposition: model.EmailPartDispositionInline, Size: 1 << 30,
			SkippedReason: model.EmailPartSkippedSizeLimit},
	}

	fake := newFakeStore()
	files := &fakeUploader{}
	if err := testUploadingHandler(fake, files).Handle(context.Background(), email); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	message := fake.messages[0]
	if message.HTMLBody != html {
		t.Fatalf("stored HTML was rewritten:\n got %q\nwant %q", message.HTMLBody, html)
	}

	manifest := fake.attachments[message.ID]
	if len(manifest) != 4 {
		t.Fatalf("manifest rows = %d, want 4", len(manifest))
	}

	want := []struct {
		position    int32
		disposition model.EmailPartDisposition
		contentID   string
		state       model.EmailAttachmentState
	}{
		{0, model.EmailPartDispositionInline, "logo@x", model.EmailAttachmentStateStored},
		{1, model.EmailPartDispositionAttachment, "unused@x", model.EmailAttachmentStateStored},
		{2, model.EmailPartDispositionAttachment, "", model.EmailAttachmentStateStored},
		{3, model.EmailPartDispositionInline, "big@x", model.EmailAttachmentStateSkipped},
	}
	for index, expected := range want {
		part := manifest[index]
		if part.Position != expected.position || part.Disposition != expected.disposition ||
			part.ContentID != expected.contentID || part.State != expected.state {
			t.Errorf("manifest[%d] = %+v, want position=%d disposition=%s content_id=%q state=%s",
				index, part, expected.position, expected.disposition, expected.contentID, expected.state)
		}
	}

	if manifest[3].SkippedReason != model.EmailPartSkippedSizeLimit || manifest[3].FileID != nil {
		t.Errorf("skipped part = %+v, want the parser reason and no file", manifest[3])
	}

	// The skipped part is the only one not uploaded, and order is preserved.
	if len(files.requests) != 3 {
		t.Fatalf("uploads = %d, want 3", len(files.requests))
	}
	for index, request := range files.requests {
		if request.Name != email.Parts[index].Name {
			t.Errorf("upload %d name = %q, want %q", index, request.Name, email.Parts[index].Name)
		}
	}
}
