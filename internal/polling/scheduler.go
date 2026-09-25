// Package polling checks the mailboxes assigned to this email-service instance.
package polling

import (
	"context"
	"log/slog"
	"sync"
	"time"

	kitdiscovery "github.com/webitel/webitel-go-kit/infra/discovery"
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	"github.com/webitel/webitel-emails/internal/inbound"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
	"github.com/webitel/webitel-emails/internal/store"
)

// profileSource loads the current profile settings and IMAP credentials.
type profileSource interface {
	Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error)
	IMAPConnection(ctx context.Context, profile *model.EmailProfile) (mailinfra.IMAPConnection, error)
}

// Scheduler starts a poll for every assigned profile that is due, with bounded concurrency.
type Scheduler struct {
	instanceID string
	cfg        config.IMAPPollingConfig
	runtime    store.EmailProfileRuntimeStore
	profiles   profileSource
	imap       mailinfra.IMAPClient
	// When nil, new messages are only counted and the cursor does not move.
	handler inbound.Handler
	log     *slog.Logger

	slots chan struct{}
	// connSlots caps real open IMAP connections; a permit is held for a connection's whole
	// life, from Connect to Close/Abort, not just while it is actively polling.
	connSlots chan struct{}
	// closing tracks async session closes so shutdown can wait for them, not just for the
	// sessions still in the map at the time it starts.
	closing sync.WaitGroup

	mu       sync.Mutex
	running  map[int64]struct{}
	sessions map[int64]*session

	workers       sync.WaitGroup
	workerCtx     context.Context
	cancelWorkers context.CancelFunc
	stopLoop      context.CancelFunc
	loopDone      chan struct{}
}

// session is an IMAP connection kept open between polls of one profile.
type session struct {
	conn mailinfra.IMAPSession
	// Profile UpdatedAt the connection was opened with; a newer one means changed settings.
	version     time.Time
	lastUsed    time.Time
	idleTimeout time.Duration
}

// Params are the scheduler dependencies; Discovery makes it stop before Consul deregistration.
type Params struct {
	fx.In

	Config     *config.Config
	InstanceID model.InstanceID
	Runtime    store.EmailProfileRuntimeStore
	Profiles   *service.EmailProfileService
	IMAP       mailinfra.IMAPClient
	Handler    inbound.Handler `optional:"true"`
	Discovery  kitdiscovery.DiscoveryProvider
	Log        *slog.Logger
	Lifecycle  fx.Lifecycle
}

// New creates the scheduler and ties it to the application lifecycle.
func New(p Params) *Scheduler {
	workerCtx, cancelWorkers := context.WithCancel(context.Background())

	s := &Scheduler{
		instanceID:    string(p.InstanceID),
		cfg:           p.Config.IMAPPolling,
		runtime:       p.Runtime,
		profiles:      p.Profiles,
		imap:          p.IMAP,
		handler:       p.Handler,
		log:           p.Log.With("component", "imap_polling"),
		slots:         make(chan struct{}, p.Config.IMAPPolling.MaxConcurrency),
		connSlots:     make(chan struct{}, p.Config.IMAPPolling.MaxOpenConnections),
		running:       make(map[int64]struct{}),
		sessions:      make(map[int64]*session),
		workerCtx:     workerCtx,
		cancelWorkers: cancelWorkers,
		loopDone:      make(chan struct{}),
	}

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			s.start()

			return nil
		},
		OnStop: s.stop,
	})

	return s
}

func (s *Scheduler) start() {
	ctx, stop := context.WithCancel(context.Background())
	s.stopLoop = stop

	go s.loop(ctx)
}

func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.loopDone)

	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()

	for {
		s.dispatch(ctx)
		s.closeIdleSessions()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dispatch starts polls for due profiles while free slots remain.
func (s *Scheduler) dispatch(ctx context.Context) {
	free := cap(s.slots) - len(s.slots)
	if free == 0 {
		return
	}

	s.mu.Lock()
	busy := len(s.running)
	s.mu.Unlock()

	// Running profiles are still due, so they are requested on top of the free slots.
	due, err := s.runtime.ListDue(ctx, s.instanceID, free+busy)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("list due email profiles", "err", err)
		}

		return
	}

	for _, runtime := range due {
		if _, ok := runtime.Assignment(); !ok || !s.tryStart(runtime.ProfileID) {
			continue
		}

		s.workers.Add(1)
		go s.run(runtime)
	}
}

// tryStart reserves a slot unless the profile is already being polled.
func (s *Scheduler) tryStart(profileID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, running := s.running[profileID]; running {
		return false
	}

	select {
	case s.slots <- struct{}{}:
	default:
		return false
	}

	s.running[profileID] = struct{}{}

	return true
}

func (s *Scheduler) run(runtime *model.EmailProfileRuntime) {
	defer func() {
		s.mu.Lock()
		delete(s.running, runtime.ProfileID)
		s.mu.Unlock()

		<-s.slots
		s.workers.Done()
	}()

	s.poll(s.workerCtx, runtime)
}

// closeIdleSessions closes connections of profiles that are no longer polled here.
func (s *Scheduler) closeIdleSessions() {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	for profileID, current := range s.sessions {
		if _, running := s.running[profileID]; running || now.Sub(current.lastUsed) < current.idleTimeout {
			continue
		}

		delete(s.sessions, profileID)
		s.closeSession(current, true)
	}
}

// stop lets running polls finish within the shutdown timeout, then interrupts the rest.
func (s *Scheduler) stop(ctx context.Context) error {
	if s.stopLoop != nil {
		s.stopLoop()
		<-s.loopDone
	}

	drained := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(drained)
	}()

	timeout := time.NewTimer(s.cfg.ShutdownTimeout)
	defer timeout.Stop()

	select {
	case <-drained:
	case <-timeout.C:
		s.log.Warn("interrupting email polls after shutdown timeout")
		s.interrupt()
	case <-ctx.Done():
		s.interrupt()
	}

	select {
	case <-drained:
	case <-ctx.Done():
	}

	s.cancelWorkers()
	s.closeAllSessions()
	s.waitClosing(ctx)

	return nil
}

// interrupt cancels running polls; their cursor and schedule are not written.
func (s *Scheduler) interrupt() {
	s.cancelWorkers()

	s.mu.Lock()
	defer s.mu.Unlock()

	for profileID, current := range s.sessions {
		if _, running := s.running[profileID]; running {
			current.conn.Abort()
		}
	}
}

// closeAllSessions starts closing every still-cached session; waitClosing then waits for those
// and for any close already started earlier (eviction, idle cleanup, a dropped profile).
func (s *Scheduler) closeAllSessions() {
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = make(map[int64]*session)
	s.mu.Unlock()

	for _, current := range sessions {
		s.closeSession(current, true)
	}
}

func (s *Scheduler) waitClosing(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.closing.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}
