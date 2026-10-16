package session_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// waitingRangeStore makes Range, once wait is set, wait for its ctx before
// it passes on message at, as for a connection of a pool the application
// holds, then read on all the same, as a store that does not check ctx
// between messages; or, if at is 0, once read, before it returns, as a
// store whose read ends as the connection does. It reports the messages of
// holes as beyond retention.
type waitingRangeStore struct {
	*memstore.Store
	at      uint64
	holes   map[uint64]bool
	wait    atomic.Bool
	late    atomic.Int32 // messages passed on once ctx was done
	waiting chan struct{}
	release chan struct{} // closed when the test ends, Range never cancelled
}

func newWaitingRangeStore(at uint64) *waitingRangeStore {
	return &waitingRangeStore{Store: memstore.New(), at: at, waiting: make(chan struct{}, 1), release: make(chan struct{})}
}

func (w *waitingRangeStore) Range(ctx context.Context, gen, begin, end uint64, fn func(uint64, []byte, store.Hole) error) error {
	if !w.wait.Load() {
		return w.Store.Range(ctx, gen, begin, end, fn)
	}
	err := w.Store.Range(context.Background(), gen, begin, end, func(seq uint64, raw []byte, hole store.Hole) error {
		if seq == w.at {
			w.block(ctx)
		}
		if ctx.Err() != nil {
			w.late.Add(1)
		}
		if w.holes[seq] {
			raw, hole = nil, store.HoleRetention
		}
		return fn(seq, raw, hole)
	})
	if w.at == 0 {
		w.block(ctx)
	}
	return err
}

// block waits for ctx, telling waitRange.
func (w *waitingRangeStore) block(ctx context.Context) {
	w.waiting <- struct{}{}
	select {
	case <-ctx.Done():
	case <-w.release:
	}
}

// waitRange waits for a Range to wait for its ctx.
func (w *waitingRangeStore) waitRange(t *testing.T) {
	t.Helper()
	select {
	case <-w.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("no Range called")
	}
}

// endDialer dials l with connections counting what is written to them once
// the session closed them.
type endDialer struct {
	l    *mem.Listener
	late atomic.Int32
}

func (d *endDialer) Dial(ctx context.Context) (transport.Conn, error) {
	tc, err := d.l.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return &endConn{Conn: tc, d: d}, nil
}

type endConn struct {
	transport.Conn
	d      *endDialer
	closed atomic.Bool
}

