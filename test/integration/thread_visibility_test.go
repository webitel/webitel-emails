//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// assignThread gives a Thread a ready message and makes userID its current agent
// in the given projected status, as the snapshot consumer would.
func assignThread(t *testing.T, db *pgxpool.Pool, threadID int64, status string, userID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
UPDATE email.thread SET status = $2, current_agent_id = $3, current_user_id = $3,
    current_attempt_id = 100, current_attempt_version = 0, projection_version = 1,
    last_message_at = coalesce(last_message_at, now())
WHERE id = $1`, threadID, status, userID); err != nil {
		t.Fatalf("assign thread: %v", err)
	}
}

func addParticipant(t *testing.T, db *pgxpool.Pool, domainID, threadID, userID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
INSERT INTO email.thread_participant (domain_id, thread_id, user_id, agent_id,
    first_version, last_version, first_participated_at, last_participated_at)
VALUES ($1, $2, $3, $3, 1, 1, now(), now())`, domainID, threadID, userID); err != nil {
		t.Fatalf("add participant: %v", err)
	}
}

func markReady(t *testing.T, db *pgxpool.Pool, threadID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`UPDATE email.thread SET last_message_at = now() WHERE id = $1`, threadID); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
}

func TestThreadVisibilityBoundary(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "visibility")
	foreignProfile := testhelpers.SeedProfile(t, db, 2, "visibility-foreign")
	s := storepostgres.NewEmailThreadReadStore(db)
	ctx := context.Background()

	current := seedThread(t, db, 1, profile, "regular", "new", nil)
	assignThread(t, db, current, "in_progress", 70)
	postponed := seedThread(t, db, 1, profile, "regular", "new", nil)
	assignThread(t, db, postponed, "distributed", 70)
	if _, err := db.Exec(ctx, `UPDATE email.thread SET status = 'postponed', postponed_until = now() + interval '1 hour' WHERE id = $1`, postponed); err != nil {
		t.Fatal(err)
	}
	offered := seedThread(t, db, 1, profile, "regular", "new", nil)
	assignThread(t, db, offered, "distributed", 70)
	history := seedThread(t, db, 1, profile, "regular", "processed", ptrTime(time.Now().UTC()))
	markReady(t, db, history)
	addParticipant(t, db, 1, history, 70)
	notReady := seedThread(t, db, 1, profile, "regular", "new", nil)
	addParticipant(t, db, 1, notReady, 70)
	service := seedThread(t, db, 1, profile, "service", "processed", ptrTime(time.Now().UTC()))
	markReady(t, db, service)
	foreign := seedThread(t, db, 2, foreignProfile, "regular", "new", nil)
	assignThread(t, db, foreign, "in_progress", 70)

	cases := []struct {
		name     string
		domainID int64
		userID   int64
		threadID int64
		want     store.EmailThreadRights
	}{
		{"current after take", 1, 70, current, store.EmailThreadRights{Visible: true, Assigned: true}},
		{"current while postponed", 1, 70, postponed, store.EmailThreadRights{Visible: true, Assigned: true}},
		{"current while only offered", 1, 70, offered, store.EmailThreadRights{Visible: true}},
		{"previous participant", 1, 70, history, store.EmailThreadRights{Visible: true}},
		{"another agent of the domain", 1, 71, current, store.EmailThreadRights{}},
		{"participant of a thread without a ready message", 1, 70, notReady, store.EmailThreadRights{}},
		{"service thread", 1, 70, service, store.EmailThreadRights{}},
		{"same user in another domain", 1, 70, foreign, store.EmailThreadRights{}},
		{"own thread asked with another domain", 2, 70, current, store.EmailThreadRights{}},
		{"missing thread", 1, 70, 1 << 60, store.EmailThreadRights{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.Rights(ctx, c.domainID, c.userID, c.threadID)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("rights = %+v, want %+v", got, c.want)
			}
		})
	}

	t.Run("a manual contact needs the assignment in the update itself", func(t *testing.T) {
		threads := storepostgres.NewEmailThreadStore(db)
		contactID := int64(42)
		for _, id := range []int64{offered, history} {
			if applied, err := threads.SetContactManually(ctx, 1, 70, id, &contactID); err != nil || applied {
				t.Fatalf("thread %d without assignment: %v, %v", id, applied, err)
			}
		}
		if applied, err := threads.SetContactManually(ctx, 1, 70, postponed, &contactID); err != nil || !applied {
			t.Fatalf("postponed thread of the agent: %v, %v", applied, err)
		}
		// Reassigned between the check and the update: the update refuses it.
		assignThread(t, db, current, "in_progress", 71)
		if applied, err := threads.SetContactManually(ctx, 1, 70, current, nil); err != nil || applied {
			t.Fatalf("reassigned thread: %v, %v", applied, err)
		}
	})
}

// assertLastMessageInvariant checks that last_message_at is set exactly for the
// Threads that have a ready message; the visibility boundary relies on it.
func assertLastMessageInvariant(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	broken := count(t, db, `
SELECT count(*) FROM email.thread t
WHERE (t.last_message_at IS NOT NULL) <> EXISTS (
    SELECT 1 FROM email.message m
    WHERE m.domain_id = t.domain_id AND m.thread_id = t.id AND m.state = 'ready')`)
	if broken != 0 {
		t.Errorf("%d threads break the last_message_at invariant", broken)
	}
}
