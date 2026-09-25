package polling

import (
	"context"
	"errors"
	"log/slog"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
	"github.com/webitel/webitel-emails/internal/store"
)

// poll performs one fenced check of a profile mailbox.
func (s *Scheduler) poll(ctx context.Context, runtime *model.EmailProfileRuntime) {
	assignment, _ := runtime.Assignment()
	log := s.log.With("profile_id", assignment.ProfileID, "domain_id", assignment.DomainID)

	if err := s.runtime.StartPoll(ctx, assignment); err != nil {
		s.handleStoreError(ctx, assignment, err, log)

		return
	}

	profile, err := s.profiles.Locate(ctx, assignment.DomainID, assignment.ProfileID)
	if err != nil {
		if kiterrors.Code(err) == codes.NotFound {
			s.dropSession(assignment.ProfileID, false)

			return
		}

		s.complete(ctx, assignment, model.EmailProfilePollResult{Error: err.Error()}, log)

		return
	}

	if !profile.Enabled {
		s.dropSession(assignment.ProfileID, true)

		return
	}

	conn, err := s.session(ctx, profile, log)
	if err != nil {
		s.dropSession(assignment.ProfileID, false)
		log.Warn("imap connection failed", "err", err)
		s.complete(ctx, assignment, failedPoll(err, nil), log)

		return
	}

	synced := s.sync(ctx, conn, profile, runtime.ProviderCursor, log)
	if synced.imapErr != nil {
		s.dropSession(assignment.ProfileID, false)
		log.Warn("imap mailbox sync failed", "err", synced.imapErr)
		s.complete(ctx, assignment, failedPoll(synced.imapErr, synced.cursor), log)

		return
	}

	result := model.EmailProfilePollResult{
		ProviderCursor:  synced.cursor,
		ConnectionState: model.EmailConnectionStateReady,
		Connected:       true,
	}
	if synced.handlerErr != nil {
		log.Error("handle inbound email", "err", synced.handlerErr)
		result.Error = "handler: " + synced.handlerErr.Error()
	}
	if synced.more {
		now := time.Now()
		result.NextCheckAt = &now
	}

	s.complete(ctx, assignment, result, log)
}

// session reuses a healthy connection opened with the current settings or opens a new one.
// A new connection holds a connSlots permit for its whole life, released only when it closes.
func (s *Scheduler) session(ctx context.Context, profile *model.EmailProfile, log *slog.Logger) (mailinfra.IMAPSession, error) {
	s.mu.Lock()
	current := s.sessions[profile.ID]
	s.mu.Unlock()

	if current != nil {
		if current.version.Equal(profile.UpdatedAt) {
			if err := current.conn.Noop(); err == nil {
				s.touchSession(profile.ID)

				return current.conn, nil
			}

			log.Debug("imap connection lost, reconnecting")
			s.dropSession(profile.ID, false)
		} else {
			log.Debug("email profile changed, reconnecting")
			s.dropSession(profile.ID, true)
		}
	}

	connection, err := s.profiles.IMAPConnection(ctx, profile)
	if err != nil {
		return nil, err
	}

	if err := s.acquireConnSlot(ctx); err != nil {
		return nil, err
	}

	conn, err := s.imap.Connect(ctx, connection)
	if err != nil {
		s.releaseConnSlot()

		return nil, err
	}

	s.mu.Lock()
	s.sessions[profile.ID] = &session{
		conn:        conn,
		version:     profile.UpdatedAt,
		lastUsed:    time.Now(),
		idleTimeout: 2*time.Duration(profile.FetchIntervalSeconds)*time.Second + time.Minute,
	}
	s.mu.Unlock()

	return conn, nil
}

// acquireConnSlot blocks until a connSlots permit is free, evicting the least recently used
// idle session first, so real open connections never exceed MaxOpenConnections.
func (s *Scheduler) acquireConnSlot(ctx context.Context) error {
	select {
	case s.connSlots <- struct{}{}:
		return nil
	default:
	}

	s.evictForCapacity()

	select {
	case s.connSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) releaseConnSlot() {
	<-s.connSlots
}

// evictForCapacity closes the least recently used idle session, if any; a session currently
// polling is never closed. Its connSlots permit is released once the close actually completes.
func (s *Scheduler) evictForCapacity() {
	s.mu.Lock()

	var (
		evictID int64
		evict   *session
	)
	for profileID, current := range s.sessions {
		if _, running := s.running[profileID]; running {
			continue
		}
		if evict == nil || current.lastUsed.Before(evict.lastUsed) {
			evictID, evict = profileID, current
		}
	}
	if evict != nil {
		delete(s.sessions, evictID)
	}

	s.mu.Unlock()

	if evict == nil {
		// Every open session is currently polling; acquireConnSlot waits for one to free up.
		return
	}

	s.closeSession(evict, true)
}

func (s *Scheduler) touchSession(profileID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if current, ok := s.sessions[profileID]; ok {
		current.lastUsed = time.Now()
	}
}

// dropSession forgets a connection and closes it, releasing its connSlots permit.
func (s *Scheduler) dropSession(profileID int64, graceful bool) {
	s.mu.Lock()
	current, ok := s.sessions[profileID]
	delete(s.sessions, profileID)
	s.mu.Unlock()

	if !ok {
		return
	}

	s.closeSession(current, graceful)
}

// closeSession closes a connection and releases its connSlots permit once done. graceful logs
// out over the network (async, tracked in closing); otherwise it aborts locally at once.
func (s *Scheduler) closeSession(current *session, graceful bool) {
	if !graceful {
		current.conn.Abort()
		s.releaseConnSlot()

		return
	}

	s.closing.Add(1)
	go func() {
		defer s.closing.Done()
		defer s.releaseConnSlot()

		_ = current.conn.Close()
	}()
}

// complete records the poll result unless shutdown interrupted it.
func (s *Scheduler) complete(
	ctx context.Context,
	assignment model.EmailProfileAssignment,
	result model.EmailProfilePollResult,
	log *slog.Logger,
) {
	if ctx.Err() != nil {
		return
	}

	if err := s.runtime.CompletePoll(ctx, assignment, result); err != nil {
		s.handleStoreError(ctx, assignment, err, log)
	}
}

// handleStoreError releases a profile that moved to another owner.
func (s *Scheduler) handleStoreError(ctx context.Context, assignment model.EmailProfileAssignment, err error, log *slog.Logger) {
	switch {
	case errors.Is(err, store.ErrStaleEmailProfileAssignment):
		log.Info("email profile is no longer assigned to this instance", "generation", assignment.Generation)
		s.dropSession(assignment.ProfileID, true)
	case ctx.Err() != nil:
		log.Debug("email poll interrupted", "err", err)
	default:
		log.Error("store email poll state", "err", err)
	}
}

// failedPoll maps a connection error to the profile state, keeping the progress already confirmed.
func failedPoll(err error, cursor *model.ProviderCursor) model.EmailProfilePollResult {
	state := model.EmailConnectionStateError
	if service.RequiresReauthorization(err) {
		state = model.EmailConnectionStateReauthorizationRequired
	}

	message := "IMAP: " + err.Error()

	return model.EmailProfilePollResult{
		ProviderCursor:  cursor,
		Error:           message,
		ConnectionState: state,
		ConnectionError: message,
	}
}
