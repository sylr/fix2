package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport"
)

// Server runs the sessions clients negotiate with it, on the connections
// they establish them on.
type Server struct {
	cfg ServerConfig
	log *slog.Logger

	mu        sync.Mutex
	sessions  map[fixp.SessionID]*Session // not finalized
	reserved  map[fixp.SessionID]bool     // being negotiated, not recorded yet
	retire    map[*Session]*time.Timer    // sessions never established, their retirement
	listeners []transport.Listener
	runCtx    context.Context // while Run runs
	stopped   bool

	conns   sync.WaitGroup // connection goroutines
	pending atomic.Int64   // connections before their establishment
	running atomic.Bool
}

// NewServer returns a Server, its sessions not finalized loaded from its
// store, unbound. It fails if cfg is invalid (*ConfigError).
func NewServer(cfg ServerConfig) (*Server, error) {
	cfg, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	srv := &Server{
		cfg: cfg, log: cfg.Logger,
		sessions: map[fixp.SessionID]*Session{}, reserved: map[fixp.SessionID]bool{}, retire: map[*Session]*time.Timer{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.StoreTimeout)
	defer cancel()
	states, err := cfg.Store.Sessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("fixp/session: listing the store's sessions: %w", err)
	}
	for _, state := range states {
		if state.Role != store.Server {
			continue
		}
		st, err := cfg.Store.Open(ctx, state.ID)
		if err != nil {
			srv.closeSessions()
			return nil, fmt.Errorf("fixp/session: opening session %s: %w", state.ID, err)
		}
		s := newSession(cfg.Config, store.Server)
		s.adopt(st)
		srv.track(s)
		srv.sessions[state.ID] = s
	}
	// Once loaded: a retirement due already runs at once, and deletes from
	// the map, which is not iterated then.
	var idles []*Session
	for _, s := range srv.sessions {
		if idle(s, s.st) {
			idles = append(idles, s)
		}
	}
	for _, s := range idles {
		from := s.st.State().NegotiatedAt
		if !s.st.State().EstablishedAt.IsZero() {
			from = time.Now() // finished, unbound since the restart
		}
		srv.scheduleRetirement(s, from)
	}
	return srv, nil
}

// closeSessions releases the stores of the sessions loaded.
func (srv *Server) closeSessions() {
	for _, s := range srv.sessions {
		s.closeStore()
	}
}

// Listen adds a listener whose connections the server accepts once Run
// runs. It may be called before or while Run runs.
func (srv *Server) Listen(l transport.Listener) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.listeners = append(srv.listeners, l)
	if srv.runCtx != nil && !srv.stopped {
		srv.conns.Add(1)
		go srv.accept(srv.runCtx, l)
	}
}

// Run accepts connections until ctx is done, then terminates the
// established sessions (Terminate Finished), closes the listeners and
// returns ctx's error. Run runs once.
func (srv *Server) Run(ctx context.Context) error {
	if !srv.running.CompareAndSwap(false, true) {
		return errors.New("fixp/session: Server.Run called twice")
	}
	srv.mu.Lock()
	srv.runCtx = ctx
	for _, l := range srv.listeners {
		srv.conns.Add(1)
		go srv.accept(ctx, l)
	}
	srv.mu.Unlock()
	if srv.cfg.Retention > 0 {
		go srv.forget(ctx)
	}
	<-ctx.Done()

	srv.mu.Lock()
	srv.stopped = true
	for _, l := range srv.listeners {
		l.Close()
	}
	sessions := make([]*Session, 0, len(srv.sessions))
	for _, s := range srv.sessions {
		sessions = append(sessions, s)
	}
	for _, t := range srv.retire {
		t.Stop()
	}
	srv.mu.Unlock()
	for _, s := range sessions {
		s.Terminate(context.Background(), "server stopping")
	}
	// The answers come within a keepalive interval, or the connections
	// close on their own then.
	done := make(chan struct{})
	go func() { srv.conns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(srv.cfg.KeepaliveInterval + srv.cfg.WriteTimeout):
	}
	for _, s := range sessions {
		s.stop(ctx.Err())
		s.closeStore()
	}
	return ctx.Err()
}

