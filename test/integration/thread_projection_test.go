//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/projection"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

type projectedThread struct {
	status, queue, attempt, attemptVersion, agent, user, handledAgent, handledUser, handledVersion, version string
	completed, postponed, changed                                                                           sql.NullTime
}

func readProjectedThread(t *testing.T, db *pgxpool.Pool, threadID int64) projectedThread {
	t.Helper()
	var row projectedThread
	if err := db.QueryRow(context.Background(), `
SELECT status, coalesce(queue_id::text, ''), coalesce(current_attempt_id::text, ''),
       coalesce(current_attempt_version::text, ''), coalesce(current_agent_id::text, ''),
       coalesce(current_user_id::text, ''), coalesce(last_handled_agent_id::text, ''),
       coalesce(last_handled_user_id::text, ''), coalesce(last_handled_version::text, ''),
       coalesce(projection_version::text, ''), completed_at, postponed_until, status_changed_at
FROM email.thread WHERE id = $1`, threadID).Scan(
		&row.status, &row.queue, &row.attempt, &row.attemptVersion, &row.agent, &row.user,
		&row.handledAgent, &row.handledUser, &row.handledVersion, &row.version,
		&row.completed, &row.postponed, &row.changed,
	); err != nil {
		t.Fatalf("read projected thread: %v", err)
	}
	return row
}

func projectionSnapshot(threadID, version, attemptID int64, state string) event.AttemptSnapshot {
	queueID, attemptVersion := int64(3), int64(0)
	e := event.AttemptSnapshot{
		EventID: uuid.New(), EventType: event.TypeAttemptSnapshot, SchemaVersion: event.AttemptSnapshotSchema,
		OccurredAt: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC).Add(time.Duration(version) * time.Second),
		DomainID:   1, ThreadID: threadID, ProjectionVersion: version, Transition: "created", Source: "system",
		AttemptID: &attemptID, AttemptVersion: &attemptVersion, AttemptState: &state, QueueID: &queueID,
	}
	if state == event.AttemptStateActive || state == event.AttemptStateLeaving {
		agentID, userID := int64(7), int64(70)
		e.AgentID, e.AgentUserID = &agentID, &userID
	}
	if state == event.AttemptStateLeaving {
		result := event.AttemptResultProcessed
		e.Transition, e.Result = "completed", &result
	}
	return e
}

