//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/projection"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func newWorkspaceHandler(db *pgxpool.Pool, files inbound.FileUploader) *inbound.PersistenceHandler {
	uow := storepostgres.NewUnitOfWork(db).WithWorkspaceEvents(true)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Storage: config.StorageConfig{MaxAttachmentAttempts: 3},
		InboundEvent: config.InboundEventConfig{
			MaxBodyTextSize: 64 << 10, MaxAddressesPerType: 100, MaxAttachments: 100,
			MaxFieldSize: 1024, MaxPayloadSize: 1 << 20,
		},
	}
	events := inbound.NewEventWriter(cfg, log)
	completer := inbound.NewMessageCompleter(uow, files, knownContact{}, events, cfg, log)

	return inbound.NewPersistenceHandler(uow, completer, events, log)
}

// workspaceEvents returns the events of a Thread as "type user access changes",
// oldest version first, and checks each row against its payload.
func workspaceEvents(t *testing.T, db *pgxpool.Pool, threadID int64) []string {
	t.Helper()

	rows, err := db.Query(context.Background(), `
SELECT event_id::text, dedup_key, routing_key, payload::text FROM email.outbox
WHERE event_type LIKE 'email.workspace.%' AND (payload->>'thread_id')::bigint = $1
ORDER BY (payload->>'workspace_version')::bigint, (payload->>'user_id')::bigint`, threadID)
	if err != nil {
		t.Fatalf("read workspace events: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var id, key, routing, payload string
		if err := rows.Scan(&id, &key, &routing, &payload); err != nil {
			t.Fatal(err)
		}
		var e event.WorkspaceEvent
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			t.Fatal(err)
		}
		want := event.WorkspaceEventID(e.EventType, e.DomainID, e.ThreadID, e.WorkspaceVersion, e.UserID).String()
		if id != want || key != want || routing != event.WorkspaceRoutingKey(e.EventType, e.DomainID, e.UserID) {
			t.Fatalf("row %s %s %s for %+v", id, key, routing, e)
		}
		got = append(got, strings.TrimPrefix(e.EventType, "email.workspace.")+" "+
			itoa(e.UserID)+" "+e.Access+" "+strings.Join(e.Changes, ","))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return got
}

func workspaceVersion(t *testing.T, db *pgxpool.Pool, threadID int64) int {
	t.Helper()

	return count(t, db, `SELECT workspace_version FROM email.thread WHERE id = $1`, threadID)
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)

	return string(b)
}

func offered(threadID, version, attemptID, agentID, userID int64) event.AttemptSnapshot {
	e := projectionSnapshot(threadID, version, attemptID, event.AttemptStateOffering)
	e.Transition, e.AgentID, e.AgentUserID = "assigned", &agentID, &userID

	return e
}

func TestWorkspaceEventsOfReadyMessages(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	ctx := context.Background()
	profile := testhelpers.SeedProfile(t, db, 1, "workspace")
	files := &stubUploader{}
	handler := newWorkspaceHandler(db, files)
	projections := storepostgres.NewEmailThreadProjectionStore(db).WithWorkspaceEvents(true)

	first := newEmail(profile, "<ws-first@x>", 1)
	if err := handler.Handle(ctx, first); err != nil {
		t.Fatal(err)
	}
	threadID := int64(count(t, db, `SELECT thread_id FROM email.message WHERE message_id = '<ws-first@x>'`))
	// Nobody sees a new conversation yet: the version grows, no event is written.
	if v, got := workspaceVersion(t, db, threadID), workspaceEvents(t, db, threadID); v != 1 || len(got) != 0 {
		t.Fatalf("version %d, events %v", v, got)
	}

	taken := projectionSnapshot(threadID, 10, 100, event.AttemptStateActive)
	taken.Transition = "taken"
	if r := applySnapshot(t, projections, taken); r.WorkspaceEvents != 1 {
		t.Fatalf("take: %+v", r)
	}

	// A reply is ready at once; a redelivery of either email changes nothing.
	reply := newEmail(profile, "<ws-reply@x>", 2, withInReplyTo("<ws-first@x>"))
	for _, email := range []*model.ParsedEmail{reply, reply, first} {
		if err := handler.Handle(ctx, email); err != nil {
			t.Fatal(err)
		}
	}

	// An email with a file becomes ready in the completion transaction, once.
	withFile := emailWithAttachment(profile, "<ws-file@x>", 3)
	withFile.InReplyTo = "<ws-first@x>"
	files.failWith = errors.New("storage unavailable")
	if err := handler.Handle(ctx, withFile); err == nil {
		t.Fatal("a failed upload was confirmed")
	}
	if v := workspaceVersion(t, db, threadID); v != 3 {
		t.Fatalf("an unfinished message raised the version to %d", v)
	}
	files.failWith = nil
	for range 2 {
		if err := handler.Handle(ctx, withFile); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"thread.changed 70 current projection,participants,actions",
		"message.created 70 current ",
		"message.created 70 current ",
	}
	if v, got := workspaceVersion(t, db, threadID), workspaceEvents(t, db, threadID); v != 4 || !slices.Equal(got, want) {
		t.Fatalf("version %d, events %q", v, got)
	}
}

