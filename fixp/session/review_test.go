package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
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
	if err := term.Decode(r.readTemplate(codec.TerminateTemplateID)); err != nil || string(term.Reason) != "application message before a Sequence" {
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
	for i := range 50 {
		r := dialRaw(t, l)
		r.negotiate(fixp.Unsequenced, "user:alice")
		r.establish(1000, codec.Optional[uint64]{}, fmt.Sprintf("user:alice%d", i%1))
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
