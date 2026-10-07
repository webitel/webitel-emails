package postgres

import (
	"context"
	"database/sql"
)

// Querier is the part of database/sql shared by *sql.DB and *sql.Tx, so a store
// runs the same code inside and outside a transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Database is the connection pool the unit of work runs on; *sql.DB satisfies it.
// The interface keeps the unit of work testable without a live database.
type Database interface {
	Querier

	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}
