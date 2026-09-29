//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/webitel/webitel-emails/internal/model"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// A broken email may be delivered again before the cursor moves, so the record
// has to be an upsert rather than an insert.
func TestQuarantineRecordIsIdempotent(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")

	failures := storepostgres.NewInboundFailureStore(db)
	failure := &model.InboundFailure{
		DomainID: 1, ProfileID: profileID,
		Mailbox: "INBOX", UIDValidity: 10, UID: 5,
		Category: model.InboundFailureMIMEParse,
		Reason:   "failed to parse MIME message",
		Size:     4096,
		Sender:   "a@x.com", Subject: "Broken",
	}

	if err := failures.Record(context.Background(), failure); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := failures.Record(context.Background(), failure); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if rows := count(t, db, `SELECT count(*) FROM email.inbound_failure`); rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}

	failure.Reason = "updated safe reason"
	failure.Size = 8192
	failure.Subject = "Updated"
	if err := failures.Record(context.Background(), failure); err != nil {
		t.Fatalf("update existing record: %v", err)
	}
	var reason, subject string
	var size int64
	if err := db.QueryRowContext(context.Background(), `
SELECT reason, message_size, subject
FROM email.inbound_failure
WHERE domain_id = 1 AND profile_id = $1 AND mailbox = 'INBOX' AND uid_validity = 10 AND uid = 5`,
		profileID).Scan(&reason, &size, &subject); err != nil {
		t.Fatalf("read updated record: %v", err)
	}
	if reason != failure.Reason || size != failure.Size || subject != failure.Subject {
		t.Fatalf("updated record = %q/%d/%q", reason, size, subject)
	}

	// The same email at a new UIDVALIDITY is a different position, so it earns
	// its own record.
	failure.UIDValidity = 11
	if err := failures.Record(context.Background(), failure); err != nil {
		t.Fatalf("record after uidvalidity change: %v", err)
	}
	if rows := count(t, db, `SELECT count(*) FROM email.inbound_failure`); rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}
}
