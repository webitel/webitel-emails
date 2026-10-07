package postgres

import (
	"database/sql"
	stderrors "errors"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL SQLSTATEs the stores translate into stable errors.
const (
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
	checkViolation      = "23514"
	notNullViolation    = "23502"
)

// ParseError maps a database error to a coded store error. Callers branch on
// the code, and the driver error stays attached as the cause.
func ParseError(err error) error {
	if err == nil {
		return nil
	}

	// Not-found never reveals whether the row exists but belongs to another
	// domain: every read is domain-scoped and both cases must look the same.
	if stderrors.Is(err, sql.ErrNoRows) {
		return kiterrors.NotFound(
			"entity does not exist or access is denied",
			kiterrors.WithID("store.pg.not_found"),
			kiterrors.WithCause(err),
		)
	}

	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		switch pgErr.Code {
		case uniqueViolation:
			return kiterrors.New(
				"entity already exists",
				kiterrors.WithCode(codes.AlreadyExists),
				kiterrors.WithID("store.pg.unique"),
				kiterrors.WithCause(err),
				kiterrors.WithValue("constraint", pgErr.ConstraintName),
			)
		case foreignKeyViolation:
			return kiterrors.Aborted(
				"referenced entity does not exist or is still referenced",
				kiterrors.WithID("store.pg.foreign_key"),
				kiterrors.WithCause(err),
				kiterrors.WithValue("constraint", pgErr.ConstraintName),
				kiterrors.WithValue("table", pgErr.TableName),
			)
		case checkViolation:
			return kiterrors.Aborted(
				"value violates a constraint",
				kiterrors.WithID("store.pg.check"),
				kiterrors.WithCause(err),
				kiterrors.WithValue("constraint", pgErr.ConstraintName),
			)
		case notNullViolation:
			return kiterrors.Aborted(
				"required value is missing",
				kiterrors.WithID("store.pg.not_null"),
				kiterrors.WithCause(err),
				kiterrors.WithValue("column", pgErr.TableName+"."+pgErr.ColumnName),
			)
		}
	}

	return kiterrors.Internal(
		"storage error",
		kiterrors.WithID("store.pg.internal"),
		kiterrors.WithCause(err),
	)
}

// isConstraintViolation reports a PostgreSQL error raised by one of the named
// constraints, for the few cases that carry their own domain meaning.
func isConstraintViolation(err error, sqlState string, constraints ...string) bool {
	var pgErr *pgconn.PgError
	if !stderrors.As(err, &pgErr) || pgErr.Code != sqlState {
		return false
	}
	if len(constraints) == 0 {
		return true
	}

	for _, constraint := range constraints {
		if pgErr.ConstraintName == constraint {
			return true
		}
	}

	return false
}
