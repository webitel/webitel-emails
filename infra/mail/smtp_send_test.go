package mail

import (
	"context"
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
		{"greeting refused", mailtest.Config{Replies: map[string]string{"EHLO": "451 4.3.2 busy"}}, nil, SMTPStageConnect, 0, "", 0},
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
