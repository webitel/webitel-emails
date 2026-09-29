//go:build integration

// Package testhelpers starts the database the integration tests run against.
package testhelpers

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/webitel/webitel-emails/cmd/migrate"
)

// DSNEnv points the tests at an existing database instead of starting a
// container, which is how they run where Docker is unavailable.
const DSNEnv = "EMAILS_TEST_POSTGRES_DSN"

const postgresImage = "postgres:18.1-alpine"

var (
	testDB        *sql.DB
	testContainer *postgres.PostgresContainer
)

// Start opens one migrated database for the integration test package. Failure
// is returned to TestMain: an explicitly requested integration run must never
// pass without executing its PostgreSQL checks.
func Start(ctx context.Context) error {
	if testDB != nil {
		return nil
	}

	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		container, err := startContainer(ctx)
		if err != nil {
			return err
		}
		testContainer = container

		dsn, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			_ = container.Terminate(context.Background())
			testContainer = nil

			return fmt.Errorf("postgres connection string: %w", err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()

		return fmt.Errorf("database is not reachable: %w", err)
	}
	if err := prepare(ctx, db); err != nil {
		_ = db.Close()

		return fmt.Errorf("prepare database: %w", err)
	}

	testDB = db

	return nil
}

// Database returns the database initialized by TestMain.
func Database(t *testing.T) *sql.DB {
	t.Helper()

	if testDB == nil {
		t.Fatal("integration database is not initialized")
	}

	return testDB
}

func startContainer(ctx context.Context) (*postgres.PostgresContainer, error) {
	container, err := postgres.Run(
		ctx,
		postgresImage,
		postgres.WithDatabase("webitel"),
		postgres.WithUsername("webitel"),
		postgres.WithPassword("webitel"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start PostgreSQL container: %w", err)
	}

	return container, nil
}

// Close releases the shared database and container after the package finishes.
func Close(ctx context.Context) error {
	var closeErr error
	if testDB != nil {
		closeErr = testDB.Close()
		testDB = nil
	}
	if testContainer != nil {
		if err := testContainer.Terminate(ctx); err != nil && closeErr == nil {
			closeErr = err
		}
		testContainer = nil
	}

	return closeErr
}

// prepare applies the embedded migrations through the same migrator the service
// uses, so the tests exercise the real migration path.
func prepare(ctx context.Context, db *sql.DB) error {
	// The Email Profile projection joins a table owned by another service, which
	// the tests stand in for.
	const directoryStub = `
CREATE SCHEMA IF NOT EXISTS directory;
CREATE TABLE IF NOT EXISTS directory.wbt_user (
    id       bigint PRIMARY KEY,
    name     text NOT NULL DEFAULT '',
    username text NOT NULL DEFAULT ''
);`

	if _, err := db.ExecContext(ctx, directoryStub); err != nil {
		return fmt.Errorf("directory stub: %w", err)
	}

	migrator, err := migrate.NewMigrator(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}

	return migrator.Run(ctx)
}

// Reset empties the email tables so each test starts from a known state.
func Reset(t *testing.T, db *sql.DB) {
	t.Helper()

	const query = `DELETE FROM email.inbound_failure; DELETE FROM email.thread; DELETE FROM email.profile;`
	if _, err := db.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("reset database: %v", err)
	}
}

// SeedProfile creates an enabled profile and returns its identifier.
func SeedProfile(t *testing.T, db *sql.DB, domainID int64, name string) int64 {
	t.Helper()

	const query = `
INSERT INTO email.profile (
    domain_id, name, email_address, imap_host, imap_port, imap_security,
    smtp_host, smtp_port, smtp_security, username
)
VALUES ($1, $2, $2 || '@example.org', 'imap.example.org', 993, 'tls', 'smtp.example.org', 465, 'tls', 'mailbox')
RETURNING id`

	var id int64
	if err := db.QueryRowContext(context.Background(), query, domainID, name).Scan(&id); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	return id
}
