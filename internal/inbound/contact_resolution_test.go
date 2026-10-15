package inbound

import (
	"context"
	"errors"
	"testing"

	contactsinfra "github.com/webitel/webitel-emails/infra/contacts"
	"github.com/webitel/webitel-emails/internal/model"
)

func TestPersistenceStoresContactResolutionOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		resolution contactsinfra.Resolution
		err        error
		state      model.EmailContactResolutionState
		contactID  int64
	}{
		{name: "not found", resolution: contactsinfra.Resolution{Outcome: contactsinfra.OutcomeNotFound}, state: model.EmailContactResolutionNotFound},
		{name: "resolved", resolution: contactsinfra.Resolution{Outcome: contactsinfra.OutcomeResolved, ContactID: 42}, state: model.EmailContactResolutionResolved, contactID: 42},
		{name: "ambiguous", resolution: contactsinfra.Resolution{Outcome: contactsinfra.OutcomeAmbiguous}, state: model.EmailContactResolutionAmbiguous},
		{name: "unavailable", err: errors.New("contacts unavailable"), state: model.EmailContactResolutionUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeStore()
			contacts := &fakeContacts{
				byAddress: map[string]contactsinfra.Resolution{"a@x.com": tt.resolution},
				err:       tt.err,
			}

			if err := testHandlerWithContacts(fake, contacts).Handle(
				context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5),
			); err != nil {
				t.Fatalf("handle: %v", err)
			}

			thread := fake.threads[0]
			if thread.ContactResolutionState != tt.state {
				t.Fatalf("resolution state = %q, want %q", thread.ContactResolutionState, tt.state)
			}
			if tt.contactID == 0 && thread.ContactID != nil {
				t.Fatalf("contact id = %v, want nil", *thread.ContactID)
			}
			if tt.contactID != 0 && (thread.ContactID == nil || *thread.ContactID != tt.contactID) {
				t.Fatalf("contact id = %v, want %d", thread.ContactID, tt.contactID)
			}
			if fake.messages[0].State != model.EmailMessageStateReady {
				t.Fatalf("message state = %q, want ready", fake.messages[0].State)
			}
			if len(contacts.addresses) != 1 || contacts.addresses[0] != "a@x.com" {
				t.Fatalf("searched addresses = %v, want [a@x.com]", contacts.addresses)
			}
			if fake.transactions != 2 {
				t.Fatalf("transactions = %d, want preparatory and final", fake.transactions)
			}
		})
	}
}

func TestPersistenceRedeliveryCompletesPendingContactResolution(t *testing.T) {
	fake := newFakeStore()
	fake.firstSenderErr = errors.New("database interrupted before contact lookup")
	contacts := &fakeContacts{byAddress: map[string]contactsinfra.Resolution{
		"a@x.com": {Outcome: contactsinfra.OutcomeResolved, ContactID: 42},
	}}
	handler := testHandlerWithContacts(fake, contacts)
	email := testEmail("<m1@x>", false, "raw-1", 10, 5)

	if err := handler.Handle(context.Background(), email); err == nil {
		t.Fatal("interrupted contact resolution was confirmed")
	}
	if len(fake.messages) != 1 || fake.messages[0].State != model.EmailMessageStateProcessing {
		t.Fatalf("messages/state = %d/%q, want one processing message", len(fake.messages), fake.messages[0].State)
	}
	if fake.threads[0].ContactResolutionState != model.EmailContactResolutionPending {
		t.Fatalf("resolution state = %q, want pending", fake.threads[0].ContactResolutionState)
	}

	fake.firstSenderErr = nil
	if err := handler.Handle(context.Background(), email); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if len(fake.messages) != 1 || fake.messages[0].State != model.EmailMessageStateReady {
		t.Fatalf("messages/state = %d/%q, want one ready message", len(fake.messages), fake.messages[0].State)
	}
	if contactID := fake.threads[0].ContactID; contactID == nil || *contactID != 42 {
		t.Fatalf("contact id = %v, want 42", contactID)
	}
}

func TestPendingThreadAlwaysUsesSenderOfEarliestMessage(t *testing.T) {
	fake := newFakeStore()
	fake.firstSenderErr = errors.New("database interrupted before contact lookup")
	contacts := &fakeContacts{byAddress: map[string]contactsinfra.Resolution{
		"a@x.com": {Outcome: contactsinfra.OutcomeResolved, ContactID: 42},
	}}
	handler := testHandlerWithContacts(fake, contacts)

	if err := handler.Handle(context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5)); err == nil {
		t.Fatal("first delivery unexpectedly completed")
	}

	fake.firstSenderErr = nil
	second := testEmail("<m2@x>", false, "raw-2", 10, 6)
	second.InReplyTo = "<m1@x>"
	second.From = []model.EmailAddress{{Address: "Z@X.com", NormalizedAddress: "z@x.com"}}
	if err := handler.Handle(context.Background(), second); err != nil {
		t.Fatalf("second delivery: %v", err)
	}

	if len(contacts.addresses) != 1 || contacts.addresses[0] != "a@x.com" {
		t.Fatalf("searched addresses = %v, want earliest sender a@x.com", contacts.addresses)
	}
	if fake.messages[1].ThreadID != fake.messages[0].ThreadID {
		t.Fatalf("messages belong to different threads: %d/%d", fake.messages[0].ThreadID, fake.messages[1].ThreadID)
	}
	if contactID := fake.threads[0].ContactID; contactID == nil || *contactID != 42 {
		t.Fatalf("contact id = %v, want 42", contactID)
	}
}

