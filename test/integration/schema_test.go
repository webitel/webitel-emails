//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

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
