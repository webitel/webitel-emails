package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// SMTPStage is the step of a send that failed.
type SMTPStage string

// Steps of a send in their order; a failure before SMTPStageFinal never let the
// server accept the email.
const (
	SMTPStageConnect SMTPStage = "connect"
	SMTPStageAuth    SMTPStage = "auth"
	SMTPStageSize    SMTPStage = "size"
	SMTPStageMail    SMTPStage = "mail"
	SMTPStageRcpt    SMTPStage = "rcpt"
	SMTPStageData    SMTPStage = "data"
	SMTPStageBody    SMTPStage = "body"
	SMTPStageCommit  SMTPStage = "commit"
	SMTPStageFinal   SMTPStage = "final"
)

// SMTPSendError tells where a send stopped and what the server replied. It never
// carries the text of the reply, which may name a recipient.
type SMTPSendError struct {
	Stage SMTPStage
	// Code is the SMTP reply code, 0 without a reply.
	Code int
	// Enhanced is the RFC 3463 status code of the reply, such as "5.3.4".
	Enhanced string
	// Unsupported marks a missing extension or authentication mechanism.
	Unsupported bool
	err         error
}

func (e *SMTPSendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("smtp: %s rejected with %d %s", e.Stage, e.Code, e.Enhanced)
	}

	return fmt.Sprintf("smtp: %s failed", e.Stage)
}

// Unwrap exposes a transport error; a server reply is never wrapped.
func (e *SMTPSendError) Unwrap() error { return e.err }

// SMTPEnvelope is MAIL FROM and the RCPT TO addresses of one email.
type SMTPEnvelope struct {
	From       string
	Recipients []string
}

// SMTPSenderConfig bounds every step of a send.
type SMTPSenderConfig struct {
	CommandTimeout    time.Duration
	DataTimeout       time.Duration
	FinalReplyTimeout time.Duration
	SendTimeout       time.Duration
	// RootCAs verifies the server; nil uses the system pool.
	RootCAs *x509.CertPool
}

// SMTPSender delivers one encoded email per call.
type SMTPSender struct {
	cfg SMTPSenderConfig
}

// NewSMTPSender creates the sender.
func NewSMTPSender(cfg SMTPSenderConfig) *SMTPSender {
	return &SMTPSender{cfg: cfg}
}

// Send delivers raw. commit runs once the whole body is written and before the
// final dot: an error from it closes the connection, so the server discards the
// email. Nil means the server accepted it, whatever happens to QUIT.
func (s *SMTPSender) Send(
	ctx context.Context,
	connection SMTPConnection,
	envelope SMTPEnvelope,
	raw []byte,
	commit func(context.Context) error,
) error {
	conn, err := s.dial(ctx, connection)
	if err != nil {
		return &SMTPSendError{Stage: SMTPStageConnect, err: err}
	}
	// Closing the socket, never QUIT, is the only way out before the final dot:
	// textproto ends an open DATA before any next command.
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: connection.Host, RootCAs: s.cfg.RootCAs}
	client, hidden, err := startSMTP(ctx, conn, connection, tlsConfig, true)
	if err != nil {
		return connectError(err)
	}

	auth, err := smtpAuth(client, connection)
	if err != nil {
		// No AUTH after a temporary refusal of EHLO: the refusal is the cause.
		if hidden != nil && errors.Is(err, ErrSMTPAuthNotSupported) {
			return replyError(SMTPStageConnect, hidden)
		}
		return &SMTPSendError{Stage: SMTPStageAuth, Unsupported: true, err: err}
	}
	if err := client.Auth(auth); err != nil {
		return replyError(SMTPStageAuth, err)
	}

	if ok, value := client.Extension("SIZE"); ok {
		if limit, err := strconv.ParseInt(value, 10, 64); err == nil && limit > 0 && dataSize(raw) > limit {
			return &SMTPSendError{Stage: SMTPStageSize}
		}
	}

	if err := client.Mail(envelope.From); err != nil {
		return replyError(SMTPStageMail, err)
	}
	for _, recipient := range envelope.Recipients {
		if err := client.Rcpt(recipient); err != nil {
			return replyError(SMTPStageRcpt, err)
		}
	}

	data, err := client.Data()
	if err != nil {
		return replyError(SMTPStageData, err)
	}
	conn.setTimeout(s.cfg.DataTimeout)
	if _, err := data.Write(raw); err != nil {
		return &SMTPSendError{Stage: SMTPStageBody, err: err}
	}
	if err := client.Text.W.Flush(); err != nil {
		return &SMTPSendError{Stage: SMTPStageBody, err: err}
	}
	if err := commit(ctx); err != nil {
		return &SMTPSendError{Stage: SMTPStageCommit, err: err}
	}

	conn.setTimeout(s.cfg.FinalReplyTimeout)
	if err := data.Close(); err != nil {
		return replyError(SMTPStageFinal, err)
	}

	conn.setTimeout(s.cfg.CommandTimeout)
	_ = client.Quit()

	return nil
}

func (s *SMTPSender) dial(ctx context.Context, connection SMTPConnection) (*deadlineConn, error) {
	address := net.JoinHostPort(connection.Host, strconv.Itoa(int(connection.Port)))
	dialer := net.Dialer{Timeout: s.cfg.CommandTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	return &deadlineConn{Conn: raw, timeout: s.cfg.CommandTimeout, end: time.Now().Add(s.cfg.SendTimeout)}, nil
}

// dataSize counts the body as SIZE does (RFC 1870): with the CRLF the DATA writer
// adds to a body without one, without the final dot.
func dataSize(raw []byte) int64 {
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		return int64(len(raw))
	}

	return int64(len(raw)) + 2
}

// connectError keeps the codes of a refused greeting, EHLO or STARTTLS like any
// other reply; anything else stays a connection failure.
func connectError(err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return replyError(SMTPStageConnect, reply)
	}

	return &SMTPSendError{Stage: SMTPStageConnect, Unsupported: errors.Is(err, ErrSMTPStartTLSNotSupported), err: err}
}

// replyError keeps the codes of a server reply and drops its text.
func replyError(stage SMTPStage, err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return &SMTPSendError{Stage: stage, Code: reply.Code, Enhanced: enhancedCode.FindString(reply.Msg)}
	}

	return &SMTPSendError{Stage: stage, err: err}
}

var enhancedCode = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}\b`)

// deadlineConn moves the deadline before every read and write: the current step's
// timeout, never past the end of the whole send.
type deadlineConn struct {
	net.Conn
	end time.Time

	mu      sync.Mutex
	timeout time.Duration
}

func (c *deadlineConn) setTimeout(timeout time.Duration) {
	c.mu.Lock()
	c.timeout = timeout
	c.mu.Unlock()
}

func (c *deadlineConn) deadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if next := time.Now().Add(c.timeout); next.Before(c.end) {
		return next
	}

	return c.end
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(c.deadline()); err != nil {
		return 0, err
	}

	return c.Conn.Read(p)
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(c.deadline()); err != nil {
		return 0, err
	}

	return c.Conn.Write(p)
}
