// Package mail provides IMAP and SMTP clients for mailbox connections.
package mail

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"github.com/webitel/webitel-emails/internal/model"
)

const defaultIMAPTimeout = 20 * time.Second

// ErrStartTLSNotSupported reports an IMAP server that does not advertise
// STARTTLS when the profile requires it.
var ErrStartTLSNotSupported = errors.New("imap: STARTTLS is not supported")

// ErrXOAUTH2NotSupported reports an IMAP server that does not advertise the
// XOAUTH2 authentication mechanism when the profile requires it.
var ErrXOAUTH2NotSupported = errors.New("imap: XOAUTH2 is not supported")

// IMAPConnection contains the settings required to validate a connection,
// either with Basic Auth or, for an OAuth2 profile, XOAUTH2.
type IMAPConnection struct {
	Host     string
	Port     int32
	Security model.EmailConnectionSecurity
	Username string
	Password string
	// AuthType selects Basic (default, including an empty value, matching
	// model.EmailProfile) or OAuth2 (XOAUTH2) authentication.
	AuthType model.EmailAuthType
	// AccessToken is the OAuth2 bearer token used for XOAUTH2 when AuthType
	// is EmailAuthTypeOAuth2. It is ignored otherwise.
	AccessToken string
}

// IMAPClient opens IMAP connections to a mailbox.
type IMAPClient interface {
	// Test validates a connection without reading mailbox data.
	Test(ctx context.Context, connection IMAPConnection) error
	// Connect opens an authenticated session that can be reused between polls.
	Connect(ctx context.Context, connection IMAPConnection) (IMAPSession, error)
}

// IMAPSession is an authenticated IMAP connection owned by one caller at a time.
type IMAPSession interface {
	// Noop checks that the connection is still alive.
	Noop() error
	// Close logs out and closes the connection.
	Close() error
	// Abort closes the connection at once, interrupting a running command.
	Abort()
	// Examine opens a mailbox read-only.
	Examine(mailbox string) (*IMAPMailbox, error)
	// SearchUIDsRange returns UIDs in [from, to] in ascending order; to == 0 means unbounded
	// (matching the IMAP '*' wildcard), for servers that do not report UIDNEXT.
	SearchUIDsRange(from, to uint32) ([]uint32, error)
	// FetchRaw fetches raw RFC822 messages with BODY.PEEK[], so \Seen is not set.
	FetchRaw(uids []uint32) ([]*IMAPMessage, error)
}

// IMAPMailbox is the state of a mailbox opened read-only.
type IMAPMailbox struct {
	Name        string
	UIDValidity uint32
	// Zero when the server did not report UIDNEXT.
	UIDNext uint32
}

// IMAPMessage is one raw RFC822 message.
type IMAPMessage struct {
	UID uint32
	Raw []byte
}

type imapClient struct {
	timeout time.Duration
}

// NewIMAPClient creates an IMAP client.
func NewIMAPClient() IMAPClient {
	return &imapClient{timeout: defaultIMAPTimeout}
}

func (c *imapClient) Test(ctx context.Context, connection IMAPConnection) error {
	session, err := c.Connect(ctx, connection)
	if err != nil {
		return err
	}

	return session.Close()
}

func (c *imapClient) Connect(ctx context.Context, connection IMAPConnection) (IMAPSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	address := net.JoinHostPort(connection.Host, strconv.Itoa(int(connection.Port)))
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: connection.Host,
	}
	dialer := contextDialer{
		ctx:     ctx,
		dialer:  &net.Dialer{Timeout: c.timeout},
		timeout: c.timeout,
	}

	var (
		imap *client.Client
		err  error
	)

	switch connection.Security {
	case model.EmailConnectionSecurityTLS:
		imap, err = client.DialWithDialerTLS(dialer, address, tlsConfig)
	case model.EmailConnectionSecurityStartTLS:
		imap, err = client.DialWithDialer(dialer, address)
	default:
		return nil, fmt.Errorf("imap: unsupported connection security %q", connection.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: connect: %w", err)
	}

	// Each command sets and then clears its own deadline, replacing the dial deadline.
	imap.Timeout = c.timeout

	if err := startIMAPSession(imap, connection, tlsConfig); err != nil {
		_ = imap.Terminate()

		return nil, err
	}

	return &imapSession{client: imap}, nil
}

func startIMAPSession(imap *client.Client, connection IMAPConnection, tlsConfig *tls.Config) error {
	if connection.Security == model.EmailConnectionSecurityStartTLS {
		supported, err := imap.SupportStartTLS()
		if err != nil {
			return fmt.Errorf("imap: check STARTTLS support: %w", err)
		}
		if !supported {
			return ErrStartTLSNotSupported
		}
		if err := imap.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("imap: start TLS: %w", err)
		}
	}

	return imapAuthenticate(imap, connection)
}

