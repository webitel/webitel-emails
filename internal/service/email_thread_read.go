package service

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const (
	defaultThreadPageSize = 20
	maxThreadPageSize     = 100
	threadPreviewRunes    = 200
)

// EmailThreadListRequest asks for a page of Threads; ListThreads validates it.
type EmailThreadListRequest struct {
	Page       int32
	Size       int32
	Sort       string
	Assignment model.EmailThreadAssignment
	Statuses   []model.EmailThreadStatus
	ProfileIDs []int64
	ContactID  int64
}

// EmailThreadReadService serves the read API of Threads. Every read goes through
// the visibility boundary of the store; nothing here widens it.
type EmailThreadReadService struct {
	threads store.EmailThreadReadStore
	links   AttachmentLinker
	log     *slog.Logger
}

// NewEmailThreadReadService creates the Thread read service. A nil linker
// reports inline images as unavailable.
func NewEmailThreadReadService(
	threads store.EmailThreadReadStore,
	links AttachmentLinker,
	log *slog.Logger,
) *EmailThreadReadService {
	if links == nil {
		links = noAttachmentLinker{}
	}

	return &EmailThreadReadService{threads: threads, links: links, log: log.With("component", "email_thread_read")}
}

// ListThreads returns one page of the user's Threads.
func (s *EmailThreadReadService) ListThreads(
	ctx context.Context,
	domainID, userID int64,
	req EmailThreadListRequest,
) ([]*model.EmailThreadView, bool, error) {
	filter, err := threadFilter(domainID, userID, req)
	if err != nil {
		return nil, false, err
	}

	views, next, err := s.threads.ListThreads(ctx, filter)
	if err != nil {
		return nil, false, err
	}
	for _, view := range views {
		normalizePreview(view)
	}

	return views, next, nil
}

// LocateThread returns one Thread the user may see; any other is not found.
func (s *EmailThreadReadService) LocateThread(
	ctx context.Context,
	domainID, userID, threadID int64,
) (*model.EmailThreadView, error) {
	if threadID <= 0 {
		return nil, errThreadIDRequired()
	}

	view, err := s.threads.LocateThread(ctx, domainID, userID, threadID)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return nil, errThreadNotFound()
	}
	normalizePreview(view)

	return view, nil
}

func threadFilter(domainID, userID int64, req EmailThreadListRequest) (model.EmailThreadFilter, error) {
	filter := model.EmailThreadFilter{
		DomainID:   domainID,
		UserID:     userID,
		Assignment: req.Assignment,
		Statuses:   req.Statuses,
		ProfileIDs: req.ProfileIDs,
		ContactID:  req.ContactID,
		Page:       int(req.Page),
		Size:       int(req.Size),
	}

	switch {
	case filter.Page < 0:
		return filter, errInvalidThreadFilter("page must not be negative")
	case filter.Page == 0:
		filter.Page = 1
	}
	switch {
	case filter.Size < 0 || filter.Size > maxThreadPageSize:
		return filter, errInvalidThreadFilter("size must be between 1 and 100")
	case filter.Size == 0:
		filter.Size = defaultThreadPageSize
	}
	switch filter.Assignment {
	case "":
		filter.Assignment = model.EmailThreadAssignmentAll
	case model.EmailThreadAssignmentCurrent, model.EmailThreadAssignmentHistory, model.EmailThreadAssignmentAll:
	default:
		return filter, errInvalidThreadFilter("unknown assignment")
	}
	for _, status := range filter.Statuses {
		switch status {
		case model.EmailThreadStatusNew, model.EmailThreadStatusDistributed, model.EmailThreadStatusInProgress,
			model.EmailThreadStatusPostponed, model.EmailThreadStatusProcessed:
		default:
			return filter, errInvalidThreadFilter("unknown status")
		}
	}
	for _, id := range filter.ProfileIDs {
		if id <= 0 {
			return filter, errInvalidThreadFilter("profile id must be positive")
		}
	}
	if filter.ContactID < 0 {
		return filter, errInvalidThreadFilter("contact id must not be negative")
	}

	sort, err := ParseThreadSort(req.Sort)
	if err != nil {
		return filter, err
	}
	filter.Sort = sort

	return filter, nil
}

var threadSortFields = map[string]model.EmailThreadSortField{
	string(model.EmailThreadSortLastMessageAt):   model.EmailThreadSortLastMessageAt,
	string(model.EmailThreadSortCreatedAt):       model.EmailThreadSortCreatedAt,
	string(model.EmailThreadSortUpdatedAt):       model.EmailThreadSortUpdatedAt,
	string(model.EmailThreadSortStatusChangedAt): model.EmailThreadSortStatusChangedAt,
}

// ParseThreadSort reads comma-separated fields with an optional "+" or "-"; an
// unknown field is an error, an empty value means "-last_message_at".
func ParseThreadSort(value string) ([]model.EmailThreadSort, error) {
	var sort []model.EmailThreadSort
	seen := map[model.EmailThreadSortField]bool{}
	for _, criterion := range strings.Split(value, ",") {
		criterion = strings.TrimSpace(criterion)
		if criterion == "" {
			continue
		}

		desc := false
		switch criterion[0] {
		case '+':
			criterion = criterion[1:]
		case '-':
			criterion, desc = criterion[1:], true
		}

		field, ok := threadSortFields[strings.TrimSpace(criterion)]
		if !ok || seen[field] {
			return nil, kiterrors.InvalidArgument(
				"unknown or repeated sort field",
				kiterrors.WithID("email.thread.invalid_sort"),
			)
		}
		seen[field] = true
		sort = append(sort, model.EmailThreadSort{Field: field, Desc: desc})
	}
	if len(sort) == 0 {
		sort = []model.EmailThreadSort{{Field: model.EmailThreadSortLastMessageAt, Desc: true}}
	}

	return sort, nil
}

// normalizePreview collapses whitespace and cuts the text to 200 characters on a
// character boundary. An HTML-only email may leave it empty.
func normalizePreview(view *model.EmailThreadView) {
	if view.Preview == nil {
		return
	}

	text := strings.Join(strings.Fields(strings.ToValidUTF8(view.Preview.Text, "")), " ")
	if utf8.RuneCountInString(text) > threadPreviewRunes {
		text = string([]rune(text)[:threadPreviewRunes])
	}
	view.Preview.Text = text
}

func errThreadNotFound() error {
	return kiterrors.NotFound(
		"email thread not found",
		kiterrors.WithID("email.thread.not_found"),
	)
}

func errInvalidThreadFilter(message string) error {
	return kiterrors.InvalidArgument(message, kiterrors.WithID("email.thread.invalid_filter"))
}