func (c *endConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func (c *endConn) Write(b []byte) (int, error) {
	if c.closed.Load() {
		c.d.late.Add(1)
	}
	return c.Conn.Write(b)
}

// waitingReplay is an initiator session logged on through an endDialer
// with orders sent, MsgSeqNum 2 on, whose peer asked for them again, from
// the Logon: the replay waits in Range.
type waitingReplay struct {
	s        *session.Session
	h        *lossRecorder
	p        *peer
	d        *endDialer
	st       *waitingRangeStore
	receipts []session.Receipt  // of the orders
	cancel   context.CancelFunc // cancels Run
	ran      chan struct{}      // closed once Run returned
}

func replayWaiting(t *testing.T, st *waitingRangeStore, orders ...*fix2.Builder) *waitingReplay {
	t.Helper()
	w := &waitingReplay{h: &lossRecorder{recorder: newRecorder()}, d: &endDialer{l: mem.Listen("acceptor")}, st: st, ran: make(chan struct{})}
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
	var err error
	w.s, err = i.Add(session.Config{
		ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler:       w.h,
		Store:         st,
		LogoutTimeout: 200 * time.Millisecond,
	}, w.d)
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	ctx, w.cancel = context.WithCancel(context.Background())
	go func() { defer close(w.ran); i.Run(ctx) }()
	t.Cleanup(func() { w.cancel(); <-w.ran })
	// Before the cleanup above, which would wait for a Range never
	// cancelled.
	t.Cleanup(func() { close(st.release) })
	w.p = acceptPeer(t, w.d.l)
	w.p.expect("A")
	w.p.send("A", 0, "98", "0", "108", "30")
	w.h.wait(t, func() bool { return w.h.logons == 1 })
	for i, o := range orders {
		r, err := w.s.Send(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		w.receipts = append(w.receipts, r)
		w.p.expect("D", "34", fmt.Sprint(i+2))
	}
	st.wait.Store(true)
	w.p.send("2", 0, "7", "1", "16", "0")
	st.waitRange(t)
	return w
}

// checkRetired checks that the replay its connection ended wrote nothing to
// it once ended, resent and gap-filled no more than written before, and
// reported lost only runs a GapFill written covered; that Range passed it
// at most one message once cancelled, which it refused; and that nothing
// halted. The losses reported are complete once the handler is called after
// them, as for the next Logon.
func (w *waitingReplay) checkRetired(t *testing.T, resent, gapFilled uint64, losses ...[2]uint64) {
	t.Helper()
	if n := w.d.late.Load(); n != 0 {
		t.Errorf("%d writes to the connection closed", n)
	}
	if n := w.st.late.Load(); n > 1 {
		t.Errorf("%d messages passed on once the read was cancelled, want 1 at most", n)
	}
	if n, g := w.s.Stats().Resent.Load(), w.s.Stats().GapFilled.Load(); n != resent || g != gapFilled {
		t.Errorf("Resent = %d, GapFilled = %d, want %d and %d", n, g, resent, gapFilled)
	}
	w.h.mu.Lock()
	var got [][2]uint64
	for _, l := range w.h.losses {
		got = append(got, [2]uint64{l.begin, l.end})
	}
	w.h.mu.Unlock()
	if !slices.Equal(got, losses) {
		t.Errorf("OnRecoveryLoss told %v, want %v", got, losses)
	}
	if err := w.s.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

// A replay waiting in Range is cancelled when its connection ends: the
// session reconnects, and resends on the next connection what it is asked
// for, having written nothing to the connection ended and reported nothing
// lost, nor what it read before the connection ended.
func TestReplayCancelledByDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   uint64
	}{
		{"waiting to read", 1},
		{"waiting once read", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newWaitingRangeStore(tc.at)
			w := replayWaiting(t, st, order("O1"))
			st.wait.Store(false)
			w.p.conn.Close()
			w.h.wait(t, func() bool { return len(w.h.logouts) == 1 })

			p := acceptPeer(t, w.d.l)
			p.seq = w.p.seq
			p.expect("A", "34", "3")
			p.send("A", 0, "98", "0", "108", "30")
			w.h.wait(t, func() bool { return w.h.logons == 2 })
			w.checkRetired(t, 0, 0)
			p.send("2", 0, "7", "1", "16", "0")
			p.expect("4", "34", "1", "36", "2", "123", "Y")
			p.expect("D", "34", "2", "43", "Y", "11", "O1")
		})
	}
}

// A replay cancelled after it wrote a chunk accounts for that chunk alone:
// the messages it resent and gap-filled, and the loss of those beyond
// retention a GapFill in it covered; not for what it read after.
func TestReplayCancelledAfterChunk(t *testing.T) {
	// The Logon, then O2 and O7 beyond retention, O3 to O6 filling more
	// than a chunk: the replay waits at O7.
	st := newWaitingRangeStore(7)
	st.holes = map[uint64]bool{2: true, 7: true}
	big := strings.Repeat("x", 20<<10)
	w := replayWaiting(t, st, order("O2"),
		order("O3").SetString(58, big), order("O4").SetString(58, big), order("O5").SetString(58, big), order("O6").SetString(58, big),
		order("O7"), order("O8"))
	w.p.expect("4", "34", "1", "36", "3", "123", "Y")
	for seq := 3; seq <= 6; seq++ {
		w.p.expect("D", "34", fmt.Sprint(seq), "43", "Y")
	}
	w.p.conn.Close()

	p := acceptPeer(t, w.d.l)
	p.seq = w.p.seq
	p.expect("A", "34", "9")
	p.send("A", 0, "98", "0", "108", "30")
	w.h.wait(t, func() bool { return w.h.logons == 2 })
	w.checkRetired(t, 4, 2, [2]uint64{2, 2})
	if err := w.receipts[0].Err(); !errors.Is(err, session.ErrSkipped) {
		t.Errorf("O2, gap-filled: Err = %v, want %v", err, session.ErrSkipped)
	}
	for _, r := range w.receipts[1:] {
		if err := r.Err(); err != nil {
			t.Errorf("an order resent or not read: Err = %v, want nil", err)
		}
	}
}

