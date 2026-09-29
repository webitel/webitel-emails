//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// The lookup order lives in SQL, where array_position and the ordering decide
// the winner. The in-memory fake only imitates that, so it is checked here.
func TestThreadingOrderInSQL(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")
	other := testhelpers.SeedProfile(t, db, 1, "other")
	handler := newHandler(db)

	deliver := func(email *model.ParsedEmail) int64 {
		t.Helper()
		if err := handler.Handle(context.Background(), email); err != nil {
			t.Fatalf("deliver %s: %v", email.MessageID, err)
		}

		return threadOf(t, db, email.MessageID)
	}

	distant := deliver(newEmail(profileID, "<distant@x>", 1))
	parent := deliver(newEmail(profileID, "<parent@x>", 2))

	// The parent is repeated inside References, as most mail clients do. Only the
	// strongest position may count, or a weaker reference would win.
	got := deliver(newEmail(profileID, "<reply@x>", 3,
		withInReplyTo("<parent@x>"),
		withReferences("<parent@x>", "<distant@x>")))
	if got != parent {
		t.Fatalf("reply landed in thread %d, want the direct parent %d (distant %d)", got, parent, distant)
	}

	// A service thread must never win a tie against a regular one.
	bounce := newEmail(profileID, "<bounce@x>", 4, withKind(model.EmailKindBounce), withInReplyTo("<ghost@x>"))
	service := deliver(bounce)
	regular := deliver(newEmail(profileID, "<ordinary@x>", 5, withInReplyTo("<ghost@x>")))
	if regular == service {
		t.Fatal("an ordinary email joined a closed service thread")
	}

	second := newEmail(profileID, "<auto@x>", 6, withKind(model.EmailKindAutoReply), withInReplyTo("<ghost@x>"))
	if got := deliver(second); got != regular {
		t.Fatalf("the auto-reply landed in thread %d, want the regular one %d", got, regular)
	}

	// Another mailbox of the same domain must stay out of reach.
	foreign := newEmail(other, "<foreign@x>", 7, withInReplyTo("<parent@x>"))
	if got := deliver(foreign); got == parent {
		t.Fatal("threading crossed the profile boundary")
	}
}

func TestRemainingThreadingRulesInSQL(t *testing.T) {
	db := testhelpers.Database(t)

	setup := func(t *testing.T) (int64, *inbound.PersistenceHandler, func(*model.ParsedEmail) int64) {
		t.Helper()
		testhelpers.Reset(t, db)
		profileID := testhelpers.SeedProfile(t, db, 1, "owner")
		handler := newHandler(db)
		deliver := func(email *model.ParsedEmail) int64 {
			t.Helper()
			if err := handler.Handle(context.Background(), email); err != nil {
				t.Fatalf("deliver %s: %v", email.MessageID, err)
			}

			return threadOf(t, db, email.MessageID)
		}

		return profileID, handler, deliver
	}

	t.Run("bounce original identifier has the highest priority", func(t *testing.T) {
		profileID, _, deliver := setup(t)
		original := deliver(newEmail(profileID, "<original@x>", 1))
		direct := deliver(newEmail(profileID, "<direct@x>", 2))
		referenced := deliver(newEmail(profileID, "<referenced@x>", 3))

		bounce := newEmail(profileID, "<bounce-priority@x>", 4,
			withKind(model.EmailKindBounce),
			withInReplyTo("<direct@x>"),
			withReferences("<referenced@x>"),
		)
		bounce.Bounce = &model.EmailBounceInfo{OriginalMessageID: "<original@x>", Status: "5.1.1"}
		if got := deliver(bounce); got != original {
			t.Fatalf("bounce landed in thread %d, want original %d (direct %d, reference %d)",
				got, original, direct, referenced)
		}
	})

	t.Run("the nearest known reference wins", func(t *testing.T) {
		profileID, _, deliver := setup(t)
		oldest := deliver(newEmail(profileID, "<oldest@x>", 1))
		nearest := deliver(newEmail(profileID, "<nearest@x>", 2))
		got := deliver(newEmail(profileID, "<reference-reply@x>", 3,
			withInReplyTo("<missing@x>"),
			withReferences("<oldest@x>", "<nearest@x>"),
		))
		if got != nearest {
			t.Fatalf("reply landed in thread %d, want nearest %d (oldest %d)", got, nearest, oldest)
		}
	})

	t.Run("a late parent joins through reverse sibling", func(t *testing.T) {
		profileID, _, deliver := setup(t)
		answer := deliver(newEmail(profileID, "<early-answer@x>", 1, withInReplyTo("<late-parent@x>")))
		if got := deliver(newEmail(profileID, "<late-parent@x>", 2)); got != answer {
			t.Fatalf("late parent landed in thread %d, want %d", got, answer)
		}
	})

	t.Run("an upward match wins over reverse sibling", func(t *testing.T) {
		profileID, _, deliver := setup(t)
		parent := deliver(newEmail(profileID, "<known-parent@x>", 1))
		reverse := deliver(newEmail(profileID, "<early-child@x>", 2, withInReplyTo("<incoming@x>")))
		got := deliver(newEmail(profileID, "<incoming@x>", 3, withInReplyTo("<known-parent@x>")))
		if got != parent {
			t.Fatalf("incoming email landed in thread %d, want upward match %d (reverse %d)", got, parent, reverse)
		}
	})
}

