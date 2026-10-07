//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func TestContactResolutionStoreUsesFirstMessageAndProtectsTenant(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)

	profileID := testhelpers.SeedProfile(t, db, 1, "contact-owner")
	threadID := seedThread(t, db, 1, profileID, "regular", "new", nil)
	store := storepostgres.NewEmailThreadStore(db)
	ctx := context.Background()

	insertMessageWithSender(t, db, 1, profileID, threadID, "<first@x>", "first@x.com", time.Now().Add(time.Hour))
	insertMessageWithSender(t, db, 1, profileID, threadID, "<second@x>", "second@x.com", time.Now().Add(-time.Hour))

	address, err := store.FirstSenderAddress(ctx, 1, threadID)
	if err != nil {
		t.Fatalf("first sender: %v", err)
	}
	if address != "first@x.com" {
		t.Fatalf("first sender = %q, want first@x.com", address)
	}
	address, err = store.FirstSenderAddress(ctx, 2, threadID)
	if err != nil || address != "" {
		t.Fatalf("foreign-domain first sender = %q, %v; want empty", address, err)
	}

	manualID := int64(42)
	applied, err := store.SetContactManually(ctx, 2, threadID, &manualID)
	if err != nil || applied {
		t.Fatalf("foreign-domain manual bind = %v, %v; want false", applied, err)
	}
	assertStoredContact(t, db, threadID, nil, model.EmailContactResolutionPending)

	applied, err = store.SetContactManually(ctx, 1, threadID, &manualID)
	if err != nil || !applied {
		t.Fatalf("manual bind = %v, %v; want true", applied, err)
	}
	assertStoredContact(t, db, threadID, &manualID, model.EmailContactResolutionResolved)

	automaticID := int64(99)
	applied, err = store.ResolveContact(
		ctx, 1, threadID, model.EmailContactResolutionResolved, &automaticID,
	)
	if err != nil || applied {
		t.Fatalf("automatic overwrite = %v, %v; want false", applied, err)
	}
	assertStoredContact(t, db, threadID, &manualID, model.EmailContactResolutionResolved)

}

func insertMessageWithSender(
	t *testing.T,
	db *sql.DB,
	domainID, profileID, threadID int64,
	messageID, sender string,
	receivedAt time.Time,
) {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
INSERT INTO email.message (domain_id, thread_id, profile_id, direction, message_id, received_at)
VALUES ($1, $2, $3, 'inbound', $4, $5)
RETURNING id`, domainID, threadID, profileID, messageID, receivedAt).Scan(&id); err != nil {
		t.Fatalf("seed message %s: %v", messageID, err)
	}
	if sender == "" {
		return
	}
	if _, err := db.ExecContext(context.Background(), `
INSERT INTO email.recipient (domain_id, message_id, type, address, normalized_address, ordinal)
VALUES ($1, $2, 'from', $3, $3, 0)`, domainID, id, sender); err != nil {
		t.Fatalf("seed sender %s: %v", sender, err)
	}
}

func assertStoredContact(
	t *testing.T,
	db *sql.DB,
	threadID int64,
	wantID *int64,
	wantState model.EmailContactResolutionState,
) {
	t.Helper()

	var contactID sql.NullInt64
	var state string
	if err := db.QueryRowContext(context.Background(), `
SELECT contact_id, contact_resolution_state FROM email.thread WHERE id = $1`, threadID).
		Scan(&contactID, &state); err != nil {
		t.Fatalf("read stored contact: %v", err)
	}
	if wantID == nil && contactID.Valid {
		t.Fatalf("stored contact id = %d, want NULL", contactID.Int64)
	}
	if wantID != nil && (!contactID.Valid || contactID.Int64 != *wantID) {
		t.Fatalf("stored contact id = %v, want %d", contactID, *wantID)
	}
	if state != string(wantState) {
		t.Fatalf("stored resolution state = %q, want %q", state, wantState)
	}
}
