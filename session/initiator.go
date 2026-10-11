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
// connected.
type Initiator struct {
	opts InitiatorOptions
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[SessionID]*initSession
	ctx      context.Context // set while running
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
	return &Initiator{opts: opts, log: opts.Logger, sessions: make(map[SessionID]*initSession)}
}

// Add creates a session from cfg, connected with dialer, now if the
// initiator runs, else once it does.
func (i *Initiator) Add(cfg Config, dialer transport.Dialer) (*Session, error) {
	if dialer == nil {
		return nil, fmt.Errorf("session %s: nil dialer", cfg.ID)
	}
	s, err := New(cfg)
	if err != nil {
		return nil, err
	}
	s.initiator = true
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.sessions[s.id]; ok {
		return nil, fmt.Errorf("session %s: already added", s.id)
	}
	is := &initSession{s: s, dialer: dialer}
	i.sessions[s.id] = is
	if i.ctx != nil {
		i.start(i.ctx, is)
	}
	return s, nil
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

// Remove logs session id out, stops it and removes it.
func (i *Initiator) Remove(ctx context.Context, id SessionID) error {
	i.mu.Lock()
	is := i.sessions[id]
	delete(i.sessions, id)
	i.mu.Unlock()
	if is == nil {
		return fmt.Errorf("session %s: not found", id)
	}
	err := is.s.Stop(ctx)
	if is.cancel != nil {
		is.cancel()
		<-is.done
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
// then logs them out, waiting up to their LogoutTimeout, and returns.
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
	sessions := make([]*Session, 0, len(i.sessions))
	for _, is := range i.sessions {
		sessions = append(sessions, is.s)
	}
	i.mu.Unlock()
	shutdown(sessions)
	cancel()
	i.wg.Wait()
	i.mu.Lock()
	i.ctx = nil
	i.mu.Unlock()
	return nil
}

// loop connects is until ctx is done, waiting between attempts.
func (i *Initiator) loop(ctx context.Context, is *initSession) {
	s := is.s
	wait := i.opts.ReconnectMin
	for ctx.Err() == nil {
		if s.isStopped() {
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
		if ctx.Err() != nil {
			return
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
	if err := s.logon(c); err != nil {
		return false, err
	}
	reason := c.run()
	s.mu.Lock()
	loggedOn := c.loggedOn
	s.mu.Unlock()
	return loggedOn, reason
}
