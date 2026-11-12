//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/projection"
	"github.com/webitel/webitel-emails/internal/service"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

type sendEnv struct {
	t       *testing.T
	db      *pgxpool.Pool
	profile int64
	inbound interface {
		Handle(context.Context, *model.ParsedEmail) error
	}
	projections store.EmailThreadProjectionStore
	contacts    *verifier
	uid         uint32
}

// verifier owns contact 5; fail makes the calls after the first one fail.
type verifier struct {
	mu    sync.Mutex
	calls int
	fail  error
}

func (v *verifier) VerifyContact(_ context.Context, _, contactID int64) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.fail != nil && v.calls > 1 {
		return false, v.fail
	}
	return contactID == 5, nil
}

func newSendEnv(t *testing.T) *sendEnv {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)

	return &sendEnv{
		t: t, db: db, profile: testhelpers.SeedProfile(t, db, 1, "send"),
		inbound:     newWorkspaceHandler(db, &stubUploader{}),
		projections: storepostgres.NewEmailThreadProjectionStore(db).WithWorkspaceEvents(true),
		contacts:    &verifier{},
	}
}

func (e *sendEnv) service(enabled, events bool) *service.EmailSendService {
	cfg := &config.Config{Outbound: config.OutboundConfig{
		Enabled: enabled, MaxRecipients: 50, MaxBodySize: 1 << 20, MaxAttachments: 15,
		MaxAttachmentsTotalSize: 20 << 20, MaxMessageSize: 30 << 20,
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reads := service.NewEmailThreadReadService(storepostgres.NewEmailThreadReadStore(e.db), nil, log)

	return service.NewEmailSendService(cfg, storepostgres.NewUnitOfWork(e.db).WithWorkspaceEvents(events), reads,
		storepostgres.NewEmailProfileStore(e.db), e.contacts)
}

// thread receives one inbound email and returns its Thread.
func (e *sendEnv) thread(messageID string) int64 {
	e.uid++
	if err := e.inbound.Handle(context.Background(), newEmail(e.profile, messageID, e.uid)); err != nil {
		e.t.Fatal(err)
	}

	return threadOf(e.t, e.db, messageID)
}

func (e *sendEnv) apply(snapshots ...event.AttemptSnapshot) {
	for _, s := range snapshots {
		applySnapshot(e.t, e.projections, s)
	}
}

func (e *sendEnv) upload(author int64, expiresAt time.Time) string {
	reference := uuid.New()
	_, err := e.db.Exec(context.Background(), `
INSERT INTO email.attachment_upload (domain_id, reference, author_user_id, file_id, file_name, mime_type, size, expires_at)
VALUES (1, $1, $2, 500, 'a.pdf', 'application/pdf', 10, $3)`, reference, author, expiresAt)
	if err != nil {
		e.t.Fatal(err)
	}

	return reference.String()
}

func mustProject(t *testing.T, e event.AttemptSnapshot) *store.EmailThreadProjection {
	t.Helper()
	p, err := projection.Project(e)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// waitForLocks waits until n sessions of the test database wait on a lock; the
// "advisory" kind narrows it to advisory locks.
func waitForLocks(t *testing.T, db *pgxpool.Pool, kind string, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		waiting := count(t, db, `SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock' AND ($1 = 'Lock' OR wait_event = $1)`, kind)
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait on %s, want %d", waiting, kind, n)
		}
	}
}

func session(user int64) *auth.Session { return &auth.Session{DomainID: 1, UserID: user} }

func reply(threadID, attemptID int64, references ...string) service.ReplyInput {
	return service.ReplyInput{
		ThreadID: threadID, RequestID: uuid.NewString(), AttemptID: attemptID,
		Message: service.OutboundMessageInput{
			To: []model.EmailAddress{{Address: "Client@X.com"}}, Bcc: []model.EmailAddress{{Address: "hidden@x.com"}},
			HTMLBody: "<p>Hello</p>", AttachmentReferences: references,
		},
	}
}

func wantID(t *testing.T, err error, id string) {
	t.Helper()
	if kiterrors.ID(err) != id {
		t.Fatalf("error = %v, want %s", err, id)
	}
}

func TestSendReplyRights(t *testing.T) {
	env := newSendEnv(t)
	svc := env.service(true, true)
	ctx := context.Background()

	taken := func(name string, state string) int64 {
		id := env.thread(name + "@x")
		env.apply(takenSnapshot(id, 10, 100, state, 7, 70))
		return id
	}
	processed := func(name string) int64 {
		id := taken(name, event.AttemptStateActive)
		env.apply(projectionSnapshot(id, 11, 100, event.AttemptStateLeaving))
		return id
	}

	active, processing := taken("r-active", event.AttemptStateActive), taken("r-processing", event.AttemptStateProcessing)
	postponed := taken("r-postponed", event.AttemptStatePostponed)
	distributed := env.thread("r-distributed@x")
	env.apply(offered(distributed, 10, 100, 7, 70))
	history := processed("r-history")
	reopened := processed("r-reopened")
	env.apply(takenSnapshot(reopened, 12, 101, event.AttemptStateActive, 8, 80))

	stale := reply(active, 100)
	stale.ExpectedAttemptVersion = 5
	otherAttempt := reply(active, 99)

	tests := []struct {
		name string
		user int64
		in   service.ReplyInput
		want string
	}{
		{"current active", 70, reply(active, 100), ""},
		{"current processing", 70, reply(processing, 100), ""},
		{"postponed", 70, reply(postponed, 100), "email.thread.reply_not_allowed"},
		{"distributed", 70, reply(distributed, 100), "email.thread.reply_not_allowed"},
		{"stale version", 70, stale, "email.thread.attempt_version_conflict"},
		{"other attempt", 70, otherAttempt, "email.thread.attempt_version_conflict"},
		{"current without attempt", 70, reply(active, 0), "email.thread.reply_not_allowed"},
		{"history", 70, reply(history, 0), ""},
		{"history with another agent", 70, reply(reopened, 0), "email.thread.reply_not_allowed"},
		{"not a participant", 90, reply(history, 0), "email.thread.not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, err := svc.SendReply(ctx, session(tt.user), tt.in)
			if tt.want != "" {
				wantID(t, err, tt.want)
				return
			}
			if err != nil || view.DeliveryStatus != model.EmailDeliveryStatusPending || view.Direction != model.EmailMessageDirectionOutbound {
				t.Fatalf("view = %+v, %v", view, err)
			}
		})
	}

	t.Run("another domain", func(t *testing.T) {
		_, err := svc.SendReply(ctx, &auth.Session{DomainID: 2, UserID: 70}, reply(active, 100))
		wantID(t, err, "email.thread.not_found")
	})
	t.Run("disabled", func(t *testing.T) {
		_, err := env.service(false, true).SendReply(ctx, session(70), reply(active, 100))
		wantID(t, err, "email.outbound.disabled")
	})
}