func applySnapshot(t *testing.T, s store.EmailThreadProjectionStore, e event.AttemptSnapshot) store.EmailThreadProjectionResult {
	t.Helper()
	if err := e.Validate(); err != nil {
		t.Fatalf("invalid snapshot fixture: %v", err)
	}
	p, err := projection.Project(e)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestThreadProjectionOrderingAndReopen(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "projection")
	s := storepostgres.NewEmailThreadProjectionStore(db)

	t.Run("duplicate, older and gap in sequence", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		waiting := projectionSnapshot(threadID, 10, 100, event.AttemptStateWaiting)
		active := projectionSnapshot(threadID, 12, 100, event.AttemptStateActive)
		active.Transition = "taken"
		if got := applySnapshot(t, s, waiting); got.Outcome != store.EmailThreadProjectionApplied {
			t.Fatalf("first snapshot: %+v", got)
		}
		if got := applySnapshot(t, s, active); got.Outcome != store.EmailThreadProjectionApplied {
			t.Fatalf("snapshot after sequence gap: %+v", got)
		}
		for _, old := range []event.AttemptSnapshot{active, waiting} {
			if got := applySnapshot(t, s, old); got.Outcome != store.EmailThreadProjectionStale {
				t.Fatalf("duplicate or older snapshot: %+v", got)
			}
		}
		row := readProjectedThread(t, db, threadID)
		if row.status != "in_progress" || row.version != "12" || row.attempt != "100" || row.agent != "7" || row.user != "70" {
			t.Fatalf("projection moved backwards: %+v", row)
		}
		postponed := projectionSnapshot(threadID, 14, 100, event.AttemptStatePostponed)
		postponed.Transition = "postponed"
		postponed.AgentID, postponed.AgentUserID = ptrInt64(7), ptrInt64(70)
		postponed.PostponedUntil = ptrTime(postponed.OccurredAt.Add(time.Hour))
		applySnapshot(t, s, postponed)
		if row = readProjectedThread(t, db, threadID); row.status != "postponed" || !row.postponed.Valid {
			t.Fatalf("postponed deadline missing: %+v", row)
		}
		resumed := projectionSnapshot(threadID, 15, 100, event.AttemptStateActive)
		resumed.Transition = "resumed"
		applySnapshot(t, s, resumed)
		if row = readProjectedThread(t, db, threadID); row.status != "in_progress" || row.postponed.Valid || row.agent != "7" {
			t.Fatalf("resuming did not clear deadline: %+v", row)
		}

		leaving := projectionSnapshot(threadID, 20, 100, event.AttemptStateLeaving)
		if got := applySnapshot(t, s, leaving); got.Outcome != store.EmailThreadProjectionApplied || !got.HandledApplied {
			t.Fatalf("final snapshot: %+v", got)
		}
		marker := projectionSnapshot(threadID, 21, 100, event.AttemptStateWaiting)
		marker.Transition, marker.AttemptID, marker.AttemptVersion, marker.AttemptState, marker.QueueID =
			event.TransitionInboundReopened, nil, nil, nil, nil
		if got := applySnapshot(t, s, marker); got.Outcome != store.EmailThreadProjectionApplied {
			t.Fatalf("reopen marker: %+v", got)
		}
		row = readProjectedThread(t, db, threadID)
		if row.status != "new" || row.version != "21" || row.queue != "3" || row.attempt != "" || row.agent != "" ||
			row.handledAgent != "7" || row.handledUser != "70" || row.postponed.Valid || row.completed.Valid {
			t.Fatalf("marker did not reopen and preserve history: %+v", row)
		}
		if got := applySnapshot(t, s, leaving); got.Outcome != store.EmailThreadProjectionStale {
			t.Fatalf("late final snapshot: %+v", got)
		}
		if row = readProjectedThread(t, db, threadID); row.status != "new" || row.version != "21" {
			t.Fatalf("late final snapshot closed the thread: %+v", row)
		}
	})

	t.Run("late final snapshot keeps the last handler after a new attempt", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		marker := projectionSnapshot(threadID, 21, 100, event.AttemptStateWaiting)
		marker.Transition, marker.AttemptID, marker.AttemptVersion, marker.AttemptState, marker.QueueID =
			event.TransitionInboundReopened, nil, nil, nil, nil
		applySnapshot(t, s, marker)
		newAttempt := projectionSnapshot(threadID, 30, 101, event.AttemptStateWaiting)
		applySnapshot(t, s, newAttempt)
		lateFinal := projectionSnapshot(threadID, 20, 100, event.AttemptStateLeaving)
		if got := applySnapshot(t, s, lateFinal); got.Outcome != store.EmailThreadProjectionStale || !got.HandledApplied {
			t.Fatalf("late last handler: %+v", got)
		}
		row := readProjectedThread(t, db, threadID)
		if row.status != "new" || row.version != "30" || row.attempt != "101" || row.handledAgent != "7" || row.handledVersion != "20" {
			t.Fatalf("late last handler replaced current attempt: %+v", row)
		}
	})

	t.Run("processing timeout closes the thread and clears the current agent", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		applySnapshot(t, s, projectionSnapshot(threadID, 10, 100, event.AttemptStateActive))
		timedOut := projectionSnapshot(threadID, 11, 100, event.AttemptStateLeaving)
		timedOut.Transition = "processing_timeout"
		result := event.AttemptResultProcessingTimeout
		timedOut.Result = &result
		if got := applySnapshot(t, s, timedOut); got.Outcome != store.EmailThreadProjectionApplied || !got.HandledApplied {
			t.Fatalf("processing timeout: %+v", got)
		}
		row := readProjectedThread(t, db, threadID)
		if row.status != "processed" || row.attempt != "" || row.agent != "" || row.user != "" ||
			row.postponed.Valid || !row.completed.Valid || row.handledAgent != "7" || row.handledUser != "70" ||
			row.handledVersion != "11" {
			t.Fatalf("processing timeout left current ownership or lost history: %+v", row)
		}
	})

	t.Run("older final snapshot does not replace the newer handler", func(t *testing.T) {
		threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
		newAttempt := projectionSnapshot(threadID, 30, 101, event.AttemptStateActive)
		newAttempt.AgentID, newAttempt.AgentUserID = ptrInt64(8), ptrInt64(80)
		applySnapshot(t, s, newAttempt)
		newFinal := projectionSnapshot(threadID, 40, 101, event.AttemptStateLeaving)
		newFinal.AgentID, newFinal.AgentUserID = ptrInt64(8), ptrInt64(80)
		applySnapshot(t, s, newFinal)
		oldFinal := projectionSnapshot(threadID, 20, 100, event.AttemptStateLeaving)
		if got := applySnapshot(t, s, oldFinal); got.Outcome != store.EmailThreadProjectionStale || got.HandledApplied {
			t.Fatalf("older final snapshot: %+v", got)
		}
		row := readProjectedThread(t, db, threadID)
		if row.status != "processed" || row.version != "40" || row.handledAgent != "8" ||
			row.handledUser != "80" || row.handledVersion != "40" {
			t.Fatalf("older final snapshot replaced the latest handler: %+v", row)
		}
	})
}