// A replay waiting in Range holds back neither Stop nor Run: the end of its
// connection, the Logout timing out, cancels it.
func TestReplayCancelledByStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*waitingReplay) error
	}{
		{"Stop", func(w *waitingReplay) error { return w.s.Stop(context.Background()) }},
		{"Run cancelled", func(w *waitingReplay) error { w.cancel(); <-w.ran; return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := replayWaiting(t, newWaitingRangeStore(1), order("O1"))
			stopped := make(chan error, 1)
			go func() { stopped <- tc.stop(w) }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Errorf("Stop = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stopping waited for the replay")
			}
			waitDone(t, w.s)
			w.cancel()
			select {
			case <-w.ran:
			case <-time.After(5 * time.Second):
				t.Fatal("Run waited for the replay")
			}
			w.checkRetired(t, 0, 0)
		})
	}
}

// A replay waiting in Range holds back our answer to the counterparty's
// Logout, or our Logout crossing its, LogoutTimeout at most: the connection
// then ends, which cancels the replay, and the session tells why. Nothing
// more is written, nothing reported lost, and the next connection resends
// what it is asked for.
func TestReplayWaitingHoldsBackLogout(t *testing.T) {
	for _, crossing := range []bool{false, true} {
		name := "counterparty's Logout"
		if crossing {
			name = "crossing Logouts"
		}
		t.Run(name, func(t *testing.T) {
			st := newWaitingRangeStore(1)
			w := replayWaiting(t, st, order("O1"))
			done := make(chan error, 1)
			if crossing {
				go func() { done <- w.s.Logout(context.Background(), "end of day") }()
				waitFor(t, func() bool { return w.s.State() == session.StateLoggingOut })
				w.p.send("5", 0)
			} else {
				w.p.send("5", 0, "58", "bye")
				waitFor(t, func() bool { return w.s.State() == session.StateLoggingOut })
				go func() { done <- w.s.Stop(context.Background()) }()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("logging out: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("logging out waited for the replay")
			}
			w.h.wait(t, func() bool { return len(w.h.logouts) == 1 })
			w.h.mu.Lock()
			reason := w.h.logouts[0]
			w.h.mu.Unlock()
			var le *session.LogoutError
			if !errors.As(reason, &le) || le.ByCounterparty == crossing || le.Err == nil || !strings.Contains(le.Err.Error(), "held back behind a resend") {
				t.Errorf("logout reason %v, want the Logout's, held back behind the resend", reason)
			}
			if !crossing {
				waitDone(t, w.s)
				w.checkRetired(t, 0, 0)
				return
			}
			st.wait.Store(false)
			p := acceptPeer(t, w.d.l)
			p.seq = w.p.seq
			p.expect("A", "34", "4")
			p.send("A", 0, "98", "0", "108", "30")
			w.h.wait(t, func() bool { return w.h.logons == 2 })
			w.checkRetired(t, 0, 0)
			p.send("2", 0, "7", "1", "16", "0")
			p.expect("4", "34", "1", "36", "2", "123", "Y")
			p.expect("D", "34", "2", "43", "Y", "11", "O1")
		})
	}
}

// blockingResend is a lossRecorder whose OnResend, once block is set, tells
// entered and waits for release: the writer goroutine, preempted after it
// checked the connection.
type blockingResend struct {
	*lossRecorder
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (h *blockingResend) OnResend(context.Context, *session.Session, uint64, *fix2.Message) bool {
	if h.block.Load() {
		h.entered <- struct{}{}
		<-h.release
	}
	return true
}

// A replay whose connection ends while OnResend runs writes nothing to it
// once ended: not the chunk the message it goes on to rewrite fills.
func TestReplayCancelledInOnResend(t *testing.T) {
	h := &blockingResend{lossRecorder: &lossRecorder{recorder: newRecorder()}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	w := &waitingReplay{h: h.lossRecorder, d: &endDialer{l: mem.Listen("acceptor")}, st: newWaitingRangeStore(0), ran: make(chan struct{})}
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
	var err error
	w.s, err = i.Add(session.Config{
		ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler:       h,
		Store:         w.st,
		LogoutTimeout: 200 * time.Millisecond,
	}, w.d)
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	ctx, w.cancel = context.WithCancel(context.Background())
	go func() { defer close(w.ran); i.Run(ctx) }()
	t.Cleanup(func() { w.cancel(); <-w.ran })
	// Before the cleanup above, which would wait for OnResend.
	release := sync.OnceFunc(func() { close(h.release) })
	t.Cleanup(release)
	w.p = acceptPeer(t, w.d.l)
	w.p.expect("A")
	w.p.send("A", 0, "98", "0", "108", "30")
	w.h.wait(t, func() bool { return w.h.logons == 1 })
	// More than a chunk on its own: written as soon as rewritten.
	if _, err := w.s.Send(context.Background(), order("O1").SetString(58, strings.Repeat("x", 70<<10))); err != nil {
		t.Fatal(err)
	}
	w.p.expect("D", "34", "2")
	h.block.Store(true)
	w.p.send("2", 0, "7", "1", "16", "0")
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("OnResend not called")
	}
	w.p.conn.Close()
	w.h.wait(t, func() bool { return len(w.h.logouts) == 1 })
	h.block.Store(false)
	release()

	// The next connection waits for the replay of the one ended.
	p := acceptPeer(t, w.d.l)
	p.seq = w.p.seq
	p.expect("A", "34", "3")
	p.send("A", 0, "98", "0", "108", "30")
	w.h.wait(t, func() bool { return w.h.logons == 2 })
	w.checkRetired(t, 0, 0)
}

// A Logon with ResetSeqNumFlag received once a stalled handler's Logout is
// queued, the phase still established, is ignored as while logging out: a
// reset would drop that Logout and the replay holding it back, and escape
// the close deadline.
func TestStallLogoutIgnoresResetLogon(t *testing.T) {
	st := newWaitingRangeStore(1)
	h := &stuck{recorder: newRecorder(), release: make(chan struct{})}
	l := mem.Listen("acceptor")
	s, _ := startInitiator(t, l, func(c *session.Config) {
		c.Store, c.Handler, c.DeliveryQueue = st, h, 1
		c.MaxDeliveryStall, c.LogoutTimeout = 50*time.Millisecond, 300*time.Millisecond
	})
	release := sync.OnceFunc(func() { close(h.release) })
	t.Cleanup(release)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	h.wait(t, func() bool { return h.logons == 1 })
	if _, err := s.Send(context.Background(), order("O1")); err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	st.wait.Store(true)
	p.send("2", 0, "7", "1", "16", "0")
	st.waitRange(t)
	for i := range 5 {
		p.send("D", 0, "11", strconv.Itoa(i))
	}
	waitFor(t, func() bool { return s.State() == session.StateStalled })
	// The reader, room made, reads the reset.
	release()
	p.send("A", 1, "98", "0", "108", "30", "141", "Y")
	p.expectSilentEnd()
	if s.State() == session.StateResettingSeqs {
		t.Error("reset accepted after our Logout")
	}
	state, err := st.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 1 {
		t.Errorf("store generation %d, want 1: reset after our Logout", state.Generation)
	}
}
