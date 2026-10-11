package session_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// peer is a scripted counterparty: the test reads and writes raw messages.
type peer struct {
	t      *testing.T
	conn   transport.Conn
	frames chan event
	seq    int
}

func acceptPeer(t *testing.T, l *mem.Listener) *peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := l.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{t: t, conn: conn, frames: make(chan event, 100), seq: 1}
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
	return p
}

// send writes a message of msgType with fields after the header, at the
// peer's next sequence number unless seq is set.
func (p *peer) send(msgType string, seq int, fields ...string) {
	p.t.Helper()
	if seq == 0 {
		seq = p.seq
		p.seq++
	}
	b := fix2.NewBuilder(session.FIX44, msgType).
		SetString(49, "ACC").SetString(56, "INI").SetInt(34, seq).
		SetString(52, string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds)))
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		b.Append(tag, []byte(fields[i+1]))
	}
	if _, err := p.conn.Write(b.Build()); err != nil {
		p.t.Fatal(err)
	}
}

// expect reads the next message and checks its MsgType and fields.
func (p *peer) expect(msgType string, fields ...string) *fix2.Message {
	p.t.Helper()
	select {
	case ev := <-p.frames:
		if ev.err != nil {
			p.t.Fatalf("connection ended (%v), expected MsgType %s", ev.err, msgType)
		}
		m, err := fix2.Parse(ev.frame)
		if err != nil {
			p.t.Fatalf("invalid message %s: %v", visible(ev.frame), err)
		}
		if string(m.MsgType()) != msgType {
			p.t.Fatalf("got %s, want MsgType %s", visible(ev.frame), msgType)
		}
		for i := 0; i+1 < len(fields); i += 2 {
			tag, _ := strconv.Atoi(fields[i])
			if got := string(m.Get(tag)); got != fields[i+1] {
				p.t.Fatalf("got %s: tag %d is %q, want %q", visible(ev.frame), tag, got, fields[i+1])
			}
		}
		return &m
	case <-time.After(5 * time.Second):
		p.t.Fatalf("timed out waiting for MsgType %s", msgType)
	}
	return nil
}

// expectEnd waits for the connection to end.
func (p *peer) expectEnd(within time.Duration) {
	p.t.Helper()
	deadline := time.After(within)
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

// startInitiator runs an initiator dialing l with cfg changes applied.
func startInitiator(t *testing.T, l *mem.Listener, change func(*session.Config)) (*session.Session, *recorder) {
	t.Helper()
	rec := newRecorder()
	cfg := session.Config{
		ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler:       rec,
		LogoutTimeout: 200 * time.Millisecond,
	}
	if change != nil {
		change(&cfg)
	}
	if os.Getenv("FIX2_TEST_LOG") != "" {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
	s, err := i.Add(cfg, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); i.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return s, rec
}

func TestInitiator_ResendsOnRequest(t *testing.T) {
	l := mem.Listen("acceptor")
	s, rec := startInitiator(t, l, nil)
	p := acceptPeer(t, l)
	p.expect("A", "34", "1", "108", "30", "98", "0")
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	for _, id := range []string{"O1", "O2"} {
		if _, err := s.Send(context.Background(), fix2.NewBuilder(session.FIX44, "D").SetString(11, id)); err != nil {
			t.Fatal(err)
		}
	}
	p.expect("D", "34", "2", "11", "O1")
	p.expect("D", "34", "3", "11", "O2")
	p.send("2", 0, "7", "1", "16", "0")
	gf := p.expect("4", "34", "1", "43", "Y", "123", "Y", "36", "2") // our Logon, gap-filled
	if gf.Get(122) == nil {
		t.Error("GapFill without OrigSendingTime")
	}
	p.expect("D", "34", "2", "43", "Y", "11", "O1")
	p.expect("D", "34", "3", "43", "Y", "11", "O2")
	p.send("5", 0)
	p.expect("5")
	p.expectEnd(time.Second)
}

func TestInitiator_RecoversGapFromLogon(t *testing.T) {
	l := mem.Listen("acceptor")
	s, rec := startInitiator(t, l, nil)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 5, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	p.expect("2", "7", "1", "16", "0")
	p.send("4", 1, "43", "Y", "122", string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Seconds)), "123", "Y", "36", "6")
	p.seq = 6
	p.send("D", 0, "11", "IN")
	rec.wait(t, func() bool { return len(rec.apps) == 1 })
	if got := s.NextTargetSeqNum(); got != 7 {
		t.Errorf("NextTargetSeqNum = %d, want 7", got)
	}
}

