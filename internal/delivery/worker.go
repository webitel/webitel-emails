// Package delivery sends accepted outbound emails over SMTP and retries a failed
// attempt while it certainly did not reach the server.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"go.uber.org/fx"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/config"
	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/outbound"
	"github.com/webitel/webitel-emails/internal/store"
)

// finishTimeout bounds the final write, also after the send was canceled.
const finishTimeout = 2 * time.Second

var errLeaseLost = errors.New("delivery: attempt is no longer this worker's")

// ProfileSource loads a profile and its SMTP credentials.
type ProfileSource interface {
	Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error)
	SMTPConnection(ctx context.Context, profile *model.EmailProfile) (mailinfra.SMTPConnection, error)
}

// FileOpener streams a stored attachment, failing unless exactly size bytes arrive.
type FileOpener interface {
	Open(ctx context.Context, domainID, fileID, size int64) (io.ReadCloser, error)
}

// Sender delivers one encoded email; commit runs before the final dot.
type Sender interface {
	Send(ctx context.Context, connection mailinfra.SMTPConnection, envelope mailinfra.SMTPEnvelope,
		raw []byte, commit func(context.Context) error) error
}

// Params are the worker dependencies.
type Params struct {
	fx.In

	Config     *config.Config
	Deliveries store.EmailDeliveryStore
	UoW        store.UnitOfWork
	Profiles   ProfileSource
	Files      FileOpener
	SMTP       Sender
	Log        *slog.Logger
	Lifecycle  fx.Lifecycle `optional:"true"`
}

// Worker claims queued emails on every instance, within its own limits.
type Worker struct {
	cfg        config.OutboundConfig
	deliveries store.EmailDeliveryStore
	uow        store.UnitOfWork
	profiles   ProfileSource
	files      FileOpener
	smtp       Sender
	log        *slog.Logger

	slots chan struct{}
	mu    sync.Mutex
	// busy counts the sends of each profile on this instance.
	busy map[int64]int

	sends     sync.WaitGroup
	sendCtx   context.Context
	cancelAll context.CancelFunc
	stopLoop  context.CancelFunc
	loopDone  chan struct{}
}

// New creates the worker and, with a lifecycle, starts and stops it with the app.
func New(p Params) *Worker {
	sendCtx, cancelAll := context.WithCancel(context.Background())
	w := &Worker{
		cfg: p.Config.Outbound, deliveries: p.Deliveries, uow: p.UoW, profiles: p.Profiles,
		files: p.Files, smtp: p.SMTP, log: p.Log.With("component", "outbound_delivery"),
		slots: make(chan struct{}, p.Config.Outbound.MaxConcurrentDeliveries), busy: map[int64]int{},
		sendCtx: sendCtx, cancelAll: cancelAll, loopDone: make(chan struct{}),
	}
	if p.Lifecycle != nil {
		p.Lifecycle.Append(fx.Hook{
			OnStart: func(context.Context) error { w.Start(); return nil },
			OnStop:  w.Stop,
		})
	}

	return w
}

// Start runs the claim loop.
func (w *Worker) Start() {
	ctx, stop := context.WithCancel(context.Background())
	w.stopLoop = stop

	go func() {
		defer close(w.loopDone)
		ticker := time.NewTicker(w.cfg.DeliveryTickInterval)
		defer ticker.Stop()
		for {
			w.dispatch(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop claims nothing new and lets sends run until shortly before the deadline of
// ctx, then cancels them, keeping time for their final writes. fx runs the stop
// hooks one after another within one budget, so there is no drain of its own.
func (w *Worker) Stop(ctx context.Context) error {
	if w.stopLoop != nil {
		w.stopLoop()
		<-w.loopDone
	}

	done := make(chan struct{})
	go func() { w.sends.Wait(); close(done) }()

	drainCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		drainCtx, cancel = context.WithDeadline(ctx, deadline.Add(-finishTimeout))
		defer cancel()
	}
	select {
	case <-done:
		return nil
	case <-drainCtx.Done():
	}

	w.log.Warn("canceling outbound sends at shutdown")
	w.cancelAll()
	select {
	case <-done:
	case <-ctx.Done():
	}

	return nil
}

func (w *Worker) dispatch(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case w.slots <- struct{}{}:
		default:
			return
		}

		d, err := w.deliveries.Claim(ctx, w.cfg.DeliveryLease, w.fullProfiles())
		if err != nil || d == nil {
			<-w.slots
			if err != nil && ctx.Err() == nil {
				w.log.Error("claim outbound email", "err", err)
			}
			return
		}

		w.mu.Lock()
		w.busy[d.ProfileID]++
		w.mu.Unlock()
		w.sends.Add(1)
		go w.run(*d)
	}
}

func (w *Worker) fullProfiles() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	full := []int64{}
	for profileID, sends := range w.busy {
		if sends >= w.cfg.MaxDeliveriesPerProfile {
			full = append(full, profileID)
		}
	}

	return full
}

