//go:build integration

package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	contactsinfra "github.com/webitel/webitel-emails/infra/contacts"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func (e *sendEnv) allowSend(profileID int64) {
	if _, err := e.db.Exec(context.Background(), `UPDATE email.profile SET allow_send = true WHERE id = $1`, profileID); err != nil {
		e.t.Fatal(err)
	}
}

func newThread(profileID int64, mode service.ContactMode, contactID int64, references ...string) service.CreateThreadInput {
	return service.CreateThreadInput{
		RequestID: uuid.NewString(), ProfileID: profileID, ContactMode: mode, ContactID: contactID,
		Message: service.OutboundMessageInput{
			To: []model.EmailAddress{{Address: "Client@X.com"}}, Subject: "Offer",
			HTMLBody: "<p>Hello</p>", AttachmentReferences: references,
		},
	}
}

func TestCreateEmailThread(t *testing.T) {
	env := newSendEnv(t)
	env.allowSend(env.profile)
	svc := env.service(true, true)
	ctx := context.Background()
	flowEvents := func() int {
		return count(t, env.db, `SELECT count(*) FROM email.outbox WHERE event_type NOT LIKE 'email.workspace.%'`)
	}

	thread, message, err := svc.CreateThread(ctx, session(70), newThread(env.profile, service.ContactModeNone, 0))
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case thread == nil || thread.ID != message.ThreadID || thread.Status != model.EmailThreadStatusProcessed ||
		thread.CompletedAt == nil || thread.ContactResolutionState != model.EmailContactResolutionUnlinked ||
		thread.Access != model.EmailThreadAccessHistory:
		t.Fatalf("thread = %+v", thread)
	case message.DeliveryStatus != model.EmailDeliveryStatusPending || message.Subject != "Offer":
		t.Fatalf("message = %+v", message)
	case count(t, env.db, `SELECT count(*) FROM email.thread_participant
WHERE thread_id = $1 AND user_id = 70 AND agent_id IS NULL AND outbound_at IS NOT NULL`, thread.ID) != 1:
		t.Fatal("author not in the journal")
	case flowEvents() != 0:
		t.Fatal("a Flow event was written")
	case workspaceVersion(t, env.db, thread.ID) != 1:
		t.Fatal("workspace version is not 1")
	case count(t, env.db, `SELECT count(*) FROM email.outbox WHERE event_type = 'email.workspace.message.created'
    AND (payload->>'thread_id')::bigint = $1 AND (payload->>'message_id')::bigint = $2`, thread.ID, message.ID) != 1:
		t.Fatal("no message.created of the new email")
	}
	if got := workspaceEvents(t, env.db, thread.ID); len(got) != 1 || got[0] != "message.created 70 history " {
		t.Fatalf("events = %v", got)
	}
	reads := storepostgres.NewEmailThreadReadStore(env.db)
	if other, _ := reads.LocateThread(ctx, 1, 80, thread.ID); other != nil {
		t.Fatal("another user sees the Thread")
	}
	if history, _, err := reads.ListThreads(ctx, model.EmailThreadFilter{
		DomainID: 1, UserID: 70, Assignment: model.EmailThreadAssignmentHistory, Page: 1, Size: 10,
	}); err != nil || len(history) != 1 || history[0].ID != thread.ID {
		t.Fatalf("history = %v, %v", history, err)
	}

	// The author writes again from history once the first email is sent.
	_, err = svc.SendReply(ctx, session(70), reply(thread.ID, 0))
	wantID(t, err, "email.thread.reply_parent_not_found")
	if _, err := env.db.Exec(ctx, `UPDATE email.message SET delivery_status = 'sent', delivery_phase = NULL,
    delivered_at = now() WHERE id = $1`, message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendReply(ctx, session(70), reply(thread.ID, 0)); err != nil {
		t.Fatalf("reply from history: %v", err)
	}

	// The client's answer continues the Thread and starts Flow; a copy of our own
	// email is a duplicate; neither changes the chosen contact.
	env.uid++
	if err := env.inbound.Handle(ctx, newEmail(env.profile, "client-answer@x", env.uid, withInReplyTo(message.InternetMessageID))); err != nil {
		t.Fatal(err)
	}
	env.uid++
	if err := env.inbound.Handle(ctx, newEmail(env.profile, message.InternetMessageID, env.uid)); err != nil {
		t.Fatal(err)
	}
	switch {
	case threadOf(t, env.db, "client-answer@x") != thread.ID:
		t.Fatal("the answer opened another Thread")
	case flowEvents() != 1:
		t.Fatalf("flow events = %d", flowEvents())
	case count(t, env.db, `SELECT count(*) FROM email.message WHERE message_id = $1`, message.InternetMessageID) != 1:
		t.Fatal("the copy of our email was stored again")
	case count(t, env.db, `SELECT count(*) FROM email.thread WHERE id = $1 AND contact_resolution_state = 'unlinked'`, thread.ID) != 1:
		t.Fatal("the contact choice changed")
	}

	linked, _, err := svc.CreateThread(ctx, session(70), newThread(env.profile, service.ContactModeLink, 5))
	if err != nil || linked.ContactID == nil || *linked.ContactID != 5 || linked.ContactResolutionState != model.EmailContactResolutionResolved {
		t.Fatalf("linked = %+v, %v", linked, err)
	}
}

