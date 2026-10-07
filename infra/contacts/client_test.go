package contacts

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	contactsgrpc "buf.build/gen/go/webitel/webitel-go/grpc/go/contacts/contactsgrpc"
	contactspb "buf.build/gen/go/webitel/webitel-go/protocolbuffers/go/contacts"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type fakeContactsAPI struct {
	contactsgrpc.ContactsClient
	requests []*contactspb.SearchContactsNARequest
	answers  []*contactspb.ContactList
}

func (f *fakeContactsAPI) SearchContactsNA(
	_ context.Context,
	req *contactspb.SearchContactsNARequest,
	_ ...grpc.CallOption,
) (*contactspb.ContactList, error) {
	f.requests = append(f.requests, proto.Clone(req).(*contactspb.SearchContactsNARequest))

	index := len(f.requests) - 1
	if index < len(f.answers) && f.answers[index] != nil {
		return f.answers[index], nil
	}

	return &contactspb.ContactList{}, nil
}

func testClient(api contactsgrpc.ContactsClient, timeout time.Duration) *Client {
	return &Client{
		api:         api,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		timeout:     timeout,
		searchSlots: make(chan struct{}, 1),
	}
}

func TestResolveByEmailBuildsExactQueryAndClassifiesMatches(t *testing.T) {
	tests := []struct {
		name    string
		data    []*contactspb.Contact
		outcome Outcome
		id      int64
	}{
		{name: "not found", outcome: OutcomeNotFound},
		{name: "resolved", data: []*contactspb.Contact{{Id: "42"}}, outcome: OutcomeResolved, id: 42},
		{name: "ambiguous", data: []*contactspb.Contact{{Id: "42"}, {Id: "43"}}, outcome: OutcomeAmbiguous},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeContactsAPI{answers: []*contactspb.ContactList{{Data: tt.data}}}
			resolved, err := testClient(api, time.Second).ResolveByEmail(
				context.Background(), 7, "a+tag*b@example.com",
			)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolved.Outcome != tt.outcome || resolved.ContactID != tt.id {
				t.Fatalf("resolution = %+v, want outcome=%s id=%d", resolved, tt.outcome, tt.id)
			}

			req := api.requests[0]
			if req.GetDomainId() != 7 || req.GetSize() != 2 {
				t.Fatalf("domain/size = %d/%d, want 7/2", req.GetDomainId(), req.GetSize())
			}
			if len(req.GetQin()) != 1 || req.GetQin()[0] != "emails" {
				t.Fatalf("qin = %v, want [emails]", req.GetQin())
			}
			if len(req.GetFields()) != 1 || req.GetFields()[0] != "id" {
				t.Fatalf("fields = %v, want [id]", req.GetFields())
			}
			if want := `/(?i)^a\+tag\u002Ab@example\.com$/`; req.GetQ() != want {
				t.Fatalf("q = %q, want %q", req.GetQ(), want)
			}
		})
	}
}

func TestExactPatternIsCaseInsensitiveAndAnchored(t *testing.T) {
	pattern := exactPattern("A@X.com")
	expression, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/"))
	if err != nil {
		t.Fatalf("compile exact pattern: %v", err)
	}

	if !expression.MatchString("a@x.COM") {
		t.Fatal("exact address did not match independently of case")
	}
	for _, address := range []string{"xa@x.com", "a@x.com.ua"} {
		if expression.MatchString(address) {
			t.Fatalf("non-exact address %q matched", address)
		}
	}
}

func TestResolveByEmailRejectsNonNumericContactID(t *testing.T) {
	api := &fakeContactsAPI{answers: []*contactspb.ContactList{{
		Data: []*contactspb.Contact{{Id: "etag-value", Etag: "42"}},
	}}}

	if _, err := testClient(api, time.Second).ResolveByEmail(
		context.Background(), 7, "a@x.com",
	); err == nil {
		t.Fatal("a non-numeric Contact.id was accepted")
	}
}

func TestVerifyContactUsesNumericIDAndDomain(t *testing.T) {
	api := &fakeContactsAPI{answers: []*contactspb.ContactList{{
		Data: []*contactspb.Contact{{Id: "42", Etag: "not-an-id"}},
	}}}
	found, err := testClient(api, time.Second).VerifyContact(context.Background(), 7, 42)
	if err != nil || !found {
		t.Fatalf("verify = %v, %v", found, err)
	}

	req := api.requests[0]
	if req.GetDomainId() != 7 || len(req.GetId()) != 1 || req.GetId()[0] != "42" {
		t.Fatalf("request domain/id = %d/%v, want 7/[42]", req.GetDomainId(), req.GetId())
	}
}