func (w *Worker) run(d store.EmailDelivery) {
	defer func() {
		w.mu.Lock()
		if w.busy[d.ProfileID]--; w.busy[d.ProfileID] <= 0 {
			delete(w.busy, d.ProfileID)
		}
		w.mu.Unlock()
		<-w.slots
		w.sends.Done()
	}()

	w.Deliver(w.sendCtx, d)
}

// Deliver makes the one SMTP attempt of a claimed email and stores its result.
func (w *Worker) Deliver(ctx context.Context, d store.EmailDelivery) {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.DeliverySendTimeout)
	defer cancel()
	log := w.log.With("domain_id", d.DomainID, "profile_id", d.ProfileID, "message_id", d.MessageID, "attempt", d.Token)

	outcome, err := w.attempt(ctx, d)
	if errors.Is(err, errLeaseLost) || outcome.Status == "" {
		// The commit did not apply or its result is unknown: recovery decides by
		// the stored phase.
		log.Warn("outbound email left to recovery", "err", err)
		return
	}

	finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancelFinish()
	applied, requeued, finishErr := w.deliveries.Finish(finishCtx, d, outcome)
	fields := []any{"status", outcome.Status, "code", outcome.Code}
	switch {
	case finishErr != nil:
		log.Error("store outbound email result", append(fields, "err", finishErr)...)
	case !applied:
		log.Warn("outbound email result was already stored", fields...)
	case outcome.Status == model.EmailDeliveryStatusSent:
		log.Info("outbound email sent")
	case requeued:
		log.Warn("outbound email retry scheduled", append([]any{"code", outcome.Code, "err", err}, smtpFields(err)...)...)
	default:
		log.Warn("outbound email failed", append([]any{"code", outcome.Code, "retryable", outcome.Retryable, "err", err},
			smtpFields(err)...)...)
	}
}

// attempt returns the outcome to store; an empty status means none may be stored.
func (w *Worker) attempt(ctx context.Context, d store.EmailDelivery) (store.EmailDeliveryOutcome, error) {
	failed := func(code model.EmailDeliveryError, retryable bool, err error) (store.EmailDeliveryOutcome, error) {
		return store.EmailDeliveryOutcome{Status: model.EmailDeliveryStatusFailed, Code: code, Retryable: retryable}, err
	}
	// gaveUp: the send timed out or the worker stops, which never is the email's fault.
	gaveUp := func() bool { return ctx.Err() != nil }

	email, files, err := w.load(ctx, d)
	defer closeAll(files)
	if err != nil {
		return failed(model.EmailDeliveryErrorInterrupted, gaveUp() || transient(err), err)
	}
	// Built before connecting: attachments come from storage, and a too large email
	// never reaches the server.
	raw, err := outbound.BuildMIME(email, w.cfg.MaxMessageSize)
	switch {
	case errors.Is(err, outbound.ErrMessageTooLarge):
		return failed(model.EmailDeliveryErrorMessageTooLarge, false, err)
	case err != nil:
		// A file storage no longer has is not retried; a cut stream is.
		return failed(model.EmailDeliveryErrorInterrupted, gaveUp() || transient(err), err)
	}
	closeAll(files)

	profile, err := w.profiles.Locate(ctx, d.DomainID, d.ProfileID)
	switch {
	case kiterrors.Code(err) == codes.NotFound:
		return failed(model.EmailDeliveryErrorProfileUnavailable, false, err)
	case err != nil:
		return failed(model.EmailDeliveryErrorInterrupted, gaveUp() || transient(err), err)
	case profile.SMTPHost == "" || profile.SMTPPort <= 0:
		return failed(model.EmailDeliveryErrorProfileUnavailable, false, nil)
	}
	connection, err := w.profiles.SMTPConnection(ctx, profile)
	if err != nil {
		code, retryable := classifyCredentials(err, gaveUp())
		return failed(code, retryable, credentialsError(err))
	}

	from, recipients := outbound.Envelope(email)
	err = w.smtp.Send(ctx, connection, mailinfra.SMTPEnvelope{From: from, Recipients: recipients}, raw,
		func(ctx context.Context) error {
			committed, err := w.deliveries.Commit(ctx, d)
			if err == nil && !committed {
				err = errLeaseLost
			}
			return err
		})
	var failure *mailinfra.SMTPSendError
	switch {
	case err == nil:
		return store.EmailDeliveryOutcome{Status: model.EmailDeliveryStatusSent}, nil
	case errors.As(err, &failure) && failure.Stage == mailinfra.SMTPStageCommit:
		return store.EmailDeliveryOutcome{}, err
	default:
		code, retryable := classify(err, gaveUp())
		return failed(code, retryable, err)
	}
}

