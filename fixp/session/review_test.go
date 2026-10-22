package session

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// recordingStore logs the calls of its sessions, in order.
type recordingStore struct {
	store.Store
	mu  sync.Mutex
	ops []string
}

func (r *recordingStore) log(op string) {
	r.mu.Lock()
	r.ops = append(r.ops, op)
	r.mu.Unlock()
}

func (r *recordingStore) calls() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.ops, " ")
}

func (r *recordingStore) Create(ctx context.Context, m store.Meta) (store.Session, error) {
	ss, err := r.Store.Create(ctx, m)
	if err != nil {
		return nil, err
	}
	r.log("Create")
	return &recordingSession{Session: ss, r: r}, nil
}

type recordingSession struct {
	store.Session
	r *recordingStore
}

func (s *recordingSession) SetEstablished(ctx context.Context, t time.Time) error {
	s.r.log("SetEstablished")
	return s.Session.SetEstablished(ctx, t)
}

func (s *recordingSession) Sync(ctx context.Context) error {
	s.r.log("Sync")
	return s.Session.Sync(ctx)
}

// The first establishment is durable before it is acknowledged.
func TestEstablishmentSyncedBeforeAck(t *testing.T) {
	rs := &recordingStore{Store: memstore.New()}
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced, server: func(c *ServerConfig) {
		c.Store = rs
	}})
	_ = p
	if got := rs.calls(); !strings.HasPrefix(got, "Create SetEstablished Sync") {
		t.Fatalf("server store calls %q", got)
	}
}

// The credentials a callback returns are its own: they are read, not
// cleared.
func TestCredentialsNotCleared(t *testing.T) {
	creds := []byte("user:alice")
	newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced, client: func(c *ClientConfig) {
		c.Credentials = func(context.Context, CredentialsRequest) ([]byte, error) { return creds, nil }
	}})
	if string(creds) != "user:alice" {
		t.Fatalf("credentials now %q", creds)
	}
}