func TestCreateEmailThreadRejects(t *testing.T) {
	env := newSendEnv(t)
	ctx := context.Background()
	replyOnly := env.profile
	foreign := testhelpers.SeedProfile(t, env.db, 2, "foreign")
	disabled := testhelpers.SeedProfile(t, env.db, 1, "disabled")
	env.allowSend(foreign)
	env.allowSend(disabled)
	if _, err := env.db.Exec(ctx, `UPDATE email.profile SET enabled = false WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	svc := env.service(true, true)

	tests := []struct {
		name string
		in   service.CreateThreadInput
		want string
	}{
		{"profile without allow_send", newThread(replyOnly, service.ContactModeNone, 0), "email.profile.send_unavailable"},
		{"profile of another domain", newThread(foreign, service.ContactModeNone, 0), "store.email_profile.not_found"},
		{"foreign contact", newThread(disabled, service.ContactModeLink, 6), "email.thread.contact_not_found"},
		{"no contact mode", newThread(disabled, service.ContactModeUnspecified, 0), "email.message.invalid_contact_mode"},
		{"zero profile", newThread(0, service.ContactModeLink, 5), "email.profile.id_required"},
		{"negative profile", newThread(-1, service.ContactModeNone, 0), "email.profile.id_required"},
		{"none with a contact", newThread(disabled, service.ContactModeNone, 5), "email.message.invalid_contact_mode"},
		{"profile with polling disabled", newThread(disabled, service.ContactModeNone, 0), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := env.contacts.calls
			_, _, err := svc.CreateThread(ctx, session(70), tt.in)
			if tt.want == "" && err != nil || tt.want != "" && kiterrors.ID(err) != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			// An invalid request never reaches Contacts.
			if tt.want == "email.profile.id_required" && env.contacts.calls != calls {
				t.Fatal("contacts were asked")
			}
		})
	}

	t.Run("contacts unavailable", func(t *testing.T) {
		env.contacts.calls, env.contacts.fail = 1, contactsinfra.ErrUnavailable
		defer func() { env.contacts.fail = nil }()
		_, _, err := svc.CreateThread(ctx, session(70), newThread(disabled, service.ContactModeLink, 5))
		wantID(t, err, "contacts.client.unavailable")
	})
	t.Run("disabled", func(t *testing.T) {
		_, _, err := env.service(false, true).CreateThread(ctx, session(70), newThread(disabled, service.ContactModeNone, 0))
		wantID(t, err, "email.outbound.disabled")
	})
}

func TestCreateEmailThreadReplay(t *testing.T) {
	env := newSendEnv(t)
	env.allowSend(env.profile)
	svc := env.service(true, true)
	ctx := context.Background()

	t.Run("equal requests share one Thread", func(t *testing.T) {
		in := newThread(env.profile, service.ContactModeNone, 0, env.upload(70, time.Now().Add(time.Hour)))
		ids := make([]int64, 4)
		var wg sync.WaitGroup
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				thread, _, err := svc.CreateThread(ctx, session(70), in)
				if err != nil {
					t.Error(err)
					return
				}
				ids[i] = thread.ID
			}()
		}
		wg.Wait()
		for _, id := range ids {
			if id != ids[0] {
				t.Fatalf("ids = %v", ids)
			}
		}
	})

	// The first request holds the request lock while it waits for its file; the
	// second fails to reach Contacts and still gets the first one's result.
	t.Run("a Contacts failure does not hide the stored request", func(t *testing.T) {
		reference := env.upload(70, time.Now().Add(time.Hour))
		in := newThread(env.profile, service.ContactModeLink, 5, reference)
		env.contacts.calls, env.contacts.fail = 0, contactsinfra.ErrUnavailable
		defer func() { env.contacts.fail = nil }()

		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Released even when the test fails, so a waiting request cannot hang the run.
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `SELECT 1 FROM email.attachment_upload WHERE reference = $1 FOR UPDATE`, reference); err != nil {
			t.Fatal(err)
		}
		var first, second *model.EmailMessageView
		var errFirst, errSecond error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, first, errFirst = svc.CreateThread(ctx, session(70), in) }()
		waitForLocks(t, env.db, "Lock", 1)
		go func() { defer wg.Done(); _, second, errSecond = svc.CreateThread(ctx, session(70), in) }()
		waitForLocks(t, env.db, "advisory", 1)
		_ = tx.Rollback(ctx)
		wg.Wait()

		if errFirst != nil || errSecond != nil || first.ID != second.ID {
			t.Fatalf("first %v %v, second %v %v", first, errFirst, second, errSecond)
		}
	})

	t.Run("a Thread out of sight is told by its id only", func(t *testing.T) {
		in := newThread(env.profile, service.ContactModeNone, 0)
		thread, message, err := svc.CreateThread(ctx, session(70), in)
		if err != nil {
			t.Fatal(err)
		}
		env.uid++
		if err := env.inbound.Handle(ctx, newEmail(env.profile, "later@x", env.uid, withInReplyTo(message.InternetMessageID))); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(ctx, `DELETE FROM email.thread_participant WHERE thread_id = $1`, thread.ID); err != nil {
			t.Fatal(err)
		}
		again, replayed, err := svc.CreateThread(ctx, session(70), in)
		if err != nil || again != nil || replayed.ID != message.ID || replayed.ThreadID != thread.ID {
			t.Fatalf("replay = %+v %+v, %v", again, replayed, err)
		}
	})

	t.Run("a request id taken meanwhile rolls the new Thread back", func(t *testing.T) {
		in := newThread(env.profile, service.ContactModeNone, 0)
		firstThread := seedThread(t, env.db, 1, env.profile, "regular", "processed", ptrTime(time.Now()))
		threads := count(t, env.db, `SELECT count(*) FROM email.thread`)
		authors := count(t, env.db, `SELECT count(*) FROM email.thread_participant WHERE outbound_at IS NOT NULL`)

		tx, err := env.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Released even when the test fails, so a waiting request cannot hang the run.
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, state, message_id, received_at,
    author_user_id, request_id, request_fingerprint, delivery_status, delivery_phase)
VALUES (1, $1, $2, 'outbound', 'ready', 'first@example.org', now(), 70, $3,
    decode(repeat('00', 32), 'hex'), 'pending', 'queued')`, firstThread, env.profile, in.RequestID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, _, err := svc.CreateThread(ctx, session(70), in); done <- err }()
		waitForLocks(t, env.db, "Lock", 1)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		wantID(t, <-done, "email.message.request_conflict")
		if count(t, env.db, `SELECT count(*) FROM email.thread`) != threads ||
			count(t, env.db, `SELECT count(*) FROM email.thread_participant WHERE outbound_at IS NOT NULL`) != authors {
			t.Fatal("the second Thread or its author was kept")
		}
	})
}

