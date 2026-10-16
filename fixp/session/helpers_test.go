package session

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// appMsg is an application message of schema 33, test's own.
type appMsg struct {
	template uint16
	body     string
}

func (m appMsg) AppendTo(dst []byte) ([]byte, error) {
	var h [8]byte
	binary.LittleEndian.PutUint16(h[0:], uint16(len(m.body)))
	binary.LittleEndian.PutUint16(h[2:], m.template)
	binary.LittleEndian.PutUint16(h[4:], 33)
	return append(append(dst, h[:]...), m.body...), nil
}

// event is what a recorder saw.
type event struct {
	kind string // established, unbound, message, applied, notapplied, finalized
	seq  uint64
	body string
	err  error
}

func (e event) String() string {
	return fmt.Sprintf("%s seq=%d body=%q err=%v", e.kind, e.seq, e.body, e.err)
}

// recorder is a Handler recording its events.
type recorder struct {
	BaseHandler
	events chan event
	// onMessage, if set, answers OnMessage.
	onMessage func(ctx context.Context, s *Session, msg []byte, in Inbound) error
	mu        sync.Mutex
	sessions  []*Session
}

func newRecorder() *recorder { return &recorder{events: make(chan event, 1024)} }

func (r *recorder) OnEstablished(_ context.Context, s *Session) {
	r.mu.Lock()
	r.sessions = append(r.sessions, s)
	r.mu.Unlock()
	r.events <- event{kind: "established"}
}

func (r *recorder) OnUnbound(_ context.Context, s *Session, err error) {
	r.events <- event{kind: "unbound", err: err}
}

func (r *recorder) OnMessage(ctx context.Context, s *Session, msg []byte, in Inbound) error {
	r.events <- event{kind: "message", seq: in.Seq, body: string(msg[8:])}
	if r.onMessage != nil {
		return r.onMessage(ctx, s, msg, in)
	}
	return nil
}

func (r *recorder) OnApplied(_ context.Context, _ *Session, from uint64, count uint32, in Inbound) error {
	r.events <- event{kind: "applied", seq: from, body: fmt.Sprint(count)}
	return nil
}

func (r *recorder) OnFinalized(context.Context, *Session) {
	r.events <- event{kind: "finalized"}
}

// next returns the next event, failing the test after a while.
func (r *recorder) next(t testing.TB) event {
	t.Helper()
	select {
	case e := <-r.events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return event{}
	}
}

// expect returns the next event, which must be of kind.
func (r *recorder) expect(t testing.TB, kind string) event {
	t.Helper()
	e := r.next(t)
	if e.kind != kind {
		t.Fatalf("event %v, want %s", e, kind)
	}
	return e
}

// quiet checks no event comes for d.
func (r *recorder) quiet(t testing.TB, d time.Duration) {
	t.Helper()
	select {
	case e := <-r.events:
		t.Fatalf("unexpected event %v", e)
	case <-time.After(d):
	}
}

var logger = slog.New(slog.NewTextHandler(io.Discard, nil))

