// Package contacts resolves Webitel contacts by email address.
package contacts

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	contactsgrpc "buf.build/gen/go/webitel/webitel-go/grpc/go/contacts/contactsgrpc"
	contactspb "buf.build/gen/go/webitel/webitel-go/protocolbuffers/go/contacts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra/webitelapp"
)

// Outcome is what a search by address found.
type Outcome string

const (
	// OutcomeResolved means exactly one contact holds the address.
	OutcomeResolved Outcome = "resolved"
	// OutcomeNotFound means no contact holds it.
	OutcomeNotFound Outcome = "not_found"
	// OutcomeAmbiguous means more than one contact holds it, so an operator
	// decides instead of the service.
	OutcomeAmbiguous Outcome = "ambiguous"
)

// Resolution is the result of one search; ContactID is set only when resolved.
type Resolution struct {
	Outcome   Outcome
	ContactID int64
}

// searchPageSize is two on purpose: one match resolves the address, a second one
// is already enough to call it ambiguous.
const searchPageSize = 2

// Client searches contacts over the shared go.webitel.app connection.
type Client struct {
	api     contactsgrpc.ContactsClient
	log     *slog.Logger
	timeout time.Duration
	// searchSlots bounds the searches running at the same time.
	searchSlots chan struct{}
}

// NewClient builds the Contacts client.
func NewClient(conn webitelapp.Conn, cfg *config.Config, log *slog.Logger) *Client {
	return &Client{
		api:         contactsgrpc.NewContactsClient(conn.ClientConn),
		log:         log.With("component", "contacts_client"),
		timeout:     cfg.Contacts.SearchTimeout,
		searchSlots: make(chan struct{}, cfg.Contacts.MaxConcurrentSearches),
	}
}

// ResolveByEmail finds the contact of one address within a domain. The address is
// matched exactly, so a longer address that merely contains it never wins.
func (c *Client) ResolveByEmail(
	ctx context.Context,
	domainID int64,
	address string,
) (Resolution, error) {
	list, err := c.search(ctx, &contactspb.SearchContactsNARequest{
		DomainId: domainID,
		Qin:      []string{"emails"},
		Q:        exactPattern(address),
		Size:     searchPageSize,
		Fields:   []string{"id"},
	})
	if err != nil {
		return Resolution{}, err
	}

	switch items := list.GetData(); len(items) {
	case 0:
		return Resolution{Outcome: OutcomeNotFound}, nil
	case 1:
		id, err := contactID(items[0])
		if err != nil {
			return Resolution{}, err
		}

		return Resolution{Outcome: OutcomeResolved, ContactID: id}, nil
	default:
		return Resolution{Outcome: OutcomeAmbiguous}, nil
	}
}

// VerifyContact reports whether a contact chosen by an operator belongs to the
// caller's domain, so a manual binding cannot reach another tenant.
func (c *Client) VerifyContact(ctx context.Context, domainID, contactID int64) (bool, error) {
	list, err := c.search(ctx, &contactspb.SearchContactsNARequest{
		DomainId: domainID,
		Id:       []string{strconv.FormatInt(contactID, 10)},
		Size:     1,
		Fields:   []string{"id"},
	})
	if err != nil {
		return false, err
	}

	return len(list.GetData()) == 1, nil
}

// search runs one request under a single deadline that covers waiting for a slot,
// the call itself and its one retry, so contact resolution can never hold up the
// email longer than the configured timeout.
func (c *Client) search(
	ctx context.Context,
	req *contactspb.SearchContactsNARequest,
) (*contactspb.ContactList, error) {
	searchCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	select {
	case c.searchSlots <- struct{}{}:
		defer func() { <-c.searchSlots }()
	case <-searchCtx.Done():
		// A caller that went away is reported as it is; running out of our own
		// deadline while queued is an unavailable service to everyone above.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		return nil, unavailable(searchCtx.Err())
	}

	list, err := c.api.SearchContactsNA(searchCtx, req)
	if err == nil {
		return list, nil
	}
	// Only an unavailable service is worth a second try, and only while the
	// deadline shared with the first attempt still has time left.
	if status.Code(err) != codes.Unavailable || searchCtx.Err() != nil {
		return nil, unavailable(err)
	}

	c.log.Debug("contacts search retried", "domain_id", req.GetDomainId())

	list, err = c.api.SearchContactsNA(searchCtx, req)
	if err != nil {
		return nil, unavailable(err)
	}

	return list, nil
}

// contactID keeps the numeric identifier only; the etag of a contact is a
// different value and is never stored as contact_id.
func contactID(contact *contactspb.Contact) (int64, error) {
	id, err := strconv.ParseInt(contact.GetId(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("contacts: unexpected contact id %q: %w", contact.GetId(), err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("contacts: unexpected contact id %d", id)
	}

	return id, nil
}

// exactPattern builds the query that matches one address and nothing else. A
// plain query is turned into an ILIKE '%value%' by go.webitel.app, which also
// matches longer addresses, so the regexp form of its search is used instead.
func exactPattern(address string) string {
	// The query is split on "*" before it reaches the regexp, so an address
	// carrying one is encoded as the ARE escape of that character instead. The
	// replacement runs on the already escaped text, so it never doubles the
	// backslash of that escape.
	escaped := strings.ReplaceAll(regexp.QuoteMeta(address), `\*`, `\u002A`)

	return `/(?i)^` + escaped + `$/`
}
