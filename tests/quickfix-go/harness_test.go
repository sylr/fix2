package quickfixgo

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// The sessions of the tests are INI->ACC on the initiator's side and
// ACC->INI on the acceptor's.
var (
	iniID = session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	accID = session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"}
)

// timeout bounds every wait of the tests.
const timeout = 5 * time.Second

// recorder is a Handler recording what it is told.
type recorder struct {
	session.BaseHandler
	mu      sync.Mutex
	logons  int
	logouts []error
	apps    []*fix2.Message // clones of the application messages delivered
	admins  []*fix2.Message // clones of the administrative messages processed
	got     chan struct{}
}

func newRecorder() *recorder { return &recorder{got: make(chan struct{}, 1000)} }

func (r *recorder) notify() {
	select {
	case r.got <- struct{}{}:
	default:
	}
}

func (r *recorder) OnLogon(context.Context, *session.Session) {
	r.mu.Lock()
	r.logons++
	r.mu.Unlock()
	r.notify()
}

func (r *recorder) OnLogout(_ context.Context, _ *session.Session, err error) {
	r.mu.Lock()
	r.logouts = append(r.logouts, err)
	r.mu.Unlock()
	r.notify()
}

func (r *recorder) FromAdmin(_ context.Context, _ *session.Session, m *fix2.Message, _ session.Inbound) {
	r.mu.Lock()
	r.admins = append(r.admins, m.Clone())
	r.mu.Unlock()
	r.notify()
}

func (r *recorder) FromApp(_ context.Context, _ *session.Session, m *fix2.Message, _ session.Inbound) error {
	r.mu.Lock()
	r.apps = append(r.apps, m.Clone())
	r.mu.Unlock()
	r.notify()
	return nil
}

// wait waits until cond, called with r locked, holds.
func (r *recorder) wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		r.mu.Lock()
		ok := cond()
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.got:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// appValues returns the values of tag in the application messages delivered.
func (r *recorder) appValues(tag int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var vs []string
	for _, m := range r.apps {
		vs = append(vs, string(m.Get(tag)))
	}
	return vs
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testLogger logs to stderr when FIX2_TEST_LOG is set.
func testLogger() *slog.Logger {
	if os.Getenv("FIX2_TEST_LOG") == "" {
		return nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// event is a frame read by a peer, or the error ending its connection.
type event struct {
	frame []byte
	err   error
}

// peer is a scripted counterparty: the test writes and reads raw messages.
type peer struct {
	t           *testing.T
	conn        transport.Conn
	frames      chan event
	seq         int
	beginString string
	sender      string // our SenderCompID, the session's TargetCompID
	target      string
}

func newPeer(t *testing.T, conn transport.Conn, beginString, sender, target string) *peer {
	p := &peer{t: t, conn: conn, frames: make(chan event, 1000), seq: 1, beginString: beginString, sender: sender, target: target}
	go func() {
		f := fix2.NewFramer(conn, fix2.FramerOptions{})
		for {
			frame, err := f.Next()
			if err != nil {
				p.frames <- event{err: err}
				return
			}
			p.frames <- event{frame: bytes.Clone(frame)}
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return p
}

// acceptPeer accepts the next connection of l as the acceptor ACC talking
// to the initiator INI.
func acceptPeer(t *testing.T, l *mem.Listener) *peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := l.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return newPeer(t, conn, session.FIX44, "ACC", "INI")
}

// dialPeer dials l as the initiator INI talking to the acceptor ACC.
func dialPeer(t *testing.T, l *mem.Listener) *peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := l.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return newPeer(t, conn, session.FIX44, "INI", "ACC")
}

// build returns a message of msgType with fields (tag, value pairs) after
// the header, at the peer's next sequence number unless seq is set.
func (p *peer) build(msgType string, seq int, fields ...string) []byte {
	if seq == 0 {
		seq = p.seq
		p.seq++
	}
	b := fix2.NewBuilder(p.beginString, msgType).
		SetString(49, p.sender).SetString(56, p.target).SetInt(34, seq).
		SetString(52, string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds)))
	for i := 0; i+1 < len(fields); i += 2 {
		tag, err := strconv.Atoi(fields[i])
		if err != nil {
			p.t.Fatalf("bad tag %q", fields[i])
		}
		b.Append(tag, []byte(fields[i+1]))
	}
	return b.Build()
}

// send writes a message built as build does.
func (p *peer) send(msgType string, seq int, fields ...string) {
	p.t.Helper()
	p.write(p.build(msgType, seq, fields...))
}

// write writes raw bytes.
func (p *peer) write(raw []byte) {
	p.t.Helper()
	if _, err := p.conn.Write(raw); err != nil {
		p.t.Fatal(err)
	}
}

// next returns the next frame read, failing if the connection ends first or
// nothing comes within timeout.
func (p *peer) next() []byte {
	p.t.Helper()
	select {
	case ev := <-p.frames:
		if ev.err != nil {
			p.t.Fatalf("connection ended: %v", ev.err)
		}
		return ev.frame
	case <-time.After(timeout):
		p.t.Fatal("timed out waiting for a message")
	}
	return nil
}

// expect reads the next message and checks its MsgType and fields (tag,
// value pairs).
func (p *peer) expect(msgType string, fields ...string) *fix2.Message {
	p.t.Helper()
	frame := p.next()
	m, err := fix2.Parse(frame)
	if err != nil {
		p.t.Fatalf("invalid message %s: %v", visible(frame), err)
	}
	if string(m.MsgType()) != msgType {
		p.t.Fatalf("got %s, want MsgType %s", visible(frame), msgType)
	}
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		if got := string(m.Get(tag)); got != fields[i+1] {
			p.t.Fatalf("got %s: tag %d is %q, want %q", visible(frame), tag, got, fields[i+1])
		}
	}
	return &m
}