func TestSendReplyStoresAndReplays(t *testing.T) {
	env := newSendEnv(t)
	svc := env.service(true, true)
	ctx := context.Background()
	threadID := env.thread("s-first@x")
	env.apply(takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))
	before := workspaceVersion(t, env.db, threadID)
	flowEvents := count(t, env.db, `SELECT count(*) FROM email.outbox WHERE event_type NOT LIKE 'email.workspace.%'`)
	inboundAt, err := storepostgres.NewEmailMessageStore(env.db).LastReceivedAt(ctx, 1, env.profile)
	if err != nil {
		t.Fatal(err)
	}

	in := reply(threadID, 100, env.upload(70, time.Now().Add(time.Hour)))
	view, err := svc.SendReply(ctx, session(70), in)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case view.InReplyTo != "s-first@x" || view.Subject != "Re: Subject s-first@x":
		t.Fatalf("headers = %q %q", view.InReplyTo, view.Subject)
	case len(view.To) != 1 || view.To[0].Address != "Client@X.com" || len(view.Attachments) != 1:
		t.Fatalf("recipients or parts = %+v %+v", view.To, view.Attachments)
	case count(t, env.db, `SELECT count(*) FROM email.recipient WHERE message_id = $1 AND type = 'bcc'`, view.ID) != 1:
		t.Fatal("bcc not stored")
	case count(t, env.db, `SELECT count(*) FROM email.message_attachment WHERE message_id = $1 AND state = 'stored' AND file_id = 500`, view.ID) != 1:
		t.Fatal("manifest not stored")
	case count(t, env.db, `SELECT count(*) FROM email.attachment_upload WHERE used_by_message_id = $1`, view.ID) != 1:
		t.Fatal("reference not used")
	case count(t, env.db, `SELECT count(*) FROM email.thread WHERE id = $1 AND last_message_at = $2`, threadID, view.ReceivedAt) != 1:
		t.Fatal("last_message_at not advanced")
	case workspaceVersion(t, env.db, threadID) != before+1:
		t.Fatal("workspace version not raised")
	case count(t, env.db, `SELECT count(*) FROM email.outbox WHERE event_type NOT LIKE 'email.workspace.%'`) != flowEvents:
		t.Fatal("a Flow event was written")
	}
	if got := workspaceEvents(t, env.db, threadID); got[len(got)-1] != "message.created 70 current " {
		t.Fatalf("events = %v", got)
	}

	// A pending email cannot be a parent.
	pendingParent := reply(threadID, 100)
	pendingParent.InReplyToMessageID = view.ID
	_, err = svc.SendReply(ctx, session(70), pendingParent)
	wantID(t, err, "email.thread.reply_parent_not_found")

	// The Thread moves on and leaves the author's view; the same request still
	// returns its message, another one conflicts.
	env.apply(projectionSnapshot(threadID, 11, 100, event.AttemptStateLeaving))
	if _, err := env.db.Exec(ctx, `DELETE FROM email.thread_participant WHERE thread_id = $1`, threadID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.SendReply(ctx, session(70), in)
	if err != nil || again.ID != view.ID || again.DeliveryStatus != model.EmailDeliveryStatusPending {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	changed := in
	changed.Message.Subject = "other"
	_, err = svc.SendReply(ctx, session(70), changed)
	wantID(t, err, "email.message.request_conflict")

	// A sent email does not move the recovery checkpoint of the mailbox.
	if got, _ := storepostgres.NewEmailMessageStore(env.db).LastReceivedAt(ctx, 1, env.profile); !got.Equal(inboundAt) {
		t.Fatalf("checkpoint = %v, want %v", got, inboundAt)
	}

}

func TestSendReplyReferences(t *testing.T) {
	env := newSendEnv(t)
	svc := env.service(true, true)
	ctx := context.Background()
	threadID := env.thread("ref-first@x")
	env.apply(takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))

	used := env.upload(70, time.Now().Add(time.Hour))
	if _, err := svc.SendReply(ctx, session(70), reply(threadID, 100, used)); err != nil {
		t.Fatal(err)
	}
	for name, reference := range map[string]string{
		"used":    used,
		"foreign": env.upload(80, time.Now().Add(time.Hour)),
		"expired": env.upload(70, time.Now().Add(-time.Second)),
		"unknown": uuid.NewString(),
	} {
		_, err := svc.SendReply(ctx, session(70), reply(threadID, 100, reference))
		if kiterrors.ID(err) != "email.attachment.reference_unavailable" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	fresh := env.upload(70, time.Now().Add(time.Hour))
	_, err := svc.SendReply(ctx, session(70), reply(threadID, 100, fresh, fresh))
	wantID(t, err, "email.attachment.reference_invalid")

	t.Run("expires while waiting for the Thread", func(t *testing.T) {
		expiresAt := time.Now().Add(2 * time.Second)
		reference := env.upload(70, expiresAt)
		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Released even when the test fails, so a waiting request cannot hang the run.
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.thread WHERE id = $1 FOR UPDATE`, threadID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := svc.SendReply(ctx, session(70), reply(threadID, 100, reference))
			done <- err
		}()
		waitForLocks(t, env.db, "Lock", 1)
		if !time.Now().Before(expiresAt) {
			t.Fatal("send started waiting after the reference expired")
		}
		time.Sleep(time.Until(expiresAt) + 100*time.Millisecond)
		_ = tx.Rollback(ctx)
		wantID(t, <-done, "email.attachment.reference_unavailable")
	})
}

func TestSendReplyConcurrency(t *testing.T) {
	env := newSendEnv(t)
	svc := env.service(true, true)
	ctx := context.Background()
	first := env.thread("c-first@x")
	second := env.thread("c-second@x")
	env.apply(
		takenSnapshot(first, 10, 100, event.AttemptStateActive, 7, 70),
		takenSnapshot(second, 10, 200, event.AttemptStateActive, 7, 70),
	)

	t.Run("equal requests share one message", func(t *testing.T) {
		in := reply(first, 100, env.upload(70, time.Now().Add(time.Hour)))
		ids := make([]int64, 4)
		var wg sync.WaitGroup
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				view, err := svc.SendReply(ctx, session(70), in)
				if err != nil {
					t.Error(err)
					return
				}
				ids[i] = view.ID
			}()
		}
		wg.Wait()
		for _, id := range ids {
			if id != ids[0] {
				t.Fatalf("ids = %v", ids)
			}
		}
	})

	// A waits on the file; B with the same id in another Thread must see A's request,
	// not a used file.
	t.Run("one request id in two Threads", func(t *testing.T) {
		reference := env.upload(70, time.Now().Add(time.Hour))
		a := reply(first, 100, reference)
		b := reply(second, 200, reference)
		b.RequestID = a.RequestID

		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Released even when the test fails, so a waiting request cannot hang the run.
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.attachment_upload WHERE reference = $1 FOR UPDATE`, reference); err != nil {
			t.Fatal(err)
		}
		var errA, errB error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, errA = svc.SendReply(ctx, session(70), a) }()
		waitForLocks(t, env.db, "Lock", 1)
		go func() { defer wg.Done(); _, errB = svc.SendReply(ctx, session(70), b) }()
		waitForLocks(t, env.db, "advisory", 1)
		_ = tx.Rollback(ctx)
		wg.Wait()

		if errA != nil {
			t.Fatalf("a: %v", errA)
		}
		wantID(t, errB, "email.message.request_conflict")
	})

	t.Run("snapshots and sends do not deadlock", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 20)
		for i := range 10 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				p := takenSnapshot(first, int64(20+i), 100, event.AttemptStateActive, 7, 70)
				if _, err := env.projections.Apply(ctx, mustProject(t, p)); err != nil {
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := svc.SendReply(ctx, session(70), reply(first, 100)); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}

