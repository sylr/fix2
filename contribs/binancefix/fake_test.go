package binancefix

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// docKey is the example private key of Binance's FIX API documentation.
const docKey = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIIJEYWtGBrhACmb9Dvy+qa8WEf0lQOl1s4CLIAB9m89u
-----END PRIVATE KEY-----
`

// wait bounds every wait of the tests for something that should happen.
const wait = 5 * time.Second

func testKey(t testing.TB) ed25519.PrivateKey {
	t.Helper()
	k, err := ParsePrivateKey([]byte(docKey))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fake is a fake Binance: its Dial hands the server end of an in-memory
// connection to the test, as a peer.
type fake struct {
	t     *testing.T
	peers chan *peer
	// wrap, if set, wraps the client end of the connections.
	wrap func(transport.Conn) transport.Conn
	mu   sync.Mutex
	dial []time.Time // when each Dial happened
}

func newFake(t *testing.T) *fake { return &fake{t: t, peers: make(chan *peer, 16)} }

func (f *fake) Dial(ctx context.Context) (transport.Conn, error) {
	f.mu.Lock()
	f.dial = append(f.dial, time.Now())
	f.mu.Unlock()
	a, b := mem.Pipe()
	if f.wrap != nil {
		a = f.wrap(a)
	}
	p := newPeer(b)
	select {
	case f.peers <- p:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a, nil
}

func (f *fake) dials() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.dial...)
}

// accept returns the next connection's peer.
func (f *fake) accept() *peer {
	f.t.Helper()
	select {
	case p := <-f.peers:
		f.t.Cleanup(func() { p.c.Close() })
		return p
	case <-time.After(wait):
		f.t.Fatal("no connection")
		return nil
	}
}

// msg is a message the peer received.
type msg struct {
	h    MessageHeader
	body []byte
	at   time.Time
}

// peer is the server end of a connection.
type peer struct {
	c    transport.Conn
	msgs chan msg
	seq  uint32 // the next to send
	err  error  // the reading's end, once msgs is closed
}

func newPeer(c transport.Conn) *peer {
	p := &peer{c: c, msgs: make(chan msg, 4096), seq: 1}
	go func() {
		defer close(p.msgs)
		fr := lesofh.NewReader(c, 0)
		for {
			b, err := fr.Next()
			if err != nil {
				p.err = err
				return
			}
			h, body, err := fixsbe.ReadHeader(b)
			if err != nil {
				p.err = err
				return
			}
			p.msgs <- msg{h: h, body: append([]byte(nil), body...), at: time.Now()}
		}
	}()
	return p
}

// next returns the next message received.
func (p *peer) next(t *testing.T) msg {
	t.Helper()
	select {
	case m, ok := <-p.msgs:
		if !ok {
			t.Fatalf("connection ended: %v", p.err)
		}
		return m
	case <-time.After(wait):
		t.Fatal("no message")
		return msg{}
	}
}

// expect returns the next message of template tid, skipping our
// Heartbeats (without TestReqID).
func (p *peer) expect(t *testing.T, tid uint16) msg {
	t.Helper()
	for {
		m := p.next(t)
		if m.h.TemplateId == tid {
			return m
		}
		if m.h.TemplateId == fixsbe.HeartbeatTemplateID {
			var hb fixsbe.Heartbeat
			if _, err := hb.DecodeBody(m.h, m.body); err == nil && len(hb.TestReqID) == 0 {
				continue
			}
		}
		t.Fatalf("template %d, want %d", m.h.TemplateId, tid)
	}
}

// ended waits for the client to close the connection, returning the
// messages received meanwhile.
func (p *peer) ended(t *testing.T) []msg {
	t.Helper()
	var ms []msg
	timeout := time.After(wait)
	for {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				return ms
			}
			ms = append(ms, m)
		case <-timeout:
			t.Fatal("connection not closed")
		}
	}
}

type encoder interface {
	AppendWithHeader([]byte, MessageHeader) ([]byte, error)
}

// send sends m with the next sequence number.
func (p *peer) send(t *testing.T, m encoder) uint32 {
	t.Helper()
	seq := p.seq
	p.sendSeq(t, m, seq)
	p.seq++
	return seq
}

// sendSeq sends m with sequence number seq.
func (p *peer) sendSeq(t *testing.T, m encoder, seq uint32) {
	t.Helper()
	b, err := m.AppendWithHeader(nil, MessageHeader{SeqNum: seq, SendingTime: time.Now().UnixMicro()})
	if err != nil {
		t.Fatal(err)
	}
	f, err := lesofh.AppendFrame(nil, b)
	if err != nil {
		t.Fatal(err)
	}
	p.write(t, f)
}

func (p *peer) write(t *testing.T, b []byte) {
	t.Helper()
	if _, err := p.c.Write(b); err != nil {
		t.Fatal(err)
	}
}

// logon reads the client's Logon, checks it and its signature, and
// answers it with a LogonAck.
func (p *peer) logon(t *testing.T, cfg *Config) *fixsbe.Logon {
	t.Helper()
	m := p.expect(t, fixsbe.LogonTemplateID)
	l := checkLogon(t, m, cfg)
	p.send(t, &fixsbe.LogonAck{EncryptMethod: Some[uint8](0), HeartBtInt: l.HeartBtInt, UUID: []byte("uuid-1")})
	return l
}

// checkLogon checks Logon m against cfg: its header, its fields, its
// signature, its SendingTime's text the default form.
func checkLogon(t *testing.T, m msg, cfg *Config) *fixsbe.Logon {
	t.Helper()
	return checkLogonAs(t, m, cfg, SignatureTimeMicros)
}

// sendingTimeForms are the texts of the Logon's SendingTime the fake
// verifies a signature with, by form: written here apart from the client's
// SignatureTime.Format, as Binance would.
var sendingTimeForms = map[SignatureTime]func(time.Time) string{
	SignatureTimeMillis:     func(t time.Time) string { return t.UTC().Format("20060102-15:04:05.000") },
	SignatureTimeMicros:     func(t time.Time) string { return t.UTC().Format("20060102-15:04:05.000000") },
	SignatureTimeSeconds:    func(t time.Time) string { return t.UTC().Format("20060102-15:04:05") },
	SignatureTimeNanos:      func(t time.Time) string { return t.UTC().Format("20060102-15:04:05.000000000") },
	SignatureTimeUnixMicros: func(t time.Time) string { return strconv.FormatInt(t.UnixMicro(), 10) },
	SignatureTimeUnixMillis: func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) },
}

// checkLogonAs checks Logon m against cfg, its signature made of the
// SendingTime's text in form f, and no other.
func checkLogonAs(t *testing.T, m msg, cfg *Config, f SignatureTime) *fixsbe.Logon {
	t.Helper()
	var l fixsbe.Logon
	if _, err := l.DecodeBody(m.h, m.body); err != nil {
		t.Fatal(err)
	}
	if m.h.SeqNum != 1 || m.h.SchemaId != 1 || m.h.Version != 1 {
		t.Errorf("Logon header %+v", m.h)
	}
	if d := time.Since(time.UnixMicro(m.h.SendingTime)); d < 0 || d > wait {
		t.Errorf("Logon sendingTime %v ago", d)
	}
	if l.EncryptMethod != Some[uint8](0) || l.ResetSeqNumFlag != Some(fixsbe.BoolEnumTrue) ||
		string(l.TargetCompId) != "SPOT" || string(l.SenderCompId) != cfg.SenderCompID || string(l.Username) != cfg.APIKey ||
		time.Duration(l.HeartBtInt)*time.Second != cfg.HeartBtInt || l.DropCopyFlag.Valid {
		t.Errorf("Logon %+v", l)
	}
	// The signature, checked as Binance would, under an assumption on
	// SendingTime's text (SignatureTime): of form f, and no other.
	sig, err := base64.StdEncoding.DecodeString(string(l.RawData))
	if err != nil {
		t.Errorf("Logon signature %q: %v", l.RawData, err)
	}
	for g, text := range sendingTimeForms {
		payload := strings.Join([]string{"A", string(l.SenderCompId), string(l.TargetCompId), strconv.FormatUint(uint64(m.h.SeqNum), 10),
			text(time.UnixMicro(m.h.SendingTime))}, "\x01")
		if ok := ed25519.Verify(testKey(t).Public().(ed25519.PublicKey), []byte(payload), sig); ok != (g == f) {
			t.Errorf("Logon signature %q verifies %q: %v", l.RawData, payload, ok)
		}
	}
	return &l
}

// event is what the recording Handler records.
type rec struct {
	kind string // "logon", "logout", "news", "reject", "er", ...
	in   Inbound
	id   string // ClOrdID, ClListID or ReqID
	ev   any    // LogonEvent, LogoutEvent, NewsEvent
	code int32
}

// recorder is a Handler recording what it receives, its hooks called
// first.
type recorder struct {
	BaseHandler
	recs chan rec

	onLogon  func(LogonEvent)
	onER     func(Inbound, *ExecutionReport)
	onLimits func(Inbound, *LimitResponse)
	onNews   func(NewsEvent)
}

func newRecorder() *recorder { return &recorder{recs: make(chan rec, 4096)} }

func (r *recorder) OnLogon(e LogonEvent) {
	if r.onLogon != nil {
		r.onLogon(e)
	}
	r.recs <- rec{kind: "logon", ev: e}
}
func (r *recorder) OnLogout(e LogoutEvent) { r.recs <- rec{kind: "logout", ev: e} }
func (r *recorder) OnNews(e NewsEvent) {
	if r.onNews != nil {
		r.onNews(e)
	}
	r.recs <- rec{kind: "news", ev: e, in: e.Inbound}
}
func (r *recorder) OnReject(in Inbound, m *Reject) {
	r.recs <- rec{kind: "reject", in: in, code: m.ErrorCode.Value}
}
func (r *recorder) OnExecutionReport(in Inbound, m *ExecutionReport) {
	if r.onER != nil {
		r.onER(in, m)
	}
	r.recs <- rec{kind: "er", in: in, id: string(m.ClOrdID)}
}
func (r *recorder) OnListStatus(in Inbound, m *ListStatus) {
	r.recs <- rec{kind: "list", in: in, id: string(m.ClListID)}
}
func (r *recorder) OnOrderCancelReject(in Inbound, m *OrderCancelReject) {
	r.recs <- rec{kind: "cancelreject", in: in, id: string(m.ClOrdID)}
}
func (r *recorder) OnLimitResponse(in Inbound, m *LimitResponse) {
	if r.onLimits != nil {
		r.onLimits(in, m)
	}
	r.recs <- rec{kind: "limits", in: in, id: string(m.ReqID)}
}
func (r *recorder) OnUnknown(in Inbound, body []byte, err error) {
	r.recs <- rec{kind: "unknown", in: in}
}

// next returns the next record of kind, skipping others.
func (r *recorder) next(t *testing.T, kind string) rec {
	t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case x := <-r.recs:
			if x.kind == kind {
				return x
			}
		case <-timeout:
			t.Fatalf("no %s event", kind)
		}
	}
}

// testConfig returns a Config dialing f, recording with h.
func testConfig(f *fake, h Handler) Config {
	return Config{
		Dialer:        f,
		APIKey:        "api-key",
		PrivateKey:    testKey(f.t),
		SenderCompID:  "TEST1",
		LogonTimeout:  wait,
		LogoutTimeout: 500 * time.Millisecond,
		Reconnect:     Reconnect{MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond},
		Handler:       h,
	}
}

// start runs a Client of cfg, its timers on interval, until the test ends.
func start(t *testing.T, cfg Config, interval time.Duration, hooks ...func(*Client)) (*Client, <-chan error) {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.interval = interval
	for _, h := range hooks {
		h(c)
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(finished)
		done <- c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(wait):
			t.Error("Run did not return")
		}
	})
	return c, done
}

// runErr waits for Run's error.
func runErr(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		t.Fatal("Run did not return")
		return nil
	}
}

// order returns a NewOrderSingle of ClOrdID id.
func order(id string) *NewOrderSingle {
	return &NewOrderSingle{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](100000000), OrdType: fixsbe.OrdTypeLimit,
		Price: Some[int64](6543210), Side: fixsbe.SideBuy, TimeInForce: Some(fixsbe.TimeInForceGoodTillCancel),
		ClOrdID: []byte(id), Symbol: []byte("BTCUSDT")}
}

// report returns an ExecutionReport of ClOrdID id.
func report(id string) *ExecutionReport {
	return &ExecutionReport{OrderID: Some[int64](1), OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideBuy, ExecType: fixsbe.ExecTypeNew,
		OrdStatus: fixsbe.OrdStatusNew, ClOrdID: []byte(id), Symbol: []byte("BTCUSDT")}
}

// decode decodes m into a message of type M.
func decode[M any, P interface {
	*M
	DecodeBody(MessageHeader, []byte) (int, error)
}](t *testing.T, m msg) *M {
	t.Helper()
	x := P(new(M))
	if _, err := x.DecodeBody(m.h, m.body); err != nil {
		t.Fatal(err)
	}
	return (*M)(x)
}

// gate wraps a connection whose writes wait for the gate to open, and may
// fail after some bytes.
type gate struct {
	transport.Conn
	mu      sync.Mutex
	closed  bool
	open    chan struct{}
	entered chan struct{} // a write waits
	fail    int           // if > 0, the next write writes fail bytes and fails
	// steps, if set, admits writes one at a time: each takes a value, -1
	// to write wholly, n >= 0 to write n bytes and fail, -2 to write
	// wholly and return only once another value is taken.
	steps    chan int
	gone     chan struct{} // closed by Close
	goneOnce sync.Once
}

func newGate(c transport.Conn) *gate {
	return &gate{Conn: c, open: make(chan struct{}), entered: make(chan struct{}, 16), gone: make(chan struct{})}
}

func (g *gate) Write(b []byte) (int, error) {
	g.mu.Lock()
	open, steps := g.open, g.steps
	g.mu.Unlock()
	if steps != nil {
		g.entered <- struct{}{}
		var n int
		select {
		case n = <-steps:
		case <-g.gone:
			return 0, errors.New("gate: closed")
		}
		switch {
		case n >= 0:
			m, _ := g.Conn.Write(b[:min(n, len(b))])
			return m, errors.New("gate: write failed")
		case n == -2:
			m, err := g.Conn.Write(b)
			select {
			case <-steps:
			case <-g.gone:
			}
			return m, err
		}
		return g.Conn.Write(b)
	}
	select {
	case <-open:
	default:
		g.entered <- struct{}{}
		<-open
	}
	g.mu.Lock()
	fail := g.fail
	g.mu.Unlock()
	if fail > 0 {
		n, _ := g.Conn.Write(b[:min(fail, len(b))])
		return n, errors.New("gate: write failed")
	}
	return g.Conn.Write(b)
}

// release opens the gate, failing the next write after n bytes if n > 0.
func (g *gate) release(n int) {
	g.mu.Lock()
	g.fail = n
	if !g.closed {
		g.closed = true
		close(g.open)
	}
	g.mu.Unlock()
}

// waitWrite waits for a write to wait at the gate.
func (g *gate) waitWrite(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(wait):
		t.Fatal("no write")
	}
}

// shut closes the gate again.
func (g *gate) shut() {
	g.mu.Lock()
	g.open, g.closed = make(chan struct{}), false
	g.mu.Unlock()
}

func (g *gate) Close() error {
	g.goneOnce.Do(func() { close(g.gone) })
	g.release(0)
	return g.Conn.Close()
}

var _ io.Writer = (*gate)(nil)

func describe(rs []*Receipt) string {
	var s []string
	for _, r := range rs {
		s = append(s, fmt.Sprintf("%v:%v", r.IDs(), r.State()))
	}
	return strings.Join(s, " ")
}