// skipUntil reads messages until one of msgType, which it returns, skipping
// Heartbeats and TestRequests of no interest.
func (p *peer) skipUntil(msgType string) *fix2.Message {
	p.t.Helper()
	for {
		frame := p.next()
		m, err := fix2.Parse(frame)
		if err != nil {
			p.t.Fatalf("invalid message %s: %v", visible(frame), err)
		}
		if string(m.MsgType()) == msgType {
			return &m
		}
	}
}

// expectNothing checks no message comes within d.
func (p *peer) expectNothing(d time.Duration) {
	p.t.Helper()
	select {
	case ev := <-p.frames:
		if ev.err != nil {
			p.t.Fatalf("connection ended (%v), expected it to stay quiet", ev.err)
		}
		p.t.Fatalf("got %s, expected nothing", visible(ev.frame))
	case <-time.After(d):
	}
}

// expectEnd waits for the connection to end, skipping messages.
func (p *peer) expectEnd() {
	p.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-p.frames:
			if ev.err != nil {
				return
			}
		case <-deadline:
			p.t.Fatal("connection still open")
		}
	}
}

// logon answers the session's Logon, as an acceptor does.
func (p *peer) logon(fields ...string) *fix2.Message {
	p.t.Helper()
	m := p.expect("A")
	p.send("A", 0, append([]string{"98", "0", "108", string(m.Get(108))}, fields...)...)
	return m
}

func visible(b []byte) string { return strings.ReplaceAll(string(b), "\x01", "|") }

