//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/model"

	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// The constraints below are the only thing standing between a bug and mixed-up
// tenants or lost history, so they are checked against the real schema.
func TestSchemaProtectsTenantsAndHistory(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)

	owner := testhelpers.SeedProfile(t, db, 1, "owner")
	neighbour := testhelpers.SeedProfile(t, db, 1, "neighbour")
	otherDomain := testhelpers.SeedProfile(t, db, 2, "other-domain")

	thread := seedThread(t, db, 1, owner, "regular", "new", nil)
	completed := time.Now().UTC()

	insertMessage := func(domainID, threadID, profileID int64, messageID string) error {
		_, err := db.ExecContext(context.Background(), `
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, message_id, received_at)
VALUES ($1, $2, $3, 'inbound', $4, now())`, domainID, threadID, profileID, messageID)

		return err
	}

	t.Run("a message cannot join a thread of another mailbox", func(t *testing.T) {
		if err := insertMessage(1, thread, neighbour, "<cross-profile@x>"); err == nil {
			t.Fatal("a message of another profile was accepted")
		}
	})

	t.Run("a message cannot join a thread of another domain", func(t *testing.T) {
		if err := insertMessage(2, thread, otherDomain, "<cross-domain@x>"); err == nil {
			t.Fatal("a message of another domain was accepted")
		}
	})

	t.Run("a service thread is always closed", func(t *testing.T) {
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.thread (domain_id, profile_id, kind, status) VALUES (1, $1, 'unknown', 'new')`, owner); err == nil {
			t.Fatal("an invalid thread kind was accepted")
		}
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.thread (domain_id, profile_id, kind, status) VALUES (1, $1, 'service', 'new')`, owner); err == nil {
			t.Fatal("an open service thread was accepted")
		}
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.thread (domain_id, profile_id, kind, status, completed_at)
VALUES (1, $1, 'service', 'processed', $2)`, owner, completed); err != nil {
			t.Fatalf("a closed service thread must be accepted: %v", err)
		}
	})

	t.Run("the lookup and deduplication indexes exist", func(t *testing.T) {
		indexes := []string{
			"email.thread_domain_status_last_message_idx",
			"email.thread_domain_profile_kind_status_last_message_idx",
			"email.thread_domain_contact_last_message_idx",
			"email.message_identity_uidx",
			"email.message_imap_identity_uidx",
			"email.message_raw_checksum_uidx",
			"email.message_thread_received_idx",
			"email.message_in_reply_to_idx",
			"email.recipient_domain_normalized_address_idx",
			"email.inbound_failure_profile_created_idx",
			"email.message_attachment_message_position_uniq",
		}
		for _, index := range indexes {
			var exists bool
			if err := db.QueryRowContext(context.Background(),
				`SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists); err != nil {
				t.Fatalf("look up index %s: %v", index, err)
			}
			if !exists {
				t.Errorf("required index %s does not exist", index)
			}
		}
	})

	t.Run("deleting a thread removes its messages and recipients", func(t *testing.T) {
		if err := insertMessage(1, thread, owner, "<cascade@x>"); err != nil {
			t.Fatal(err)
		}
		var messageID int64
		if err := db.QueryRowContext(context.Background(),
			`SELECT id FROM email.message WHERE message_id = '<cascade@x>'`).Scan(&messageID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.recipient (domain_id, message_id, type, address, normalized_address, ordinal)
VALUES (1, $1, 'to', 'A@X.com', 'a@x.com', 0)`, messageID); err != nil {
			t.Fatal(err)
		}

		if _, err := db.ExecContext(context.Background(), `DELETE FROM email.thread WHERE id = $1`, thread); err != nil {
			t.Fatal(err)
		}
		if count(t, db, `SELECT count(*) FROM email.message`) != 0 ||
			count(t, db, `SELECT count(*) FROM email.recipient`) != 0 {
			t.Fatal("messages or recipients survived their thread")
		}
	})

	t.Run("a profile with history cannot be deleted, one with only quarantine can", func(t *testing.T) {
		kept := seedThread(t, db, 1, owner, "regular", "new", nil)
		_ = kept
		if _, err := db.ExecContext(context.Background(), `DELETE FROM email.profile WHERE id = $1`, owner); err == nil {
			t.Fatal("a profile holding conversation history was deleted")
		}

		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.inbound_failure (domain_id, profile_id, mailbox, uid_validity, uid, category, reason)
VALUES (1, $1, 'INBOX', 10, 5, 'mime_parse', 'failed to parse MIME message')`, neighbour); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `DELETE FROM email.profile WHERE id = $1`, neighbour); err != nil {
			t.Fatalf("quarantine rows must not hold a profile: %v", err)
		}
		if count(t, db, `SELECT count(*) FROM email.inbound_failure`) != 0 {
			t.Fatal("quarantine rows survived their profile")
		}
	})
}