// forget drops the finalized sessions beyond the retention.
func (srv *Server) forget(ctx context.Context) {
	every := max(min(srv.cfg.Retention/10, time.Hour), time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		sctx, cancel := context.WithTimeout(ctx, srv.cfg.StoreTimeout)
		if err := srv.cfg.Store.Forget(sctx, time.Now().Add(-srv.cfg.Retention)); err != nil && ctx.Err() == nil {
			srv.logWarn("forgetting finalized sessions", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (srv *Server) logWarn(msg string, args ...any) {
	if srv.log != nil {
		srv.log.Warn(msg, args...)
	}
}

// Session returns the session of id, nil if the server has none.
func (srv *Server) Session(id fixp.SessionID) *Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.sessions[id]
}

// Sessions returns the server's sessions not finalized.
func (srv *Server) Sessions() []*Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	sessions := make([]*Session, 0, len(srv.sessions))
	for _, s := range srv.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// ErrNoSession is a Retire of a session the server does not have.
var ErrNoSession = errors.New("fixp/session: no such session")

// Retire ends a session administratively, as an abandoned one: it is
// terminated if established, then finalized as it is, its flows not
// finished, durably (store.State.Retired). Its ID stays refused until
// forgotten (ServerConfig.Retention). Once begun, a retirement completes,
// ctx done or not: its waits are bounded by the write and store timeouts.
func (srv *Server) Retire(ctx context.Context, id fixp.SessionID) error {
	srv.mu.Lock()
	s := srv.sessions[id]
	srv.mu.Unlock()
	if s == nil {
		return ErrNoSession
	}
	return srv.retireSession(s, "session retired", false)
}

// retireSession retires s; automatic, only if it is unbound and was never
// established. Once claimed, the retirement completes whatever happens to
// the caller: half done, a restart would resurrect the session.
func (srv *Server) retireSession(s *Session, reason string, automatic bool) error {
	// The claim, under the lock the establishment takes to leave
	// StateUnbound: one of them wins.
	s.mu.Lock()
	c, st := s.conn, s.st
	switch {
	case s.state == StateFinalized || st == nil:
		s.mu.Unlock()
		return ErrNoSession
	case automatic && s.state == StateEstablishing && idle(s, st):
		s.mu.Unlock()
		return errEstablishing
	case automatic && (s.state != StateUnbound || !idle(s, st)):
		s.mu.Unlock()
		return nil
	}
	s.state = StateFinalized // no new establishment, no Send
	s.mu.Unlock()

	srv.mu.Lock()
	if srv.sessions[s.ID()] == s {
		delete(srv.sessions, s.ID())
	}
	if t := srv.retire[s]; t != nil {
		t.Stop()
		delete(srv.retire, s)
	}
	srv.mu.Unlock()

	if c != nil {
		c.fail(reason, nil)
		<-c.unbound // bounded: fail closes within the write timeout
	}
	sctx, cancel := s.storeCtx()
	err := st.Retire(sctx, time.Now())
	if err == nil {
		err = st.Sync(sctx)
	}
	cancel()
	if err != nil {
		err = fmt.Errorf("fixp/session: retiring: %w", err)
		s.mu.Lock()
		s.state, s.err = StateStopped, err
		s.mu.Unlock()
		s.doneOnce.Do(func() { close(s.done) })
		s.closeStore()
		return err
	}
	s.logInfo("retired", "reason", reason)
	s.doneOnce.Do(func() { close(s.done) })
	s.closeStore()
	s.handler.OnFinalized(s.cbCtx, s)
	return nil
}

// idle reports, under s.mu, whether s is retired automatically when
// unbound for the UnestablishedTimeout: never established, or both flows
// finished without the Terminate exchange that finalizes it.
func idle(s *Session, st store.Session) bool {
	return st.State().EstablishedAt.IsZero() || s.outFin.Finished && s.inFin.Finished
}

// track hooks the server to s's finalization and unbinding.
func (srv *Server) track(s *Session) {
	s.onFinalized = func(s *Session) {
		srv.mu.Lock()
		if srv.sessions[s.ID()] == s {
			delete(srv.sessions, s.ID())
		}
		if t := srv.retire[s]; t != nil {
			t.Stop()
			delete(srv.retire, s)
		}
		srv.mu.Unlock()
		s.closeStore()
	}
	s.onUnbound = func(s *Session) {
		if out, in := s.finishes(); out.Finished && in.Finished {
			srv.scheduleRetirement(s, time.Now())
		}
	}
}

// scheduleRetirement retires s if it is not established within the
// UnestablishedTimeout of its negotiation, or of the end of its last
// connection, both its flows finished.
func (srv *Server) scheduleRetirement(s *Session, negotiatedAt time.Time) {
	d := time.Until(negotiatedAt.Add(srv.cfg.UnestablishedTimeout))
	// Published before the callback can retire s, which deletes it.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if t := srv.retire[s]; t != nil {
		t.Stop() // replaced
	}
	srv.retire[s] = time.AfterFunc(max(d, 0), func() {
		if errors.Is(srv.retireSession(s, "never established", true), errEstablishing) {
			// Retired later if the establishment fails.
			srv.scheduleRetirement(s, time.Now())
		}
	})
}

// errEstablishing is an automatic retirement of a session being
// established: it is tried again later.
var errEstablishing = errors.New("fixp/session: session being established")

// established cancels the retirement of s, once established.
func (srv *Server) established(s *Session) {
	srv.mu.Lock()
	if t := srv.retire[s]; t != nil {
		t.Stop()
		delete(srv.retire, s)
	}
	srv.mu.Unlock()
}

// accept accepts the connections of l until ctx is done.
func (srv *Server) accept(ctx context.Context, l transport.Listener) {
	defer srv.conns.Done()
	for {
		t, err := l.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			srv.logWarn("accepting", "addr", l.Addr().String(), "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if srv.pending.Add(1) > int64(srv.cfg.MaxPendingConnections) {
			srv.pending.Add(-1)
			t.Close()
			continue
		}
		srv.conns.Add(1)
		go srv.serve(ctx, t)
	}
}

// peer is what the transport knows of a connection's peer.
func peer(t transport.Conn, req *AuthRequest) {
	req.RemoteAddr = t.RemoteAddr()
	if tc, ok := t.(transport.TLSConn); ok {
		state := tc.ConnectionState()
		req.TLS = &state
	}
	if pc, ok := t.(transport.PrincipalConn); ok {
		req.Principal = pc.Principal()
	}
}

// serve runs a connection: Negotiate, Establish, then its session.
func (srv *Server) serve(ctx context.Context, t transport.Conn) {
	defer srv.conns.Done()
	pending := true
	defer func() {
		if pending {
			srv.pending.Add(-1)
		}
	}()
	stop := context.AfterFunc(ctx, func() { t.Close() })
	deadline := time.Now().Add(srv.cfg.FirstMessageTimeout)
	if tc, ok := t.(transport.TLSConn); ok {
		hctx, cancel := context.WithDeadline(ctx, deadline)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			stop()
			t.Close()
			return
		}
	}
	r := sofh.NewReader(t, sofh.ReaderOptions{MaxLength: srv.cfg.MaxFirstFrameLength})
	negotiated := false
	for {
		t.SetReadDeadline(deadline)
		enc, msg, err := r.Next()
		if err != nil {
			break
		}
		kind, h, _, err := codec.Classify(enc, msg)
		if err != nil || kind != codec.Session {
			break
		}
		if h.TemplateId == codec.NegotiateTemplateID && !negotiated {
			if !srv.negotiate(ctx, t, msg) {
				break
			}
			negotiated = true
			deadline = time.Now().Add(srv.cfg.FirstMessageTimeout)
			continue
		}
		if h.TemplateId != codec.EstablishTemplateID {
			break
		}
		s, c := srv.establish(ctx, t, r, msg)
		if c == nil {
			break
		}
		stop()
		pending = false
		srv.pending.Add(-1)
		r.SetMaxLength(srv.cfg.MaxFrameLength)
		s.run(ctx, c, "server stopping")
		return
	}
	stop()
	t.Close()
}

// write writes a frame of the handshake.
func (srv *Server) write(t transport.Conn, f []byte) error {
	t.SetWriteDeadline(time.Now().Add(srv.cfg.WriteTimeout))
	_, err := t.Write(f)
	return err
}

// clockSkewed reports whether a request's timestamp is out of the range
// the server accepts (§7.1.7.4).
func (srv *Server) clockSkewed(ts uint64) bool {
	d := time.Since(fromNanos(ts))
	return ts < uint64(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()) || d > srv.cfg.MaxClockSkew || d < -srv.cfg.MaxClockSkew
}

// authenticate runs ServerConfig.Authenticate, bounded by the handshake.
func (srv *Server) authenticate(ctx context.Context, req *AuthRequest) (string, error) {
	actx, cancel := context.WithTimeout(ctx, srv.cfg.HandshakeTimeout)
	defer cancel()
	return srv.cfg.Authenticate(actx, req)
}

// negotiate answers a Negotiate (§4.2), and reports whether the session
// is negotiated.
func (srv *Server) negotiate(ctx context.Context, t transport.Conn, msg []byte) bool {
	var n codec.Negotiate
	if err := n.Decode(msg); err != nil {
		return false
	}
	id := fixp.SessionID(n.SessionId)
	reject := func(code fixp.NegotiationRejectCode, reason string) bool {
		srv.write(t, mustFrame(&codec.NegotiationReject{
			SessionId: n.SessionId, RequestTimestamp: n.Timestamp,
			Code: codec.NegotiationRejectCode(code), Reason: truncate(reason, srv.cfg.MaxReasonLength),
		}))
		return false
	}
	switch {
	case !id.Valid():
		return reject(fixp.NegotiationUnspecified, "session ID is not a version 4 UUID")
	case srv.clockSkewed(n.Timestamp):
		return reject(fixp.NegotiationUnspecified, "timestamp out of range")
	case len(n.Credentials) > srv.cfg.MaxCredentialsLength:
		return reject(fixp.NegotiationCredentials, "")
	}
	client := fixp.FlowType(n.ClientFlow)
	if !flowsAllowed(client, srv.cfg.Flow) || len(srv.cfg.ClientFlows) > 0 && !slices.Contains(srv.cfg.ClientFlows, client) {
		return reject(fixp.NegotiationFlowTypeNotSupported, "")
	}
	req := &AuthRequest{SessionID: id, Timestamp: fromNanos(n.Timestamp), ClientFlow: client, Credentials: n.Credentials}
	peer(t, req)
	owner, err := srv.authenticate(ctx, req)
	clear(msg) // the credentials, as they were read
	if err != nil {
		srv.logWarn("negotiation refused", "fixp_session", id.String(), "remote", t.RemoteAddr().String(), "err", err)
		var rej *fixp.NegotiationRejectedError
		if errors.As(err, &rej) {
			return reject(rej.Code, rej.Reason)
		}
		return reject(fixp.NegotiationCredentials, "")
	}

	// The ID is reserved while the store records it; the session is
	// published only once it holds its store.
	srv.mu.Lock()
	switch {
	case srv.stopped:
		srv.mu.Unlock()
		return false
	case srv.sessions[id] != nil || srv.reserved[id]:
		srv.mu.Unlock()
		return reject(fixp.NegotiationDuplicateID, "")
	case len(srv.sessions)+len(srv.reserved) >= srv.cfg.MaxSessions:
		srv.mu.Unlock()
		return reject(fixp.NegotiationUnspecified, "too many sessions")
	}
	srv.reserved[id] = true
	srv.mu.Unlock()
	now := time.Now()
	sctx, cancel := context.WithTimeout(context.Background(), srv.cfg.StoreTimeout)
	st, err := srv.cfg.Store.Create(sctx, store.Meta{
		ID: id, Role: store.Server, ClientFlow: client, ServerFlow: srv.cfg.Flow, Owner: owner, NegotiatedAt: now,
	})
	cancel()
	srv.mu.Lock()
	delete(srv.reserved, id)
	if err != nil {
		srv.mu.Unlock()
		if errors.Is(err, store.ErrExists) {
			return reject(fixp.NegotiationDuplicateID, "")
		}
		srv.logWarn("recording a negotiated session", "fixp_session", id.String(), "err", err)
		return reject(fixp.NegotiationUnspecified, "")
	}
	if srv.stopped {
		// Run took its sessions already: this one would never be stopped.
		srv.mu.Unlock()
		st.Close()
		return false
	}
	s := newSession(srv.cfg.Config, store.Server)
	s.adopt(st)
	srv.track(s)
	srv.sessions[id] = s
	srv.mu.Unlock()
	srv.scheduleRetirement(s, now)
	s.logInfo("negotiated", "owner", owner, "client_flow", client.String(), "server_flow", srv.cfg.Flow.String())
	srv.write(t, mustFrame(&codec.NegotiationResponse{
		SessionId: n.SessionId, RequestTimestamp: n.Timestamp, ServerFlow: codec.FlowType(srv.cfg.Flow),
	}))
	return true
}

// establish answers an Establish (§4.3), and returns the session and the
// connection to run it on, nil if rejected.
func (srv *Server) establish(ctx context.Context, t transport.Conn, r *sofh.Reader, msg []byte) (*Session, *conn) {
	var e codec.Establish
	if err := e.Decode(msg); err != nil {
		return nil, nil
	}
	id := fixp.SessionID(e.SessionId)
	reject := func(code fixp.EstablishmentRejectCode, reason string) (*Session, *conn) {
		srv.write(t, mustFrame(&codec.EstablishmentReject{
			SessionId: e.SessionId, RequestTimestamp: e.Timestamp,
			Code: codec.EstablishmentRejectCode(code), Reason: truncate(reason, srv.cfg.MaxReasonLength),
		}))
		return nil, nil
	}
	srv.mu.Lock()
	s := srv.sessions[id]
	srv.mu.Unlock()
	keepalive := time.Duration(e.KeepaliveInterval) * time.Millisecond
	switch {
	case s == nil:
		return reject(fixp.EstablishmentUnnegotiated, "")
	case len(e.Credentials) > srv.cfg.MaxCredentialsLength:
		return reject(fixp.EstablishmentCredentials, "")
	case srv.clockSkewed(e.Timestamp):
		return reject(fixp.EstablishmentUnspecified, "timestamp out of range")
	case keepalive < srv.cfg.MinKeepalive || keepalive > srv.cfg.MaxKeepalive:
		return reject(fixp.EstablishmentKeepaliveInterval, fmt.Sprintf("between %v and %v", srv.cfg.MinKeepalive, srv.cfg.MaxKeepalive))
	}
	out, in := s.Flows()
	req := &AuthRequest{Establish: true, SessionID: id, Timestamp: fromNanos(e.Timestamp), ClientFlow: in, Credentials: e.Credentials, Owner: s.Owner()}
	peer(t, req)
	owner, err := srv.authenticate(ctx, req)
	clear(msg)
	if err == nil && owner != s.Owner() {
		err = fmt.Errorf("authenticated as %q, the session's owner is %q", owner, s.Owner())
	}
	if err != nil {
		s.logWarn("establishment refused", "remote", t.RemoteAddr().String(), "err", err)
		var rej *fixp.EstablishmentRejectedError
		if errors.As(err, &rej) {
			return reject(rej.Code, rej.Reason)
		}
		return reject(fixp.EstablishmentCredentials, "")
	}

	s.mu.Lock()
	switch {
	case s.state == StateFinalized:
		s.mu.Unlock()
		return reject(fixp.EstablishmentUnnegotiated, "")
	case s.state == StateStopped:
		s.mu.Unlock()
		return reject(fixp.EstablishmentUnspecified, "session halted")
	case s.state != StateUnbound:
		s.mu.Unlock()
		return reject(fixp.EstablishmentAlreadyEstablished, "")
	}
	s.state = StateEstablishing
	s.mu.Unlock()
	unbind := func() {
		s.mu.Lock()
		if s.state == StateEstablishing {
			s.state = StateUnbound
		}
		s.mu.Unlock()
	}
	c := newConn(s, t, r, keepalive)
	if e.NextSeqNo.Valid {
		if err := c.checkNextSeqNo(e.NextSeqNo.Value); err != nil {
			unbind()
			var pe *protocolError
			reason := err.Error()
			if errors.As(err, &pe) {
				reason = pe.reason
			}
			return reject(fixp.EstablishmentUnspecified, reason)
		}
	}
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	if st.State().EstablishedAt.IsZero() {
		sctx, cancel := s.storeCtx()
		// Durable before the ack: a crash must not retire, as never
		// established, a session the client was told is.
		err := st.SetEstablished(sctx, time.Now())
		if err == nil {
			err = st.Sync(sctx)
		}
		cancel()
		if err != nil {
			s.halt(fmt.Errorf("fixp/session: recording the establishment: %w", err))
			return reject(fixp.EstablishmentUnspecified, "session halted")
		}
	}
	srv.established(s)
	ack := codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: millis(srv.cfg.KeepaliveInterval)}
	if out.Sequenced() {
		// The next written: messages queued go out after the ack.
		ack.NextSeqNo = codec.Some(s.q.nextToWrite())
	}
	if err := srv.write(t, mustFrame(&ack)); err != nil {
		unbind()
		return nil, nil
	}
	return s, c
}

// ConfigError is an invalid configuration.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("fixp/session: config: %s: %s", e.Field, e.Reason)
}
