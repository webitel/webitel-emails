package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the pgx surface shared by the pool and a transaction.
type Querier interface {
	Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, query string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) pgx.Row
}

// Database is the pool surface needed to start a unit-of-work transaction.
type Database interface {
	Querier

	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}
