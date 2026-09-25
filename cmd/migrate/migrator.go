package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"github.com/webitel/webitel-emails/migrations"
)

const schemaVersionTable = "email_service_schema_version"

// Migrator applies the SQL migrations embedded in the service binary.
type Migrator struct {
	provider *goose.Provider
	log      *slog.Logger
}

// NewMigrator creates a Goose migration provider over the shared database connection.
func NewMigrator(db *sql.DB, log *slog.Logger) (*Migrator, error) {
	store, err := database.NewStore(database.DialectPostgres, schemaVersionTable)
	if err != nil {
		return nil, fmt.Errorf("migrations: create version store: %w", err)
	}

	provider, err := goose.NewProvider(
		goose.DialectCustom,
		db,
		migrations.EmbedMigrations,
		goose.WithStore(store),
		goose.WithSlog(log),
		goose.WithVerbose(true),
	)
	if err != nil {
		return nil, fmt.Errorf("migrations: create provider: %w", err)
	}

	return &Migrator{provider: provider, log: log}, nil
}

// Run applies all pending migrations in version order.
func (m *Migrator) Run(ctx context.Context) error {
	results, err := m.provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrations: up: %w", err)
	}
	if len(results) == 0 {
		m.log.Info("database migrations are up to date")
		return nil
	}

	for _, result := range results {
		attrs := []any{
			"direction", result.Direction,
			"version", result.Source.Version,
			"source", result.Source.Path,
			"duration", result.Duration,
			"empty", result.Empty,
		}
		if result.Error != nil {
			attrs = append(attrs, "error", result.Error)
			m.log.Error("database migration failed", attrs...)
			continue
		}

		m.log.Info("database migration applied", attrs...)
	}

	return nil
}