func TestSendTooLarge(t *testing.T) {
	p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.Unsequenced, client: func(c *ClientConfig) {
		c.MaxPendingBytes = 64
	}})
	if _, err := p.client.Session().Send(bg, appMsg{1, strings.Repeat("x", 100)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Send of 114 bytes, MaxPendingBytes 64: %v", err)
	}
	if _, err := p.client.Session().Send(bg, appMsg{1, "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigBounds(t *testing.T) {
	var ce *ConfigError
	_, err := NewClient(ClientConfig{Config: Config{Handler: BaseHandler{}, MaxReasonLength: 70000}, Store: memstore.New()}, mem.Listen("x"))
	if !errors.As(err, &ce) || ce.Field != "MaxReasonLength" {
		t.Errorf("MaxReasonLength 70000: %v", err)
	}
}

// A MessageTemplate, a session message, ends the numbering: an
// application message must follow a Sequence again.
func TestMessageTemplateEndsNumbering(t *testing.T) {
	l := mem.Listen("server")
	h := newRecorder()
	_, cancel, done := newServer(t, l, memstore.New(), h, fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)
	r.negotiate(fixp.Idempotent, "user:alice")
	r.establish(1000, codec.Optional[uint64]{}, "user:alice")
	h.expect(t, "established")
	r.send(&codec.Sequence{NextSeqNo: 1})
	r.send(appMsg{1, "one"})
	r.send(&codec.MessageTemplate{EncodingType: 0xEB50, Version: []byte("1.0"), Template: []byte("<xml/>")})
	r.send(appMsg{1, "two"})
	var term codec.Terminate
	if _, err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil || string(term.Reason) != "application message before a Sequence" {
		t.Fatalf("Terminate: %+v %q, %v", term, term.Reason, err)
	}
	if e := h.expect(t, "message"); e.body != "one" {
		t.Fatalf("got %v", e)
	}
}

// A server starting with a session negotiated long ago and never
// established retires it at once, safely.
func TestStartupRetirement(t *testing.T) {
	st := memstore.New()
	var ids []fixp.SessionID
	for range 20 {
		id := fixp.NewSessionID()
		ids = append(ids, id)
		if _, err := st.Create(bg, store.Meta{ID: id, Role: store.Server, ServerFlow: fixp.Unsequenced, ClientFlow: fixp.Unsequenced, NegotiatedAt: time.Now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	h := newRecorder()
	srv, cancel, done := newServer(t, mem.Listen("server"), st, h, fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	for range ids {
		h.expect(t, "finalized")
	}
	if n := len(srv.Sessions()); n != 0 {
		t.Fatalf("%d sessions left", n)
	}
	for _, id := range ids {
		ss, _ := st.Open(bg, id)
		if !ss.State().Retired {
			t.Errorf("%s not retired", id)
		}
	}
}

// Retire racing negotiations and establishments never finds a session
// half made.
func TestRetireRacesNegotiation(t *testing.T) {
	l := mem.Listen("server")
	srv, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
	defer func() { cancel(); <-done }()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, s := range srv.Sessions() {
				srv.Retire(bg, s.ID())
			}
		}
	})
	for range 50 {
		r := dialRaw(t, l)
		r.negotiate(fixp.Unsequenced, "user:alice")
		r.establish(1000, codec.Optional[uint64]{}, "user:alice")
	}
	close(stop)
	wg.Wait()
}

// slowWriteConn delivers what it writes, then returns late: the peer may
// answer before Write returns.
type slowWriteConn struct{ transport.Conn }

func (c slowWriteConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	time.Sleep(30 * time.Millisecond)
	return n, err
}

// The answer to our Terminate, read before the Write of the Terminate
// returned, is still its answer.
func TestTerminateAnsweredBeforeWriteReturns(t *testing.T) {
	l := mem.Listen("server")
	srvH, cliH := newRecorder(), newRecorder()
	_, cancelSrv, srvDone := newServer(t, l, memstore.New(), srvH, fixp.Unsequenced, nil)
	defer func() { cancelSrv(); <-srvDone }()
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		c, err := l.Dial(ctx)
		if err != nil {
			return nil, err
		}
		return slowWriteConn{c}, nil
	})
	c := newClient(t, dialer, memstore.New(), cliH, fixp.Unsequenced, func(c *ClientConfig) {
		c.KeepaliveInterval = time.Second
	})
	cancelCli, cliDone := runClient(c)
	defer func() { cancelCli(); <-cliDone }()
	cliH.expect(t, "established")
	srvH.expect(t, "established")
	c.Session().Terminate(bg, "bye")
	var te *fixp.TerminatedError
	if e := cliH.expect(t, "unbound"); !errors.As(e.err, &te) || te.ByPeer || te.Reason != "bye" {
		t.Fatalf("client unbound: %v", e.err)
	}
	if e := srvH.expect(t, "unbound"); !errors.As(e.err, &te) || !te.ByPeer {
		t.Fatalf("server unbound: %v", e.err)
	}
}

// lateListener accepts connections whose writes deliver delay late, once
// delay is set: a peer whose writes run late.
type lateListener struct {
	*mem.Listener
	delay *atomic.Int64 // nanoseconds
}

func (l lateListener) Accept(ctx context.Context) (transport.Conn, error) {
	c, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return lateWriteConn{c, l.delay}, nil
}

type lateWriteConn struct {
	transport.Conn
	delay *atomic.Int64
}

func (c lateWriteConn) Write(b []byte) (int, error) {
	time.Sleep(time.Duration(c.delay.Load()))
	return c.Conn.Write(b)
}

// The side that sent the first Terminate waits for its answer the longer
// of the two sides' keepalive intervals: a server whose interval is longer
// than the client's, its writes late, answers past the client's interval
// and the session is finalized on that connection. Waiting its own
// interval, the client closed it first, connected again and again, and
// was finalized only by chance.
func TestTerminateAnsweredPastOurInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := mem.Listen("server")
		var delay atomic.Int64
		srvH, cliH := newRecorder(), newRecorder()
		_, cancelSrv, srvDone := newServer(t, lateListener{l, &delay}, memstore.New(), srvH, fixp.None, func(c *ServerConfig) {
			c.KeepaliveInterval = time.Second
		})
		defer func() { cancelSrv(); <-srvDone }()
		var dials atomic.Int32
		dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
			dials.Add(1)
			return l.Dial(ctx)
		})
		c := newClient(t, dialer, memstore.New(), cliH, fixp.Unsequenced, nil) // 50ms keepalives
		cancelCli, cliDone := runClient(c)
		defer func() { cancelCli(); <-cliDone }()
		cliH.expect(t, "established")
		srvH.expect(t, "established")
		delay.Store(int64(200 * time.Millisecond))
		if err := c.Session().Finish(bg); err != nil {
			t.Fatal(err)
		}
		// Finalized on the first connection, a rejected one ending the
		// session as well.
		for e := cliH.next(t); ; e = cliH.next(t) {
			if dials.Load() > 1 {
				t.Fatal("connected again: the Terminate's answer was not awaited")
			}
			if e.kind == "finalized" {
				break
			}
		}
	})
}

// gatedUnbound holds its first OnUnbound until release, recording whether
// another of the session's callbacks runs meanwhile.
type gatedUnbound struct {
	*recorder
	unbound, release chan struct{}
	open             func() // closes release, once
	unbounds         atomic.Int32
	active, overlap  atomic.Bool
}