func TestWorkspaceEventsOfProjectionAndContact(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	ctx := context.Background()
	profile := testhelpers.SeedProfile(t, db, 1, "workspace")
	projections := storepostgres.NewEmailThreadProjectionStore(db).WithWorkspaceEvents(true)

	threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
	if _, err := db.Exec(ctx, `UPDATE email.thread SET last_message_at = now() WHERE id = $1`, threadID); err != nil {
		t.Fatal(err)
	}

	waiting := projectionSnapshot(threadID, 11, 100, event.AttemptStateWaiting)
	waiting.Transition = "returned"
	taken := projectionSnapshot(threadID, 13, 101, event.AttemptStateActive)
	taken.Transition = "taken"
	lateOffer := offered(threadID, 12, 101, 8, 80)
	completed := projectionSnapshot(threadID, 14, 101, event.AttemptStateLeaving)

	for i, step := range []struct {
		snapshot event.AttemptSnapshot
		outcome  store.EmailThreadProjectionOutcome
	}{
		{offered(threadID, 10, 100, 8, 80), store.EmailThreadProjectionApplied},
		// The offer was not taken: its user loses the Thread.
		{waiting, store.EmailThreadProjectionApplied},
		// A repeat changes nothing visible.
		{waiting, store.EmailThreadProjectionStale},
		{taken, store.EmailThreadProjectionApplied},
		// A late offer adds its action only; the old offer's user is not told.
		{lateOffer, store.EmailThreadProjectionStale},
		{completed, store.EmailThreadProjectionApplied},
	} {
		if r := applySnapshot(t, projections, step.snapshot); r.Outcome != step.outcome {
			t.Fatalf("step %d: %+v", i, r)
		}
	}

	want := []string{
		"thread.changed 80 current projection,actions",
		"thread.changed 80 none ",
		"thread.changed 70 current projection,participants,actions",
		"thread.changed 70 current actions",
		"thread.changed 70 history projection,actions",
	}
	if v, got := workspaceVersion(t, db, threadID), workspaceEvents(t, db, threadID); v != 5 || !slices.Equal(got, want) {
		t.Fatalf("version %d, events %q", v, got)
	}

	// The read API shows both versions to the participant.
	view, err := storepostgres.NewEmailThreadReadStore(db).LocateThread(ctx, 1, 70, threadID)
	if err != nil || view == nil || view.WorkspaceVersion != 5 || view.ProjectionVersion == nil || *view.ProjectionVersion != 14 {
		t.Fatalf("view %+v, %v", view, err)
	}

	// One change, two users: each row has the routing key of its own user.
	t.Run("transfer", func(t *testing.T) {
		moved := seedThread(t, db, 1, profile, "regular", "new", nil)
		if _, err := db.Exec(ctx, `UPDATE email.thread SET last_message_at = now() WHERE id = $1`, moved); err != nil {
			t.Fatal(err)
		}
		took := projectionSnapshot(moved, 30, 300, event.AttemptStateActive)
		took.Transition = "taken"
		applySnapshot(t, projections, took)
		applySnapshot(t, projections, offered(moved, 31, 301, 9, 90))
		want := []string{
			"thread.changed 70 current projection,participants,actions",
			"thread.changed 70 history projection,actions",
			"thread.changed 90 current projection,actions",
		}
		if got := workspaceEvents(t, db, moved); !slices.Equal(got, want) {
			t.Fatalf("events %q", got)
		}
	})

	t.Run("contact", func(t *testing.T) {
		other := seedThread(t, db, 1, profile, "regular", "new", nil)
		if _, err := db.Exec(ctx, `UPDATE email.thread SET last_message_at = now() WHERE id = $1`, other); err != nil {
			t.Fatal(err)
		}
		active := projectionSnapshot(other, 20, 200, event.AttemptStateActive)
		active.Transition = "taken"
		applySnapshot(t, projections, active)

		contacts := storepostgres.NewEmailThreadContactStore(db, true)
		contactID := int64(55)
		var updated []time.Time
		for range 2 {
			if applied, err := contacts.SetContactManually(ctx, 1, 70, other, &contactID); err != nil || !applied {
				t.Fatalf("bind: %v, %v", applied, err)
			}
			var at time.Time
			if err := db.QueryRow(ctx, `SELECT updated_at FROM email.thread WHERE id = $1`, other).Scan(&at); err != nil {
				t.Fatal(err)
			}
			updated = append(updated, at)
		}
		// The same contact again is applied but is no change, not even of updated_at.
		if !updated[1].Equal(updated[0]) {
			t.Fatalf("updated_at moved: %v", updated)
		}
		want := []string{"thread.changed 70 current projection,participants,actions", "thread.changed 70 current contact"}
		if got := workspaceEvents(t, db, other); !slices.Equal(got, want) {
			t.Fatalf("events %q", got)
		}
	})

	t.Run("the same automatic outcome is no change", func(t *testing.T) {
		threads := storepostgres.NewEmailThreadStore(db)
		for i, want := range []bool{true, false} {
			if applied, err := threads.ResolveContact(ctx, 1, threadID, model.EmailContactResolutionUnavailable, nil); err != nil || applied != want {
				t.Fatalf("write %d: %v, %v", i, applied, err)
			}
		}
	})

	t.Run("disabled events keep the version", func(t *testing.T) {
		quiet := seedThread(t, db, 1, profile, "regular", "new", nil)
		if _, err := db.Exec(ctx, `UPDATE email.thread SET last_message_at = now() WHERE id = $1`, quiet); err != nil {
			t.Fatal(err)
		}
		active := projectionSnapshot(quiet, 30, 300, event.AttemptStateActive)
		active.Transition = "taken"
		applySnapshot(t, storepostgres.NewEmailThreadProjectionStore(db), active)
		if v, got := workspaceVersion(t, db, quiet), workspaceEvents(t, db, quiet); v != 1 || len(got) != 0 {
			t.Fatalf("version %d, events %v", v, got)
		}
	})

	t.Run("a failed event rolls the snapshot back", func(t *testing.T) {
		broken := seedThread(t, db, 1, profile, "regular", "new", nil)
		if _, err := db.Exec(ctx, `UPDATE email.thread SET last_message_at = now() WHERE id = $1`, broken); err != nil {
			t.Fatal(err)
		}
		// A row holding the id of the coming event makes its insert fail.
		taken := event.WorkspaceEventID(event.TypeWorkspaceThreadChanged, 1, broken, 1, 70)
		if _, err := db.Exec(ctx, `
INSERT INTO email.outbox (event_id, domain_id, event_type, schema_version, exchange, routing_key, payload)
VALUES ($1, 1, 'test', 1, 'x', 'x', '{}')`, taken.String()); err != nil {
			t.Fatal(err)
		}
		active := projectionSnapshot(broken, 40, 400, event.AttemptStateActive)
		active.Transition = "taken"
		p, err := projection.Project(active)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := projections.Apply(ctx, p); err == nil {
			t.Fatal("a snapshot was applied without its event")
		}
		row := readProjectedThread(t, db, broken)
		if row.version != "" || workspaceVersion(t, db, broken) != 0 ||
			count(t, db, `SELECT count(*) FROM email.thread_action WHERE thread_id = $1`, broken) != 0 {
			t.Fatalf("partial change left: %+v", row)
		}
	})
}

