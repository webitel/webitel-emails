package mail

import (
	"context"
	cryptotls "crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/infra/mail/mailtest"
	"github.com/webitel/webitel-emails/internal/model"
)

var testEmail = []byte("Subject: hi\r\n\r\nHello\r\n")

func send(t *testing.T, server *mailtest.Server, connection SMTPConnection, commit func(context.Context) error) error {
	t.Helper()
	return sendRaw(t, server, connection, commit, testEmail)
}

func sendRaw(t *testing.T, server *mailtest.Server, connection SMTPConnection, commit func(context.Context) error, raw []byte) error {
	t.Helper()
	connection.Host, connection.Port, connection.Username = server.Host, server.Port, "agent@example.org"
	if commit == nil {
		commit = func(context.Context) error { return nil }
	}
	sender := NewSMTPSender(SMTPSenderConfig{
		CommandTimeout: time.Second, DataTimeout: time.Second, FinalReplyTimeout: 500 * time.Millisecond,
		SendTimeout: 5 * time.Second, RootCAs: server.RootCAs,
	})

	return sender.Send(context.Background(), connection, SMTPEnvelope{From: "agent@example.org", Recipients: []string{"a@x.com", "b@x.com"}}, raw, commit)
}

func stage(err error) (SMTPStage, int, string) {
	var sendErr *SMTPSendError
	if !errors.As(err, &sendErr) {
		return "", 0, ""
	}
	return sendErr.Stage, sendErr.Code, sendErr.Enhanced
}

func TestSMTPSendAuthenticates(t *testing.T) {
	for _, tt := range []struct {
		name       string
		connection SMTPConnection
		startTLS   bool
		offered    string
		mechanism  string
	}{
		{"PLAIN over TLS", SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}, false, "", "PLAIN"},
		{"LOGIN over TLS", SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}, false, "LOGIN", "LOGIN"},
		{"PLAIN over STARTTLS", SMTPConnection{Security: model.EmailConnectionSecurityStartTLS, Password: "p"}, true, "", "PLAIN"},
		{"XOAUTH2 over STARTTLS", SMTPConnection{Security: model.EmailConnectionSecurityStartTLS,
			AuthType: model.EmailAuthTypeOAuth2, AccessToken: "token"}, true, "", "XOAUTH2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := mailtest.Start(t, mailtest.Config{StartTLS: tt.startTLS, Mechanisms: tt.offered})
			if err := send(t, server, tt.connection, nil); err != nil {
				t.Fatal(err)
			}
			got := server.Accepted()
			if len(got) != 1 || got[0].Mechanism != tt.mechanism || len(got[0].Recipients) != 2 ||
				!strings.Contains(string(got[0].Data), "Hello") {
				t.Fatalf("accepted = %+v", got)
			}
		})
	}

}