func newGatedUnbound() *gatedUnbound {
	h := &gatedUnbound{recorder: newRecorder(), unbound: make(chan struct{}), release: make(chan struct{})}
	h.open = sync.OnceFunc(func() { close(h.release) })
	return h
}

func (h *gatedUnbound) during() {
	if h.active.Load() {
		h.overlap.Store(true)
	}
}

func (h *gatedUnbound) OnEstablished(ctx context.Context, s *Session) {
	h.during()
	h.recorder.OnEstablished(ctx, s)
}

func (h *gatedUnbound) OnMessage(ctx context.Context, s *Session, msg []byte, in Inbound) error {
	h.during()
	return h.recorder.OnMessage(ctx, s, msg, in)
}

func (h *gatedUnbound) OnFinalized(ctx context.Context, s *Session) {
	h.during()
	h.recorder.OnFinalized(ctx, s)
}

func (h *gatedUnbound) OnUnbound(ctx context.Context, s *Session, err error) {
	if h.unbounds.Add(1) == 1 {
		h.active.Store(true)
		close(h.unbound)
		<-h.release
		h.active.Store(false)
	}
	h.recorder.OnUnbound(ctx, s, err)
}

// noEvent checks the handler has heard of nothing, once every goroutine of
// the bubble is blocked.
func noEvent(t *testing.T, r *recorder, while string) {
	t.Helper()
	synctest.Wait()
	select {
	case e := <-r.events:
		t.Fatalf("%v %s", e, while)
	default:
	}
}

// A connection established while the last one's OnUnbound runs is acked
// once it returns: its OnEstablished and OnMessage do not run beside it
// (A8-01).
func TestReestablishWaitsForUnbound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newGatedUnbound()
		l := mem.Listen("server")
		_, cancel, done := newServer(t, l, memstore.New(), h, fixp.Unsequenced, nil)
		defer func() { cancel(); <-done }()
		defer h.open()
		first := dialRaw(t, l)
		first.negotiate(fixp.Unsequenced, "user:alice")
		first.establish(1000, codec.Optional[uint64]{}, "user:alice")
		h.expect(t, "established")
		first.c.Close()
		<-h.unbound

		second := dialRaw(t, l)
		second.id = first.id
		second.send(&codec.Establish{SessionId: second.id, Timestamp: nanos(time.Now()), KeepaliveInterval: 1000, Credentials: []byte("user:alice")})
		noEvent(t, h.recorder, "while OnUnbound runs")
		h.open()
		h.expect(t, "unbound")
		var ack codec.EstablishmentAck
		if _, err := ack.Decode(second.readTemplate(codec.EstablishmentAckTemplateID)); err != nil {
			t.Fatal(err)
		}
		h.expect(t, "established")
		second.send(appMsg{1, "after reconnecting"})
		h.expect(t, "message")
		if h.overlap.Load() {
			t.Error("a callback of the new connection ran beside OnUnbound")
		}
	})
}

// A retirement while OnUnbound runs returns once the session is retired
// durably, without waiting for the callback; OnFinalized follows when it
// returns (A8-01).
func TestRetireDuringUnbound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var h *gatedUnbound
		var srvStore *memstore.Store
		rs := newRawSession(t, fixp.Unsequenced, fixp.Unsequenced, codec.Optional[uint64]{}, func(c *ServerConfig) {
			h = newGatedUnbound()
			h.recorder = c.Handler.(*recorder)
			c.Handler = h
			srvStore = c.Store.(*memstore.Store)
		})
		defer h.open()
		s := rs.session(t)
		rs.r.c.Close()
		<-h.unbound
		if err := rs.srv.Retire(bg, s.ID()); err != nil {
			t.Fatalf("Retire: %v", err)
		}
		if stored, _ := srvStore.Open(bg, s.ID()); !stored.State().Retired {
			t.Errorf("not retired durably when Retire returned: %+v", stored.State())
		}
		noEvent(t, h.recorder, "while OnUnbound runs")
		h.open()
		h.expect(t, "unbound")
		h.expect(t, "finalized")
		if h.overlap.Load() {
			t.Error("OnFinalized ran beside OnUnbound")
		}
	})
}

// retiringUnbound retires its session from OnUnbound, with ctx or with a
// context of its own.
type retiringUnbound struct {
	*recorder
	srv     atomic.Pointer[Server]
	own     bool
	retired chan error
}

func (h *retiringUnbound) OnUnbound(ctx context.Context, s *Session, err error) {
	if h.own {
		ctx = context.Background()
	}
	h.retired <- h.srv.Load().Retire(ctx, s.ID())
	h.recorder.OnUnbound(ctx, s, err)
}

