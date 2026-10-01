package service

import (
	"context"
	"testing"

	"github.com/webitel/webitel-emails/internal/store"
)

type fakeContactVerifier struct {
	domainID  int64
	contactID int64
	found     bool
	err       error
}

func (f *fakeContactVerifier) VerifyContact(_ context.Context, domainID, contactID int64) (bool, error) {
	f.domainID, f.contactID = domainID, contactID

	return f.found, f.err
}

type fakeManualThreadStore struct {
	store.EmailThreadStore
	domainID  int64
	threadID  int64
	contactID *int64
	applied   bool
	err       error
}

func (f *fakeManualThreadStore) SetContactManually(
	_ context.Context,
	domainID, threadID int64,
	contactID *int64,
) (bool, error) {
	f.domainID, f.threadID, f.contactID = domainID, threadID, contactID

	return f.applied, f.err
}

func TestEmailThreadServiceBindsVerifiedContactInCallerDomain(t *testing.T) {
	threads := &fakeManualThreadStore{applied: true}
	contacts := &fakeContactVerifier{found: true}
	service := NewEmailThreadService(threads, contacts)

	if err := service.BindContact(context.Background(), 7, 11, 42); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if contacts.domainID != 7 || contacts.contactID != 42 {
		t.Fatalf("verified domain/contact = %d/%d", contacts.domainID, contacts.contactID)
	}
	if threads.domainID != 7 || threads.threadID != 11 || threads.contactID == nil || *threads.contactID != 42 {
		t.Fatalf("stored domain/thread/contact = %d/%d/%v", threads.domainID, threads.threadID, threads.contactID)
	}
}

func TestEmailThreadServiceRejectsContactOutsideDomain(t *testing.T) {
	threads := &fakeManualThreadStore{applied: true}
	contacts := &fakeContactVerifier{found: false}
	service := NewEmailThreadService(threads, contacts)

	if err := service.BindContact(context.Background(), 7, 11, 42); err == nil {
		t.Fatal("a missing contact was accepted")
	}
	if threads.threadID != 0 {
		t.Fatal("the Thread was changed before the contact passed tenant verification")
	}
}

func TestEmailThreadServiceUnbindsAndKeepsManualDecision(t *testing.T) {
	threads := &fakeManualThreadStore{applied: true}
	service := NewEmailThreadService(threads, &fakeContactVerifier{})

	if err := service.UnbindContact(context.Background(), 7, 11); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if threads.domainID != 7 || threads.threadID != 11 || threads.contactID != nil {
		t.Fatalf("stored domain/thread/contact = %d/%d/%v", threads.domainID, threads.threadID, threads.contactID)
	}
}

var _ ContactVerifier = (*fakeContactVerifier)(nil)
var _ store.EmailThreadStore = (*fakeManualThreadStore)(nil)