// The attachment manifest is what makes the lifecycle of a part unforgeable, so
// its constraints are checked against the real schema rather than trusted.
func TestAttachmentManifestConstraints(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)

	profileID := testhelpers.SeedProfile(t, db, 1, "owner")
	thread := seedThread(t, db, 1, profileID, "regular", "new", nil)

	var messageID int64
	if err := db.QueryRowContext(context.Background(), `
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, message_id, received_at)
VALUES (1, $1, $2, 'inbound', '<manifest@x>', now()) RETURNING id`, thread, profileID).Scan(&messageID); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	insert := func(domainID int64, columns, values string) error {
		query := fmt.Sprintf(`
INSERT INTO email.message_attachment (domain_id, message_id, disposition, file_name, mime_type, size, position%s)
VALUES ($1, $2, 'attachment', 'a.pdf', 'application/pdf', 1, $3%s)`, columns, values)
		_, err := db.ExecContext(context.Background(), query, domainID, messageID, nextPosition())

		return err
	}

	rejected := []struct {
		name    string
		columns string
		values  string
	}{
		{"pending part carrying a file", ", file_id", ", 7"},
		{"stored part without a file", ", state", ", 'stored'"},
		{"skipped part without a reason", ", state", ", 'skipped'"},
		{"skipped part carrying a file", ", state, skipped_reason, file_id", ", 'skipped', 'attachment_read_error', 7"},
		{"unknown skip reason", ", state, skipped_reason", ", 'skipped', 'because'"},
		{"unknown disposition", ", disposition", ", 'embedded'"},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			if err := insert(1, tt.columns, tt.values); err == nil {
				t.Fatal("the database accepted an impossible manifest row")
			}
		})
	}

	// Every reason the parser can produce must satisfy the CHECK: the Go constants
	// and the SQL list are two sources that must not drift apart.
	t.Run("every parser skip reason is storable", func(t *testing.T) {
		reasons := []model.EmailPartSkippedReason{
			model.EmailPartSkippedCountLimit,
			model.EmailPartSkippedSizeLimit,
			model.EmailPartSkippedTotalLimit,
			model.EmailPartSkippedReadError,
		}
		for _, reason := range reasons {
			if err := insert(1, ", state, skipped_reason", fmt.Sprintf(", 'skipped', '%s'", reason)); err != nil {
				t.Errorf("reason %s was refused by the schema: %v", reason, err)
			}
		}
	})

	t.Run("a part cannot belong to a message of another tenant", func(t *testing.T) {
		if err := insert(2, "", ""); err == nil {
			t.Fatal("a part of another domain was accepted")
		}
	})

	t.Run("one position holds one part", func(t *testing.T) {
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.message_attachment (domain_id, message_id, disposition, file_name, mime_type, size, position)
VALUES (1, $1, 'attachment', 'first.pdf', 'application/pdf', 1, 100)`, messageID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.message_attachment (domain_id, message_id, disposition, file_name, mime_type, size, position)
VALUES (1, $1, 'attachment', 'second.pdf', 'application/pdf', 1, 100)`, messageID); err == nil {
			t.Fatal("two parts share one position")
		}
	})

	t.Run("a negative attachment attempt count is refused", func(t *testing.T) {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE email.message SET attachment_attempts = -1 WHERE id = $1`, messageID); err == nil {
			t.Fatal("a negative attempt count was accepted")
		}
	})

	t.Run("deleting the message removes its manifest", func(t *testing.T) {
		if count(t, db, `SELECT count(*) FROM email.message_attachment`) == 0 {
			t.Fatal("no manifest rows to delete")
		}
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM email.message WHERE id = $1`, messageID); err != nil {
			t.Fatal(err)
		}
		if count(t, db, `SELECT count(*) FROM email.message_attachment`) != 0 {
			t.Fatal("manifest rows survived their message")
		}
	})
}

// nextPosition keeps every accepted row at a position of its own.
var manifestPosition int

func nextPosition() int {
	manifestPosition++

	return manifestPosition
}
