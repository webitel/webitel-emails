package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
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
)

// SMTPConnection contains the settings required to validate Basic Auth.
type SMTPConnection struct {
	Host     string
	Port     int32
	Security model.EmailConnectionSecurity
	Username string
	Password string
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

	var smtpConnection net.Conn = rawConnection
	switch connection.Security {
	case model.EmailConnectionSecurityTLS:
		tlsConnection := tls.Client(rawConnection, tlsConfig)
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp: TLS handshake: %w", err)
		}
		smtpConnection = tlsConnection
	case model.EmailConnectionSecurityStartTLS:
	default:
		return fmt.Errorf("smtp: unsupported connection security %q", connection.Security)
	}

	client, err := smtp.NewClient(smtpConnection, connection.Host)
	if err != nil {
		return fmt.Errorf("smtp: initialize client: %w", err)
	}
	defer client.Close()

	if connection.Security == model.EmailConnectionSecurityStartTLS {
		if supported, _ := client.Extension("STARTTLS"); !supported {
			return ErrSMTPStartTLSNotSupported
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("smtp: start TLS: %w", err)
		}
	}

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

func smtpAuth(client *smtp.Client, connection SMTPConnection) (smtp.Auth, error) {
	supported, mechanisms := client.Extension("AUTH")
	if !supported {
		return nil, ErrSMTPAuthNotSupported
	}

	for _, mechanism := range strings.Fields(strings.ToUpper(mechanisms)) {
		if mechanism == "PLAIN" {
			return smtp.PlainAuth("", connection.Username, connection.Password, connection.Host), nil
		}
	}
	for _, mechanism := range strings.Fields(strings.ToUpper(mechanisms)) {
		if mechanism == "LOGIN" {
			return loginAuth{
				username: connection.Username,
				password: connection.Password,
				host:     connection.Host,
			}, nil
		}
	}

	return nil, ErrSMTPAuthNotSupported
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
