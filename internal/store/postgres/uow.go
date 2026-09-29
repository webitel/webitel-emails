package postgres

import (
	"context"
	"database/sql"
	stderrors "errors"
	"fmt"

	"github.com/webitel/webitel-emails/internal/store"
)

// unitOfWork runs stores on a querier: the pool outside a transaction and the
// open transaction inside one.
type unitOfWork struct {
	db      Database
	querier Querier
}

var _ store.UnitOfWork = (*unitOfWork)(nil)

// NewUnitOfWork creates a unit of work running on db.
func NewUnitOfWork(db Database) *unitOfWork {
	return &unitOfWork{db: db, querier: db}
}

// EmailThreadStore returns the Thread store bound to the current querier.
// Built on every call, so a transactional copy never holds an older querier.
func (u *unitOfWork) EmailThreadStore() store.EmailThreadStore {
	return NewEmailThreadStore(u.querier)
}

// EmailMessageStore returns the Message store bound to the current querier.
func (u *unitOfWork) EmailMessageStore() store.EmailMessageStore {
	return NewEmailMessageStore(u.querier)
}

// EmailRecipientStore returns the recipient store bound to the current querier.
func (u *unitOfWork) EmailRecipientStore() store.EmailRecipientStore {
	return NewEmailRecipientStore(u.querier)
}

// emailProfileLockNamespace is "EMLS" in ASCII and occupies the high half of the
// advisory lock key, so email-service keys never meet call_center's small ones.
const emailProfileLockNamespace = 0x454D4C53

// LockEmailProfile takes a transaction-scoped advisory lock on the profile and
// waits for the current holder; a canceled context aborts the wait.
func (u *unitOfWork) LockEmailProfile(ctx context.Context, profileID int64) error {
	key := int64(emailProfileLockNamespace)<<32 | int64(uint32(profileID))
	if _, err := u.querier.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		return fmt.Errorf("store: lock email profile %d: %w", profileID, err)
	}

	return nil
}

// WithinTransaction runs fn in one transaction. A unit of work already inside a
// transaction joins it instead of nesting; a panic rolls back and is re-raised.
func (u *unitOfWork) WithinTransaction(
	ctx context.Context,
	fn func(ctx context.Context, uow store.UnitOfWork) error,
) error {
	if _, ok := u.querier.(*sql.Tx); ok {
		return fn(ctx, u)
	}

	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}

	// Value-copy carries every field, including ones added later; only the
	// querier switches to the transaction.
	txUow := *u
	txUow.querier = tx

	defer func() {
		if p := recover(); p != nil {
			_ = rollback(tx)

			panic(p)
		}
	}()

	if err := fn(ctx, &txUow); err != nil {
		if rbErr := rollback(tx); rbErr != nil {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}

		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}

	return nil
}

// rollback ignores ErrTxDone: once ctx is canceled database/sql rolls the
// transaction back on its own, and the explicit rollback then finds it done.
func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !stderrors.Is(err, sql.ErrTxDone) {
		return err
	}

	return nil
}