func TestSendReplyTransaction(t *testing.T) {
	env := newSendEnv(t)
	ctx := context.Background()
	threadID := env.thread("tx-first@x")
	env.apply(takenSnapshot(threadID, 10, 100, event.AttemptStateActive, 7, 70))

	t.Run("an outbox failure rolls everything back", func(t *testing.T) {
		if _, err := env.db.Exec(ctx, `
CREATE FUNCTION email.fail_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox down'; END $$;
CREATE TRIGGER fail_outbox BEFORE INSERT ON email.outbox FOR EACH ROW EXECUTE FUNCTION email.fail_outbox();`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = env.db.Exec(ctx, `DROP TRIGGER fail_outbox ON email.outbox; DROP FUNCTION email.fail_outbox();`)
		})
		version := workspaceVersion(t, env.db, threadID)
		reference := env.upload(70, time.Now().Add(time.Hour))
		if _, err := env.service(true, true).SendReply(ctx, session(70), reply(threadID, 100, reference)); err == nil {
			t.Fatal("send succeeded")
		}
		if count(t, env.db, `SELECT count(*) FROM email.message WHERE direction = 'outbound'`) != 0 ||
			count(t, env.db, `SELECT count(*) FROM email.attachment_upload WHERE used_by_message_id IS NOT NULL`) != 0 ||
			workspaceVersion(t, env.db, threadID) != version {
			t.Fatal("partial send was committed")
		}
	})

	t.Run("without events the version still grows", func(t *testing.T) {
		version := workspaceVersion(t, env.db, threadID)
		events := len(workspaceEvents(t, env.db, threadID))
		if _, err := env.service(true, false).SendReply(ctx, session(70), reply(threadID, 100)); err != nil {
			t.Fatal(err)
		}
		if workspaceVersion(t, env.db, threadID) != version+1 || len(workspaceEvents(t, env.db, threadID)) != events {
			t.Fatal("version or events after a send without events")
		}
	})
}

