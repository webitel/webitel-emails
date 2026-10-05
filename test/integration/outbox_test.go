//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/webitel/webitel-emails/config"
	contactsinfra "github.com/webitel/webitel-emails/infra/contacts"
	"github.com/webitel/webitel-emails/internal/event"
	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// knownContact resolves every address to one contact, so the event carries it.
type knownContact struct{}

func (knownContact) ResolveByEmail(context.Context, int64, string) (contactsinfra.Resolution, error) {
	return contactsinfra.Resolution{Outcome: contactsinfra.OutcomeResolved, ContactID: 77}, nil
}

func newEventHandler(db *sql.DB, files inbound.FileUploader, maxPayloadSize int) *inbound.PersistenceHandler {
	uow := storepostgres.NewUnitOfWork(db)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Storage: config.StorageConfig{MaxAttachmentAttempts: 3},
		InboundEvent: config.InboundEventConfig{
			MaxBodyTextSize: 64 << 10, MaxAddressesPerType: 100, MaxAttachments: 100,
			MaxFieldSize: 1024, MaxPayloadSize: maxPayloadSize,
		},
	}

	events := inbound.NewEventWriter(cfg, log)
	completer := inbound.NewMessageCompleter(uow, files, knownContact{}, events, cfg, log)

	return inbound.NewPersistenceHandler(uow, completer, events, log)
}

// inboundEvents decodes the stored events keyed by the RFC Message-ID.
func inboundEvents(t *testing.T, db *sql.DB) map[string]event.InboundMessage {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
SELECT payload::text, dedup_key, schema_version FROM email.outbox WHERE event_type = $1`, event.TypeInboundMessage)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()

	events := make(map[string]event.InboundMessage)
	for rows.Next() {
		var (
			payload, dedupKey string
			schemaVersion     int
		)
		if err := rows.Scan(&payload, &dedupKey, &schemaVersion); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}

		decoded, err := event.UnmarshalInboundMessage([]byte(payload))
		if err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if dedupKey != event.InboundMessageDedupKey(decoded.DomainID, decoded.MessageID) || schemaVersion != 1 {
			t.Fatalf("row dedup_key %q schema %d", dedupKey, schemaVersion)
		}
		events[decoded.Snapshot.InternetMessageID] = decoded
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}

	return events
}

// The event commits with the ready Message or not at all, on both paths.
func TestInboundEventCommitsWithReadyMessage(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	ctx := context.Background()

	profile := testhelpers.SeedProfile(t, db, 1, "events")
	if _, err := db.ExecContext(ctx, `UPDATE email.profile SET flow_id = 42 WHERE id = $1`, profile); err != nil {
		t.Fatalf("set flow: %v", err)
	}

	files := &stubUploader{}
	handler := newEventHandler(db, files, 1<<20)

	t.Run("both completion paths record one event", func(t *testing.T) {
		// A new conversation resolves its contact, so it completes in the final transaction.
		first := newEmail(profile, "<first@x>", 1)
		// A reply to a resolved conversation is stored in one transaction.
		reply := newEmail(profile, "<reply@x>", 2, withInReplyTo("<first@x>"))

		for _, email := range []*model.ParsedEmail{first, reply, first} {
			if err := handler.Handle(ctx, email); err != nil {
				t.Fatalf("handle %s: %v", email.MessageID, err)
			}
		}

		events := inboundEvents(t, db)
		if len(events) != 2 {
			t.Fatalf("events = %d, want one per message despite the redelivery", len(events))
		}
		for _, id := range []string{"<first@x>", "<reply@x>"} {
			e := events[id]
			if e.FlowID == nil || *e.FlowID != 42 || e.Snapshot.ContactID == nil || *e.Snapshot.ContactID != 77 {
				t.Fatalf("%s: flow %v contact %v", id, e.FlowID, e.Snapshot.ContactID)
			}
			if len(e.Snapshot.To) != 2 || e.Snapshot.From[0].Address != "A@X.com" {
				t.Fatalf("%s: recipients %+v", id, e.Snapshot)
			}
		}
	})

	t.Run("auto-reply records no event", func(t *testing.T) {
		autoReply := newEmail(profile, "<auto@x>", 3, withKind(model.EmailKindAutoReply))
		if err := handler.Handle(ctx, autoReply); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if _, ok := inboundEvents(t, db)["<auto@x>"]; ok {
			t.Fatal("an auto-reply recorded a Flow event")
		}
	})

	t.Run("event appears only when the attachments finish", func(t *testing.T) {
		email := emailWithAttachment(profile, "<files@x>", 4)

		files.failWith = errors.New("storage unavailable")
		if err := handler.Handle(ctx, email); err == nil {
			t.Fatal("a failed upload was confirmed")
		}
		if _, ok := inboundEvents(t, db)["<files@x>"]; ok {
			t.Fatal("an unfinished message recorded an event")
		}

		files.failWith = nil
		if err := handler.Handle(ctx, email); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if e, ok := inboundEvents(t, db)["<files@x>"]; !ok || len(e.Snapshot.Attachments) != 1 {
			t.Fatalf("event %+v recorded %v", e.Snapshot.Attachments, ok)
		}
	})

	t.Run("a failed event rolls the ready transition back", func(t *testing.T) {
		// A payload limit no event fits makes the final transaction fail.
		broken := newEventHandler(db, files, 10)
		first := newEmail(profile, "<broken-new@x>", 5)
		reply := newEmail(profile, "<broken-reply@x>", 6, withInReplyTo("<first@x>"))

		for _, email := range []*model.ParsedEmail{first, reply} {
			if err := broken.Handle(ctx, email); err == nil {
				t.Fatalf("%s: confirmed without its event", email.MessageID)
			}
		}

		ready := count(t, db, `
SELECT count(*) FROM email.message WHERE message_id IN ('<broken-new@x>', '<broken-reply@x>') AND state = 'ready'`)
		if ready != 0 {
			t.Fatalf("%d messages became ready without an event", ready)
		}

		for _, email := range []*model.ParsedEmail{first, reply} {
			if err := handler.Handle(ctx, email); err != nil {
				t.Fatalf("redelivery of %s: %v", email.MessageID, err)
			}
		}
		events := inboundEvents(t, db)
		if _, ok := events["<broken-new@x>"]; !ok {
			t.Fatal("the redelivered new conversation has no event")
		}
		if _, ok := events["<broken-reply@x>"]; !ok {
			t.Fatal("the redelivered reply has no event")
		}
	})
}