func TestUnavailableThreadRetriesWithSenderOfEarliestMessage(t *testing.T) {
	fake := newFakeStore()
	contacts := &fakeContacts{err: errors.New("contacts unavailable")}
	handler := testHandlerWithContacts(fake, contacts)

	if err := handler.Handle(context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if fake.threads[0].ContactResolutionState != model.EmailContactResolutionUnavailable {
		t.Fatalf("resolution state = %q, want unavailable", fake.threads[0].ContactResolutionState)
	}

	contacts.err = nil
	contacts.byAddress = map[string]contactsinfra.Resolution{
		"a@x.com": {Outcome: contactsinfra.OutcomeResolved, ContactID: 42},
	}
	second := testEmail("<m2@x>", false, "raw-2", 10, 6)
	second.InReplyTo = "<m1@x>"
	second.From = []model.EmailAddress{{Address: "Z@X.com", NormalizedAddress: "z@x.com"}}
	if err := handler.Handle(context.Background(), second); err != nil {
		t.Fatalf("second delivery: %v", err)
	}

	want := []string{"a@x.com", "a@x.com"}
	if len(contacts.addresses) != len(want) || contacts.addresses[0] != want[0] || contacts.addresses[1] != want[1] {
		t.Fatalf("searched addresses = %v, want %v", contacts.addresses, want)
	}
	if contactID := fake.threads[0].ContactID; contactID == nil || *contactID != 42 {
		t.Fatalf("contact id = %v, want 42", contactID)
	}
}

func TestResolvedThreadKeepsSingleTransactionPath(t *testing.T) {
	fake := newFakeStore()
	contacts := &fakeContacts{byAddress: map[string]contactsinfra.Resolution{
		"a@x.com": {Outcome: contactsinfra.OutcomeResolved, ContactID: 42},
	}}
	handler := testHandlerWithContacts(fake, contacts)
	if err := handler.Handle(context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	fake.transactions = 0
	second := testEmail("<m2@x>", false, "raw-2", 10, 6)
	second.InReplyTo = "<m1@x>"
	if err := handler.Handle(context.Background(), second); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if fake.transactions != 1 {
		t.Fatalf("transactions = %d, want one for an already resolved Thread", fake.transactions)
	}
	if len(contacts.addresses) != 1 {
		t.Fatalf("contact lookup repeated: %v", contacts.addresses)
	}
}

func TestServiceThreadSkipsContactResolution(t *testing.T) {
	fake := newFakeStore()
	contacts := &fakeContacts{}
	email := testEmail("<auto@x>", false, "raw-auto", 10, 5)
	email.Kind = model.EmailKindAutoReply

	if err := testHandlerWithContacts(fake, contacts).Handle(context.Background(), email); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(contacts.addresses) != 0 {
		t.Fatalf("service Thread searched Contacts: %v", contacts.addresses)
	}
	if fake.threads[0].ContactResolutionState != model.EmailContactResolutionNotApplicable {
		t.Fatalf("resolution state = %q, want not_applicable", fake.threads[0].ContactResolutionState)
	}
	if fake.transactions != 1 {
		t.Fatalf("transactions = %d, want one", fake.transactions)
	}
}

func TestManualContactDecisionWinsAutomaticResolution(t *testing.T) {
	fake := newFakeStore()
	manualID := int64(99)
	contacts := &fakeContacts{
		byAddress: map[string]contactsinfra.Resolution{
			"a@x.com": {Outcome: contactsinfra.OutcomeResolved, ContactID: 42},
		},
		beforeReturn: func() {
			applied, err := fake.EmailThreadStore().SetContactManually(
				context.Background(), 1, fake.threads[0].ID, &manualID,
			)
			if err != nil || !applied {
				t.Fatalf("manual bind = %v, %v", applied, err)
			}
		},
	}

	if err := testHandlerWithContacts(fake, contacts).Handle(
		context.Background(), testEmail("<m1@x>", false, "raw-1", 10, 5),
	); err != nil {
		t.Fatalf("handle: %v", err)
	}
	thread := fake.threads[0]
	if thread.ContactID == nil || *thread.ContactID != manualID {
		t.Fatalf("contact id = %v, want manual id %d", thread.ContactID, manualID)
	}
	if thread.ContactResolutionState != model.EmailContactResolutionResolved {
		t.Fatalf("resolution state = %q, want resolved", thread.ContactResolutionState)
	}
}