// A Thread holding only outbound email replies to the latest sent one.
func TestSendReplyParentOfOutboundThread(t *testing.T) {
	env := newSendEnv(t)
	ctx := context.Background()
	threadID := seedThread(t, env.db, 1, env.profile, "regular", "processed", ptrTime(time.Now()))
	for _, query := range []string{`
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, state, message_id, received_at,
    author_user_id, request_id, request_fingerprint, delivery_status, delivered_at)
VALUES (1, $1, $2, 'outbound', 'ready', 'sent@example.org', now() - interval '1 hour', 70, gen_random_uuid(),
    decode(repeat('00', 32), 'hex'), 'sent', now())`, `
INSERT INTO email.thread_participant (domain_id, thread_id, user_id, outbound_at, first_participated_at, last_participated_at)
SELECT 1, $1, 70, now(), now(), now() WHERE $2::bigint > 0`, `
UPDATE email.thread SET last_message_at = now() - interval '1 hour' WHERE id = $1 AND $2::bigint > 0`,
	} {
		if _, err := env.db.Exec(ctx, query, threadID, env.profile); err != nil {
			t.Fatal(err)
		}
	}

	view, err := env.service(true, true).SendReply(ctx, session(70), reply(threadID, 0))
	if err != nil || view.InReplyTo != "sent@example.org" {
		t.Fatalf("view = %+v, %v", view, err)
	}
}

