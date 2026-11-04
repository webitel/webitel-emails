package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/webitel/webitel-emails/internal/emailhtml"
	"github.com/webitel/webitel-emails/internal/model"
)

// ErrFileLinksUnavailable means storage issued no links in time: messages lose their
// inline images and an attachment link fails as unavailable.
var ErrFileLinksUnavailable = errors.New("file links are unavailable")

// AttachmentLinker issues short-lived links to stored files. It is asked only
// for files of messages already read through the visibility boundary.
type AttachmentLinker interface {
	// InlineLinks returns a link for each file of a page of messages.
	InlineLinks(ctx context.Context, domainID int64, fileIDs []int64) (map[int64]string, error)
	// Link returns a link to one file and the time it stops working at the latest.
	Link(ctx context.Context, domainID, fileID int64) (string, time.Time, error)
}

// noAttachmentLinker stands in when no linker is given: every link is
// unavailable.
type noAttachmentLinker struct{}

func (noAttachmentLinker) InlineLinks(context.Context, int64, []int64) (map[int64]string, error) {
	return nil, ErrFileLinksUnavailable
}

func (noAttachmentLinker) Link(context.Context, int64, int64) (string, time.Time, error) {
	return "", time.Time{}, ErrFileLinksUnavailable
}

// Only raster images are linked inline; an SVG or any other type stays a plain
// attachment the user downloads.
var inlineImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// presentedBody is a sanitized body waiting for its inline links.
type presentedBody struct {
	message  *model.EmailMessageView
	document *emailhtml.Document
	// files maps a "cid:" reference to the one part it resolves to.
	files map[string]int64
}

// presentPage drops unfinished parts, sanitizes the HTML and links the page's inline
// images with one request; the raw HTML is always cleared.
func (s *EmailThreadReadService) presentPage(
	ctx context.Context,
	domainID int64,
	messages []*model.EmailMessageView,
) error {
	bodies := make([]presentedBody, 0, len(messages))
	var fileIDs []int64
	requested := map[int64]bool{}
	removed, remote := 0, 0

	for _, message := range messages {
		s.dropUnfinishedParts(message)
		raw := message.RawHTML
		message.RawHTML = ""
		message.InlineContentStatus = model.EmailInlineContentNone
		if strings.TrimSpace(raw) == "" {
			continue
		}

		document, err := emailhtml.Sanitize(raw)
		if err != nil {
			return err
		}
		message.RemoteContentBlocked = document.RemoteContentBlocked
		removed += document.RemovedElements
		remote += document.RemoteReferences

		body := presentedBody{message: message, document: document, files: map[string]int64{}}
		for _, ref := range document.ContentIDReferences() {
			fileID, ok := inlineFile(message.Attachments, ref)
			if !ok {
				continue
			}
			body.files[ref] = fileID
			if !requested[fileID] {
				requested[fileID] = true
				fileIDs = append(fileIDs, fileID)
			}
		}
		bodies = append(bodies, body)
	}
	// Aggregates only: no HTML, URL or Content-ID is logged.
	if len(bodies) > 0 {
		s.log.Debug("email bodies sanitized",
			"messages", len(bodies), "removed_elements", removed, "remote_blocked", remote)
	}

	links := map[int64]string{}
	unavailable := false
	if len(fileIDs) > 0 {
		issued, err := s.links.InlineLinks(ctx, domainID, fileIDs)
		switch {
		case errors.Is(err, ErrFileLinksUnavailable):
			unavailable = true
		case err != nil:
			return err
		default:
			links = issued
		}
	}

	for _, body := range bodies {
		if len(body.files) > 0 {
			body.message.InlineContentStatus = model.EmailInlineContentOK
		}
		rendered, err := body.document.Render(func(ref string) (string, bool) {
			fileID, ok := body.files[ref]
			if !ok {
				return "", false
			}
			link, ok := links[fileID]
			if !ok {
				body.message.InlineContentStatus = model.EmailInlineContentUnavailable
			}
			return link, ok
		})
		if err != nil {
			return err
		}
		if unavailable && len(body.files) > 0 {
			body.message.InlineContentStatus = model.EmailInlineContentUnavailable
		}
		body.message.SanitizedHTML = rendered
	}

	return nil
}

// inlineFile finds the one stored inline raster image of the message that a
// reference names. No match or several matches leave the reference unresolved.
func inlineFile(parts []model.EmailAttachmentView, ref string) (int64, bool) {
	var found *int64
	for _, part := range parts {
		if part.Disposition != model.EmailPartDispositionInline || part.State != model.EmailAttachmentStateStored ||
			part.FileID == nil || !inlineImageTypes[strings.ToLower(part.MimeType)] ||
			!emailhtml.MatchesContentID(ref, part.ContentID) {
			continue
		}
		if found != nil {
			return 0, false
		}
		found = part.FileID
	}
	if found == nil {
		return 0, false
	}

	return *found, true
}

// dropUnfinishedParts removes parts a ready message must not have; one found
// anyway is a data error, logged without content.
func (s *EmailThreadReadService) dropUnfinishedParts(message *model.EmailMessageView) {
	parts := message.Attachments[:0]
	for _, part := range message.Attachments {
		if part.State != model.EmailAttachmentStateStored && part.State != model.EmailAttachmentStateSkipped {
			s.log.Warn("ready email message has an unfinished part",
				"message_id", message.ID, "attachment_id", part.ID, "state", part.State)
			continue
		}
		parts = append(parts, part)
	}
	message.Attachments = parts
}
