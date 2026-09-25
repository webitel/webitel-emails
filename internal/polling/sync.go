package polling

import (
	"context"
	"errors"
	"log/slog"
	"math"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
)

// syncResult is the outcome of reading one mailbox; cursor holds only confirmed progress.
type syncResult struct {
	cursor     *model.ProviderCursor
	more       bool
	imapErr    error
	handlerErr error
}

// sync hands new messages to the handler in UID order and advances the cursor past confirmed ones.
func (s *Scheduler) sync(
	ctx context.Context,
	conn mailinfra.IMAPSession,
	profile *model.EmailProfile,
	stored *model.ProviderCursor,
	log *slog.Logger,
) syncResult {
	mailbox, err := conn.Examine(profile.Mailbox)
	if err != nil {
		return syncResult{imapErr: err}
	}

	var cursor model.IMAPCursor
	switch {
	case stored.IsEmpty() || stored.IMAP.Mailbox != mailbox.Name:
		// Existing messages are not imported: the first cursor starts at the current end of the mailbox.
		last, err := lastUID(conn, mailbox)
		if err != nil {
			return syncResult{imapErr: err}
		}

		log.Info("imap cursor initialized", "mailbox", mailbox.Name, "uid_validity", mailbox.UIDValidity, "last_uid", last)

		return syncResult{cursor: imapCursor(model.IMAPCursor{Mailbox: mailbox.Name, UIDValidity: mailbox.UIDValidity, LastUID: last})}
	case stored.IMAP.UIDValidity != mailbox.UIDValidity:
		log.Warn("imap uidvalidity changed, resyncing mailbox", "mailbox", mailbox.Name, "old", stored.IMAP.UIDValidity, "new", mailbox.UIDValidity)
		cursor = model.IMAPCursor{Mailbox: mailbox.Name, UIDValidity: mailbox.UIDValidity}
	default:
		cursor = *stored.IMAP
	}

	uids, err := searchNewUIDs(ctx, conn, cursor.LastUID, mailbox.UIDNext, s.cfg.MaxMessagesPerPoll)
	if err != nil {
		return syncResult{cursor: imapCursor(cursor), imapErr: err}
	}

	if s.handler == nil {
		if len(uids) > 0 {
			log.Info("new emails are waiting for a handler", "mailbox", mailbox.Name, "count", len(uids))
		}

		return syncResult{cursor: imapCursor(cursor)}
	}

	more := len(uids) > s.cfg.MaxMessagesPerPoll
	if more {
		uids = uids[:s.cfg.MaxMessagesPerPoll]
	}

	for len(uids) > 0 {
		batch := uids[:min(s.cfg.FetchBatchSize, len(uids))]
		uids = uids[len(batch):]

		handled := make(map[uint32]struct{}, len(batch))
		var (
			handlerErr error
			maxHandled uint32
		)
		err := conn.FetchRaw(batch, func(message *mailinfra.IMAPMessage) {
			if handlerErr != nil {
				return
			}
			if err := ctx.Err(); err != nil {
				handlerErr = err

				return
			}

			handlerErr = s.handler.Handle(ctx, &inbound.Message{
				DomainID:     profile.DomainID,
				ProfileID:    profile.ID,
				Mailbox:      mailbox.Name,
				UIDValidity:  mailbox.UIDValidity,
				UID:          message.UID,
				InternalDate: message.InternalDate,
				Raw:          message.Raw,
			})
			if handlerErr == nil {
				handled[message.UID] = struct{}{}
				maxHandled = max(maxHandled, message.UID)
			}
		})
		if err != nil {
			advanceConfirmed(&cursor, batch, handled)
			if errors.Is(err, mailinfra.ErrMessageTooLarge) {
				return syncResult{cursor: imapCursor(cursor), handlerErr: err}
			}

			return syncResult{cursor: imapCursor(cursor), imapErr: err}
		}
		if handlerErr != nil {
			advanceConfirmed(&cursor, batch, handled)

			return syncResult{cursor: imapCursor(cursor), handlerErr: handlerErr}
		}
		if maxHandled > cursor.LastUID {
			cursor.LastUID = maxHandled
		}
	}

	return syncResult{cursor: imapCursor(cursor), more: more}
}

func advanceConfirmed(cursor *model.IMAPCursor, requested []uint32, handled map[uint32]struct{}) {
	for _, uid := range requested {
		if _, ok := handled[uid]; !ok {
			return
		}

		cursor.LastUID = uid
	}
}

// searchNewUIDs collects UIDs after "after" in bounded windows, so a large backlog never lands
// in memory at once. A sparse window is not a stop condition: only limit+1, uidNext or ctx is.
func searchNewUIDs(ctx context.Context, conn mailinfra.IMAPSession, after, uidNext uint32, limit int) ([]uint32, error) {
	if after == math.MaxUint32 {
		return nil, nil
	}

	if uidNext == 0 {
		return conn.SearchUIDsRange(after+1, 0)
	}

	const window = 10_000

	var all []uint32
	start := after + 1

	for start < uidNext {
		end := start + window - 1
		if end < start || end >= uidNext {
			end = uidNext - 1
		}

		found, err := conn.SearchUIDsRange(start, end)
		if err != nil {
			return nil, err
		}

		all = append(all, found...)

		if limit > 0 && len(all) > limit {
			return all[:limit+1], nil
		}
		if end+1 <= start { // end sits at the top of uint32; nothing more to search
			break
		}

		start = end + 1

		select {
		case <-ctx.Done():
			return all, ctx.Err()
		default:
		}
	}

	return all, nil
}

// lastUID returns the highest existing UID, searching when the server omitted UIDNEXT.
func lastUID(conn mailinfra.IMAPSession, mailbox *mailinfra.IMAPMailbox) (uint32, error) {
	if mailbox.UIDNext > 0 {
		return mailbox.UIDNext - 1, nil
	}

	uids, err := conn.SearchUIDsRange(1, 0)
	if err != nil || len(uids) == 0 {
		return 0, err
	}

	return uids[len(uids)-1], nil
}

func imapCursor(cursor model.IMAPCursor) *model.ProviderCursor {
	return &model.ProviderCursor{IMAP: &cursor}
}
