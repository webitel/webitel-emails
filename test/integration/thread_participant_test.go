//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/projection"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

type participantRow struct {
	agent                     int64
	firstVersion, lastVersion sql.NullInt64
	firstAt, lastAt           sql.NullTime
	backfilledAt              sql.NullTime
}

func readParticipants(t *testing.T, db *pgxpool.Pool, threadID int64) map[int64]participantRow {
	t.Helper()
	rows, err := db.Query(context.Background(), `
SELECT user_id, agent_id, first_version, last_version, first_participated_at, last_participated_at, backfilled_at
FROM email.thread_participant WHERE thread_id = $1`, threadID)
	if err != nil {
		t.Fatalf("read participants: %v", err)
	}
	defer rows.Close()
	out := map[int64]participantRow{}
	for rows.Next() {
		var user int64
		var row participantRow
		if err := rows.Scan(&user, &row.agent, &row.firstVersion, &row.lastVersion,
			&row.firstAt, &row.lastAt, &row.backfilledAt); err != nil {
			t.Fatalf("scan participant: %v", err)
		}
		out[user] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("participants: %v", err)
	}
	return out
}

// takenSnapshot is a snapshot of state with the given agent and user.
func takenSnapshot(threadID, version, attemptID int64, state string, agent, user int64) event.AttemptSnapshot {
	e := projectionSnapshot(threadID, version, attemptID, state)
	e.AgentID, e.AgentUserID = ptrInt64(agent), ptrInt64(user)
	switch state {
	case event.AttemptStateOffering:
		e.Transition = "assigned"
	case event.AttemptStateActive:
		e.Transition = "taken"
	case event.AttemptStatePostponed:
		e.Transition = "postponed"
		e.PostponedUntil = ptrTime(e.OccurredAt.Add(time.Hour))
	case event.AttemptStateProcessing:
		e.Transition = "processing_started"
	}
	return e
}

