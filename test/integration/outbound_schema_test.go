//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func TestOutboundMessageConstraints(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "outbound")
	thread := seedThread(t, db, 1, profileID, "regular", "processed", ptrTime(time.Now()))

	n := 0
	insert := func(columns, values string) error {
		n++
		_, err := db.Exec(context.Background(), fmt.Sprintf(`
INSERT INTO email.message (domain_id, thread_id, profile_id, message_id, received_at, %s)
VALUES (1, $1, $2, $3, now(), %s)`, columns, values), thread, profileID, fmt.Sprintf("<out-%d@x>", n))
		return err
	}
	const outbound = `direction, state, author_user_id, request_id, request_fingerprint, delivery_status`
	values := func(author int, request, status string) string {
		return fmt.Sprintf(`'outbound', 'ready', %d, '%s', decode(repeat('ab', 32), 'hex'), '%s'`, author, request, status)
	}
	const request = "6f1c2a10-0000-4000-8000-000000000001"

	accepted := []struct{ name, columns, values string }{
		{"queued", outbound + ", delivery_phase", values(5, request, "pending") + ", 'queued'"},
		{"same request of another author", outbound + ", delivery_phase", values(6, request, "pending") + ", 'queued'"},
		{"sending under a lease", outbound + ", delivery_phase, delivery_lease_until, delivery_attempts",
			values(5, "6f1c2a10-0000-4000-8000-000000000002", "pending") + ", 'sending', now(), 1"},
		{"sent", outbound + ", delivered_at", values(5, "6f1c2a10-0000-4000-8000-000000000003", "sent") + ", now()"},
		{"failed", outbound + ", delivery_error_code", values(5, "6f1c2a10-0000-4000-8000-000000000004", "failed") + ", 'unknown_result'"},
	}
	for _, tt := range accepted {
		if err := insert(tt.columns, tt.values); err != nil {
			t.Errorf("%s was refused: %v", tt.name, err)
		}
	}

	rejected := []struct{ name, columns, values string }{
		{"the same request of an author again", outbound + ", delivery_phase", values(5, request, "pending") + ", 'queued'"},
		{"outbound without a request", "direction, state, author_user_id, request_fingerprint, delivery_status, delivery_phase",
			`'outbound', 'ready', 5, decode(repeat('ab', 32), 'hex'), 'pending', 'queued'`},
		{"outbound without an author", "direction, state, request_id, request_fingerprint, delivery_status, delivery_phase",
			`'outbound', 'ready', gen_random_uuid(), decode(repeat('ab', 32), 'hex'), 'pending', 'queued'`},
		{"outbound without a fingerprint", "direction, state, author_user_id, request_id, delivery_status, delivery_phase",
			`'outbound', 'ready', 5, gen_random_uuid(), 'pending', 'queued'`},
		{"outbound still processing", "direction, state, author_user_id, request_id, request_fingerprint, delivery_status, delivery_phase",
			`'outbound', 'processing', 5, gen_random_uuid(), decode(repeat('ab', 32), 'hex'), 'pending', 'queued'`},
		{"pending without a phase", outbound, values(5, "6f1c2a10-0000-4000-8000-000000000005", "pending")},
		{"sending without a lease", outbound + ", delivery_phase", values(5, "6f1c2a10-0000-4000-8000-000000000006", "pending") + ", 'sending'"},
		{"sent without a time", outbound, values(5, "6f1c2a10-0000-4000-8000-000000000007", "sent")},
		{"failed without a code", outbound, values(5, "6f1c2a10-0000-4000-8000-000000000008", "failed")},
		{"unknown code", outbound + ", delivery_error_code", values(5, "6f1c2a10-0000-4000-8000-000000000009", "failed") + ", 'oops'"},
		{"inbound with delivery", "direction, delivery_status, delivery_phase", `'inbound', 'pending', 'queued'`},
		{"inbound with an author", "direction, author_user_id", `'inbound', 5`},
	}
	for _, tt := range rejected {
		if err := insert(tt.columns, tt.values); err == nil {
			t.Errorf("%s was accepted", tt.name)
		}
	}
}