// run runs f, an Initiator's or Acceptor's Run, until the test ends.
func run(t *testing.T, f func(context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// iniConfig returns the configuration of session INI->ACC, with change
// applied.
func iniConfig(h session.Handler, change func(*session.Config)) session.Config {
	cfg := session.Config{
		ID:            iniID,
		Handler:       h,
		LogoutTimeout: 200 * time.Millisecond,
		Logger:        testLogger(),
	}
	if change != nil {
		change(&cfg)
	}
	return cfg
}

// accConfig returns the configuration of session ACC->INI, with change
// applied.
func accConfig(h session.Handler, change func(*session.Config)) session.Config {
	cfg := iniConfig(h, change)
	if change == nil || cfg.ID == iniID {
		cfg.ID = accID
	}
	return cfg
}

// startInitiator runs an initiator of session INI->ACC, configured by change,
// dialing a new listener it returns.
func startInitiator(t *testing.T, change func(*session.Config)) (*session.Session, *recorder, *mem.Listener) {
	t.Helper()
	rec := newRecorder()
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond})
	s, err := i.Add(iniConfig(rec, change), l)
	if err != nil {
		t.Fatal(err)
	}
	run(t, i.Run)
	return s, rec, l
}

// loggedOn returns an initiator session INI->ACC logged on to a scripted
// acceptor.
func loggedOn(t *testing.T, change func(*session.Config)) (*session.Session, *recorder, *peer) {
	t.Helper()
	s, rec, l := startInitiator(t, change)
	p := acceptPeer(t, l)
	p.logon()
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	return s, rec, p
}

// startAcceptor runs an acceptor of session ACC->INI, configured by change,
// on a new listener it returns.
func startAcceptor(t *testing.T, change func(*session.Config)) (*session.Session, *recorder, *mem.Listener) {
	t.Helper()
	rec := newRecorder()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{Logger: testLogger()})
	s, err := a.Add(accConfig(rec, change))
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	run(t, a.Run)
	return s, rec, l
}

// acceptedPeer returns an acceptor session ACC->INI logged on with a
// scripted initiator.
func acceptedPeer(t *testing.T, change func(*session.Config)) (*session.Session, *recorder, *peer) {
	t.Helper()
	s, rec, l := startAcceptor(t, change)
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	return s, rec, p
}

// pair runs an initiator and an acceptor of beginString logged on to each
// other, configured by changes of their own.
func pair(t *testing.T, iniChange, accChange func(*session.Config)) (ini, acc *session.Session, ir, ar *recorder) {
	t.Helper()
	acc, ar, l := startAcceptor(t, accChange)
	ir = newRecorder()
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond})
	ini, err := i.Add(iniConfig(ir, iniChange), l)
	if err != nil {
		t.Fatal(err)
	}
	run(t, i.Run)
	ir.wait(t, "initiator logon", func() bool { return ir.logons >= 1 })
	ar.wait(t, "acceptor logon", func() bool { return ar.logons >= 1 })
	return ini, acc, ir, ar
}

// knownIssue skips a test reproducing an issue fix2 shares with QuickFIX/Go,
// unless FIX2_QUICKFIX_ISSUES is set: why says how fix2 behaves.
func knownIssue(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("FIX2_QUICKFIX_ISSUES") == "" {
		t.Skip("fix2 shares this issue (FIX2_QUICKFIX_ISSUES=1 runs it): " + why)
	}
}

// dict loads a dictionary of cmd/fixgen/testdata: FIX40, FIX44, FIX50SP2 or
// FIXT11.
func dict(t testing.TB, name string) *dictionary.Dict {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
	d, err := xmldict.LoadFile(filepath.Join(dir, "cmd", "fixgen", "testdata", name+".xml"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// dictXML loads a dictionary from QuickFIX XML.
func dictXML(t testing.TB, xml string) *dictionary.Dict {
	t.Helper()
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// raw builds a whole message from "tag=value" fields separated by |, with
// BodyLength and CheckSum computed: 8 is the first field given, 9 and 10 are
// left out.
func raw(fields string) []byte {
	parts := strings.Split(strings.Trim(fields, "|"), "|")
	head := parts[0] + "\x01"
	var body []byte
	for _, f := range parts[1:] {
		body = append(body, f...)
		body = append(body, 1)
	}
	msg := []byte(head)
	msg = append(msg, "9="...)
	msg = strconv.AppendInt(msg, int64(len(body)), 10)
	msg = append(msg, 1)
	msg = append(msg, body...)
	return fix2.AppendTrailer(msg, 0)
}