func TestAttachmentManifestWithFiles(t *testing.T) {
	env := newSendEnv(t)
	ctx := context.Background()
	env.thread("m-first@x")
	messageID := int64(count(t, env.db, `SELECT id FROM email.message WHERE message_id = 'm-first@x'`))
	fileID := int64(9)
	err := storepostgres.NewEmailMessageAttachmentStore(env.db).CreateBulk(ctx, 1, messageID, []*model.EmailMessageAttachment{
		{Disposition: model.EmailPartDispositionAttachment, FileName: "a", MimeType: "text/plain", Position: 10, State: model.EmailAttachmentStatePending},
		{Disposition: model.EmailPartDispositionAttachment, FileName: "b", MimeType: "text/plain", Position: 11,
			State: model.EmailAttachmentStateSkipped, SkippedReason: model.EmailPartSkippedSizeLimit},
		{Disposition: model.EmailPartDispositionAttachment, FileName: "c", MimeType: "text/plain", Position: 12,
			State: model.EmailAttachmentStateStored, FileID: &fileID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, env.db, `SELECT count(*) FROM email.message_attachment WHERE message_id = $1 AND position >= 10
    AND (file_id IS NULL) = (state <> 'stored') AND coalesce(file_id, 9) = 9`, messageID); n != 3 {
		t.Fatalf("manifest rows = %d", n)
	}
}
