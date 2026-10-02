package polling

import (
	"context"
	"log/slog"
	"time"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/model"
)

// recoveryCheckpoint returns the time the scan must resume from. A change that
// arrives mid-recovery keeps the original checkpoint: the scan has not passed it
// yet, and moving it forward would skip unread emails.
func recoveryCheckpoint(stored model.IMAPCursor) time.Time {
	if stored.Recovery != nil {
		return stored.Recovery.Checkpoint
	}

	return stored.LastInternalDate
}

// startRecovery plans a bounded re-read after the mailbox reassigned its UIDs.
// The old UID position is meaningless now, so the scan resumes from the time of
// the newest known email instead of from the beginning of the mailbox.
func startRecovery(
	stored model.IMAPCursor,
	mailbox *mailinfra.IMAPMailbox,
	end uint32,
	checkpoint time.Time,
	log *slog.Logger,
) model.IMAPCursor {
	if checkpoint.IsZero() {
		// Nothing is known to have been read, so there is no gap to recover.
		// Importing the whole mailbox instead is out of scope.
		log.Warn("imap uidvalidity changed without a checkpoint; starting at the end of the mailbox",
			"mailbox", mailbox.Name, "old", stored.UIDValidity, "new", mailbox.UIDValidity)

		return model.IMAPCursor{Mailbox: mailbox.Name, UIDValidity: mailbox.UIDValidity, LastUID: end}
	}

	log.Warn("imap uidvalidity changed, recovering from the last known email",
		"mailbox", mailbox.Name, "old", stored.UIDValidity, "new", mailbox.UIDValidity,
		"checkpoint", checkpoint, "to_uid", end)

	return model.IMAPCursor{
		Mailbox:          mailbox.Name,
		UIDValidity:      mailbox.UIDValidity,
		LastInternalDate: checkpoint,
		Recovery: &model.IMAPRecovery{
			// IMAP SEARCH compares whole days, so the window opens a day early and
			// the exact checkpoint decides what is actually new.
			Since:      checkpoint.AddDate(0, 0, -1),
			Checkpoint: checkpoint,
			ToUID:      end,
		},
	}
}

// recover reads one bounded portion of the recovery range. Progress is kept in
// the cursor, so an interrupted recovery resumes instead of starting over.
func (s *Scheduler) recover(
	ctx context.Context,
	conn mailinfra.IMAPSession,
	profile *model.EmailProfile,
	cursor model.IMAPCursor,
	log *slog.Logger,
) syncResult {
	recovery := cursor.Recovery
	if cursor.LastUID >= recovery.ToUID {
		return syncResult{cursor: imapCursor(finishRecovery(cursor, log))}
	}

	// Searched in the same bounded windows as ordinary polling, so a large
	// mailbox never returns its whole UID range in one response.
	uids, err := searchUIDs(ctx, conn, searchRange{
		after: cursor.LastUID,
		to:    recovery.ToUID,
		since: recovery.Since,
		limit: s.cfg.MaxMessagesPerPoll,
	})
	if err != nil {
		return syncResult{cursor: imapCursor(cursor), imapErr: err}
	}
	if len(uids) == 0 {
		return syncResult{cursor: imapCursor(finishRecovery(cursor, log))}
	}

	limited := len(uids) > s.cfg.MaxMessagesPerPoll
	if limited {
		uids = uids[:s.cfg.MaxMessagesPerPoll]
	}

	// Only the dates first: downloading bodies just to discard them would make
	// recovery cost as much as importing the whole window.
	dates, err := conn.FetchInternalDates(uids)
	if err != nil {
		return syncResult{cursor: imapCursor(cursor), imapErr: err}
	}

	handled := make(map[uint32]time.Time, len(uids))
	fresh := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		date, known := dates[uid]
		// Strictly older than the checkpoint means the email was already read
		// under the previous UIDs, or it predates this profile. An email sharing
		// the checkpoint's timestamp is passed on: deduplication tells the one
		// already stored from a different email that merely arrived together.
		if known && !date.IsZero() && date.Before(recovery.Checkpoint) {
			handled[uid] = date

			continue
		}

		fresh = append(fresh, uid)
	}

	imapErr, handlerErr := s.deliver(ctx, conn, profile, cursor, fresh, handled)
	advanceConfirmed(&cursor, uids, handled)

	if imapErr != nil || handlerErr != nil {
		return syncResult{cursor: imapCursor(cursor), imapErr: imapErr, handlerErr: handlerErr}
	}
	if !limited {
		return syncResult{cursor: imapCursor(finishRecovery(cursor, log))}
	}

	return syncResult{cursor: imapCursor(cursor), more: true}
}

// finishRecovery hands the cursor back to ordinary polling at the end of the
// recovered range, so later emails are read as usual.
func finishRecovery(cursor model.IMAPCursor, log *slog.Logger) model.IMAPCursor {
	log.Info("imap recovery finished",
		"mailbox", cursor.Mailbox, "uid_validity", cursor.UIDValidity, "last_uid", cursor.Recovery.ToUID)

	cursor.LastUID = cursor.Recovery.ToUID
	cursor.Recovery = nil

	return cursor
}