// Retire called from OnUnbound, whatever its context, returns without
// waiting for it; OnFinalized follows once it returns.
func TestRetireFromUnbound(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprintf("own context %v", own), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := &retiringUnbound{recorder: newRecorder(), own: own, retired: make(chan error, 1)}
				l := mem.Listen("server")
				srv, cancel, done := newServer(t, l, memstore.New(), h, fixp.Unsequenced, nil)
				defer func() { cancel(); <-done }()
				h.srv.Store(srv)
				r := dialRaw(t, l)
				r.negotiate(fixp.Unsequenced, "user:alice")
				r.establish(1000, codec.Optional[uint64]{}, "user:alice")
				h.expect(t, "established")
				r.c.Close()
				h.expect(t, "unbound")
				h.expect(t, "finalized")
				if err := <-h.retired; err != nil {
					t.Fatal(err)
				}
				if srv.Session(r.id) != nil {
					t.Error("retired session still served")
				}
			})
		})
	}
}

// A retirement while a finished session's OnUnbound runs, before its
// finalization, is refused: the session is finalized once, by its
// connection's end (A8-01).
func TestRetireWhileFinalizing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var h *gatedUnbound
		p := newPair(t, pairOptions{clientFlow: fixp.Unsequenced, serverFlow: fixp.None, server: func(c *ServerConfig) {
			h = newGatedUnbound()
			h.recorder = c.Handler.(*recorder)
			c.Handler = h
		}})
		defer h.open()
		cs, ss := p.client.Session(), p.serverSession(t)
		id := cs.ID()
		if err := cs.Finish(bg); err != nil {
			t.Fatal(err)
		}
		<-h.unbound
		if st := ss.State(); st != StateFinalizing {
			t.Fatalf("state %s while OnUnbound runs, want finalizing", st)
		}
		retired := make(chan error, 1)
		go func() { retired <- p.srv.Retire(bg, id) }()
		synctest.Wait()
		h.open()
		if err := <-retired; !errors.Is(err, ErrNoSession) {
			t.Errorf("Retire while finalizing: %v", err)
		}
		p.srvH.skipUntil(t, "finalized")
		noEvent(t, p.srvH, "after finalizing")
		if h.overlap.Load() {
			t.Error("OnFinalized ran beside OnUnbound")
		}
		stored, _ := p.srvStore.Open(bg, id)
		if s := stored.State(); s.FinalizedAt.IsZero() || s.Retired {
			t.Errorf("stored %+v", s)
		}
	})
}

// closeGateListener holds the first Close of each connection it accepts
// until its gate opens.
type closeGateListener struct {
	*mem.Listener
	conns chan *closeGateConn
}

func (l closeGateListener) Accept(ctx context.Context) (transport.Conn, error) {
	c, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	g := &closeGateConn{Conn: c, closing: make(chan struct{}), release: make(chan struct{})}
	g.open = sync.OnceFunc(func() { close(g.release) })
	l.conns <- g
	return g, nil
}

type closeGateConn struct {
	transport.Conn
	closing, release chan struct{}
	open             func() // closes release, once
	once             sync.Once
}

func (c *closeGateConn) Close() error {
	c.once.Do(func() {
		close(c.closing)
		<-c.release
	})
	return c.Conn.Close()
}

// A retirement claiming a session whose connection ends on the finished
// exchange, before the session is unbound, finalizes it alone: the
// connection's end does not finalize it again (A8-01).
func TestRetireAfterFinishedExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := mem.Listen("server")
		gl := closeGateListener{l, make(chan *closeGateConn, 1)}
		srvH, cliH := newRecorder(), newRecorder()
		srvStore := memstore.New()
		srv, cancelSrv, srvDone := newServer(t, gl, srvStore, srvH, fixp.None, nil)
		defer func() { cancelSrv(); <-srvDone }()
		c := newClient(t, l, memstore.New(), cliH, fixp.Unsequenced, nil)
		cancelCli, cliDone := runClient(c)
		defer func() { cancelCli(); <-cliDone }()
		srvH.expect(t, "established")
		g := <-gl.conns
		defer g.open()
		id := c.Session().ID()
		ss := srv.Session(id)
		if err := c.Session().Finish(bg); err != nil {
			t.Fatal(err)
		}
		<-g.closing
		retired := make(chan error, 1)
		go func() { retired <- srv.Retire(bg, id) }()
		// Claimed, the retirement then waits on the gated close's Once: a
		// mutex, which neither synctest.Wait nor the fake clock counts as
		// blocked.
		for ss.State() != StateFinalized {
			runtime.Gosched()
		}
		g.open()
		if err := <-retired; err != nil {
			t.Fatalf("Retire: %v", err)
		}
		srvH.skipUntil(t, "finalized")
		noEvent(t, srvH, "after the retirement")
		stored, _ := srvStore.Open(bg, id)
		if s := stored.State(); !s.Retired {
			t.Errorf("stored %+v, want retired", s)
		}
	})
}
