package mail

import (
	"fmt"
	"net/smtp"

	"github.com/emersion/go-sasl"
)

// xoauth2Mechanism is the SASL/AUTH mechanism name IMAP and SMTP servers
// advertise for OAuth2 bearer-token authentication, as described by Google's
// XOAUTH2 protocol (https://developers.google.com/gmail/imap/xoauth2-protocol)
// and used the same way by Microsoft 365/Outlook.
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

// Next responds to the server's XOAUTH2 error challenge (a JSON-encoded
// status/schemes/scope payload sent when the access token is rejected). Per
// the XOAUTH2 protocol, the client must reply with an empty response rather
// than abort the exchange itself; go-imap then reads the server's own
// authoritative failure as the tagged response and returns that as the
// error from Authenticate, instead of a locally synthesized one.
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

// Next responds to the server's XOAUTH2 error challenge the same way as
// xoauth2IMAPClient.Next: an empty, non-nil response rather than an abort,
// so net/smtp.Client.Auth reads the server's own final SMTP reply (e.g. a
// 535) as the returned error instead of one synthesized locally. more is
// false only after that final reply, which net/smtp already turns into the
// returned error itself, so there is nothing left to send.
func (a *xoauth2SMTPAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}

	return []byte{}, nil
}
