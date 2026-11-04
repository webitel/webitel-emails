package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
)

const (
	defaultMessagePageSize = 20
	maxMessagePageSize     = 100
	messageCursorVersion   = 1
)

// EmailMessageListRequest asks for a page of the messages of one Thread.
type EmailMessageListRequest struct {
	ThreadID int64
	Size     int32
	Cursor   string
	Desc     bool
}

// messageCursor is the opaque page cursor; it carries its order, so it cannot be
// replayed with the other one.
type messageCursor struct {
	Version    int   `json:"v"`
	ReceivedAt int64 `json:"t"`
	ID         int64 `json:"i"`
	Desc       bool  `json:"d"`
}

// receivedCursor is a cursor as decoded: a missing or null field stays nil, so
// it is rejected instead of being read as zero.
type receivedCursor struct {
	Version    *int   `json:"v"`
	ReceivedAt *int64 `json:"t"`
	ID         *int64 `json:"i"`
	Desc       *bool  `json:"d"`
}

// ListMessages returns one page of the ready messages of a Thread the user may
// see. A Thread the user may not see is not found, whatever its messages.
func (s *EmailThreadReadService) ListMessages(
	ctx context.Context,
	domainID, userID int64,
	req EmailMessageListRequest,
) ([]*model.EmailMessageView, string, error) {
	if req.ThreadID <= 0 {
		return nil, "", errThreadIDRequired()
	}
	size := int(req.Size)
	switch {
	case size < 0 || size > maxMessagePageSize:
		return nil, "", errInvalidThreadFilter("size must be between 1 and 100")
	case size == 0:
		size = defaultMessagePageSize
	}
	after, err := decodeMessageCursor(req.Cursor, req.Desc)
	if err != nil {
		return nil, "", err
	}
	if err := s.requireVisible(ctx, domainID, userID, req.ThreadID); err != nil {
		return nil, "", err
	}

	messages, next, err := s.threads.ListMessages(ctx, model.EmailMessageFilter{
		DomainID: domainID,
		UserID:   userID,
		ThreadID: req.ThreadID,
		After:    after,
		Desc:     req.Desc,
		Size:     size,
	})
	if err != nil {
		return nil, "", err
	}
	if err := s.presentPage(ctx, domainID, messages); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if next && len(messages) > 0 {
		last := messages[len(messages)-1]
		nextCursor = encodeMessageCursor(last.ReceivedAt, last.ID, req.Desc)
	}

	return messages, nextCursor, nil
}

// LocateMessage returns one ready message of a Thread the user may see.
func (s *EmailThreadReadService) LocateMessage(
	ctx context.Context,
	domainID, userID, threadID, messageID int64,
) (*model.EmailMessageView, error) {
	if threadID <= 0 {
		return nil, errThreadIDRequired()
	}
	if messageID <= 0 {
		return nil, kiterrors.InvalidArgument("email message id is required", kiterrors.WithID("email.message.id_required"))
	}
	if err := s.requireVisible(ctx, domainID, userID, threadID); err != nil {
		return nil, err
	}

	message, err := s.threads.LocateMessage(ctx, domainID, userID, threadID, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, kiterrors.NotFound("email message not found", kiterrors.WithID("email.message.not_found"))
	}
	if err := s.presentPage(ctx, domainID, []*model.EmailMessageView{message}); err != nil {
		return nil, err
	}

	return message, nil
}

func (s *EmailThreadReadService) requireVisible(ctx context.Context, domainID, userID, threadID int64) error {
	rights, err := s.threads.Rights(ctx, domainID, userID, threadID)
	if err != nil {
		return err
	}
	if !rights.Visible {
		return errThreadNotFound()
	}

	return nil
}

func encodeMessageCursor(receivedAt time.Time, id int64, desc bool) string {
	payload, _ := json.Marshal(messageCursor{
		Version:    messageCursorVersion,
		ReceivedAt: receivedAt.UnixMicro(),
		ID:         id,
		Desc:       desc,
	})

	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeMessageCursor(value string, desc bool) (*model.EmailMessageCursor, error) {
	if value == "" {
		return nil, nil
	}

	invalid := kiterrors.InvalidArgument("invalid cursor", kiterrors.WithID("email.thread.invalid_cursor"))
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, invalid
	}
	var cursor receivedCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return nil, invalid
	}
	if cursor.Version == nil || cursor.ReceivedAt == nil || cursor.ID == nil || cursor.Desc == nil {
		return nil, invalid
	}
	if *cursor.Version != messageCursorVersion || *cursor.ID <= 0 || *cursor.Desc != desc {
		return nil, invalid
	}

	return &model.EmailMessageCursor{ReceivedAt: time.UnixMicro(*cursor.ReceivedAt).UTC(), ID: *cursor.ID}, nil
}
