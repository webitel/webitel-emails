package service

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

type fakeReadStore struct {
	store.EmailThreadReadStore
	rights   store.EmailThreadRights
	messages []*model.EmailMessageView
	next     bool
	filter   *model.EmailMessageFilter
}

func (f *fakeReadStore) Rights(context.Context, int64, int64, int64) (store.EmailThreadRights, error) {
	return f.rights, nil
}

func (f *fakeReadStore) ListMessages(_ context.Context, filter model.EmailMessageFilter) ([]*model.EmailMessageView, bool, error) {
	f.filter = &filter
	return f.messages, f.next, nil
}

func (f *fakeReadStore) LocateMessage(context.Context, int64, int64, int64, int64) (*model.EmailMessageView, error) {
	if len(f.messages) == 0 {
		return nil, nil
	}
	return f.messages[0], nil
}

func newReadService(s *fakeReadStore) *EmailThreadReadService {
	return NewEmailThreadReadService(s, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestMessageCursorRoundTripAndTampering(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 123456000, time.UTC)
	cursor := encodeMessageCursor(at, 42, true)
	got, err := decodeMessageCursor(cursor, true)
	if err != nil || got == nil || !got.ReceivedAt.Equal(at) || got.ID != 42 {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if got, err := decodeMessageCursor("", false); got != nil || err != nil {
		t.Fatalf("empty cursor: %+v, %v", got, err)
	}

	bad := []struct {
		value string
		desc  bool
	}{
		{cursor, false},
		{"not base64!", true},
		{base64.RawURLEncoding.EncodeToString([]byte("{")), true},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"t":1,"i":1,"d":true}`)), true},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"t":1,"i":0,"d":true}`)), true},
		// Every field is required: a missing or null one is not read as zero.
		{base64.RawURLEncoding.EncodeToString([]byte(`{"t":1,"i":1,"d":false}`)), false},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"i":1,"d":false}`)), false},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"t":1,"d":false}`)), false},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"t":1,"i":1}`)), false},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"t":null,"i":1,"d":false}`)), false},
	}
	for _, c := range bad {
		if _, err := decodeMessageCursor(c.value, c.desc); kiterrors.ID(err) != "email.thread.invalid_cursor" {
			t.Errorf("%q: %v", c.value, err)
		}
	}
}

func TestListMessagesChecksTheThreadAndBuildsTheNextCursor(t *testing.T) {
	invisible := &fakeReadStore{}
	if _, _, err := newReadService(invisible).ListMessages(context.Background(), 1, 70,
		EmailMessageListRequest{ThreadID: 5}); kiterrors.ID(err) != "email.thread.not_found" || invisible.filter != nil {
		t.Fatalf("invisible thread: %v, store asked %v", err, invisible.filter != nil)
	}

	last := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	visible := &fakeReadStore{
		rights:   store.EmailThreadRights{Visible: true},
		messages: []*model.EmailMessageView{{ID: 1, ReceivedAt: last.Add(-time.Hour)}, {ID: 2, ReceivedAt: last}},
		next:     true,
	}
	_, next, err := newReadService(visible).ListMessages(context.Background(), 1, 70,
		EmailMessageListRequest{ThreadID: 5, Desc: true})
	if err != nil || visible.filter.Size != defaultMessagePageSize || !visible.filter.Desc || visible.filter.UserID != 70 {
		t.Fatalf("list: %v, filter %+v", err, visible.filter)
	}
	after, err := decodeMessageCursor(next, true)
	if err != nil || after.ID != 2 || !after.ReceivedAt.Equal(last) {
		t.Fatalf("next cursor %+v, %v", after, err)
	}

	visible.next = false
	if _, next, _ := newReadService(visible).ListMessages(context.Background(), 1, 70,
		EmailMessageListRequest{ThreadID: 5}); next != "" {
		t.Fatalf("last page has a cursor %q", next)
	}

	for _, size := range []int32{-1, maxMessagePageSize + 1} {
		if _, _, err := newReadService(visible).ListMessages(context.Background(), 1, 70,
			EmailMessageListRequest{ThreadID: 5, Size: size}); kiterrors.ID(err) != "email.thread.invalid_filter" {
			t.Errorf("size %d: %v", size, err)
		}
	}
}

func TestPresentDropsUnfinishedPartsAndRawHTML(t *testing.T) {
	fileID := int64(9)
	s := &fakeReadStore{
		rights: store.EmailThreadRights{Visible: true},
		messages: []*model.EmailMessageView{{
			ID:      1,
			RawHTML: "<script>alert(1)</script>",
			Attachments: []model.EmailAttachmentView{
				{ID: 1, State: model.EmailAttachmentStateStored, FileID: &fileID},
				{ID: 2, State: model.EmailAttachmentStatePending},
				{ID: 3, State: model.EmailAttachmentStateSkipped},
			},
		}},
	}
	message, err := newReadService(s).LocateMessage(context.Background(), 1, 70, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if message.RawHTML != "" || len(message.Attachments) != 2 ||
		message.Attachments[0].ID != 1 || message.Attachments[1].ID != 3 {
		t.Fatalf("presented %+v", message)
	}

	if _, err := newReadService(&fakeReadStore{rights: store.EmailThreadRights{Visible: true}}).
		LocateMessage(context.Background(), 1, 70, 5, 1); kiterrors.ID(err) != "email.message.not_found" {
		t.Fatalf("missing message: %v", err)
	}
	if _, err := newReadService(&fakeReadStore{}).LocateMessage(context.Background(), 1, 70, 5, 1); kiterrors.ID(err) != "email.thread.not_found" {
		t.Fatalf("invisible thread: %v", err)
	}
}
