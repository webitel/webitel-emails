//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
	storepostgres "github.com/webitel/webitel-emails/internal/store/postgres"
	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

// The unit of work is about real transaction behaviour, which no fake can show:
// what actually commits, what rolls back, and what a cancelled context leaves.
func TestUnitOfWorkTransactions(t *testing.T) {
	db := testhelpers.Database(t)
	testhelpers.Reset(t, db)
	profileID := testhelpers.SeedProfile(t, db, 1, "owner")
	uow := storepostgres.NewUnitOfWork(db)

	newThread := func() *model.EmailThread {
		return &model.EmailThread{
			DomainID: 1, ProfileID: profileID,
			Kind: model.EmailThreadKindRegular, Status: model.EmailThreadStatusNew, Subject: "Subject",
		}
	}
	threads := func() int { return count(t, db, `SELECT count(*) FROM email.thread`) }

	t.Run("a successful function commits", func(t *testing.T) {
		before := threads()
		err := uow.WithinTransaction(context.Background(), func(ctx context.Context, uow store.UnitOfWork) error {
			_, err := uow.EmailThreadStore().Create(ctx, newThread())

			return err
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if threads() != before+1 {
			t.Fatal("the committed thread is missing")
		}
	})

	t.Run("an error rolls everything back", func(t *testing.T) {
		before := threads()
		boom := errors.New("boom")
		err := uow.WithinTransaction(context.Background(), func(ctx context.Context, uow store.UnitOfWork) error {
			if _, err := uow.EmailThreadStore().Create(ctx, newThread()); err != nil {
				return err
			}

			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want boom", err)
		}
		if threads() != before {
			t.Fatal("a rolled back thread was committed")
		}
	})

	t.Run("a panic rolls back and is raised again", func(t *testing.T) {
		before := threads()
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic was swallowed")
				}
			}()
			_ = uow.WithinTransaction(context.Background(), func(ctx context.Context, uow store.UnitOfWork) error {
				if _, err := uow.EmailThreadStore().Create(ctx, newThread()); err != nil {
					return err
				}

				panic("inside the transaction")
			})
		}()
		if threads() != before {
			t.Fatal("a thread survived the panic")
		}
	})

	t.Run("a nested call rolls back with the outer transaction", func(t *testing.T) {
		before := threads()
		boom := errors.New("outer failed")
		err := uow.WithinTransaction(context.Background(), func(ctx context.Context, outer store.UnitOfWork) error {
			if _, err := outer.EmailThreadStore().Create(ctx, newThread()); err != nil {
				return err
			}

			if err := outer.WithinTransaction(ctx, func(ctx context.Context, inner store.UnitOfWork) error {
				_, err := inner.EmailThreadStore().Create(ctx, newThread())

				return err
			}); err != nil {
				return err
			}

			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want outer failure", err)
		}
		if threads() != before {
			t.Fatal("the nested write committed outside the outer transaction")
		}
	})

	t.Run("a cancelled context neither commits nor reports success", func(t *testing.T) {
		before := threads()
		ctx, cancel := context.WithCancel(context.Background())
		err := uow.WithinTransaction(ctx, func(ctx context.Context, uow store.UnitOfWork) error {
			if _, err := uow.EmailThreadStore().Create(ctx, newThread()); err != nil {
				return err
			}
			cancel()
			// database/sql rolls the transaction back on its own once ctx is done.
			time.Sleep(100 * time.Millisecond)

			return nil
		})
		if err == nil {
			t.Fatal("a cancelled transaction reported success")
		}
		if threads() != before {
			t.Fatal("a cancelled transaction left data behind")
		}
	})
}
