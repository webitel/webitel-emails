package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/webitel/webitel-emails/internal/model"
)

const defaultSMTPTimeout = 20 * time.Second

var (
	// ErrSMTPStartTLSNotSupported reports an SMTP server that does not
	// advertise STARTTLS when the profile requires it.
	ErrSMTPStartTLSNotSupported = errors.New("smtp: STARTTLS is not supported")
	// ErrSMTPAuthNotSupported reports an SMTP server without a supported Basic
	// authentication mechanism.
	ErrSMTPAuthNotSupported = errors.New("smtp: Basic authentication is not supported")
	// ErrSMTPXOAUTH2NotSupported reports an SMTP server that does not advertise
	// the XOAUTH2 authentication mechanism when the profile requires it.
	ErrSMTPXOAUTH2NotSupported = errors.New("smtp: XOAUTH2 is not supported")
)

// SMTPConnection contains the settings required to validate a connection,
// either with Basic Auth or, for an OAuth2 profile, XOAUTH2.
type SMTPConnection struct {
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

// SMTPClient validates an SMTP connection without sending a message.
type SMTPClient interface {
	Test(ctx context.Context, connection SMTPConnection) error
}

type smtpClient struct {
	timeout time.Duration
}

// NewSMTPClient creates an SMTP connection checker.
func NewSMTPClient() SMTPClient {
	return &smtpClient{timeout: defaultSMTPTimeout}
}

func (c *smtpClient) Test(ctx context.Context, connection SMTPConnection) error {
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

	rawConnection, err := dialer.Dial("tcp", address)
	if err != nil {
		return fmt.Errorf("smtp: connect: %w", err)
	}
	defer rawConnection.Close()

	client, _, err := startSMTP(ctx, rawConnection, connection, tlsConfig, false)
	if err != nil {
		return err
	}
	defer client.Close()

	auth, err := smtpAuth(client, connection)
	if err != nil {
		return err
	}
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("smtp: authenticate: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("smtp: quit: %w", err)
	}

	return nil
}

// startSMTP greets the server and applies the configured TLS mode.
// With greet, it preserves a temporary EHLO refusal that the HELO fallback can hide.
func startSMTP(
	ctx context.Context,
	conn net.Conn,
	connection SMTPConnection,
	tlsConfig *tls.Config,
	greet bool,
) (*smtp.Client, *textproto.Error, error) {
	switch connection.Security {
	case model.EmailConnectionSecurityTLS:
		tlsConnection := tls.Client(conn, tlsConfig)
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return nil, nil, fmt.Errorf("smtp: TLS handshake: %w", err)
		}
		conn = tlsConnection
	case model.EmailConnectionSecurityStartTLS:
	default:
		return nil, nil, fmt.Errorf("smtp: unsupported connection security %q", connection.Security)
	}

	client, err := smtp.NewClient(conn, connection.Host)
	if err != nil {
		return nil, nil, fmt.Errorf("smtp: initialize client: %w", err)
	}
	var hidden *textproto.Error
	if greet {
		// Over the reader, not the connection: net/smtp tells TLS by the type of
		// the connection.
		recorder := &helloRecorder{}
		client.Text.R = bufio.NewReader(io.TeeReader(client.Text.R, recorder))
		recorder.recording = true
		err := client.Hello("localhost")
		recorder.recording = false
		hidden = ehloRefusal(recorder.replies.Bytes())
		if err != nil {
			client.Close()
			// A refused HELO after a temporary refusal of EHLO: EHLO is the cause.
			if hidden != nil {
				return nil, nil, fmt.Errorf("smtp: greeting: %w", hidden)
			}
			return nil, nil, fmt.Errorf("smtp: greeting: %w", err)
		}
	}

	if connection.Security == model.EmailConnectionSecurityStartTLS {
		if supported, _ := client.Extension("STARTTLS"); !supported {
			client.Close()
			if hidden != nil {
				return nil, nil, fmt.Errorf("smtp: greeting: %w", hidden)
			}
			return nil, nil, ErrSMTPStartTLSNotSupported
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			client.Close()
			return nil, nil, fmt.Errorf("smtp: start TLS: %w", err)
		}
	}

	return client, hidden, nil
}

// maxHelloReplies bounds what is kept of the replies to EHLO and HELO.
const maxHelloReplies = 4 << 10

// helloRecorder keeps what the server sends while net/smtp greets it: net/smtp
// drops the reply to EHLO when it falls back to HELO.
type helloRecorder struct {
	recording bool
	replies   bytes.Buffer
}

func (r *helloRecorder) Write(p []byte) (int, error) {
	if r.recording {
		r.replies.Write(p[:min(len(p), maxHelloReplies-r.replies.Len())])
	}

	return len(p), nil
}

// ehloRefusal reads the first recorded reply, the one to EHLO, and returns it when
// it is a temporary refusal; a refusal for good means a server without EHLO.
func ehloRefusal(replies []byte) *textproto.Error {
	line, _, _ := bytes.Cut(replies, []byte("\n"))
	line = bytes.TrimRight(line, "\r")
	if len(line) < 4 || (line[3] != ' ' && line[3] != '-') {
		return nil
	}
	code, err := strconv.Atoi(string(line[:3]))
	if err != nil || code < 400 || code >= 500 {
		return nil
	}

	return &textproto.Error{Code: code, Msg: string(line[4:])}
}

func smtpAuth(client *smtp.Client, connection SMTPConnection) (smtp.Auth, error) {
	supported, mechanisms := client.Extension("AUTH")
	if !supported {
		return nil, ErrSMTPAuthNotSupported
	}

	if connection.AuthType == model.EmailAuthTypeOAuth2 {
		if !smtpMechanismSupported(mechanisms, xoauth2Mechanism) {
			return nil, ErrSMTPXOAUTH2NotSupported
		}

		return newXOAUTH2SMTPAuth(connection.Username, connection.AccessToken), nil
	}

	if smtpMechanismSupported(mechanisms, "PLAIN") {
		return smtp.PlainAuth("", connection.Username, connection.Password, connection.Host), nil
	}
	if smtpMechanismSupported(mechanisms, "LOGIN") {
		return loginAuth{
			username: connection.Username,
			password: connection.Password,
			host:     connection.Host,
		}, nil
	}

	return nil, ErrSMTPAuthNotSupported
}

// smtpMechanismSupported reports whether mechanism is present in the
// space-separated AUTH mechanism list the server advertised.
func smtpMechanismSupported(mechanisms, mechanism string) bool {
	for _, candidate := range strings.Fields(strings.ToUpper(mechanisms)) {
		if candidate == mechanism {
			return true
		}
	}

	return false
}

type loginAuth struct {
	username string
	password string
	host     string
}

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("smtp: unencrypted authentication is not allowed")
	}
	if server.Name != a.host {
		return "", nil, errors.New("smtp: wrong server name")
	}

	return "LOGIN", nil, nil
}

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}

	switch {
	case bytes.Equal(challenge, []byte("Username:")):
		return []byte(a.username), nil
	case bytes.Equal(challenge, []byte("Password:")):
		return []byte(a.password), nil
	default:
		return nil, errors.New("smtp: unexpected authentication challenge")
	}
}