func TestInitiator_LogonTimeout(t *testing.T) {
	l := mem.Listen("acceptor")
	startInitiator(t, l, func(c *session.Config) { c.LogonTimeout = 200 * time.Millisecond })
	p := acceptPeer(t, l)
	p.expect("A")
	start := time.Now()
	p.expectEnd(2 * time.Second)
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("disconnected after %v, before the logon timeout", d)
	}
	// And dials again.
	p2 := acceptPeer(t, l)
	p2.expect("A")
}

func TestInitiator_Liveness(t *testing.T) {
	l := mem.Listen("acceptor")
	_, rec := startInitiator(t, l, func(c *session.Config) { c.HeartBtInt = time.Second })
	p := acceptPeer(t, l)
	p.expect("A", "108", "1")
	p.send("A", 0, "98", "0", "108", "1")
	rec.wait(t, func() bool { return rec.logons == 1 })
	p.expect("0")                // heartbeat after 1s of silence
	p.expect("1", "112", "TEST") // TestRequest after 1.2s without a word from us
	p.expectEnd(3 * time.Second) // then disconnected
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	reason := rec.logouts[0]
	rec.mu.Unlock()
	var le *session.LogoutError
	if !errors.As(reason, &le) {
		t.Errorf("logout reason %v", reason)
	}
}

// signer signs Logons with their header fields joined by "|", or fails.
type signer struct {
	*recorder
	fail error
}

func (s signer) SignLogon(_ context.Context, _ *session.Session, h session.LogonHeader, b *fix2.Builder) error {
	if s.fail != nil {
		return s.fail
	}
	sig := "A|" + h.SenderCompID + "|" + h.TargetCompID + "|" + strconv.FormatUint(h.MsgSeqNum, 10) + "|" + h.SendingTime
	b.SetData(95, 96, []byte(sig))
	return nil
}

func TestInitiator_SignsLogon(t *testing.T) {
	l := mem.Listen("acceptor")
	startInitiator(t, l, func(c *session.Config) {
		c.Handler = signer{recorder: c.Handler.(*recorder)}
		c.ResetOnLogon = true
	})
	p := acceptPeer(t, l)
	m := p.expect("A", "34", "1", "141", "Y")
	want := "A|INI|ACC|1|" + string(m.Get(52))
	if got := string(m.Get(96)); got != want {
		t.Errorf("RawData = %q, want %q", got, want)
	}
	if got := string(m.Get(95)); got != strconv.Itoa(len(want)) {
		t.Errorf("RawDataLength = %s, want %d", got, len(want))
	}
}

func TestInitiator_SignatureFailureCancelsLogon(t *testing.T) {
	l := mem.Listen("acceptor")
	startInitiator(t, l, func(c *session.Config) {
		c.Handler = signer{recorder: c.Handler.(*recorder), fail: errors.New("no key")}
	})
	p := acceptPeer(t, l)
	p.expectEnd(2 * time.Second)
}

// logoutWatcher records the Text of Logouts its FromAdmin sees.
type logoutWatcher struct {
	*recorder
	texts chan string
}

func (w logoutWatcher) FromAdmin(_ context.Context, _ *session.Session, m *fix2.Message, _ session.Inbound) {
	if t := string(m.MsgType()); t == "5" || t == "3" {
		w.texts <- t + ":" + string(m.Get(58))
	}
}

func TestInitiator_LogonRefusedByLogout(t *testing.T) {
	l := mem.Listen("acceptor")
	texts := make(chan string, 1)
	startInitiator(t, l, func(c *session.Config) {
		c.Handler = logoutWatcher{recorder: c.Handler.(*recorder), texts: texts}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("5", 0, "58", "Invalid signature")
	p.expectEnd(time.Second)
	select {
	case got := <-texts:
		if got != "5:Invalid signature" {
			t.Errorf("FromAdmin got Logout Text %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FromAdmin did not see the Logout")
	}
}

// As Binance does: a Reject of the Logon, then a Logout.
func TestInitiator_LogonRejected(t *testing.T) {
	l := mem.Listen("acceptor")
	texts := make(chan string, 2)
	startInitiator(t, l, func(c *session.Config) {
		c.Handler = logoutWatcher{recorder: c.Handler.(*recorder), texts: texts}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("3", 0, "45", "1", "372", "A", "373", "8", "58", "API-key format invalid.")
	p.expectEnd(time.Second)
	select {
	case got := <-texts:
		if got != "3:API-key format invalid." {
			t.Errorf("FromAdmin got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FromAdmin did not see the Reject")
	}
}
