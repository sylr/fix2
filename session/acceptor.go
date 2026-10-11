package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/transport"
)

// AcceptorOptions configures an Acceptor. The zero value is valid.
type AcceptorOptions struct {
	// Logger receives the acceptor's events; nothing is logged if nil.
	Logger *slog.Logger
	// LogonTimeout bounds the wait for a connection's first message, and
	// for a TLS handshake, 10s if 0.
	LogonTimeout time.Duration
	// MaxPreLogonBytes bounds the first message of a connection, 16 KiB if
	// 0: a Logon is small, and nothing is known of the peer yet.
	MaxPreLogonBytes int
	// MaxPendingConnections bounds the connections waiting for their first
	// message, 1024 if 0: others are closed at once.
	MaxPendingConnections int
	// Resolve, if set, creates a session for a Logon no session matches,
	// returning its configuration, or an error to refuse it.
	Resolve func(ctx context.Context, req *LogonRequest) (*Config, error)
	// Route, if set, picks among sessions whose wire identity is the same,
	// told apart by their Qualifier, using what the transport knows of the
	// peer (address, TLS certificate, principal).
	Route func(ctx context.Context, req *LogonRequest, candidates []SessionID) (SessionID, error)
}

// Acceptor serves sessions on listeners: each connection is routed to a
// session by the identities its Logon carries.
type Acceptor struct {
	opts AcceptorOptions
	log  *slog.Logger

	mu        sync.Mutex
	sessions  map[SessionID]*Session
	byWire    map[SessionID][]*Session // keyed by the counterparty's view of the wire identity
	listeners []transport.Listener
	ctx       context.Context // set while running
	wg        sync.WaitGroup
	pending   chan struct{} // semaphore of connections before their Logon
}

// NewAcceptor returns an Acceptor.
func NewAcceptor(opts AcceptorOptions) *Acceptor {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.LogonTimeout <= 0 {
		opts.LogonTimeout = 10 * time.Second
	}
	if opts.MaxPreLogonBytes <= 0 {
		opts.MaxPreLogonBytes = 16 << 10
	}
	if opts.MaxPendingConnections <= 0 {
		opts.MaxPendingConnections = 1024
	}
	return &Acceptor{
		opts:     opts,
		log:      opts.Logger,
		sessions: make(map[SessionID]*Session),
		byWire:   make(map[SessionID][]*Session),
		pending:  make(chan struct{}, opts.MaxPendingConnections),
	}
}

// Add creates a session from cfg, served once its counterparty connects.
func (a *Acceptor) Add(cfg Config) (*Session, error) {
	s, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if err := a.add(s); err != nil {
		return nil, err
	}
	return s, nil
}

func (a *Acceptor) add(s *Session) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sessions[s.id]; ok {
		return fmt.Errorf("session %s: already added", s.id)
	}
	wire := s.id.reversed()
	if others := a.byWire[wire]; len(others) > 0 && a.opts.Route == nil {
		return fmt.Errorf("session %s: same wire identity as session %s: set AcceptorOptions.Route to tell them apart", s.id, others[0].id)
	}
	s.initiator = false
	a.sessions[s.id] = s
	a.byWire[wire] = append(a.byWire[wire], s)
	if a.ctx != nil {
		s.startSchedule(a.ctx)
	}
	return nil
}

// Remove logs session id out, if connected, stops it and removes it.
func (a *Acceptor) Remove(ctx context.Context, id SessionID) error {
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil {
		a.mu.Unlock()
		return fmt.Errorf("session %s: not found", id)
	}
	delete(a.sessions, id)
	wire := id.reversed()
	list := a.byWire[wire]
	for i, o := range list {
		if o == s {
			a.byWire[wire] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(a.byWire[wire]) == 0 {
		delete(a.byWire, wire)
	}
	a.mu.Unlock()
	return s.Stop(ctx)
}

// Session returns session id, or nil.
func (a *Acceptor) Session(id SessionID) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// Sessions returns the acceptor's sessions.
func (a *Acceptor) Sessions() []*Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		out = append(out, s)
	}
	return out
}

// Listen serves connections from l, now if the acceptor runs, else once it
// does.
func (a *Acceptor) Listen(l transport.Listener) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listeners = append(a.listeners, l)
	if a.ctx != nil {
		a.wg.Add(1)
		go a.acceptLoop(a.ctx, l)
	}
}

// Run serves the listeners until ctx is done, then logs every session out,
// waiting up to their LogoutTimeout, closes the listeners and returns.
func (a *Acceptor) Run(ctx context.Context) error {
	a.mu.Lock()
	if a.ctx != nil {
		a.mu.Unlock()
		return errors.New("session: acceptor already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.ctx = runCtx
	for _, s := range a.sessions {
		s.startSchedule(runCtx)
	}
	for _, l := range a.listeners {
		a.wg.Add(1)
		go a.acceptLoop(runCtx, l)
	}
	a.mu.Unlock()

	<-runCtx.Done()
	a.mu.Lock()
	for _, l := range a.listeners {
		l.Close()
	}
	sessions := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	shutdown(sessions)
	a.wg.Wait()
	a.mu.Lock()
	a.ctx = nil
	a.mu.Unlock()
	return nil
}

// shutdown logs sessions out, in parallel, each within its LogoutTimeout
// plus a margin.
func shutdown(sessions []*Session) {
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.LogoutTimeout+time.Second)
			defer cancel()
			s.Logout(ctx, "shutting down")
		}()
	}
	wg.Wait()
}