// Ready Messages, snapshots and contacts of one Thread take its lock in one order:
// none deadlocks, and every change gets its own version.
func TestWorkspaceVersionUnderConcurrentChanges(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	ctx := context.Background()
	profile := testhelpers.SeedProfile(t, db, 1, "workspace")
	projections := storepostgres.NewEmailThreadProjectionStore(db).WithWorkspaceEvents(true)
	contacts := storepostgres.NewEmailThreadContactStore(db, true)
	uow := storepostgres.NewUnitOfWork(db).WithWorkspaceEvents(true)

	threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
	if _, err := db.Exec(ctx, `UPDATE email.thread SET last_message_at = now() WHERE id = $1`, threadID); err != nil {
		t.Fatal(err)
	}
	taken := projectionSnapshot(threadID, 1, 100, event.AttemptStateActive)
	taken.Transition = "taken"
	applySnapshot(t, projections, taken)
	messageID := int64(count(t, db, `
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, message_id, received_at)
VALUES (1, $1, $2, 'inbound', '<ws-lock@x>', now()) RETURNING id`, threadID, profile))

	const rounds = 20
	var wg sync.WaitGroup
	errs := make(chan error, 3*rounds)
	for i := range rounds {
		wg.Add(3)
		go func() {
			defer wg.Done()
			renewed := projectionSnapshot(threadID, int64(10+i), 100, event.AttemptStateActive)
			renewed.Transition = "renewed"
			p, err := projection.Project(renewed)
			if err == nil {
				_, err = projections.Apply(ctx, p)
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			contactID := int64(55 + i%2)
			_, err := contacts.SetContactManually(ctx, 1, 70, threadID, &contactID)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			// The completion order: the Message first, then the Thread.
			errs <- uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
				if _, _, err := uow.EmailMessageStore().MarkReady(ctx, 1, messageID); err != nil {
					return err
				}
				if err := uow.EmailThreadStore().AdvanceLastMessage(ctx, 1, profile, threadID, time.Now()); err != nil {
					return err
				}
				_, err := uow.EmailWorkspaceEventStore().Record(ctx, store.EmailWorkspaceChange{
					DomainID: 1, ThreadID: threadID, EventType: event.TypeWorkspaceMessageCreated, MessageID: &messageID,
				})

				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// One event per version for the single recipient, with no gap or repeat.
	versions := count(t, db, `SELECT workspace_version FROM email.thread WHERE id = $1`, threadID)
	distinct := count(t, db, `
SELECT count(DISTINCT (payload->>'workspace_version')::bigint) FROM email.outbox
WHERE event_type LIKE 'email.workspace.%' AND (payload->>'thread_id')::bigint = $1`, threadID)
	if versions != distinct || versions < 1+2*rounds {
		t.Fatalf("version %d, distinct event versions %d", versions, distinct)
	}
}
