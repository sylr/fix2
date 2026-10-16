package session_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store/memstore"
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

// startInitiator runs an initiator dialing d with cfg changes applied.
func startInitiator(t *testing.T, d transport.Dialer, change func(*session.Config)) (*session.Session, *recorder) {
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
	s, err := i.Add(cfg, d)
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

// blockingSigner signs once release is closed.
type blockingSigner struct {
	*recorder
	signing chan struct{}
	release chan struct{}
}

func (s blockingSigner) SignLogon(_ context.Context, _ *session.Session, _ session.LogonHeader, b *fix2.Builder) error {
	close(s.signing)
	<-s.release
	b.SetData(95, 96, []byte("sig"))
	return nil
}

// A window ending while the Logon is signed disconnects: no Logout goes
// out before the Logon.
func TestInitiator_WindowEndsWhileSigning(t *testing.T) {
	l := mem.Listen("acceptor")
	now := time.Now()
	signing, release := make(chan struct{}), make(chan struct{})
	startInitiator(t, l, func(c *session.Config) {
		c.Handler = blockingSigner{recorder: c.Handler.(*recorder), signing: signing, release: release}
		c.Schedule = windows{{Start: now, End: now.Add(300 * time.Millisecond), ID: "w"}}
	})
	p := acceptPeer(t, l)
	<-signing
	time.Sleep(500 * time.Millisecond) // the window ends
	close(release)
	select {
	case ev := <-p.frames:
		if ev.err == nil {
			t.Fatalf("sent %s, want a disconnection", visible(ev.frame))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still connected after the window ended")
	}
}

// blockingLogon blocks OnLogon until release is closed.
type blockingLogon struct {
	*recorder
	release chan struct{}
}

func (b blockingLogon) OnLogon(context.Context, *session.Session) { <-b.release }

// A Logon counts as processed at once: an OnLogon still running does not
// hold back LastMsgSeqNumProcessed (369).
func TestInitiator_LastProcessedWhileOnLogonRuns(t *testing.T) {
	l := mem.Listen("acceptor")
	release := make(chan struct{})
	defer close(release)
	startInitiator(t, l, func(c *session.Config) {
		c.Handler = blockingLogon{recorder: c.Handler.(*recorder), release: release}
		c.LastMsgSeqNumProcessed = true
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	p.send("1", 0, "112", "PING")
	p.expect("0", "112", "PING", "369", "2")
}

// Logged out by us, the session reports why, not the Text of the
// counterparty's answer, which usually has none.
func TestLogoutReasonIsOurs(t *testing.T) {
	l := mem.Listen("acceptor")
	ini, rec := startInitiator(t, l, nil)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	done := make(chan error, 1)
	go func() { done <- ini.Logout(context.Background(), "end of day") }()
	p.expect("5", "58", "end of day")
	p.send("5", 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	reason := rec.logouts[0]
	rec.mu.Unlock()
	var le *session.LogoutError
	if !errors.As(reason, &le) || le.ByCounterparty || le.Text != "end of day" {
		t.Errorf("logout reason %v, want ours: end of day", reason)
	}
}

// A counterparty hanging up rather than answering our Logout answers it
// too: the session reports why it logged out.
func TestLogoutReasonOnHangUp(t *testing.T) {
	l := mem.Listen("acceptor")
	ini, rec := startInitiator(t, l, nil)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	done := make(chan error, 1)
	go func() { done <- ini.Logout(context.Background(), "end of day") }()
	p.expect("5", "58", "end of day")
	p.conn.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	rec.mu.Lock()
	reason := rec.logouts[0]
	rec.mu.Unlock()
	var le *session.LogoutError
	if !errors.As(reason, &le) || le.ByCounterparty || le.Text != "end of day" || le.Err == nil {
		t.Errorf("logout reason %v, want ours, end of day, and the hang up", reason)
	}
}

// The counterparty's Logout is answered however long writing the answer
// takes, longer than LogoutTimeout or past a heartbeat due: answering it,
// the session waits for no Logout reply, and sends nothing after it.
func TestLogoutAnsweredOnSlowWrite(t *testing.T) {
	for _, tc := range []struct {
		name           string
		heartBt, write time.Duration
	}{
		{name: "beyond LogoutTimeout", heartBt: 30 * time.Second, write: 300 * time.Millisecond},
		// Longer than the heartbeat interval: one falls due while our
		// Logout is written, whatever was sent before it.
		{name: "past a heartbeat", heartBt: time.Second, write: 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSyncStore()
			l := mem.Listen("acceptor")
			_, rec := startInitiator(t, l, func(c *session.Config) { c.Store = st; c.HeartBtInt = tc.heartBt })
			p := acceptPeer(t, l)
			p.expect("A")
			p.send("A", 0, "98", "0", "108", strconv.Itoa(int(tc.heartBt/time.Second)))
			rec.wait(t, func() bool { return rec.logons == 1 })
			st.delay.Store(int64(tc.write))
			p.send("5", 0)
			// Heartbeats before our Logout are due: none after it.
			for _, m := range p.until("5") {
				if typ := string(m.MsgType()); typ != "0" && typ != "5" {
					t.Fatalf("got MsgType %s before our Logout", typ)
				}
			}
			p.expectSilentEnd()
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
			rec.mu.Lock()
			reason := rec.logouts[0]
			rec.mu.Unlock()
			var le *session.LogoutError
			if !errors.As(reason, &le) || !le.ByCounterparty {
				t.Errorf("logout reason %v, want the counterparty's", reason)
			}
		})
	}
}

// syncGate is a store whose Sync, once block is set, tells entered and
// waits for release: what the writer is about to write waits.
type syncGate struct {
	*memstore.Store
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *syncGate) Sync(ctx context.Context, _, _ uint64) error {
	if g.block.Load() {
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// The answer to the counterparty's Logout is the last message sent: what
// the counterparty sends while it waits to be written goes unanswered, and
// uses up no MsgSeqNum, a ResendRequest included.
func TestLogoutAnswerIsLast(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(*peer)
	}{
		{"nothing more", nil},
		{"TestRequest", func(p *peer) { p.send("1", 0, "112", "PING") }},
		{"Reject", func(p *peer) { p.send("1", 0) }},
		{"ResendRequest", func(p *peer) { p.send("2", 0, "7", "1", "16", "0") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &syncGate{Store: memstore.New(), entered: make(chan struct{}, 1), release: make(chan struct{})}
			buf := &logBuffer{}
			l := mem.Listen("acceptor")
			s, rec := startInitiator(t, l, func(c *session.Config) {
				c.Store = st
				c.Logger = slog.New(slog.NewTextHandler(buf, nil))
			})
			// Before the cleanup above, which would wait for the Sync.
			release := sync.OnceFunc(func() { close(st.release) })
			t.Cleanup(release)
			p := acceptPeer(t, l)
			p.expect("A")
			p.send("A", 0, "98", "0", "108", "30")
			rec.wait(t, func() bool { return rec.logons == 1 })
			st.block.Store(true)
			p.send("5", 0)
			select {
			case <-st.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("our Logout not written")
			}
			if tc.send != nil {
				tc.send(p)
				waitFor(t, func() bool { return strings.Contains(buf.String(), "after the counterparty's Logout ignored") })
			}
			st.block.Store(false)
			release()
			p.expect("5", "34", "2")
			p.expectSilentEnd()
			if n := s.NextSenderSeqNum(); n != 3 {
				t.Errorf("NextSenderSeqNum = %d, want 3: nothing after our Logout", n)
			}
		})
	}
}

// A replay asked for before the counterparty's Logout, waiting for the
// writer, is still served: our answer, held back behind it, comes last.
func TestLogoutAnswerAfterReplayAskedBefore(t *testing.T) {
	st := &syncGate{Store: memstore.New(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	l := mem.Listen("acceptor")
	s, rec := startInitiator(t, l, func(c *session.Config) { c.Store = st })
	release := sync.OnceFunc(func() { close(st.release) })
	t.Cleanup(release)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	st.block.Store(true)
	if _, err := s.Send(context.Background(), order("O1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("order not written")
	}
	p.send("2", 0, "7", "2", "16", "0")
	p.send("5", 0)
	waitFor(t, func() bool { return s.NextTargetSeqNum() == 4 })
	st.block.Store(false)
	release()
	p.expect("D", "34", "2", "11", "O1")
	p.expect("D", "34", "2", "43", "Y", "11", "O1")
	p.expect("5", "34", "3")
	p.expectSilentEnd()
	if n := s.NextSenderSeqNum(); n != 4 {
		t.Errorf("NextSenderSeqNum = %d, want 4", n)
	}
}

// A counterparty's Logout is its last message: what it sends while our answer
// waits to be written, or after its answer to ours, is not handled, a message
// with a malformed tag included, nor counted as processed.
func TestLogoutIsLastHandled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ours      bool // we log out first
		malformed bool
	}{
		{"counterparty's Logout", false, false},
		{"counterparty's Logout, malformed after", false, true},
		{"answer to ours", true, false},
		{"answer to ours, malformed after", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &syncGate{Store: memstore.New(), entered: make(chan struct{}, 1), release: make(chan struct{})}
			buf := &logBuffer{}
			l := mem.Listen("acceptor")
			s, rec := startInitiator(t, l, func(c *session.Config) {
				c.Store = st
				c.Logger = slog.New(slog.NewTextHandler(buf, nil))
			})
			release := sync.OnceFunc(func() { close(st.release) })
			t.Cleanup(release)
			p := acceptPeer(t, l)
			p.expect("A")
			p.send("A", 0, "98", "0", "108", "30")
			rec.wait(t, func() bool { return rec.logons == 1 })
			// Our Logout, or our answer to the counterparty's, waits to be
			// written.
			st.block.Store(true)
			done := make(chan error, 1)
			if tc.ours {
				go func() { done <- s.Logout(context.Background(), "end of day") }()
			} else {
				p.send("5", 0)
			}
			select {
			case <-st.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("our Logout not written")
			}
			if tc.ours {
				p.send("5", 0)
			}
			body := "35=D|49=ACC|56=INI|34=3|52=" + sendingTime() + "|11=AFTER-LOGOUT|"
			if tc.malformed {
				body += "abc=1|"
			}
			p.writeRaw(rawFrame(body, ""))
			waitFor(t, func() bool { return strings.Contains(buf.String(), "after the counterparty's Logout ignored") })
			st.block.Store(false)
			release()
			p.expect("5", "34", "2")
			p.expectSilentEnd()
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
			rec.mu.Lock()
			apps := append([]string(nil), rec.apps...)
			rec.mu.Unlock()
			if len(apps) != 0 {
				t.Errorf("delivered %q after the Logout, want none", apps)
			}
			if n := s.NextTargetSeqNum(); n != 3 {
				t.Errorf("NextTargetSeqNum = %d, want 3", n)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				state, err := st.Store.Open(context.Background())
				if err == nil && state.NextTarget == 3 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("stored NextTarget = %d (%v), want 3", state.NextTarget, err)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
