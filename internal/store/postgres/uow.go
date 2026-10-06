package postgres

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// unitOfWork runs stores on a querier: the pool outside a transaction and the
// open transaction inside one.
type unitOfWork struct {
	db      Database
	querier Querier
	inTx    bool
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

// EmailMessageAttachmentStore returns the manifest store bound to the current querier.
func (u *unitOfWork) EmailMessageAttachmentStore() store.EmailMessageAttachmentStore {
	return NewEmailMessageAttachmentStore(u.querier)
}

// InboundFailureStore returns the quarantine store bound to the current querier.
func (u *unitOfWork) InboundFailureStore() store.InboundFailureStore {
	return NewInboundFailureStore(u.querier)
}

// EmailProfileRoutingStore returns the profile routing store bound to the current querier.
func (u *unitOfWork) EmailProfileRoutingStore() store.EmailProfileRoutingStore {
	return NewEmailProfileRoutingStore(u.querier)
}

// OutboxStore returns the Outbox store bound to the current querier.
func (u *unitOfWork) OutboxStore() store.OutboxStore {
	return NewOutboxStore(u.querier)
}

// EnsureEmailProfileAssignment checks the fencing token without locking the row.
func (u *unitOfWork) EnsureEmailProfileAssignment(
	ctx context.Context,
	assignment model.EmailProfileAssignment,
) error {
	const query = `
SELECT 1 FROM email.profile_runtime
WHERE profile_id = $1 AND owner_instance_id = $2 AND assignment_generation = $3`

	var fenced int
	err := u.querier.QueryRow(
		ctx,
		query,
		assignment.ProfileID,
		assignment.OwnerInstanceID,
		assignment.Generation,
	).Scan(&fenced)

	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		return store.ErrStaleEmailProfileAssignment
	case err != nil:
		return fmt.Errorf("store: check email profile %d assignment: %w", assignment.ProfileID, err)
	default:
		return nil
	}
}

// emailProfileLockNamespace is "EMLS" in ASCII and occupies the high half of the
// advisory lock key, so email-service keys never meet call_center's small ones.
const emailProfileLockNamespace = 0x454D4C53

// LockEmailProfile takes a transaction-scoped advisory lock on the profile and
// waits for the current holder; a canceled context aborts the wait.
func (u *unitOfWork) LockEmailProfile(ctx context.Context, profileID int64) error {
	key := int64(emailProfileLockNamespace)<<32 | int64(uint32(profileID))
	if _, err := u.querier.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
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
	if u.inTx {
		return fn(ctx, u)
	}

	tx, err := u.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}

	// Value-copy carries every field, including ones added later; only the
	// querier switches to the transaction.
	txUow := *u
	txUow.querier = tx
	txUow.inTx = true

	defer func() {
		if p := recover(); p != nil {
			_ = rollback(ctx, tx)

			panic(p)
		}
	}()

	if err := fn(ctx, &txUow); err != nil {
		if rbErr := rollback(ctx, tx); rbErr != nil {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}

		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}

	return nil
}

// rollback uses its own deadline because the operation context may be canceled.
func rollback(operationCtx context.Context, tx pgx.Tx) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(operationCtx), 5*time.Second)
	defer cancel()

	if err := tx.Rollback(ctx); err != nil && !stderrors.Is(err, pgx.ErrTxClosed) {
		return err
	}

	return nil
}
