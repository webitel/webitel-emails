//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func TestThreadActionJournal(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "actions")
	projections := storepostgres.NewEmailThreadProjectionStore(db)
	reads := storepostgres.NewEmailThreadReadStore(db)
	ctx := context.Background()
	seedUser(t, db, 70, 1, "Sender")
	seedUser(t, db, 80, 1, "Outsider")

	thread := seedThread(t, db, 1, profile, "regular", "new", nil)
	markReady(t, db, thread)

	offered := takenSnapshot(thread, 1, 5, event.AttemptStateOffering, 7, 70)
	taken := takenSnapshot(thread, 2, 5, event.AttemptStateActive, 7, 70)
	moved := takenSnapshot(thread, 3, 5, event.AttemptStateLeaving, 7, 70)
	moved.Transition, moved.Source, moved.Result = event.TransitionTransferred, event.SourceEngine, ptrString(event.AttemptResultTransfer)
	moved.ActorUserID, moved.Comment, moved.TransferToQueueID = ptrInt64(70), ptrString("to sales"), ptrInt64(4)
	created := projectionSnapshot(thread, 4, 6, event.AttemptStateWaiting)
	created.Transition, created.Source, created.ActorUserID = event.TransitionTransferCreated, event.SourceEngine, ptrInt64(70)

	// Out of order and repeated: one row per projection_version, stale snapshots included.
	for _, e := range []event.AttemptSnapshot{offered, moved, created, taken} {
		if r := applySnapshot(t, projections, e); !r.ActionRecorded {
			t.Fatalf("version %d: action not recorded", e.ProjectionVersion)
		}
	}
	if r := applySnapshot(t, projections, moved); r.ActionRecorded {
		t.Fatal("a repeated snapshot added an action")
	}
	if got := readProjectedThread(t, db, thread); got.status != "new" || got.handledUser != "70" || got.completed.Valid || got.user != "" {
		t.Fatalf("thread after transfer: %+v", got)
	}

	page, next, err := reads.ListActions(ctx, model.EmailThreadActionFilter{DomainID: 1, UserID: 70, ThreadID: thread, Size: 3})
	if err != nil || !next || len(page) != 3 {
		t.Fatalf("first page = %d, %v, %v", len(page), next, err)
	}
	if page[0].ProjectionVersion != 1 || page[2].Transition != event.TransitionTransferred ||
		page[2].Comment != "to sales" || page[2].Actor == nil || page[2].Actor.Name != "Sender" ||
		page[2].TransferToQueueID == nil || *page[2].TransferToQueueID != 4 || page[1].Agent == nil || page[1].Agent.ID != 70 {
		t.Fatalf("first page: %+v %+v %+v", page[0], page[1], page[2])
	}
	rest, next, err := reads.ListActions(ctx, model.EmailThreadActionFilter{DomainID: 1, UserID: 70, ThreadID: thread, After: 3, Size: 3})
	if err != nil || next || len(rest) != 1 || rest[0].Transition != event.TransitionTransferCreated {
		t.Fatalf("second page = %+v, %v, %v", rest, next, err)
	}

	// The boundary of the read API: neither another user nor another domain sees the journal.
	for _, f := range []model.EmailThreadActionFilter{
		{DomainID: 1, UserID: 80, ThreadID: thread, Size: 10},
		{DomainID: 2, UserID: 70, ThreadID: thread, Size: 10},
	} {
		if got, _, err := reads.ListActions(ctx, f); err != nil || len(got) != 0 {
			t.Fatalf("%+v sees %d actions, %v", f, len(got), err)
		}
	}

	deleted, err := projections.DeleteActionsBefore(ctx, time.Now(), 2)
	if err != nil || deleted != 2 || count(t, db, `SELECT count(*) FROM email.thread_action WHERE thread_id = $1`, thread) != 2 {
		t.Fatalf("retention deleted %d, %v", deleted, err)
	}
}

func ptrString(v string) *string { return &v }
