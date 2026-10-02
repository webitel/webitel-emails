//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/lib/pq"
)

func checksumOf(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))

	return sum[:]
}

func pqArray(target *[]string) any { return pq.Array(target) }

func seedThread(t *testing.T, db *sql.DB, domainID, profileID int64, kind, status string, completedAt *time.Time) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(context.Background(), `
INSERT INTO email.thread (domain_id, profile_id, kind, status, subject, completed_at)
VALUES ($1, $2, $3, $4, 'Subject', $5) RETURNING id`, domainID, profileID, kind, status, completedAt).Scan(&id)
	if err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	return id
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()

	var total int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}

	return total
}

// readRecipients returns "type:address:ordinal" in storage order.
func readRecipients(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
SELECT type, normalized_address, ordinal FROM email.recipient ORDER BY id`)
	if err != nil {
		t.Fatalf("read recipients: %v", err)
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var kind, address string
		var ordinal int
		if err := rows.Scan(&kind, &address, &ordinal); err != nil {
			t.Fatalf("scan recipient: %v", err)
		}
		result = append(result, kind+":"+address+":"+strconv.Itoa(ordinal))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read recipients: %v", err)
	}

	return result
}

// threadOf returns the thread a stored message belongs to.
func threadOf(t *testing.T, db *sql.DB, messageID string) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT thread_id FROM email.message WHERE message_id = $1`, messageID).Scan(&id); err != nil {
		t.Fatalf("thread of %s: %v", messageID, err)
	}

	return id
}