func init() {
	if os.Getenv("FIXP_TEST_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
}

// fast is the timing of tests: short intervals.
func fast(h Handler) Config {
	return Config{Handler: h, KeepaliveInterval: 50 * time.Millisecond, HandshakeTimeout: time.Second, Logger: logger}
}

// pair is a server and a client of it, over in-memory connections.
type pair struct {
	srv       *Server
	srvH      *recorder
	srvStore  *memstore.Store
	client    *Client
	clientH   *recorder
	cliStore  *memstore.Store
	l         *mem.Listener
	cancelSrv context.CancelFunc
	cancelCli context.CancelFunc
	srvDone   chan error
	cliDone   chan error
}

type pairOptions struct {
	clientFlow, serverFlow fixp.FlowType
	server                 func(*ServerConfig)
	client                 func(*ClientConfig)
}

// authenticate accepts the credentials "user:<name>", owned by <name>.
func authenticate(_ context.Context, req *AuthRequest) (string, error) {
	if len(req.Credentials) < 5 || string(req.Credentials[:5]) != "user:" {
		return "", errors.New("bad credentials")
	}
	return string(req.Credentials[5:]), nil
}

func credentials(name string) func(context.Context, CredentialsRequest) ([]byte, error) {
	return func(context.Context, CredentialsRequest) ([]byte, error) { return []byte("user:" + name), nil }
}

func newServer(t testing.TB, l *mem.Listener, st store.Store, h Handler, flow fixp.FlowType, opt func(*ServerConfig)) (*Server, context.CancelFunc, chan error) {
	t.Helper()
	cfg := ServerConfig{Config: fast(h), Flow: flow, Store: st, Authenticate: authenticate, MinKeepalive: 10 * time.Millisecond}
	if opt != nil {
		opt(&cfg)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	return srv, cancel, done
}

func newClient(t testing.TB, d transport.Dialer, st store.Store, h Handler, flow fixp.FlowType, opt func(*ClientConfig)) *Client {
	t.Helper()
	cfg := ClientConfig{
		Config: fast(h), Flow: flow, Store: st, Credentials: credentials("alice"),
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 50 * time.Millisecond,
	}
	if opt != nil {
		opt(&cfg)
	}
	c, err := NewClient(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func runClient(c *Client) (context.CancelFunc, chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return cancel, done
}

// newPair starts a server and a client, and waits for their establishment.
func newPair(t testing.TB, o pairOptions) *pair {
	t.Helper()
	p := &pair{srvH: newRecorder(), clientH: newRecorder(), srvStore: memstore.New(), cliStore: memstore.New(), l: mem.Listen("server")}
	p.srv, p.cancelSrv, p.srvDone = newServer(t, p.l, p.srvStore, p.srvH, o.serverFlow, o.server)
	p.client = newClient(t, p.l, p.cliStore, p.clientH, o.clientFlow, o.client)
	p.cancelCli, p.cliDone = runClient(p.client)
	t.Cleanup(p.stop)
	p.clientH.expect(t, "established")
	p.srvH.expect(t, "established")
	return p
}

func (p *pair) stop() {
	p.cancelCli()
	p.cancelSrv()
	<-p.cliDone
	<-p.srvDone
}

// serverSession returns the server's session of the client's.
func (p *pair) serverSession(t testing.TB) *Session {
	t.Helper()
	s := p.srv.Session(p.client.Session().ID())
	if s == nil {
		t.Fatal("no server session")
	}
	return s
}

// raw is a peer speaking FIXP by hand.
type raw struct {
	t  testing.TB
	c  transport.Conn
	r  *sofh.Reader
	id fixp.SessionID
	ts uint64
}

func dialRaw(t testing.TB, l *mem.Listener) *raw {
	t.Helper()
	c, err := l.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &raw{t: t, c: c, r: sofh.NewReader(c, sofh.ReaderOptions{}), id: fixp.NewSessionID()}
}

func (r *raw) send(m encoder) {
	r.t.Helper()
	f, err := frame(m)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.c.Write(f); err != nil {
		r.t.Fatal(err)
	}
}

func (r *raw) sendBytes(b []byte) {
	r.t.Helper()
	if _, err := r.c.Write(b); err != nil {
		r.t.Fatal(err)
	}
}

// read reads the next message, its header and body.
func (r *raw) read() (codec.MessageHeader, []byte) {
	r.t.Helper()
	r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	enc, msg, err := r.r.Next()
	if err != nil {
		r.t.Fatalf("reading: %v", err)
	}
	_, h, _, err := codec.Classify(enc, msg)
	if err != nil {
		r.t.Fatalf("classifying: %v", err)
	}
	return h, append([]byte(nil), msg...)
}

// readTemplate reads the next message, which must be of template, skipping
// heartbeats.
func (r *raw) readTemplate(template uint16) []byte {
	r.t.Helper()
	for {
		h, msg := r.read()
		if h.SchemaId == codec.SchemaID && (h.TemplateId == codec.SequenceTemplateID || h.TemplateId == codec.UnsequencedHeartbeatTemplateID) && template != h.TemplateId {
			continue
		}
		if h.TemplateId != template {
			r.t.Fatalf("template %d of schema %d, want %d", h.TemplateId, h.SchemaId, template)
		}
		return msg
	}
}

// closed checks the connection ends.
func (r *raw) closed() {
	r.t.Helper()
	r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := r.r.Next(); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				r.t.Fatal("connection not closed")
			}
			return
		}
	}
}

func (r *raw) negotiate(flow fixp.FlowType, creds string) []byte {
	r.t.Helper()
	r.ts = nanos(time.Now())
	r.send(&codec.Negotiate{SessionId: r.id, Timestamp: r.ts, ClientFlow: codec.FlowType(flow), Credentials: []byte(creds)})
	h, msg := r.read()
	if h.TemplateId != codec.NegotiationResponseTemplateID && h.TemplateId != codec.NegotiationRejectTemplateID {
		r.t.Fatalf("answer to Negotiate: template %d", h.TemplateId)
	}
	return msg
}

func (r *raw) establish(keepalive uint32, next codec.Optional[uint64], creds string) []byte {
	r.t.Helper()
	r.ts = nanos(time.Now())
	r.send(&codec.Establish{SessionId: r.id, Timestamp: r.ts, KeepaliveInterval: keepalive, NextSeqNo: next, Credentials: []byte(creds)})
	h, msg := r.read()
	if h.TemplateId != codec.EstablishmentAckTemplateID && h.TemplateId != codec.EstablishmentRejectTemplateID {
		r.t.Fatalf("answer to Establish: template %d", h.TemplateId)
	}
	return msg
}

// rawServer is a server speaking FIXP by hand, on a listener.
func acceptRaw(t testing.TB, l *mem.Listener) *raw {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := l.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &raw{t: t, c: c, r: sofh.NewReader(c, sofh.ReaderOptions{})}
}