func TestRegularEmailNeverJoinsServiceThreadInSQL(t *testing.T) {
	db := testhelpers.Database(t)

	tests := []struct {
		name     string
		service  func(profileID int64) *model.ParsedEmail
		ordinary func(profileID int64) *model.ParsedEmail
	}{
		{
			name: "direct parent",
			service: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<service-parent@x>", 1, withKind(model.EmailKindAutoReply))
			},
			ordinary: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<ordinary-direct@x>", 2, withInReplyTo("<service-parent@x>"))
			},
		},
		{
			name: "references",
			service: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<service-reference@x>", 1, withKind(model.EmailKindAutoReply))
			},
			ordinary: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<ordinary-reference@x>", 2,
					withInReplyTo("<missing@x>"), withReferences("<service-reference@x>"))
			},
		},
		{
			name: "sibling",
			service: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<service-sibling@x>", 1,
					withKind(model.EmailKindAutoReply), withInReplyTo("<unknown-parent@x>"))
			},
			ordinary: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<ordinary-sibling@x>", 2, withInReplyTo("<unknown-parent@x>"))
			},
		},
		{
			name: "reverse sibling",
			service: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<service-child@x>", 1,
					withKind(model.EmailKindAutoReply), withInReplyTo("<ordinary-parent@x>"))
			},
			ordinary: func(profileID int64) *model.ParsedEmail {
				return newEmail(profileID, "<ordinary-parent@x>", 2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testhelpers.Reset(t, db)
			profileID := testhelpers.SeedProfile(t, db, 1, "owner")
			handler := newHandler(db)

			serviceEmail := tt.service(profileID)
			if err := handler.Handle(context.Background(), serviceEmail); err != nil {
				t.Fatalf("deliver service email: %v", err)
			}
			serviceThread := threadOf(t, db, serviceEmail.MessageID)

			ordinaryEmail := tt.ordinary(profileID)
			if err := handler.Handle(context.Background(), ordinaryEmail); err != nil {
				t.Fatalf("deliver ordinary email: %v", err)
			}
			ordinaryThread := threadOf(t, db, ordinaryEmail.MessageID)
			if ordinaryThread == serviceThread {
				t.Fatal("ordinary email joined a service thread")
			}

			var kind, status string
			if err := db.QueryRowContext(context.Background(),
				`SELECT kind, status FROM email.thread WHERE id = $1`, ordinaryThread).Scan(&kind, &status); err != nil {
				t.Fatalf("read ordinary thread: %v", err)
			}
			if kind != "regular" || status != "new" {
				t.Fatalf("ordinary thread = %s/%s, want regular/new", kind, status)
			}
		})
	}
}

func TestThreadingIsScopedToDomainAndProfileInSQL(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	owner := testhelpers.SeedProfile(t, db, 1, "owner")
	otherProfile := testhelpers.SeedProfile(t, db, 1, "other-profile")
	otherDomain := testhelpers.SeedProfile(t, db, 2, "other-domain")
	handler := newHandler(db)

	for _, identity := range []struct {
		domainID  int64
		profileID int64
		uid       uint32
	}{
		{domainID: 1, profileID: otherProfile, uid: 1},
		{domainID: 2, profileID: otherDomain, uid: 1},
	} {
		email := newEmail(identity.profileID, "<foreign-parent@x>", identity.uid)
		email.DomainID = identity.domainID
		if err := handler.Handle(context.Background(), email); err != nil {
			t.Fatalf("seed foreign parent: %v", err)
		}
	}

	child := newEmail(owner, "<owner-child@x>", 2, withInReplyTo("<foreign-parent@x>"))
	if err := handler.Handle(context.Background(), child); err != nil {
		t.Fatalf("deliver owner child: %v", err)
	}
	var messages int
	if err := db.QueryRowContext(context.Background(), `
SELECT count(*) FROM email.message WHERE thread_id = (
    SELECT thread_id FROM email.message
    WHERE domain_id = 1 AND profile_id = $1 AND message_id = '<owner-child@x>'
)`, owner).Scan(&messages); err != nil {
		t.Fatalf("count owner thread messages: %v", err)
	}
	if messages != 1 {
		t.Fatalf("owner thread contains %d messages, want only its own child", messages)
	}
}

