package service

import (
	"context"
	"encoding/base64"
	"encoding/json"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
)

const (
	defaultActionPageSize = 20
	maxActionPageSize     = 100
	actionCursorVersion   = 1
)

// EmailThreadActionListRequest asks for a page of the actions of one Thread.
type EmailThreadActionListRequest struct {
	ThreadID int64
	Size     int32
	Cursor   string
}

// actionCursor is the opaque page cursor: the last projection_version of a page.
type actionCursor struct {
	Version *int   `json:"v"`
	After   *int64 `json:"p"`
}

// ListActions returns one page of the journal of a Thread the user may see, oldest first.
func (s *EmailThreadReadService) ListActions(
	ctx context.Context,
	domainID, userID int64,
	req EmailThreadActionListRequest,
) ([]*model.EmailThreadActionView, string, error) {
	if req.ThreadID <= 0 {
		return nil, "", errThreadIDRequired()
	}
	size := int(req.Size)
	switch {
	case size < 0 || size > maxActionPageSize:
		return nil, "", errInvalidThreadFilter("size must be between 1 and 100")
	case size == 0:
		size = defaultActionPageSize
	}
	after, err := decodeActionCursor(req.Cursor)
	if err != nil {
		return nil, "", err
	}
	if err := s.requireVisible(ctx, domainID, userID, req.ThreadID); err != nil {
		return nil, "", err
	}

	actions, next, err := s.threads.ListActions(ctx, model.EmailThreadActionFilter{
		DomainID: domainID,
		UserID:   userID,
		ThreadID: req.ThreadID,
		After:    after,
		Size:     size,
	})
	if err != nil {
		return nil, "", err
	}

	var nextCursor string
	if next && len(actions) > 0 {
		nextCursor = encodeActionCursor(actions[len(actions)-1].ProjectionVersion)
	}

	return actions, nextCursor, nil
}

func encodeActionCursor(after int64) string {
	version := actionCursorVersion
	payload, _ := json.Marshal(actionCursor{Version: &version, After: &after})

	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeActionCursor(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}

	invalid := kiterrors.InvalidArgument("invalid cursor", kiterrors.WithID("email.thread.invalid_cursor"))
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, invalid
	}
	var cursor actionCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return 0, invalid
	}
	if cursor.Version == nil || cursor.After == nil || *cursor.Version != actionCursorVersion || *cursor.After <= 0 {
		return 0, invalid
	}

	return *cursor.After, nil
}
