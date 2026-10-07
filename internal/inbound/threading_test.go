package inbound

import (
	"context"
	"hash/fnv"
	"testing"

	"github.com/webitel/webitel-emails/internal/model"
)

// deliver stores one email and returns the Thread it landed in.
func deliver(t *testing.T, handler *PersistenceHandler, fake *fakeStore, email *model.ParsedEmail) int64 {
	t.Helper()
	if err := handler.Handle(context.Background(), email); err != nil {
		t.Fatalf("deliver %s: %v", email.MessageID, err)
	}

	return fake.messages[len(fake.messages)-1].ThreadID
}

// reply builds an email that answers parent and carries the given chain. The
// mailbox position is derived from the identifier so two emails never share it.
func reply(messageID, inReplyTo string, references ...string) *model.ParsedEmail {
	position := fnv.New32a()
	_, _ = position.Write([]byte(messageID))

	email := testEmail(messageID, false, "raw-"+messageID, 10, position.Sum32())
	email.InReplyTo = inReplyTo
	email.References = references

	return email
}

func TestThreadingFollowsTheAgreedOrder(t *testing.T) {
	t.Run("direct parent wins", func(t *testing.T) {
		fake := newFakeStore()
		handler := testHandler(fake)

		root := deliver(t, handler, fake, reply("<root@x>", ""))
		if got := deliver(t, handler, fake, reply("<a1@x>", "<root@x>", "<root@x>")); got != root {
			t.Fatalf("reply landed in thread %d, want %d", got, root)
		}
	})

	t.Run("references fall back to the nearest known ancestor", func(t *testing.T) {
		fake := newFakeStore()
		handler := testHandler(fake)

		old := deliver(t, handler, fake, reply("<old@x>", ""))
		near := deliver(t, handler, fake, reply("<near@x>", ""))

		// The parent itself is unknown, so the closest stored reference decides.
		got := deliver(t, handler, fake, reply("<a2@x>", "<missing@x>", "<old@x>", "<near@x>"))
		if got != near {
			t.Fatalf("landed in thread %d, want the nearest ancestor %d (oldest %d)", got, near, old)
		}
	})

	t.Run("in-reply-to beats references", func(t *testing.T) {
		fake := newFakeStore()
		handler := testHandler(fake)

		deliver(t, handler, fake, reply("<ref@x>", ""))
		parent := deliver(t, handler, fake, reply("<parent@x>", ""))

		if got := deliver(t, handler, fake, reply("<a3@x>", "<parent@x>", "<ref@x>")); got != parent {
			t.Fatalf("landed in thread %d, want the direct parent %d", got, parent)
		}
	})

	t.Run("sibling groups replies to an unknown parent", func(t *testing.T) {
		fake := newFakeStore()
		handler := testHandler(fake)

		first := deliver(t, handler, fake, reply("<s1@x>", "<unknown@x>"))
		if got := deliver(t, handler, fake, reply("<s2@x>", "<unknown@x>")); got != first {
			t.Fatalf("siblings split into threads %d and %d", first, got)
		}
	})

	t.Run("reverse sibling adopts a late parent", func(t *testing.T) {
		fake := newFakeStore()
		handler := testHandler(fake)

		// The answer arrives first; its parent only shows up afterwards.
		answer := deliver(t, handler, fake, reply("<late-answer@x>", "<late-parent@x>"))
		if got := deliver(t, handler, fake, reply("<late-parent@x>", "")); got != answer {
			t.Fatalf("the late parent opened thread %d instead of joining %d", got, answer)
		}
	})
}

func TestThreadingKeepsServiceEmailsApart(t *testing.T) {
	service := func(messageID, inReplyTo string, kind model.EmailKind) *model.ParsedEmail {
		email := reply(messageID, inReplyTo)
		email.Kind = kind

		return email
	}

	t.Run("an ordinary email never joins a service thread", func(t *testing.T) {
		fake := newFakeStore()
		handler := testHandler(fake)

		closed := deliver(t, handler, fake, service("<auto@x>", "<unknown@x>", model.EmailKindAutoReply))
		if fake.threads[0].Kind != model.EmailThreadKindService ||
			fake.threads[0].Status != model.EmailThreadStatusProcessed ||
			fake.threads[0].CompletedAt == nil {
			t.Fatalf("unmatched auto-reply must open a closed service thread, got %#v", fake.threads[0])
		}

		// The same unknown parent would make them siblings, were it allowed.
		if got := deliver(t, handler, fake, reply("<real@x>", "<unknown@x>")); got == closed {
			t.Fatal("an operator email landed in a closed service thread")
		}
		if fake.threads[1].Kind != model.EmailThreadKindRegular {
			t.Fatalf("kind = %q, want regular", fake.threads[1].Kind)
		}
	})

	t.Run("a bounce joins the thread of the email it reports on", func(t *testing.T) {
		fake := newFakeStore()
		handler := testHandler(fake)

		original := deliver(t, handler, fake, reply("<sent@x>", ""))

		bounce := service("<bounce@x>", "", model.EmailKindBounce)
		bounce.Bounce = &model.EmailBounceInfo{OriginalMessageID: "<sent@x>", Status: "5.1.1"}

		if got := deliver(t, handler, fake, bounce); got != original {
			t.Fatalf("bounce landed in thread %d, want %d", got, original)
		}
		if fake.threads[0].Status != model.EmailThreadStatusNew {
			t.Fatalf("the bounce changed the thread status to %q", fake.threads[0].Status)
		}
	})
}

func TestThreadingKeepsPriorityWhenIdentifiersRepeat(t *testing.T) {
	fake := newFakeStore()
	handler := testHandler(fake)

	referenced := deliver(t, handler, fake, reply("<ref@x>", ""))
	parent := deliver(t, handler, fake, reply("<parent@x>", ""))
	if referenced == parent {
		t.Fatal("test setup: both ancestors share a thread")
	}

	// The parent is listed among the references as well, which is what most mail
	// clients do. It must still outrank the other reference.
	got := deliver(t, handler, fake, reply("<a@x>", "<parent@x>", "<parent@x>", "<ref@x>"))
	if got != parent {
		t.Fatalf("landed in thread %d, want the direct parent %d", got, parent)
	}
}

func TestThreadingPrefersRegularThreadOverService(t *testing.T) {
	fake := newFakeStore()
	handler := testHandler(fake)

	// A bounce with no known original opens a closed service thread first.
	bounce := reply("<bounce@x>", "<ghost@x>")
	bounce.Kind = model.EmailKindBounce
	service := deliver(t, handler, fake, bounce)

	// An ordinary reply to the same unknown parent opens a regular one.
	regular := deliver(t, handler, fake, reply("<reply@x>", "<ghost@x>"))
	if regular == service {
		t.Fatal("test setup: the ordinary email joined the service thread")
	}

	// A later service email may see both, and must choose the regular thread.
	second := reply("<bounce2@x>", "<ghost@x>")
	second.Kind = model.EmailKindAutoReply
	if got := deliver(t, handler, fake, second); got != regular {
		t.Fatalf("landed in thread %d, want the regular one %d", got, regular)
	}
}