// Deduplication rests on three unique indexes; only the database can prove that
// each of them recognizes its own kind of redelivery.
func TestDeduplicationAcrossIdentities(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")
	handler := newHandler(db)

	generated := func(messageID, raw string, uidValidity, uid uint32) *model.ParsedEmail {
		email := newEmail(profileID, messageID, uid)
		email.MessageIDGenerated = true
		email.UIDValidity = uidValidity
		email.RawSHA256 = checksumOf(raw)

		return email
	}

	tests := []struct {
		name   string
		first  *model.ParsedEmail
		second *model.ParsedEmail
	}{
		{
			name:   "same mail identifier",
			first:  newEmail(profileID, "<same@x>", 1),
			second: newEmail(profileID, "<same@x>", 1),
		},
		{
			name:   "same mailbox position",
			first:  newEmail(profileID, "<position-a@x>", 2),
			second: newEmail(profileID, "<position-b@x>", 2),
		},
		{
			name:   "same raw after uidvalidity changed",
			first:  generated("imap.one@webitel.local", "identical", 10, 3),
			second: generated("imap.two@webitel.local", "identical", 11, 9),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testhelpers.Reset(t, db)
			profileID = testhelpers.SeedProfile(t, db, 1, "owner")
			tt.first.ProfileID, tt.second.ProfileID = profileID, profileID

			if err := handler.Handle(context.Background(), tt.first); err != nil {
				t.Fatalf("first delivery: %v", err)
			}
			if err := handler.Handle(context.Background(), tt.second); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			if messages := count(t, db, `SELECT count(*) FROM email.message`); messages != 1 {
				t.Fatalf("messages = %d, want 1", messages)
			}
			if threads := count(t, db, `SELECT count(*) FROM email.thread`); threads != 1 {
				t.Fatalf("threads = %d, want 1", threads)
			}
		})
	}

	t.Run("the same mail identifier in the other direction", func(t *testing.T) {
		testhelpers.Reset(t, db)
		profileID := testhelpers.SeedProfile(t, db, 1, "owner")
		threadID := seedThread(t, db, 1, profileID, "regular", "new", nil)
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.message (
    domain_id, thread_id, profile_id, direction, state, message_id, received_at
) VALUES (1, $1, $2, 'outbound', 'ready', '<direction@x>', now())`, threadID, profileID); err != nil {
			t.Fatalf("seed outbound message: %v", err)
		}

		if err := newHandler(db).Handle(context.Background(), newEmail(profileID, "<direction@x>", 20)); err != nil {
			t.Fatalf("deliver inbound copy: %v", err)
		}
		if messages := count(t, db, `SELECT count(*) FROM email.message`); messages != 1 {
			t.Fatalf("messages = %d, want one logical message across directions", messages)
		}
	})

	t.Run("ready and failed redelivery are terminal no-ops", func(t *testing.T) {
		testhelpers.Reset(t, db)
		profileID := testhelpers.SeedProfile(t, db, 1, "owner")
		handler := newHandler(db)
		email := newEmail(profileID, "<terminal@x>", 21)
		if err := handler.Handle(context.Background(), email); err != nil {
			t.Fatalf("first delivery: %v", err)
		}

		var messageID, threadID int64
		if err := db.QueryRowContext(context.Background(), `
SELECT id, thread_id FROM email.message WHERE domain_id = 1 AND profile_id = $1 AND message_id = $2`,
			profileID, email.MessageID).Scan(&messageID, &threadID); err != nil {
			t.Fatalf("read stored identity: %v", err)
		}
		baselineRecipients := count(t, db, `SELECT count(*) FROM email.recipient`)

		if err := handler.Handle(context.Background(), email); err != nil {
			t.Fatalf("ready retry: %v", err)
		}
		if _, err := db.ExecContext(context.Background(),
			`UPDATE email.message SET state = 'failed' WHERE id = $1`, messageID); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.inbound_failure (domain_id, profile_id, mailbox, uid_validity, uid, category, reason)
VALUES (1, $1, 'INBOX', 10, 21, 'attachment_upload', 'failed to store attachments')`, profileID); err != nil {
			t.Fatalf("seed quarantine: %v", err)
		}
		if err := handler.Handle(context.Background(), email); err != nil {
			t.Fatalf("failed retry: %v", err)
		}

		var storedMessageID, storedThreadID int64
		if err := db.QueryRowContext(context.Background(), `
SELECT id, thread_id FROM email.message WHERE domain_id = 1 AND profile_id = $1 AND message_id = $2`,
			profileID, email.MessageID).Scan(&storedMessageID, &storedThreadID); err != nil {
			t.Fatalf("read identity after retries: %v", err)
		}
		if storedMessageID != messageID || storedThreadID != threadID {
			t.Fatalf("identity changed from %d/%d to %d/%d", messageID, threadID, storedMessageID, storedThreadID)
		}
		if count(t, db, `SELECT count(*) FROM email.thread`) != 1 ||
			count(t, db, `SELECT count(*) FROM email.message`) != 1 ||
			count(t, db, `SELECT count(*) FROM email.recipient`) != baselineRecipients ||
			count(t, db, `SELECT count(*) FROM email.inbound_failure`) != 1 {
			t.Fatal("a terminal redelivery changed persisted data")
		}
	})
}