func TestListEmailSenderProfiles(t *testing.T) {
	env := newSendEnv(t)
	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `DELETE FROM directory.wbt_user`); err != nil {
		t.Fatal(err)
	}
	seedUser(t, env.db, 70, 1, "Olena")
	// Reset keeps directory users, which other tests seed again.
	t.Cleanup(func() { _, _ = env.db.Exec(context.Background(), `DELETE FROM directory.wbt_user WHERE id = 70`) })
	disabled := testhelpers.SeedProfile(t, env.db, 1, "b-disabled")
	replyOnly := testhelpers.SeedProfile(t, env.db, 1, "c-reply-only")
	env.allowSend(env.profile)
	env.allowSend(disabled)
	if _, err := env.db.Exec(ctx, `UPDATE email.profile SET signature = 'Regards, ${agent_name}', enabled = false WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}

	profiles, err := env.service(true, true).ListSenderProfiles(ctx, session(70))
	if err != nil || len(profiles) != 2 || profiles[0].ID != disabled || profiles[1].ID != env.profile ||
		profiles[0].Signature != "Regards, Olena" {
		t.Fatalf("profiles = %+v, %v (reply only %d)", profiles, err, replyOnly)
	}
	_, err = env.service(false, true).ListSenderProfiles(ctx, &auth.Session{DomainID: 1, UserID: 70})
	wantID(t, err, "email.outbound.disabled")
}
