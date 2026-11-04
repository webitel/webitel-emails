//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// insertMessage stores a message in the given state with a sender and a plain
// text body; a ready message moves last_message_at as the pipeline does.
func insertMessage(
	t *testing.T, db *pgxpool.Pool,
	domainID, profileID, threadID int64,
	messageID, state, sender, body string,
	receivedAt time.Time,
) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := db.QueryRow(ctx, `
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, message_id, state, body_text, received_at)
VALUES ($1, $2, $3, 'inbound', $4, $5, $6, $7) RETURNING id`,
		domainID, threadID, profileID, messageID, state, body, receivedAt).Scan(&id); err != nil {
		t.Fatalf("seed message %s: %v", messageID, err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO email.recipient (domain_id, message_id, type, address, normalized_address, display_name, ordinal)
VALUES ($1, $2, 'from', $3, lower($3), 'Sender', 0)`, domainID, id, sender); err != nil {
		t.Fatalf("seed sender: %v", err)
	}
	if state == "ready" {
		if _, err := db.Exec(ctx, `
UPDATE email.thread SET last_message_at = GREATEST(coalesce(last_message_at, $2), $2) WHERE id = $1`,
			threadID, receivedAt); err != nil {
			t.Fatalf("advance last message: %v", err)
		}
	}
	return id
}

func seedUser(t *testing.T, db *pgxpool.Pool, id, domainID int64, name string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO directory.wbt_user (id, dc, name, username) VALUES ($1, $2, $3, $4)`,
		id, domainID, name, fmt.Sprintf("user%d", id)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func threadIDs(views []*model.EmailThreadView) []int64 {
	ids := make([]int64, 0, len(views))
	for _, view := range views {
		ids = append(ids, view.ID)
	}
	return ids
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestThreadReadModel(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	if _, err := db.Exec(context.Background(), `DELETE FROM directory.wbt_user`); err != nil {
		t.Fatal(err)
	}
	profile := testhelpers.SeedProfile(t, db, 1, "read")
	other := testhelpers.SeedProfile(t, db, 1, "read-other")
	s := storepostgres.NewEmailThreadReadStore(db)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	seedUser(t, db, 70, 1, "Alice")
	seedUser(t, db, 80, 2, "Bob of another domain")

	current := seedThread(t, db, 1, profile, "regular", "new", nil)
	insertMessage(t, db, 1, profile, current, "<c1@x>", "ready", "first@x.com", "first", base)
	insertMessage(t, db, 1, profile, current, "<c2@x>", "ready", "latest@x.com", "  latest\n body ", base.Add(3*time.Hour))
	insertMessage(t, db, 1, profile, current, "<c3@x>", "processing", "pending@x.com", "pending", base.Add(5*time.Hour))
	assignThread(t, db, current, "in_progress", 70)

	history := seedThread(t, db, 1, other, "regular", "processed", ptrTime(base))
	insertMessage(t, db, 1, other, history, "<h1@x>", "ready", "h@x.com", "", base.Add(2*time.Hour))
	addParticipant(t, db, 1, history, 70)
	if _, err := db.Exec(ctx, `
UPDATE email.thread SET last_handled_agent_id = 8, last_handled_user_id = 80, last_handled_version = 1,
    contact_id = 500, contact_resolution_state = 'resolved' WHERE id = $1`, history); err != nil {
		t.Fatal(err)
	}

	foreignUser := seedThread(t, db, 1, profile, "regular", "new", nil)
	insertMessage(t, db, 1, profile, foreignUser, "<f1@x>", "ready", "f@x.com", "f", base.Add(time.Hour))
	assignThread(t, db, foreignUser, "in_progress", 90)
	addParticipant(t, db, 1, foreignUser, 71)

	processingOnly := seedThread(t, db, 1, profile, "regular", "new", nil)
	insertMessage(t, db, 1, profile, processingOnly, "<p1@x>", "processing", "p@x.com", "p", base)
	addParticipant(t, db, 1, processingOnly, 70)

	list := func(filter model.EmailThreadFilter) ([]*model.EmailThreadView, bool) {
		t.Helper()
		filter.DomainID, filter.UserID = 1, 70
		if filter.Page == 0 {
			filter.Page = 1
		}
		if filter.Size == 0 {
			filter.Size = 20
		}
		views, next, err := s.ListThreads(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		return views, next
	}

	t.Run("assignment views", func(t *testing.T) {
		for assignment, want := range map[model.EmailThreadAssignment][]int64{
			model.EmailThreadAssignmentCurrent: {current},
			model.EmailThreadAssignmentHistory: {history},
			model.EmailThreadAssignmentAll:     {current, history},
		} {
			views, _ := list(model.EmailThreadFilter{Assignment: assignment})
			if got := threadIDs(views); !sameIDs(got, want) {
				t.Errorf("%s: %v, want %v", assignment, got, want)
			}
		}
	})

	t.Run("filters", func(t *testing.T) {
		views, _ := list(model.EmailThreadFilter{Assignment: model.EmailThreadAssignmentAll,
			Statuses: []model.EmailThreadStatus{model.EmailThreadStatusProcessed}})
		if !sameIDs(threadIDs(views), []int64{history}) {
			t.Errorf("status: %v", threadIDs(views))
		}
		views, _ = list(model.EmailThreadFilter{Assignment: model.EmailThreadAssignmentAll, ProfileIDs: []int64{profile}})
		if !sameIDs(threadIDs(views), []int64{current}) {
			t.Errorf("profile: %v", threadIDs(views))
		}
		views, _ = list(model.EmailThreadFilter{Assignment: model.EmailThreadAssignmentAll, ContactID: 500})
		if !sameIDs(threadIDs(views), []int64{history}) {
			t.Errorf("contact: %v", threadIDs(views))
		}
		views, _ = list(model.EmailThreadFilter{Assignment: model.EmailThreadAssignmentAll,
			Sort: []model.EmailThreadSort{{Field: model.EmailThreadSortLastMessageAt}}})
		if !sameIDs(threadIDs(views), []int64{history, current}) {
			t.Errorf("ascending: %v", threadIDs(views))
		}
	})

	t.Run("the view of a current and a history thread", func(t *testing.T) {
		view, err := s.LocateThread(ctx, 1, 70, current)
		if err != nil || view == nil {
			t.Fatalf("locate current: %v, %v", view, err)
		}
		if view.Access != model.EmailThreadAccessCurrent || view.CurrentAttemptID == nil || *view.CurrentAttemptID != 100 ||
			view.CurrentAttemptVersion == nil || view.CurrentUser == nil || view.CurrentUser.Name != "Alice" ||
			view.Profile.ID != profile || view.Profile.Name == "" || view.Status != model.EmailThreadStatusInProgress {
			t.Fatalf("current view %+v", view)
		}
		if p := view.Preview; p == nil || p.Text != "  latest\n body " || p.From == nil || p.From.Address != "latest@x.com" ||
			!p.ReceivedAt.Equal(base.Add(3*time.Hour)) || p.Direction != model.EmailMessageDirectionInbound {
			t.Fatalf("preview took a processing or older message: %+v", view.Preview)
		}

		view, err = s.LocateThread(ctx, 1, 70, history)
		if err != nil || view == nil {
			t.Fatalf("locate history: %v, %v", view, err)
		}
		if view.Access != model.EmailThreadAccessHistory || view.CurrentAttemptID != nil || view.CurrentUser != nil ||
			view.LastHandledUser == nil || view.LastHandledUser.ID != 80 || view.LastHandledUser.Name != "" ||
			view.ContactID == nil || *view.ContactID != 500 || view.CompletedAt == nil {
			t.Fatalf("history view %+v", view)
		}
		if view.Preview == nil || view.Preview.Text != "" {
			t.Fatalf("html-only preview %+v", view.Preview)
		}
	})

	t.Run("a user without a directory row keeps the id", func(t *testing.T) {
		view, err := s.LocateThread(ctx, 1, 90, foreignUser)
		if err != nil || view == nil || view.CurrentUser == nil || view.CurrentUser.ID != 90 || view.CurrentUser.Name != "" {
			t.Fatalf("view %+v, %v", view, err)
		}
	})

	t.Run("invisible threads are not found", func(t *testing.T) {
		for _, c := range []struct {
			domain, user, thread int64
		}{{1, 70, foreignUser}, {1, 70, processingOnly}, {2, 70, current}, {1, 71, current}} {
			if view, err := s.LocateThread(ctx, c.domain, c.user, c.thread); err != nil || view != nil {
				t.Errorf("%+v: %+v, %v", c, view, err)
			}
		}
	})

	t.Run("pages are stable when the sort key ties", func(t *testing.T) {
		tied := make([]int64, 0, 3)
		for i := 0; i < 3; i++ {
			id := seedThread(t, db, 1, profile, "regular", "new", nil)
			insertMessage(t, db, 1, profile, id, "<tie"+string(rune('a'+i))+"@x>", "ready", "t@x.com", "t", base.Add(10*time.Hour))
			addParticipant(t, db, 1, id, 72)
			tied = append(tied, id)
		}
		var seen []int64
		for page := 1; page <= 3; page++ {
			filter := model.EmailThreadFilter{DomainID: 1, UserID: 72, Assignment: model.EmailThreadAssignmentAll, Page: page, Size: 1}
			views, next, err := s.ListThreads(ctx, filter)
			if err != nil || len(views) != 1 || next != (page < 3) {
				t.Fatalf("page %d: %d items, next %v, %v", page, len(views), next, err)
			}
			seen = append(seen, views[0].ID)
		}
		if !sameIDs(seen, []int64{tied[2], tied[1], tied[0]}) {
			t.Fatalf("tied pages %v, want ids descending %v", seen, tied)
		}
	})
}
