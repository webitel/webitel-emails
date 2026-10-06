package service

import (
	"context"
	"errors"
	"testing"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/model"
)

type fakeIMAPTester struct {
	mailinfra.IMAPClient
	test func(context.Context) error
}

func (f fakeIMAPTester) Test(ctx context.Context, _ mailinfra.IMAPConnection) error {
	return f.test(ctx)
}

type fakeSMTPTester struct {
	test func(context.Context) error
}

func (f fakeSMTPTester) Test(ctx context.Context, _ mailinfra.SMTPConnection) error {
	return f.test(ctx)
}

// startThenWait marks this check as started and waits for the other one, so a
// sequential run times out instead of returning err.
func startThenWait(started chan<- struct{}, other <-chan struct{}, err error) func(context.Context) error {
	return func(ctx context.Context) error {
		close(started)

		select {
		case <-other:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func TestTestConnectionsRunsIMAPAndSMTPConcurrently(t *testing.T) {
	imapStarted, smtpStarted := make(chan struct{}), make(chan struct{})
	s := &EmailProfileService{
		imap: fakeIMAPTester{test: startThenWait(imapStarted, smtpStarted, errors.New("imap: login failed"))},
		smtp: fakeSMTPTester{test: startThenWait(smtpStarted, imapStarted, errors.New("smtp: connect failed"))},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	imapResult, smtpResult := s.testConnections(ctx, mailinfra.IMAPConnection{}, mailinfra.SMTPConnection{})

	if imapResult.Success || imapResult.Error != "imap: login failed" {
		t.Fatalf("IMAP result = %+v, want its own error", imapResult)
	}
	if smtpResult.Success || smtpResult.Error != "smtp: connect failed" {
		t.Fatalf("SMTP result = %+v, want its own error", smtpResult)
	}
}

func TestPrepareEmailProfileAppliesDefaultsAndRequiresAuthType(t *testing.T) {
	valid := func(authType model.EmailAuthType) *model.EmailProfile {
		return &model.EmailProfile{
			Name: "Support", EmailAddress: "support@example.org", Username: "support",
			IMAPHost: "imap.example.org", IMAPPort: 993, IMAPSecurity: model.EmailConnectionSecurityTLS,
			SMTPHost: "smtp.example.org", SMTPPort: 465, SMTPSecurity: model.EmailConnectionSecurityTLS,
			AuthType: authType,
		}
	}

	profile := valid(model.EmailAuthTypeBasic)
	if err := prepareEmailProfile(profile); err != nil {
		t.Fatalf("prepare basic profile: %v", err)
	}
	if profile.Mailbox != model.DefaultEmailProfileMailbox ||
		profile.FetchIntervalSeconds != model.DefaultEmailProfileFetchIntervalSeconds {
		t.Fatalf("defaults = mailbox %q, interval %d", profile.Mailbox, profile.FetchIntervalSeconds)
	}

	err := prepareEmailProfile(valid(""))
	if kiterrors.ID(err) != "email.profile.auth_type_required" {
		t.Fatalf("unspecified auth type error = %v, want email.profile.auth_type_required", err)
	}
}