func TestSMTPSendStops(t *testing.T) {
	tls := SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}
	tests := []struct {
		name     string
		cfg      mailtest.Config
		commit   error
		stage    SMTPStage
		code     int
		enhanced string
		accepted int
	}{
		{"larger than SIZE", mailtest.Config{Size: 10}, nil, SMTPStageSize, 0, "", 0},
		{"EHLO refused", mailtest.Config{Replies: map[string]string{"EHLO": "451 4.3.2 busy"}}, nil, SMTPStageConnect, 451, "4.3.2", 0},
		{"greeting refused", mailtest.Config{Replies: map[string]string{"GREETING": "421 4.3.2 try later"}}, nil, SMTPStageConnect, 421, "4.3.2", 0},
		{"auth refused", mailtest.Config{Replies: map[string]string{"AUTH": "535 5.7.8 bad credentials"}}, nil, SMTPStageAuth, 535, "5.7.8", 0},
		{"mail too large", mailtest.Config{Replies: map[string]string{"MAIL": "552 5.3.4 too big"}}, nil, SMTPStageMail, 552, "5.3.4", 0},
		{"one recipient refused", mailtest.Config{Replies: map[string]string{"RCPT b@x.com": "550 5.1.1 b@x.com unknown"}}, nil, SMTPStageRcpt, 550, "5.1.1", 0},
		{"final rejected", mailtest.Config{Replies: map[string]string{"FINAL": "554 5.7.1 spam"}}, nil, SMTPStageFinal, 554, "5.7.1", 0},
		{"final without reply", mailtest.Config{Replies: map[string]string{"FINAL": mailtest.FinalHang}}, nil, SMTPStageFinal, 0, "", 1},
		{"commit refused", mailtest.Config{}, errors.New("lease lost"), SMTPStageCommit, 0, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := mailtest.Start(t, tt.cfg)
			err := send(t, server, tls, func(context.Context) error { return tt.commit })
			st, code, enhanced := stage(err)
			if st != tt.stage || code != tt.code || enhanced != tt.enhanced || len(server.Accepted()) != tt.accepted {
				t.Fatalf("err = %v (%s %d %s), accepted %d", err, st, code, enhanced, len(server.Accepted()))
			}
			// The reply text, which may name a recipient, never leaves the sender.
			if err != nil && strings.Contains(err.Error(), "@") {
				t.Fatalf("error carries an address: %v", err)
			}
		})
	}

	// STARTTLS refused with a reply keeps its codes; an untrusted certificate stays
	// a connection failure with its cause.
	t.Run("STARTTLS refused", func(t *testing.T) {
		server := mailtest.Start(t, mailtest.Config{StartTLS: true, Replies: map[string]string{"STARTTLS": "454 4.7.0 TLS not available"}})
		err := send(t, server, SMTPConnection{Security: model.EmailConnectionSecurityStartTLS, Password: "p"}, nil)
		if st, code, enhanced := stage(err); st != SMTPStageConnect || code != 454 || enhanced != "4.7.0" {
			t.Fatalf("err = %v (%s %d %s)", err, st, code, enhanced)
		}
	})
	// net/smtp falls back to HELO after a refused EHLO and drops its reply; without
	// extensions the send cannot go on, and the refusal of EHLO is what counts.
	for _, tt := range []struct {
		name       string
		cfg        mailtest.Config
		connection SMTPConnection
		code       int
		enhanced   string
		stage      SMTPStage
		supported  bool
	}{
		{"EHLO 4xx then HELO, no AUTH", mailtest.Config{Replies: map[string]string{"EHLO": "451 4.3.2 busy", "HELO": "250 fake"}},
			SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}, 451, "4.3.2", SMTPStageConnect, true},
		{"EHLO 4xx then HELO, no STARTTLS", mailtest.Config{StartTLS: true, Replies: map[string]string{"EHLO": "451 4.3.2 busy", "HELO": "250 fake"}},
			SMTPConnection{Security: model.EmailConnectionSecurityStartTLS, Password: "p"}, 451, "4.3.2", SMTPStageConnect, true},
		{"first EHLO 4xx, HELO, EHLO again with AUTH", mailtest.Config{Replies: map[string]string{"EHLO#1": "451 4.3.2 busy", "HELO": "250 fake"}},
			SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}, 451, "4.3.2", SMTPStageConnect, true},
		{"first EHLO 4xx, HELO, EHLO again with STARTTLS", mailtest.Config{StartTLS: true, Replies: map[string]string{"EHLO#1": "451 4.3.2 busy", "HELO": "250 fake"}},
			SMTPConnection{Security: model.EmailConnectionSecurityStartTLS, Password: "p"}, 451, "4.3.2", SMTPStageConnect, true},
		{"EHLO 4xx then HELO refused", mailtest.Config{Replies: map[string]string{"EHLO": "451 4.3.2 busy", "HELO": "550 5.5.1 use EHLO"}},
			SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}, 451, "4.3.2", SMTPStageConnect, true},
		{"EHLO 5xx then HELO refused", mailtest.Config{Replies: map[string]string{"EHLO": "502 5.5.1 no EHLO", "HELO": "550 5.5.1 use EHLO"}},
			SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}, 550, "5.5.1", SMTPStageConnect, true},
		{"EHLO 5xx then HELO stays unsupported", mailtest.Config{Replies: map[string]string{"EHLO": "502 5.5.1 no EHLO", "HELO": "250 fake"}},
			SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p"}, 0, "", SMTPStageAuth, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := mailtest.Start(t, tt.cfg)
			err := send(t, server, tt.connection, nil)
			var failure *SMTPSendError
			if !errors.As(err, &failure) || failure.Stage != tt.stage || failure.Code != tt.code ||
				failure.Enhanced != tt.enhanced || failure.Unsupported == tt.supported || strings.Contains(err.Error(), "busy") {
				t.Fatalf("err = %v (%+v)", err, failure)
			}
		})
	}

	t.Run("untrusted certificate", func(t *testing.T) {
		server := mailtest.Start(t, mailtest.Config{})
		connection := SMTPConnection{Security: model.EmailConnectionSecurityTLS, Password: "p",
			Host: server.Host, Port: server.Port, Username: "agent@example.org"}
		sender := NewSMTPSender(SMTPSenderConfig{CommandTimeout: time.Second, DataTimeout: time.Second,
			FinalReplyTimeout: time.Second, SendTimeout: 5 * time.Second})
		err := sender.Send(context.Background(), connection, SMTPEnvelope{From: "agent@example.org", Recipients: []string{"a@x.com"}},
			testEmail, func(context.Context) error { return nil })
		var verify *cryptotls.CertificateVerificationError
		if st, code, _ := stage(err); st != SMTPStageConnect || code != 0 || !errors.As(err, &verify) {
			t.Fatalf("err = %v (%s %d)", err, st, code)
		}
	})

	// A refused commit must not let the server see the final dot.
	t.Run("commit refused leaves the body unfinished", func(t *testing.T) {
		server := mailtest.Start(t, mailtest.Config{})
		_ = send(t, server, tls, func(context.Context) error { return errors.New("lease lost") })
		time.Sleep(50 * time.Millisecond)
		if _, discarded := server.Sessions(); discarded != 1 || len(server.Accepted()) != 0 {
			t.Fatalf("discarded %d, accepted %d", discarded, len(server.Accepted()))
		}
	})

	// DATA ends a body without a final CRLF with one, and SIZE counts it.
	t.Run("SIZE counts the CRLF the DATA writer adds", func(t *testing.T) {
		raw := []byte("Subject: hi\r\n\r\nHello")
		server := mailtest.Start(t, mailtest.Config{Size: int64(len(raw))})
		if st, _, _ := stage(sendRaw(t, server, tls, nil, raw)); st != SMTPStageSize {
			t.Fatalf("stage = %q", st)
		}
		server = mailtest.Start(t, mailtest.Config{Size: int64(len(raw)) + 2})
		if err := sendRaw(t, server, tls, nil, raw); err != nil || len(server.Accepted()) != 1 {
			t.Fatalf("at the limit: %v", err)
		}
	})

	t.Run("accepted before a lost QUIT", func(t *testing.T) {
		server := mailtest.Start(t, mailtest.Config{DropOnQuit: true})
		if err := send(t, server, tls, nil); err != nil || len(server.Accepted()) != 1 {
			t.Fatalf("err = %v, accepted %d", err, len(server.Accepted()))
		}
	})
}

func TestEHLORefusal(t *testing.T) {
	for _, tt := range []struct {
		replies  string
		code     int
		enhanced string
	}{
		{"451-4.3.2 busy\r\n451 4.3.2 later\r\n250 fake\r\n", 451, "4.3.2"},
		{"421 4.3.2 closing\r\n", 421, "4.3.2"},
		{"250-fake\r\n250 AUTH PLAIN\r\n", 0, ""},
		{"502 5.5.1 no EHLO\r\n250 fake\r\n", 0, ""},
		{"45", 0, ""},
		{"", 0, ""},
	} {
		got := ehloRefusal([]byte(tt.replies))
		code, enhanced := 0, ""
		if got != nil {
			code, enhanced = got.Code, enhancedCode.FindString(got.Msg)
		}
		if code != tt.code || enhanced != tt.enhanced {
			t.Errorf("%q: %d %q", tt.replies, code, enhanced)
		}
	}
}