type imapSession struct {
	client *client.Client
}

func (s *imapSession) Noop() error {
	if err := s.client.Noop(); err != nil {
		return fmt.Errorf("imap: noop: %w", err)
	}

	return nil
}

func (s *imapSession) Close() error {
	if err := s.client.Logout(); err != nil {
		_ = s.client.Terminate()

		return fmt.Errorf("imap: logout: %w", err)
	}

	return nil
}

func (s *imapSession) Abort() {
	_ = s.client.Terminate()
}

func (s *imapSession) Examine(mailbox string) (*IMAPMailbox, error) {
	status, err := s.client.Select(mailbox, true)
	if err != nil {
		return nil, fmt.Errorf("imap: examine %q: %w", mailbox, err)
	}

	return &IMAPMailbox{Name: mailbox, UIDValidity: status.UidValidity, UIDNext: status.UidNext}, nil
}

func (s *imapSession) SearchUIDsRange(from, to uint32) ([]uint32, error) {
	// A range ending with * always includes the last message, so out-of-range UIDs are
	// filtered out below regardless of which bound (or both) the caller gave.
	set := new(imap.SeqSet)
	set.AddRange(from, to)

	criteria := imap.NewSearchCriteria()
	criteria.Uid = set

	found, err := s.client.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("imap: search uids: %w", err)
	}

	uids := make([]uint32, 0, len(found))
	for _, id := range found {
		if id < from || (to != 0 && id > to) {
			continue
		}

		uids = append(uids, id)
	}
	slices.Sort(uids)

	return slices.Compact(uids), nil
}

func (s *imapSession) FetchRaw(uids []uint32) ([]*IMAPMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}

	set := new(imap.SeqSet)
	set.AddNum(uids...)

	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, section.FetchItem()}

	fetched := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- s.client.UidFetch(set, items, fetched)
	}()

	messages := make([]*IMAPMessage, 0, len(uids))
	var readErr error
	for message := range fetched {
		if message.Uid == 0 {
			// Not attributable to any message; expunged messages are simply missing below.
			continue
		}

		body := message.GetBody(section)
		if body == nil {
			// The UID exists but the section is missing: a server error, not an expunge, so it
			// fails the batch instead of letting the cursor silently skip the message.
			if readErr == nil {
				readErr = fmt.Errorf("imap: message %d has no body in the FETCH response", message.Uid)
			}

			continue
		}

		raw, err := io.ReadAll(body)
		if err != nil && readErr == nil {
			readErr = fmt.Errorf("imap: read message %d: %w", message.Uid, err)
		}

		messages = append(messages, &IMAPMessage{UID: message.Uid, Raw: raw})
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("imap: fetch messages: %w", err)
	}
	if readErr != nil {
		return nil, readErr
	}

	// Messages expunged after the search are simply missing from the result.
	slices.SortFunc(messages, func(a, b *IMAPMessage) int { return cmp.Compare(a.UID, b.UID) })

	return messages, nil
}

// imapAuthenticate logs in with Basic Auth, or, for an OAuth2 profile,
// authenticates over XOAUTH2 using the caller-supplied access token.
func imapAuthenticate(imapClient *client.Client, connection IMAPConnection) error {
	if connection.AuthType != model.EmailAuthTypeOAuth2 {
		if err := imapClient.Login(connection.Username, connection.Password); err != nil {
			return fmt.Errorf("imap: authenticate: %w", err)
		}

		return nil
	}

	supported, err := imapClient.SupportAuth(xoauth2Mechanism)
	if err != nil {
		return fmt.Errorf("imap: check XOAUTH2 support: %w", err)
	}
	if !supported {
		return ErrXOAUTH2NotSupported
	}
	if err := imapClient.Authenticate(newXOAUTH2IMAPClient(connection.Username, connection.AccessToken)); err != nil {
		return fmt.Errorf("imap: authenticate: %w", err)
	}

	return nil
}

type contextDialer struct {
	ctx     context.Context
	dialer  *net.Dialer
	timeout time.Duration
}

func (d contextDialer) Dial(network, address string) (net.Conn, error) {
	connection, err := d.dialer.DialContext(d.ctx, network, address)
	if err != nil {
		return nil, err
	}

	if d.timeout > 0 {
		if err := connection.SetDeadline(time.Now().Add(d.timeout)); err != nil {
			connection.Close()
			return nil, err
		}
	}

	return connection, nil
}