// The author of a new outbound Thread is a participant before any snapshot; a
// later Take of the same user completes that row instead of adding another.
func TestThreadParticipantOutboundAuthor(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "author")
	threadID := seedThread(t, db, 1, profile, "regular", "processed", ptrTime(time.Now()))
	ctx := context.Background()

	// Only the outbound author may lack the agent.
	for _, tt := range []struct{ name, columns, values string }{
		{"no origin", "", ""},
		{"snapshot", ", first_version, last_version, first_participated_at, last_participated_at", ", 1, 1, now(), now()"},
		{"backfill", ", backfilled_at", ", now()"},
	} {
		if _, err := db.Exec(ctx, `
INSERT INTO email.thread_participant (domain_id, thread_id, user_id, agent_id`+tt.columns+`)
VALUES (1, $1, 80, NULL`+tt.values+`)`, threadID); err == nil {
			t.Fatalf("a %s participant without an agent was accepted", tt.name)
		}
	}
	sentAt := time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC) // before the take
	if _, err := db.Exec(ctx, `
INSERT INTO email.thread_participant
    (domain_id, thread_id, user_id, first_participated_at, last_participated_at, outbound_at)
VALUES (1, $1, 80, $2, $2, $2)`, threadID, sentAt); err != nil {
		t.Fatalf("author row: %v", err)
	}

	take := takenSnapshot(threadID, 12, 100, event.AttemptStateActive, 8, 80)
	if got := applySnapshot(t, storepostgres.NewEmailThreadProjectionStore(db), take); !got.ParticipantRecorded {
		t.Fatalf("take did not complete the author row: %+v", got)
	}
	rows := readParticipants(t, db, threadID)
	row := rows[80]
	if len(rows) != 1 || row.agent != 8 || row.firstVersion.Int64 != 12 || row.lastVersion.Int64 != 12 ||
		!row.firstAt.Time.Equal(sentAt) || !row.lastAt.Time.Equal(take.OccurredAt) {
		t.Fatalf("author row after take: %+v", rows)
	}
	if count(t, db, `SELECT count(*) FROM email.thread_participant WHERE thread_id = $1 AND outbound_at IS NOT NULL`, threadID) != 1 {
		t.Fatal("the take lost the outbound origin")
	}
}

func TestAttachmentUploadStore(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	ctx := context.Background()
	uploads := storepostgres.NewEmailAttachmentUploadStore(db)
	profileID := testhelpers.SeedProfile(t, db, 1, "uploads")
	thread := seedThread(t, db, 1, profileID, "regular", "processed", ptrTime(time.Now()))

	now := time.Now()
	create := func(t *testing.T, expiresAt time.Time) uuid.UUID {
		t.Helper()
		u := &model.EmailAttachmentUpload{
			DomainID: 1, Reference: uuid.New(), AuthorUserID: 70, FileID: 9,
			FileName: "a.pdf", MimeType: "application/pdf", Size: 3, ExpiresAt: expiresAt,
		}
		if err := uploads.Create(ctx, u); err != nil {
			t.Fatalf("create upload: %v", err)
		}
		return u.Reference
	}
	expired, used, live := create(t, now.Add(-time.Hour)), create(t, now.Add(-time.Hour)), create(t, now.Add(time.Hour))

	var messageID int64
	if err := db.QueryRow(ctx, `
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, state, message_id, received_at,
    author_user_id, request_id, request_fingerprint, delivery_status, delivery_phase)
VALUES (1, $1, $2, 'outbound', 'ready', '<upload@x>', now(), 70, gen_random_uuid(), decode(repeat('ab', 32), 'hex'), 'pending', 'queued')
RETURNING id`, thread, profileID).Scan(&messageID); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE email.attachment_upload SET used_by_message_id = $1 WHERE reference = $2`, messageID, used); err != nil {
		t.Fatalf("attach upload: %v", err)
	}

	deleted, err := uploads.DeleteExpiredUnused(ctx, now, 100)
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup deleted %d, %v; want only the expired unused upload", deleted, err)
	}
	for ref, want := range map[uuid.UUID]int{expired: 0, used: 1, live: 1} {
		if got := count(t, db, `SELECT count(*) FROM email.attachment_upload WHERE reference = $1`, ref); got != want {
			t.Errorf("upload %s rows = %d, want %d", ref, got, want)
		}
	}

	t.Run("the schema refuses an empty or foreign upload", func(t *testing.T) {
		bad := &model.EmailAttachmentUpload{DomainID: 1, Reference: uuid.New(), AuthorUserID: 70, FileID: 9,
			FileName: "a.pdf", MimeType: "application/pdf", ExpiresAt: now}
		if err := uploads.Create(ctx, bad); err == nil {
			t.Fatal("an upload of zero bytes was accepted")
		}
		if _, err := db.Exec(ctx, `UPDATE email.attachment_upload SET domain_id = 2 WHERE reference = $1`, used); err == nil {
			t.Fatal("an upload moved to the domain of another message")
		}
	})

	t.Run("deleting the message removes its upload", func(t *testing.T) {
		if _, err := db.Exec(ctx, `DELETE FROM email.message WHERE id = $1`, messageID); err != nil {
			t.Fatal(err)
		}
		if count(t, db, `SELECT count(*) FROM email.attachment_upload WHERE reference = $1`, used) != 0 {
			t.Fatal("the upload of a deleted message remained")
		}
	})
}
