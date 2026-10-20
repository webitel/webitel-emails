package service

import (
	"context"
	"testing"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/store"
)

type fakeContactVerifier struct {
	calls     int
	domainID  int64
	contactID int64
	found     bool
	err       error
}

func (f *fakeContactVerifier) VerifyContact(_ context.Context, domainID, contactID int64) (bool, error) {
	f.calls++
	f.domainID, f.contactID = domainID, contactID

	return f.found, f.err
}

type fakeManualThreadStore struct {
	store.EmailThreadStore
	calls     int
	domainID  int64
	userID    int64
	threadID  int64
	contactID *int64
	applied   bool
	err       error
}

func (f *fakeManualThreadStore) SetContactManually(
	_ context.Context,
	domainID, userID, threadID int64,
	contactID *int64,
) (bool, error) {
	f.calls++
	f.domainID, f.userID, f.threadID, f.contactID = domainID, userID, threadID, contactID

	return f.applied, f.err
}

// fakeRights answers from a queue, so a test can change the rights between the
// first check and the reclassification after a failed update.
type fakeRights struct {
	answers []store.EmailThreadRights
	calls   int
	userID  int64
}

func (f *fakeRights) Rights(_ context.Context, _, userID, _ int64) (store.EmailThreadRights, error) {
	f.userID = userID
	answer := f.answers[min(f.calls, len(f.answers)-1)]
	f.calls++

	return answer, nil
}

var (
	assigned  = store.EmailThreadRights{Visible: true, Assigned: true}
	history   = store.EmailThreadRights{Visible: true}
	invisible = store.EmailThreadRights{}
)

func TestEmailThreadServiceBindsVerifiedContactForAssignedAgent(t *testing.T) {
	threads := &fakeManualThreadStore{applied: true}
	contacts := &fakeContactVerifier{found: true}
	rights := &fakeRights{answers: []store.EmailThreadRights{assigned}}
	service := NewEmailThreadService(threads, rights, contacts)

	if err := service.BindContact(context.Background(), 7, 70, 11, 42); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if contacts.domainID != 7 || contacts.contactID != 42 || rights.userID != 70 {
		t.Fatalf("verified domain/contact/user = %d/%d/%d", contacts.domainID, contacts.contactID, rights.userID)
	}
	if threads.domainID != 7 || threads.userID != 70 || threads.threadID != 11 ||
		threads.contactID == nil || *threads.contactID != 42 {
		t.Fatalf("stored %+v", threads)
	}
}

func TestEmailThreadServiceChecksTheThreadBeforeContacts(t *testing.T) {
	for name, rights := range map[string]store.EmailThreadRights{"invisible": invisible, "not assigned": history} {
		t.Run(name, func(t *testing.T) {
			threads := &fakeManualThreadStore{applied: true}
			contacts := &fakeContactVerifier{found: true}
			service := NewEmailThreadService(threads, &fakeRights{answers: []store.EmailThreadRights{rights}}, contacts)

			err := service.BindContact(context.Background(), 7, 70, 11, 42)
			want := "email.thread.not_found"
			if rights.Visible {
				want = "email.thread.not_assigned"
			}
			if kiterrors.ID(err) != want {
				t.Fatalf("error = %v, want %s", err, want)
			}
			if contacts.calls != 0 || threads.calls != 0 {
				t.Fatalf("contacts asked %d times, thread written %d times", contacts.calls, threads.calls)
			}
		})
	}
}

func TestEmailThreadServiceRejectsContactOutsideDomain(t *testing.T) {
	threads := &fakeManualThreadStore{applied: true}
	contacts := &fakeContactVerifier{found: false}
	service := NewEmailThreadService(threads, &fakeRights{answers: []store.EmailThreadRights{assigned}}, contacts)

	if err := service.BindContact(context.Background(), 7, 70, 11, 42); kiterrors.ID(err) != "email.thread.contact_not_found" {
		t.Fatalf("a missing contact: %v", err)
	}
	if threads.calls != 0 {
		t.Fatal("the Thread was changed before the contact passed tenant verification")
	}
}

func TestEmailThreadServiceReclassifiesAnUpdateThatLostTheAssignment(t *testing.T) {
	for name, later := range map[string]store.EmailThreadRights{"reassigned": history, "gone": invisible} {
		t.Run(name, func(t *testing.T) {
			threads := &fakeManualThreadStore{applied: false}
			rights := &fakeRights{answers: []store.EmailThreadRights{assigned, later}}
			service := NewEmailThreadService(threads, rights, &fakeContactVerifier{found: true})

			err := service.BindContact(context.Background(), 7, 70, 11, 42)
			want := "email.thread.not_assigned"
			if !later.Visible {
				want = "email.thread.not_found"
			}
			if kiterrors.ID(err) != want || rights.calls != 2 {
				t.Fatalf("error = %v after %d checks, want %s", err, rights.calls, want)
			}
		})
	}
}

func TestEmailThreadServiceUnbindsForAssignedAgentOnly(t *testing.T) {
	threads := &fakeManualThreadStore{applied: true}
	service := NewEmailThreadService(threads, &fakeRights{answers: []store.EmailThreadRights{assigned}}, &fakeContactVerifier{})

	if err := service.UnbindContact(context.Background(), 7, 70, 11); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if threads.domainID != 7 || threads.userID != 70 || threads.threadID != 11 || threads.contactID != nil {
		t.Fatalf("stored %+v", threads)
	}

	refused := &fakeManualThreadStore{applied: true}
	service = NewEmailThreadService(refused, &fakeRights{answers: []store.EmailThreadRights{history}}, &fakeContactVerifier{})
	if err := service.UnbindContact(context.Background(), 7, 70, 11); kiterrors.ID(err) != "email.thread.not_assigned" || refused.calls != 0 {
		t.Fatalf("unbind by a previous participant: %v", err)
	}
}

var _ ContactVerifier = (*fakeContactVerifier)(nil)
var _ ThreadRightsReader = (*fakeRights)(nil)
var _ store.EmailThreadStore = (*fakeManualThreadStore)(nil)