func (a *Acceptor) acceptLoop(ctx context.Context, l transport.Listener) {
	defer a.wg.Done()
	for {
		tc, err := l.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			a.log.LogAttrs(ctx, slog.LevelWarn, "accept failed", slog.String("listener", l.Addr().String()), slog.Any("error", err))
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			continue
		}
		select {
		case a.pending <- struct{}{}:
		default:
			a.log.LogAttrs(ctx, slog.LevelWarn, "too many connections waiting for their Logon: closed", slog.String("remote", tc.RemoteAddr().String()))
			tc.Close()
			continue
		}
		a.wg.Add(1)
		go a.serve(ctx, tc)
	}
}

// serve reads a connection's first message, routes it to a session and runs
// the session on it.
func (a *Acceptor) serve(ctx context.Context, tc transport.Conn) {
	defer a.wg.Done()
	remote := tc.RemoteAddr().String()
	released := false
	release := func() {
		if !released {
			released = true
			<-a.pending
		}
	}
	defer release()
	refuse := func(why string, args ...slog.Attr) {
		a.log.LogAttrs(ctx, slog.LevelWarn, "connection refused: "+why, append([]slog.Attr{slog.String("remote", remote)}, args...)...)
		tc.Close()
	}

	deadline := time.Now().Add(a.opts.LogonTimeout)
	tc.SetReadDeadline(deadline)
	if tlsConn, ok := tc.(transport.TLSConn); ok {
		hctx, cancel := context.WithDeadline(ctx, deadline)
		err := tlsConn.HandshakeContext(hctx)
		cancel()
		if err != nil {
			refuse("TLS handshake failed", slog.Any("error", err))
			return
		}
	}
	framer := fix2.NewFramer(tc, fix2.FramerOptions{MaxBodyLength: a.opts.MaxPreLogonBytes})
	frame, err := framer.Next()
	if err != nil {
		refuse("no valid first message", slog.Any("error", err))
		return
	}
	var m fix2.Message
	if err := fix2.ParseInto(&m, frame, fix2.DefaultProfile()); err != nil {
		refuse("first message does not parse", slog.Any("error", err))
		return
	}
	if string(m.MsgType()) != "A" {
		refuse("first message is not a Logon", slog.String("msg_type", string(m.MsgType())))
		return
	}
	// The identities as the counterparty sends them: its Sender is our
	// Target.
	wire := SessionID{
		BeginString:      string(m.BeginString()),
		SenderCompID:     string(m.Get(tagSenderCompID)),
		SenderSubID:      string(m.Get(tagSenderSubID)),
		SenderLocationID: string(m.Get(tagSenderLocationID)),
		TargetCompID:     string(m.Get(tagTargetCompID)),
		TargetSubID:      string(m.Get(tagTargetSubID)),
		TargetLocationID: string(m.Get(tagTargetLocationID)),
	}
	req := &LogonRequest{ID: wire.reversed(), Logon: &m, RemoteAddr: tc.RemoteAddr()}
	if t, ok := tc.(transport.TLSConn); ok {
		st := t.ConnectionState()
		req.TLS = &st
	}
	if p, ok := tc.(transport.PrincipalConn); ok {
		req.Principal = p.Principal()
	}
	s, err := a.route(ctx, wire, req)
	if err != nil {
		refuse(err.Error(), slog.String("logon_for", wire.reversed().String()))
		return
	}
	tc.SetReadDeadline(time.Time{})
	framer.SetMaxBodyLength(fix2.MaxBodyLength)
	extra, err := s.toAdmin("A")
	if err != nil {
		refuse("Logon answer cancelled by the handler", slog.String("session", s.id.String()), slog.Any("error", err))
		return
	}
	c, err := s.attach(tc, framer)
	if err != nil {
		// A second connection for a logged on session is closed without a
		// word, as FIX requires: a Logout would take a sequence number.
		refuse("session "+s.id.String()+": "+err.Error(), slog.String("session", s.id.String()))
		return
	}
	s.mu.Lock()
	c.logonExtra = extra
	s.mu.Unlock()
	release()
	c.handleFrame(frame)
	reason := c.run()
	s.log.LogAttrs(ctx, slog.LevelDebug, "connection ended", slog.Any("reason", reason))
}

// route returns the session a Logon is for.
func (a *Acceptor) route(ctx context.Context, wire SessionID, req *LogonRequest) (*Session, error) {
	a.mu.Lock()
	candidates := append([]*Session(nil), a.byWire[wire]...)
	a.mu.Unlock()
	switch len(candidates) {
	case 0:
		if a.opts.Resolve == nil {
			return nil, errors.New("no session for this Logon")
		}
		cfg, err := a.opts.Resolve(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("dynamic session refused: %w", err)
		}
		if cfg == nil {
			return nil, errors.New("no session for this Logon")
		}
		s, err := New(*cfg)
		if err != nil {
			return nil, err
		}
		if s.id.reversed() != wire {
			return nil, fmt.Errorf("dynamic session %s does not match the Logon's identities", s.id)
		}
		if err := a.add(s); err != nil {
			// Another connection created it meanwhile.
			if existing := a.Session(s.id); existing != nil {
				return existing, nil
			}
			return nil, err
		}
		return s, nil
	case 1:
		if a.opts.Route == nil {
			return candidates[0], nil
		}
	}
	if a.opts.Route == nil {
		return nil, errors.New("several sessions match this Logon")
	}
	ids := make([]SessionID, len(candidates))
	for i, s := range candidates {
		ids[i] = s.id
	}
	id, err := a.opts.Route(ctx, req, ids)
	if err != nil {
		return nil, fmt.Errorf("routing refused: %w", err)
	}
	for _, s := range candidates {
		if s.id == id {
			return s, nil
		}
	}
	return nil, fmt.Errorf("routing chose %s, not a candidate", id)
}