func TestThreadProjectionIsolationRollbackAndConcurrency(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profile := testhelpers.SeedProfile(t, db, 1, "projection-isolation")
	s := storepostgres.NewEmailThreadProjectionStore(db)
	threadID := seedThread(t, db, 1, profile, "regular", "new", nil)
	serviceID := seedThread(t, db, 1, profile, "service", "processed", ptrTime(time.Now().UTC()))

	if got := applySnapshot(t, s, projectionSnapshot(serviceID, 10, 100, event.AttemptStateWaiting)); got.Outcome != store.EmailThreadProjectionService {
		t.Fatalf("service thread projected: %+v", got)
	}
	foreign := projectionSnapshot(threadID, 10, 100, event.AttemptStateWaiting)
	foreign.DomainID = 2
	if got := applySnapshot(t, s, foreign); got.Outcome != store.EmailThreadProjectionNotFound {
		t.Fatalf("foreign domain projected: %+v", got)
	}
	if got := applySnapshot(t, s, projectionSnapshot(1<<60, 10, 100, event.AttemptStateWaiting)); got.Outcome != store.EmailThreadProjectionNotFound {
		t.Fatalf("missing thread projected: %+v", got)
	}

	bad := &store.EmailThreadProjection{
		DomainID: 1, ThreadID: threadID, Version: 15, OccurredAt: time.Now().UTC(), Status: model.EmailThreadStatusNew,
		HandledAgentID: ptrInt64(7),
	}
	if _, err := s.Apply(context.Background(), bad); !errors.Is(err, store.ErrEmailThreadProjectionRejected) {
		t.Fatalf("invalid last handler was not rejected: %v", err)
	}
	if row := readProjectedThread(t, db, threadID); row.version != "" || row.handledAgent != "" {
		t.Fatalf("partial update after rollback: %+v", row)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, snapshot := range []event.AttemptSnapshot{
		projectionSnapshot(threadID, 20, 100, event.AttemptStateWaiting),
		projectionSnapshot(threadID, 30, 100, event.AttemptStateActive),
	} {
		wg.Add(1)
		go func(e event.AttemptSnapshot) {
			defer wg.Done()
			<-start
			p, err := projection.Project(e)
			if err == nil {
				_, err = s.Apply(context.Background(), p)
			}
			results <- err
		}(snapshot)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("parallel apply: %v", err)
		}
	}
	if row := readProjectedThread(t, db, threadID); row.status != "in_progress" || row.version != "30" || row.agent != "7" {
		t.Fatalf("parallel snapshots were not monotonic: %+v", row)
	}
}

func ptrInt64(v int64) *int64        { return &v }
func ptrTime(v time.Time) *time.Time { return &v }
