package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/sylr/fix2/session/transport"
)

// InitiatorOptions configures an Initiator. The zero value is valid.
type InitiatorOptions struct {
	// Logger receives the initiator's events; nothing is logged if nil.
	Logger *slog.Logger
	// ReconnectMin and ReconnectMax bound the wait between connection
	// attempts, which doubles after each failure, with jitter: 1s and 30s
	// if 0.
	ReconnectMin, ReconnectMax time.Duration
	// DialTimeout bounds each connection attempt, 10s if 0.
	DialTimeout time.Duration
}

// Initiator connects sessions to their counterparties and keeps them
// connected, except a session that halted (see Session.Err): it is no
// longer dialed.
type Initiator struct {
	opts InitiatorOptions
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[SessionID]*initSession
	adding   map[SessionID]struct{} // reserved by an Add opening the store
	stopping stoppingSessions       // removed, not yet done
	ctx      context.Context        // set while running
	closing  bool                   // Run is shutting down: no session added
	wg       sync.WaitGroup
}

type initSession struct {
	s      *Session
	dialer transport.Dialer
	cancel context.CancelFunc
	done   chan struct{}
}

// NewInitiator returns an Initiator.
func NewInitiator(opts InitiatorOptions) *Initiator {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.ReconnectMin <= 0 {
		opts.ReconnectMin = time.Second
	}
	if opts.ReconnectMax <= 0 {
		opts.ReconnectMax = 30 * time.Second
	}
	if opts.ReconnectMax < opts.ReconnectMin {
		opts.ReconnectMax = opts.ReconnectMin
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	return &Initiator{
		opts:     opts,
		log:      opts.Logger,
		sessions: make(map[SessionID]*initSession),
		adding:   make(map[SessionID]struct{}),
		stopping: make(stoppingSessions),
	}
}

// Add creates a session from cfg, connected with dialer, now if the
// initiator runs, else once it does. The ID of a session added, being
// added, or removed until that session's Done is closed, is refused
// without opening cfg.Store: it may be in use. If Add fails, cfg.Store is
// closed only if Add opened it.
func (i *Initiator) Add(cfg Config, dialer transport.Dialer) (*Session, error) {
	if dialer == nil {
		return nil, fmt.Errorf("session %s: nil dialer", cfg.ID)
	}
	i.mu.Lock()
	err := i.reserveLocked(cfg.ID)
	i.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s, err := New(cfg)
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.adding, cfg.ID)
	if err != nil {
		return nil, err
	}
	s.initiator = true
	if i.closing {
		s.store.Close()
		return nil, fmt.Errorf("session %s: initiator shutting down", s.id)
	}
	is := &initSession{s: s, dialer: dialer}
	i.sessions[s.id] = is
	if i.ctx != nil {
		i.start(i.ctx, is)
	}
	return s, nil
}

// reserveLocked reserves id for an Add, which releases it once done. It
// fails if the initiator shuts down, or a session has id, is being added
// with it, or was removed and is still stopping.
func (i *Initiator) reserveLocked(id SessionID) error {
	_, adding := i.adding[id]
	switch {
	case i.closing:
		return fmt.Errorf("session %s: initiator shutting down", id)
	case i.sessions[id] != nil:
		return fmt.Errorf("session %s: already added", id)
	case adding:
		return fmt.Errorf("session %s: being added", id)
	}
	if err := i.stopping.check(id); err != nil {
		return err
	}
	i.adding[id] = struct{}{}
	return nil
}

// start runs is's connection loop. The caller holds i.mu.
func (i *Initiator) start(ctx context.Context, is *initSession) {
	ctx, is.cancel = context.WithCancel(ctx)
	is.done = make(chan struct{})
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		defer close(is.done)
		i.loop(ctx, is)
	}()
}

// Remove logs session id out, stops it and removes it. Its ID can be added
// again once the session's Done is closed. Called from one of the
// session's callbacks, it returns at once, as Stop does.
func (i *Initiator) Remove(ctx context.Context, id SessionID) error {
	i.mu.Lock()
	is := i.sessions[id]
	delete(i.sessions, id)
	if is != nil {
		i.stopping[id] = is.s.Done()
	}
	i.mu.Unlock()
	if is == nil {
		return fmt.Errorf("session %s: not found", id)
	}
	err := is.s.Stop(ctx)
	if is.cancel != nil {
		is.cancel()
		// The connection loop may be running the callback calling.
		if !is.s.ownCallback(ctx) {
			<-is.done
		}
	}
	return err
}

