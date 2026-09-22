// Package mail provides clients for validating mailbox connections.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

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

// IMAPClient validates an IMAP connection without reading mailbox data.
type IMAPClient interface {
	Test(ctx context.Context, connection IMAPConnection) error
}

type imapClient struct {
	timeout time.Duration
}

// NewIMAPClient creates an IMAP connection checker.
func NewIMAPClient() IMAPClient {
	return &imapClient{timeout: defaultIMAPTimeout}
}

func (c *imapClient) Test(ctx context.Context, connection IMAPConnection) error {
	if err := ctx.Err(); err != nil {
		return err
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
		return fmt.Errorf("imap: unsupported connection security %q", connection.Security)
	}
	if err != nil {
		return fmt.Errorf("imap: connect: %w", err)
	}
	defer imap.Terminate()

	imap.Timeout = c.timeout

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

	if err := imapAuthenticate(imap, connection); err != nil {
		return err
	}
	if err := imap.Logout(); err != nil {
		return fmt.Errorf("imap: logout: %w", err)
	}

	return nil
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
