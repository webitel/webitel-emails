package polling

import (
	"context"
	"log/slog"
	"math"
	"time"

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
		last, err := lastUID(conn, mailbox)
		if err != nil {
			return syncResult{imapErr: err}
		}

		checkpoint := recoveryCheckpoint(*stored.IMAP)
		if checkpoint.IsZero() {
			// A cursor written before recovery existed carries no checkpoint, so
			// the newest stored email is the next best place to resume from.
			checkpoint, err = s.messages.LastReceivedAt(ctx, profile.DomainID, profile.ID)
			if err != nil {
				return syncResult{handlerErr: err}
			}
		}

		cursor = startRecovery(*stored.IMAP, mailbox, last, checkpoint, log)
		if cursor.Recovery == nil {
			return syncResult{cursor: imapCursor(cursor)}
		}

		return s.recover(ctx, conn, profile, cursor, log)
	case stored.IMAP.Recovery != nil:
		return s.recover(ctx, conn, profile, *stored.IMAP, log)
	default:
		cursor = *stored.IMAP
	}

	uids, err := searchUIDs(ctx, conn, searchRange{
		after: cursor.LastUID,
		to:    lastSearchableUID(mailbox),
		limit: s.cfg.MaxMessagesPerPoll,
	})
	if err != nil {
		return syncResult{cursor: imapCursor(cursor), imapErr: err}
	}

	more := len(uids) > s.cfg.MaxMessagesPerPoll
	if more {
		uids = uids[:s.cfg.MaxMessagesPerPoll]
	}

	handled := make(map[uint32]time.Time, len(uids))
	imapErr, handlerErr := s.deliver(ctx, conn, profile, cursor, uids, handled)
	advanceConfirmed(&cursor, uids, handled)

	return syncResult{cursor: imapCursor(cursor), more: more, imapErr: imapErr, handlerErr: handlerErr}
}

// deliver walks uids in fetch-sized batches and hands each message to the
// handler, recording every confirmed one. It stops at the first failure, so the
// caller can advance the cursor only over the confirmed prefix.
func (s *Scheduler) deliver(
	ctx context.Context,
	conn mailinfra.IMAPSession,
	profile *model.EmailProfile,
	cursor model.IMAPCursor,
	uids []uint32,
	handled map[uint32]time.Time,
) (imapErr, handlerErr error) {
	for len(uids) > 0 {
		batch := uids[:min(s.cfg.FetchBatchSize, len(uids))]
		uids = uids[len(batch):]

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
				Mailbox:      cursor.Mailbox,
				UIDValidity:  cursor.UIDValidity,
				UID:          message.UID,
				InternalDate: message.InternalDate,
				Size:         int64(message.Size),
				Raw:          message.Raw,
				TooLarge:     message.TooLarge,
			})
			if handlerErr == nil {
				handled[message.UID] = message.InternalDate
			}
		})
		if err != nil {
			return err, handlerErr
		}
		if handlerErr != nil {
			return nil, handlerErr
		}
	}

	return nil, nil
}

// advanceConfirmed moves the cursor over the confirmed prefix only, so an email
// that failed is read again. The checkpoint follows the same prefix.
func advanceConfirmed(cursor *model.IMAPCursor, requested []uint32, handled map[uint32]time.Time) {
	for _, uid := range requested {
		date, ok := handled[uid]
		if !ok {
			return
		}

		cursor.LastUID = uid
		if date.After(cursor.LastInternalDate) {
			cursor.LastInternalDate = date
		}
	}
}

// searchRange describes what to look for: UIDs after "after", up to and
// including "to" (zero means unbounded), optionally limited by INTERNALDATE.
type searchRange struct {
	after uint32
	to    uint32
	since time.Time
	limit int
}

// searchUIDs collects matching UIDs in bounded windows, so neither a large
// backlog nor a wide recovery range ever lands in memory at once. A sparse
// window is not a stop condition: only limit+1, the upper bound or ctx is.
func searchUIDs(ctx context.Context, conn mailinfra.IMAPSession, window searchRange) ([]uint32, error) {
	if window.after == math.MaxUint32 {
		return nil, nil
	}

	start := window.after + 1
	if window.to == 0 {
		return conn.SearchUIDsRange(start, 0, window.since)
	}

	const size = 10_000

	var all []uint32
	for start <= window.to {
		end := start + size - 1
		if end < start || end > window.to {
			end = window.to
		}

		found, err := conn.SearchUIDsRange(start, end, window.since)
		if err != nil {
			return nil, err
		}

		all = append(all, found...)

		if window.limit > 0 && len(all) > window.limit {
			return all[:window.limit+1], nil
		}
		if end == math.MaxUint32 { // nothing above the top of uint32
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

// lastSearchableUID is the highest UID worth searching, or zero when the server
// did not report UIDNEXT and the search must stay unbounded.
func lastSearchableUID(mailbox *mailinfra.IMAPMailbox) uint32 {
	if mailbox.UIDNext == 0 {
		return 0
	}

	return mailbox.UIDNext - 1
}

// lastUID returns the highest existing UID, searching when the server omitted UIDNEXT.
func lastUID(conn mailinfra.IMAPSession, mailbox *mailinfra.IMAPMailbox) (uint32, error) {
	if mailbox.UIDNext > 0 {
		return mailbox.UIDNext - 1, nil
	}

	uids, err := conn.SearchUIDsRange(1, 0, time.Time{})
	if err != nil || len(uids) == 0 {
		return 0, err
	}

	return uids[len(uids)-1], nil
}

func imapCursor(cursor model.IMAPCursor) *model.ProviderCursor {
	return &model.ProviderCursor{IMAP: &cursor}
}
