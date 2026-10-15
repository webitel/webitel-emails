package mail

import (
	"fmt"
	"net/smtp"

	"github.com/emersion/go-sasl"
)

// xoauth2Mechanism is the SASL/AUTH mechanism name IMAP and SMTP servers
// advertise for OAuth2 bearer tokens, defined by Google and reused by Microsoft 365.
const xoauth2Mechanism = "XOAUTH2"

// xoauth2InitialResponse builds the initial client response both IMAP and
// SMTP expect for XOAUTH2.
func xoauth2InitialResponse(username, accessToken string) []byte {
	return []byte("user=" + username + "\x01auth=Bearer " + accessToken + "\x01\x01")
}

// xoauth2IMAPClient implements github.com/emersion/go-sasl.Client for IMAP.
type xoauth2IMAPClient struct {
	username    string
	accessToken string
}

// newXOAUTH2IMAPClient builds the SASL client used to authenticate an IMAP
// connection with an OAuth2 access token.
func newXOAUTH2IMAPClient(username, accessToken string) sasl.Client {
	return &xoauth2IMAPClient{username: username, accessToken: accessToken}
}

func (a *xoauth2IMAPClient) Start() (mech string, initialResponse []byte, err error) {
	return xoauth2Mechanism, xoauth2InitialResponse(a.username, a.accessToken), nil
}

// Next answers the server's XOAUTH2 error challenge with an empty response, as the
// protocol requires, so Authenticate returns the server's own failure, not a local one.
func (a *xoauth2IMAPClient) Next(challenge []byte) ([]byte, error) {
	return []byte{}, nil
}

// xoauth2SMTPAuth implements net/smtp.Auth for SMTP.
type xoauth2SMTPAuth struct {
	username    string
	accessToken string
}

// newXOAUTH2SMTPAuth builds the smtp.Auth used to authenticate an SMTP
// connection with an OAuth2 access token.
func newXOAUTH2SMTPAuth(username, accessToken string) smtp.Auth {
	return &xoauth2SMTPAuth{username: username, accessToken: accessToken}
}

func (a *xoauth2SMTPAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, fmt.Errorf("smtp: unencrypted authentication is not allowed")
	}

	return xoauth2Mechanism, xoauth2InitialResponse(a.username, a.accessToken), nil
}

// Next answers the XOAUTH2 error challenge like xoauth2IMAPClient.Next: an empty,
// non-nil response, so net/smtp returns the server's own final reply as the error.
func (a *xoauth2SMTPAuth) Next(challenge []byte, more bool) ([]byte, error) {
	// more is false only after the final reply, which net/smtp already returns as the error.
	if !more {
		return nil, nil
	}

	return []byte{}, nil
}