// load reads the stored email. Its headers come from what was accepted, so a rebuild
// keeps them; the attachments open lazily, one at a time, while the MIME is written.
func (w *Worker) load(ctx context.Context, d store.EmailDelivery) (*outbound.Email, []*lazyFile, error) {
	message, err := w.uow.EmailMessageStore().Get(ctx, d.DomainID, d.MessageID)
	if err != nil {
		return nil, nil, err
	}
	recipients, err := w.uow.EmailRecipientStore().List(ctx, d.DomainID, d.MessageID)
	if err != nil {
		return nil, nil, err
	}
	parts, err := w.uow.EmailMessageAttachmentStore().List(ctx, d.DomainID, d.MessageID)
	if err != nil {
		return nil, nil, err
	}

	email := &outbound.Email{
		MessageID: message.MessageID, Subject: message.Subject,
		HTMLBody: message.HTMLBody, TextBody: message.TextBody,
		InReplyTo: message.InReplyTo, References: message.References,
	}
	if message.SentAt != nil {
		email.Date = *message.SentAt
	}
	for _, r := range recipients {
		address := model.EmailAddress{Name: r.DisplayName, Address: r.Address, NormalizedAddress: r.NormalizedAddress}
		switch r.Type {
		case model.EmailRecipientTypeFrom:
			email.Sender.Name, email.Sender.Address = r.DisplayName, r.Address
		case model.EmailRecipientTypeReplyTo:
			email.Sender.ReplyTo = r.Address
		case model.EmailRecipientTypeTo:
			email.To = append(email.To, address)
		case model.EmailRecipientTypeCc:
			email.Cc = append(email.Cc, address)
		case model.EmailRecipientTypeBcc:
			email.Bcc = append(email.Bcc, address)
		}
	}

	files := make([]*lazyFile, 0, len(parts))
	for _, part := range parts {
		if part.FileID == nil {
			continue
		}
		file := &lazyFile{open: func() (io.ReadCloser, error) {
			return w.files.Open(ctx, d.DomainID, *part.FileID, part.Size)
		}}
		files = append(files, file)
		email.Attachments = append(email.Attachments, outbound.Attachment{
			FileName: part.FileName, MimeType: part.MimeType, Content: file,
		})
	}

	return email, files, nil
}

// lazyFile opens its stream on the first read, so only one is open at a time.
type lazyFile struct {
	open   func() (io.ReadCloser, error)
	reader io.ReadCloser
	done   bool
}

func (f *lazyFile) Read(p []byte) (int, error) {
	if f.done {
		return 0, io.EOF
	}
	if f.reader == nil {
		reader, err := f.open()
		if err != nil {
			return 0, err
		}
		f.reader = reader
	}
	n, err := f.reader.Read(p)
	if err == io.EOF {
		f.done = true
		f.close()
	}

	return n, err
}

func (f *lazyFile) close() {
	if f.reader != nil {
		_ = f.reader.Close()
		f.reader = nil
	}
}

func closeAll(files []*lazyFile) {
	for _, f := range files {
		f.close()
	}
}

// credentialsError keeps only the error ID: a reply of the OAuth provider may name
// the account.
func credentialsError(err error) error {
	id := kiterrors.ID(err)
	if id == "" {
		id = "unknown"
	}

	return fmt.Errorf("delivery: credentials unavailable (%s)", id)
}

// smtpFields are the codes of a server reply, never its text.
func smtpFields(err error) []any {
	var failure *mailinfra.SMTPSendError
	if !errors.As(err, &failure) {
		return nil
	}

	return []any{"smtp_stage", failure.Stage, "smtp_code", failure.Code, "smtp_enhanced", failure.Enhanced}
}