func TestThreadParticipantJournal(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "participants")
	s := storepostgres.NewEmailThreadProjectionStore(db)

	t.Run("an offer alone records nobody, a take records the agent", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		offer := takenSnapshot(threadID, 10, 100, event.AttemptStateOffering, 7, 70)
		if got := applySnapshot(t, s, offer); got.ParticipantRecorded {
			t.Fatalf("offer recorded a participant: %+v", got)
		}
		released := projectionSnapshot(threadID, 11, 100, event.AttemptStateWaiting)
		released.Transition = "auto_released"
		applySnapshot(t, s, released)
		if rows := readParticipants(t, db, threadID); len(rows) != 0 {
			t.Fatalf("auto released offer left a participant: %+v", rows)
		}

		take := takenSnapshot(threadID, 12, 100, event.AttemptStateActive, 8, 80)
		if got := applySnapshot(t, s, take); !got.ParticipantRecorded {
			t.Fatalf("take did not record the participant: %+v", got)
		}
		row, ok := readParticipants(t, db, threadID)[80]
		if !ok || row.agent != 8 || row.firstVersion.Int64 != 12 || row.lastVersion.Int64 != 12 ||
			!row.firstAt.Time.Equal(take.OccurredAt) || row.backfilledAt.Valid {
			t.Fatalf("participant after take: %+v", row)
		}
	})

	t.Run("order, duplicates and stale snapshots only widen the row", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		final := projectionSnapshot(threadID, 20, 100, event.AttemptStateLeaving)
		applySnapshot(t, s, final)
		take := takenSnapshot(threadID, 12, 100, event.AttemptStateActive, 7, 70)
		if got := applySnapshot(t, s, take); got.Outcome != store.EmailThreadProjectionStale || !got.ParticipantRecorded {
			t.Fatalf("late take: %+v", got)
		}
		if got := applySnapshot(t, s, take); got.ParticipantRecorded {
			t.Fatalf("duplicate take changed the row: %+v", got)
		}
		row := readParticipants(t, db, threadID)[70]
		if row.firstVersion.Int64 != 12 || row.lastVersion.Int64 != 20 ||
			!row.firstAt.Time.Equal(take.OccurredAt) || !row.lastAt.Time.Equal(final.OccurredAt) {
			t.Fatalf("participant range: %+v", row)
		}

		marker := projectionSnapshot(threadID, 21, 100, event.AttemptStateWaiting)
		marker.Transition, marker.AttemptID, marker.AttemptVersion, marker.AttemptState, marker.QueueID =
			event.TransitionInboundReopened, nil, nil, nil, nil
		applySnapshot(t, s, marker)
		lateOther := takenSnapshot(threadID, 15, 100, event.AttemptStateProcessing, 9, 90)
		if got := applySnapshot(t, s, lateOther); got.Outcome != store.EmailThreadProjectionStale || !got.ParticipantRecorded {
			t.Fatalf("stale snapshot after the marker: %+v", got)
		}
		if rows := readParticipants(t, db, threadID); len(rows) != 2 || rows[90].agent != 9 {
			t.Fatalf("participants after the marker: %+v", rows)
		}
	})

	t.Run("the agent follows only a newer version", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		applySnapshot(t, s, takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))
		applySnapshot(t, s, takenSnapshot(threadID, 5, 99, event.AttemptStatePostponed, 6, 70))
		if row := readParticipants(t, db, threadID)[70]; row.agent != 7 || row.firstVersion.Int64 != 5 {
			t.Fatalf("older version replaced the agent: %+v", row)
		}
		applySnapshot(t, s, takenSnapshot(threadID, 30, 101, event.AttemptStateActive, 9, 70))
		if row := readParticipants(t, db, threadID)[70]; row.agent != 9 || row.lastVersion.Int64 != 30 {
			t.Fatalf("newer version kept the old agent: %+v", row)
		}
	})

	t.Run("foreign domain and service thread gain no row", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		serviceID := seedThread(t, db, 1, profile, "service", "processed", ptrTime(time.Now().UTC()))
		foreign := takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70)
		foreign.DomainID = 2
		if got := applySnapshot(t, s, foreign); got.Outcome != store.EmailThreadProjectionNotFound || got.ParticipantRecorded {
			t.Fatalf("foreign domain: %+v", got)
		}
		if got := applySnapshot(t, s, takenSnapshot(serviceID, 10, 100, event.AttemptStateActive, 7, 70)); got.ParticipantRecorded {
			t.Fatalf("service thread: %+v", got)
		}
		if len(readParticipants(t, db, threadID)) != 0 || len(readParticipants(t, db, serviceID)) != 0 {
			t.Fatal("rows written for a foreign domain or a service thread")
		}
	})

	t.Run("a snapshot refines a backfilled row and keeps its origin", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "processed", ptrTime(time.Now().UTC()))
		if _, err := db.Exec(context.Background(), `
INSERT INTO email.thread_participant (domain_id, thread_id, user_id, agent_id, backfilled_at)
VALUES (1, $1, 70, 6, now())`, threadID); err != nil {
			t.Fatal(err)
		}
		take := takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70)
		if got := applySnapshot(t, s, take); !got.ParticipantRecorded {
			t.Fatalf("snapshot did not refine the backfilled row: %+v", got)
		}
		row := readParticipants(t, db, threadID)[70]
		if row.agent != 7 || row.firstVersion.Int64 != 10 || row.lastVersion.Int64 != 10 ||
			!row.firstAt.Valid || !row.backfilledAt.Valid {
			t.Fatalf("refined backfilled row: %+v", row)
		}
	})

	t.Run("the schema refuses a snapshot row without a version and cascades", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		_, err := db.Exec(context.Background(), `
INSERT INTO email.thread_participant (domain_id, thread_id, user_id, agent_id) VALUES (1, $1, 70, 7)`, threadID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "thread_participant_origin_valid" {
			t.Fatalf("row without version or origin: %v", err)
		}
		_, err = db.Exec(context.Background(), `
INSERT INTO email.thread_participant (domain_id, thread_id, user_id, agent_id, backfilled_at)
VALUES (2, $1, 70, 7, now())`, threadID)
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "thread_participant_thread_fk" {
			t.Fatalf("row of a thread of another domain: %v", err)
		}
		applySnapshot(t, s, takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))
		if _, err := db.Exec(context.Background(), `DELETE FROM email.thread WHERE id = $1`, threadID); err != nil {
			t.Fatal(err)
		}
		if rows := readParticipants(t, db, threadID); len(rows) != 0 {
			t.Fatalf("participants outlived their thread: %+v", rows)
		}
	})

	t.Run("parallel snapshots of one participant end in one widened row", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		snapshots := []event.AttemptSnapshot{
			takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70),
			takenSnapshot(threadID, 11, 100, event.AttemptStatePostponed, 7, 70),
			takenSnapshot(threadID, 12, 100, event.AttemptStateActive, 7, 70),
			projectionSnapshot(threadID, 13, 100, event.AttemptStateLeaving),
		}
		start := make(chan struct{})
		errs := make(chan error, len(snapshots))
		var wg sync.WaitGroup
		for _, e := range snapshots {
			wg.Add(1)
			go func(e event.AttemptSnapshot) {
				defer wg.Done()
				<-start
				p, err := projection.Project(e)
				if err == nil {
					_, err = s.Apply(context.Background(), p)
				}
				errs <- err
			}(e)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("parallel apply: %v", err)
			}
		}
		rows := readParticipants(t, db, threadID)
		if row := rows[70]; len(rows) != 1 || row.firstVersion.Int64 != 10 || row.lastVersion.Int64 != 13 || row.agent != 7 {
			t.Fatalf("parallel participant: %+v", rows)
		}
	})
}