// Session returns session id, or nil.
func (i *Initiator) Session(id SessionID) *Session {
	i.mu.Lock()
	defer i.mu.Unlock()
	if is := i.sessions[id]; is != nil {
		return is.s
	}
	return nil
}

// Sessions returns the initiator's sessions.
func (i *Initiator) Sessions() []*Session {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]*Session, 0, len(i.sessions))
	for _, is := range i.sessions {
		out = append(out, is.s)
	}
	return out
}

// Run connects the sessions and keeps them connected until ctx is done,
// then stops them (see Session.Stop): the sessions cannot be run again.
// Each Stop is bounded by the session's LogoutTimeout plus a second. Run
// returns once the sessions' connections wound down, which waits for the
// callbacks run by their goroutines (Authenticate, ToAdmin, FromAdmin,
// OnResend), but not for those of the sessions' delivery goroutines
// (OnLogon, FromApp, OnLogout, OnRecoveryLoss): stuck, they may still run,
// and a session's store is closed once they returned. Each session's Done
// tells when.
func (i *Initiator) Run(ctx context.Context) error {
	i.mu.Lock()
	if i.ctx != nil {
		i.mu.Unlock()
		return errors.New("session: initiator already running")
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i.ctx = loopCtx
	for _, is := range i.sessions {
		i.start(loopCtx, is)
	}
	i.mu.Unlock()

	<-ctx.Done()
	i.mu.Lock()
	i.closing = true
	sessions := make([]*Session, 0, len(i.sessions))
	for _, is := range i.sessions {
		sessions = append(sessions, is.s)
	}
	i.mu.Unlock()
	shutdown(sessions)
	cancel()
	i.wg.Wait()
	i.mu.Lock()
	i.ctx, i.closing = nil, false
	i.mu.Unlock()
	return nil
}

// loop connects is until ctx is done, waiting between attempts.
func (i *Initiator) loop(ctx context.Context, is *initSession) {
	s := is.s
	wait := i.opts.ReconnectMin
	s.startSchedule(ctx)
	for ctx.Err() == nil {
		if s.isStopped() {
			return
		}
		if herr := s.Err(); herr != nil {
			// No connection can succeed until the store is reconciled.
			s.log.LogAttrs(ctx, slog.LevelError, "session halted: not reconnecting", slog.Any("error", herr))
			return
		}
		select {
		case <-s.windowSignal():
		case <-ctx.Done():
			return
		}
		dctx, cancel := context.WithTimeout(ctx, i.opts.DialTimeout)
		tc, err := is.dialer.Dial(dctx)
		cancel()
		if err == nil {
			loggedOn, cerr := s.connect(tc)
			if loggedOn {
				wait = i.opts.ReconnectMin
			}
			err = cerr
		}
		if ctx.Err() != nil || s.isStopped() {
			return
		}
		if s.Err() != nil {
			continue // without waiting: the loop stops
		}
		if err != nil {
			s.log.LogAttrs(ctx, slog.LevelInfo, "connection ended, reconnecting", slog.Duration("in", wait), slog.Any("reason", err))
		}
		jitter := time.Duration(rand.Int64N(int64(wait)/5 + 1))
		select {
		case <-time.After(wait + jitter):
		case <-ctx.Done():
			return
		}
		wait = min(2*wait, i.opts.ReconnectMax)
	}
}

// connect runs the session on tc, as an initiator, until it disconnects. It
// reports whether it logged on.
func (s *Session) connect(tc transport.Conn) (bool, error) {
	c, err := s.attach(tc, nil)
	if err != nil {
		tc.Close()
		return false, err
	}
	// A Logon refused by the handler closed the connection: run still
	// winds it down, so that the next one need not wait for it.
	_ = s.logon(c)
	reason := c.run()
	s.mu.Lock()
	loggedOn := c.loggedOn
	s.mu.Unlock()
	return loggedOn, reason
}
