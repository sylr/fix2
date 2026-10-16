package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// syncStore is a store whose Appends are durable only once Sync returns:
// the memstore it wraps holds what would survive a crash.
type syncStore struct {
	*memstore.Store

	mu      sync.Mutex
	pending []pendingAppend
	syncErr error
	syncs   int
	atClose uint64 // the durable NextSender when the session closed it

	// hang makes Sync wait for its context to end, or for release, and
	// return hangErr then, the context's error if nil. hung counts the
	// Syncs that did, hungFor is how long the last one waited. delay makes
	// every Sync take that long first.
	hang    atomic.Bool
	hangErr error
	release chan struct{}
	hung    atomic.Int32
	hungFor atomic.Int64
	delay   atomic.Int64
}

type pendingAppend struct {
	gen, seq uint64
	raw      []byte
}

var _ store.Syncer = (*syncStore)(nil)

func newSyncStore() *syncStore {
	return &syncStore{Store: memstore.New(), release: make(chan struct{})}
}

func (b *syncStore) Append(_ context.Context, gen, seq uint64, raw []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, pendingAppend{gen, seq, bytes.Clone(raw)})
	return nil
}

func (b *syncStore) Sync(ctx context.Context, gen, upTo uint64) error {
	if d := time.Duration(b.delay.Load()); d > 0 {
		time.Sleep(d)
	}
	if b.hang.Load() {
		start := time.Now()
		b.hung.Add(1)
		defer func() { b.hungFor.Store(int64(time.Since(start))) }()
		select {
		case <-ctx.Done():
			if b.hangErr != nil {
				return b.hangErr
			}
			return ctx.Err()
		case <-b.release:
			return errors.New("released")
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.syncs++
	if b.syncErr != nil {
		return b.syncErr
	}
	rest := b.pending[:0]
	for _, p := range b.pending {
		if p.gen != gen || p.seq > upTo {
			rest = append(rest, p)
			continue
		}
		if err := b.Store.Append(ctx, p.gen, p.seq, p.raw); err != nil {
			return err
		}
	}
	b.pending = rest
	return nil
}

// Close keeps the durable state, for the test to read.
func (b *syncStore) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.atClose = b.durable()
	return nil
}

// durable returns the durable NextSender.
func (b *syncStore) durable() uint64 {
	st, _ := b.Store.Open(context.Background())
	return st.NextSender
}

func (b *syncStore) failSyncs(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.syncErr = err
}

func (b *syncStore) syncCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.syncs
}

func (b *syncStore) closedAt() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.atClose
}

// durableDialer dials l with connections checking, as each write is made,
// that every message it carries is already durable in st.
type durableDialer struct {
	l     *mem.Listener
	st    *syncStore
	once  bool // a single connection: no reconnection
	dials atomic.Int32

	mu  sync.Mutex
	bad []string
}

func (d *durableDialer) Dial(ctx context.Context) (transport.Conn, error) {
	if d.dials.Add(1) > 1 && d.once {
		return nil, errors.New("no more connections")
	}
	tc, err := d.l.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return durableConn{tc, d}, nil
}

type durableConn struct {
	transport.Conn
	d *durableDialer
}

func (c durableConn) Write(b []byte) (int, error) {
	durable := c.d.st.durable()
	f := fix.NewFramer(bytes.NewReader(b), fix.FramerOptions{})
	for {
		frame, err := f.Next()
		if err != nil {
			break
		}
		m, err := fix.Parse(frame)
		if err != nil {
			continue
		}
		seq, _ := fix.ParseInt(m.Get(34))
		last := uint64(seq)
		if string(m.MsgType()) == "4" {
			// A GapFill stands for the messages up to NewSeqNo-1.
			if v, err := fix.ParseInt(m.Get(36)); err == nil && v > 0 {
				last = uint64(v) - 1
			}
		}
		if last >= durable {
			c.d.mu.Lock()
			c.d.bad = append(c.d.bad, fmt.Sprintf("%s written while only messages below %d are durable", visible(frame), durable))
			c.d.mu.Unlock()
		}
	}
	return c.Conn.Write(b)
}

// verify fails t if a message was written before it was durable.
func (d *durableDialer) verify(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.bad {
		t.Error(b)
	}
}

// logOn runs an initiator dialing d, and logs it on to a peer accepted on l.
func logOn(t *testing.T, d transport.Dialer, l *mem.Listener, change func(*session.Config)) (*session.Session, *recorder, *peer) {
	t.Helper()
	s, rec := startInitiator(t, d, change)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	return s, rec, p
}

// syncedSession returns an initiator session using st, logged on to a peer
// through a durableDialer.
func syncedSession(t *testing.T, st *syncStore, once bool) (*session.Session, *recorder, *peer, *durableDialer) {
	t.Helper()
	d := &durableDialer{l: mem.Listen("acceptor"), st: st, once: once}
	s, rec, p := logOn(t, d, d.l, func(c *session.Config) { c.Store = st })
	return s, rec, p, d
}

// dialPeer connects a scripted counterparty to an acceptor listening on l.
func dialPeer(t *testing.T, l *mem.Listener) *peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := l.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{t: t, conn: conn, frames: make(chan event, 100), seq: 1}
	go func() {
		f := fix.NewFramer(conn, fix.FramerOptions{})
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

// expectSilentEnd waits for the connection to end with nothing more
// written.
func (p *peer) expectSilentEnd() {
	p.t.Helper()
	select {
	case ev := <-p.frames:
		if ev.err == nil {
			p.t.Fatalf("got %s, want the connection closed", visible(ev.frame))
		}
	case <-time.After(5 * time.Second):
		p.t.Fatal("connection still open")
	}
}

// sendOrders writes n orders in a single write: a burst the session reads
// at once.
func (p *peer) sendOrders(n int) {
	p.t.Helper()
	var buf []byte
	for i := range n {
		b := fix.NewBuilder(session.FIX44, "D").
			SetString(49, "ACC").SetString(56, "INI").SetInt(34, p.seq).
			SetString(52, string(fix.AppendUTCTimestamp(nil, time.Now(), fix.Milliseconds))).
			SetString(11, fmt.Sprintf("IN%d", i))
		p.seq++
		buf = append(buf, b.Build()...)
	}
	if _, err := p.conn.Write(buf); err != nil {
		p.t.Fatal(err)
	}
}

// expectNoRedial checks that the initiator dialing l does not connect
// again: its session is halted. A negative, it waits a fixed while, a few
// times the initiator's longest reconnection wait.
func expectNoRedial(t *testing.T, l *mem.Listener) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if c, err := l.Accept(ctx); err == nil {
		c.Close()
		t.Error("the halted session connected again")
	}
}

// expectHalted checks that s halted with an error wrapping want.
func expectHalted(t *testing.T, s *session.Session, want error) {
	t.Helper()
	if err := s.Err(); !errors.Is(err, want) {
		t.Errorf("Err = %v, want one wrapping %v", err, want)
	}
	if st := s.State(); st != session.StateStopped {
		t.Errorf("State = %s, want stopped", st)
	}
}

// Every message, Logon included, is durable before it is written, whatever
// the batches the writer makes.
func TestStoreSyncedBeforeWrite(t *testing.T) {
	st := newSyncStore()
	s, _, p, d := syncedSession(t, st, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const n = 50
	rs := make([]session.Receipt, n)
	for i := range rs {
		r, err := s.Send(ctx, order(fmt.Sprintf("O%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		rs[i] = r
	}
	for _, r := range rs {
		if err := r.Wait(ctx, session.Written); err != nil {
			t.Fatal(err)
		}
	}
	for i := range n {
		p.expect("D", "11", fmt.Sprintf("O%d", i))
	}
	d.verify(t)
	if got, want := st.durable(), s.NextSenderSeqNum(); got != want {
		t.Errorf("durable NextSender = %d, want %d", got, want)
	}
	if syncs, sent := st.syncCount(), int(s.NextSenderSeqNum()-1); syncs == 0 || syncs > sent {
		t.Errorf("%d Syncs for %d messages, want at least one and at most one a message", syncs, sent)
	}
}

// A Sync failing keeps its batch from being written: the connection drops,
// and the session reconnects unless the outcome is uncertain, which halts
// it.
func TestStoreSyncFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		halt bool
	}{
		{"error", errors.New("disk busy"), false},
		{"unknown outcome", store.ErrUnknownOutcome, true},
		{"generation gone", store.ErrGenerationGone, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSyncStore()
			s, rec, p, d := syncedSession(t, st, false)
			st.failSyncs(tc.err)
			r, err := s.Send(context.Background(), order("O1"))
			if err != nil {
				t.Fatal(err)
			}
			p.expectSilentEnd()
			rec.wait(t, func() bool { return len(rec.logouts) == 1 })
			wctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := r.Wait(wctx, session.Written); err == nil {
				t.Error("Written although never written")
			}
			if tc.halt {
				expectHalted(t, s, tc.err)
				expectNoRedial(t, d.l)
				return
			}
			if err := s.Err(); err != nil {
				t.Errorf("Err = %v, want nil: the next Sync covers the message again", err)
			}
			st.failSyncs(nil)
			p2 := acceptPeer(t, d.l)
			p2.expect("A", "34", "3")
			d.verify(t)
		})
	}
}

// A replay syncs what it resends first; it is not written if Sync fails.
func TestStoreSyncReplay(t *testing.T) {
	st := newSyncStore()
	s, _, p, d := syncedSession(t, st, false)
	if _, err := s.Send(context.Background(), order("O1")); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "2")
	p.send("2", 0, "7", "2", "16", "0")
	p.expect("D", "34", "2", "43", "Y")
	before := st.syncCount()
	st.failSyncs(errors.New("disk busy"))
	p.send("2", 0, "7", "2", "16", "0")
	p.expectSilentEnd()
	if st.syncCount() == before {
		t.Error("the replay did not Sync")
	}
	d.verify(t)
}

// Stop syncs what was stored but not yet synced before closing the store,
// and reports a Sync failure.
func TestStoreSyncOnStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"synced", nil},
		{"sync fails", errors.New("disk gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSyncStore()
			s, _, p, _ := syncedSession(t, st, true)
			st.failSyncs(errors.New("disk busy"))
			if _, err := s.Send(context.Background(), order("O1")); err != nil {
				t.Fatal(err)
			}
			p.expectSilentEnd()
			st.failSyncs(tc.err)
			err := s.Stop(context.Background())
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Errorf("Stop = %v, want the Sync error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, want := st.closedAt(), s.NextSenderSeqNum(); got != want {
				t.Errorf("durable NextSender when closed = %d, want %d", got, want)
			}
		})
	}
}

// plainStore hides the memstore's Syncer.
type plainStore struct{ store.Store }

// A store without Syncer works as before.
func TestStoreWithoutSyncer(t *testing.T) {
	s, _, p := loggedOn(t, func(c *session.Config) { c.Store = plainStore{memstore.New()} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range []string{"O1", "O2", "O3"} {
		r, err := s.Send(ctx, order(id))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Wait(ctx, session.Written); err != nil {
			t.Fatal(err)
		}
		p.expect("D", "11", id)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// targetStore makes SetNextTarget fail on demand, and records what it set.
type targetStore struct {
	*memstore.Store
	fail atomic.Bool
	err  error

	mu   sync.Mutex
	sets []uint64
}

func (f *targetStore) SetNextTarget(ctx context.Context, gen, next uint64) error {
	if f.fail.Load() {
		return f.err
	}
	f.mu.Lock()
	f.sets = append(f.sets, next)
	f.mu.Unlock()
	return f.Store.SetNextTarget(ctx, gen, next)
}

func (f *targetStore) persisted() uint64 {
	st, _ := f.Store.Open(context.Background())
	return st.NextTarget
}

// countLog counts the records with message msg.
type countLog struct {
	msg string
	n   *atomic.Int64
}

func (c countLog) Enabled(context.Context, slog.Level) bool { return true }
func (c countLog) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c countLog) WithGroup(string) slog.Handler            { return c }
func (c countLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == c.msg {
		c.n.Add(1)
	}
	return nil
}

const deliveryQueue = 16

// Received messages processed but not recorded stay within a batch: a
// failure to record them drops the connection and holds the delivery of
// those received after them, and no new connection starts until they are
// recorded. Those are delivered then: none is lost.
func TestStoreTargetFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"error", errors.New("disk full")},
		{"unknown outcome", store.ErrUnknownOutcome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &targetStore{Store: memstore.New(), err: tc.err}
			logged := new(atomic.Int64)
			l := mem.Listen("acceptor")
			s, rec, p := logOn(t, l, l, func(c *session.Config) {
				c.Store = fs
				c.DeliveryQueue = deliveryQueue
				c.Logger = slog.New(countLog{msg: "recording received messages processed failed", n: logged})
			})
			// The Logon recorded first: the failure comes amid the orders.
			waitFor(t, func() bool { return fs.persisted() == s.NextTargetSeqNum() })
			fs.fail.Store(true)
			p.sendOrders(300)
			p.expectEnd(5 * time.Second)
			time.Sleep(20 * time.Millisecond)
			read, persisted := s.NextTargetSeqNum(), fs.persisted()
			if read-persisted > 64+deliveryQueue+4 {
				t.Errorf("%d messages read beyond the %d recorded", read-persisted, persisted)
			}
			rec.mu.Lock()
			if n := len(rec.apps); uint64(n) > persisted-2+64 {
				t.Errorf("%d orders processed, %d beyond the %d recorded", n, uint64(n)-(persisted-2), persisted-2)
			}
			rec.mu.Unlock()
			if err := s.Err(); err != nil {
				t.Errorf("Err = %v, want nil: the counter is set again", err)
			}
			// The next connection waits for the processed messages to be
			// recorded.
			p2 := acceptPeer(t, l)
			p2.expectSilentEnd()
			fs.fail.Store(false)
			p3 := acceptPeer(t, l)
			p3.expect("A")
			waitFor(t, func() bool { return fs.persisted() == s.NextTargetSeqNum() })
			// Every order read is processed: none is lost.
			rec.mu.Lock()
			if n, want := len(rec.apps), int(read)-2; n != want {
				t.Errorf("%d orders processed, want the %d read", n, want)
			}
			rec.mu.Unlock()
			if n := logged.Load(); n != 1 {
				t.Errorf("failure logged %d times, want once", n)
			}
		})
	}
}

// SetNextTargetSeqNum told the store moved on halts the session, whatever
// that is joined with; an outcome unknown alone, which the next
// SetNextTarget settles, or another failure, only fails the call.
func TestStoreSetNextTargetSeqNumFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		halt bool
	}{
		{"generation gone", store.ErrGenerationGone, true},
		{"joined with an unknown outcome", errors.Join(store.ErrUnknownOutcome, fmt.Errorf("store: %w", store.ErrSeqMismatch)), true},
		{"unknown outcome", store.ErrUnknownOutcome, false},
		{"error", errors.New("disk full"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &targetStore{Store: memstore.New(), err: tc.err}
			s, _, _ := loggedOn(t, func(c *session.Config) { c.Store = fs })
			waitFor(t, func() bool { return fs.persisted() == s.NextTargetSeqNum() })
			before := s.NextTargetSeqNum()
			fs.fail.Store(true)
			if err := s.SetNextTargetSeqNum(100); !errors.Is(err, tc.err) {
				t.Fatalf("SetNextTargetSeqNum: %v, want %v", err, tc.err)
			}
			if n := s.NextTargetSeqNum(); n != before {
				t.Errorf("NextTargetSeqNum() = %d, want %d unchanged", n, before)
			}
			if halted := s.Err() != nil; halted != tc.halt {
				t.Errorf("halted = %t (Err() = %v), want %t", halted, s.Err(), tc.halt)
			}
			if !tc.halt && s.State() != session.StateLoggedOn {
				t.Errorf("State() = %s, want %s", s.State(), session.StateLoggedOn)
			}
		})
	}
}

// A failure to record received messages that tells the store moved on
// halts the session, even joined with ErrUnknownOutcome.
func TestStoreTargetDivergenceHalts(t *testing.T) {
	for _, structural := range divergences {
		for _, err := range withUnknownOutcome(structural) {
			t.Run(err.Error(), func(t *testing.T) {
				fs := &targetStore{Store: memstore.New(), err: err}
				l := mem.Listen("acceptor")
				s, _, p := logOn(t, l, l, func(c *session.Config) { c.Store = fs; c.DeliveryQueue = deliveryQueue })
				waitFor(t, func() bool { return fs.persisted() == s.NextTargetSeqNum() })
				fs.fail.Store(true)
				p.sendOrders(300)
				p.expectEnd(5 * time.Second)
				waitFor(t, func() bool { return s.Err() != nil })
				expectHalted(t, s, structural)
				expectNoRedial(t, l)
			})
		}
	}
}

// A healthy store records the processed prefix every 64 messages at most,
// and all of it once idle.
func TestStoreTargetBatching(t *testing.T) {
	fs := &targetStore{Store: memstore.New()}
	s, rec, p := loggedOn(t, func(c *session.Config) { c.Store = fs })
	p.sendOrders(300)
	rec.wait(t, func() bool { return len(rec.apps) == 300 })
	waitFor(t, func() bool { return fs.persisted() == s.NextTargetSeqNum() })
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for i := 1; i < len(fs.sets); i++ {
		if d := fs.sets[i] - fs.sets[i-1]; d > 64 {
			t.Errorf("NextTarget recorded %d then %d: %d messages apart", fs.sets[i-1], fs.sets[i], d)
		}
	}
	if len(fs.sets) >= 300 {
		t.Errorf("NextTarget recorded %d times for 300 messages", len(fs.sets))
	}
}

// resetStore makes Reset, or the Open reloading the state after it, fail
// on demand.
type resetStore struct {
	*memstore.Store
	err       error
	commit    bool // the failing Reset is made all the same
	failReset atomic.Bool
	failOpen  atomic.Bool
	resets    atomic.Int64
	opens     atomic.Int64
}

func (f *resetStore) Reset(ctx context.Context, gen uint64, at time.Time, win string) error {
	defer f.resets.Add(1)
	if f.failReset.Load() {
		if f.commit {
			_ = f.Store.Reset(ctx, gen, at, win)
		}
		return f.err
	}
	return f.Store.Reset(ctx, gen, at, win)
}

func (f *resetStore) Open(ctx context.Context) (store.State, error) {
	if f.opens.Add(1) > 1 && f.failOpen.Load() {
		return store.State{}, errors.New("open: read failure")
	}
	return f.Store.Open(ctx)
}

var resetFailures = []struct {
	name   string
	err    error
	commit bool
	halt   bool
}{
	{"error", errors.New("disk full"), false, false},
	{"unknown outcome", store.ErrUnknownOutcome, false, true},
	{"unknown outcome committed", store.ErrUnknownOutcome, true, true},
}

// An initiator's ResetOnLogon that fails sends no Logon.
func TestStoreResetOnLogonFails(t *testing.T) {
	for _, tc := range resetFailures {
		t.Run(tc.name, func(t *testing.T) {
			fs := &resetStore{Store: memstore.New(), err: tc.err, commit: tc.commit}
			fs.failReset.Store(true)
			l := mem.Listen("acceptor")
			s, _ := startInitiator(t, l, func(c *session.Config) { c.Store = fs; c.ResetOnLogon = true })
			p := acceptPeer(t, l)
			p.expectSilentEnd()
			if tc.halt {
				expectHalted(t, s, tc.err)
				expectNoRedial(t, l)
				return
			}
			fs.failReset.Store(false)
			p2 := acceptPeer(t, l)
			p2.expect("A", "34", "1", "141", "Y")
		})
	}
}

// A Logon answer with ResetSeqNumFlag whose reset fails does not log on.
func TestStoreResetOnLogonAnswerFails(t *testing.T) {
	for _, tc := range resetFailures {
		t.Run(tc.name, func(t *testing.T) {
			fs := &resetStore{Store: memstore.New(), err: tc.err, commit: tc.commit}
			fs.failReset.Store(true)
			l := mem.Listen("acceptor")
			s, rec := startInitiator(t, l, func(c *session.Config) { c.Store = fs })
			p := acceptPeer(t, l)
			p.expect("A", "34", "1")
			p.send("A", 0, "98", "0", "108", "30", "141", "Y")
			p.expectSilentEnd()
			if st := s.State(); st == session.StateLoggedOn {
				t.Errorf("State = %s after the reset failed", st)
			}
			if tc.halt {
				expectHalted(t, s, tc.err)
				expectNoRedial(t, l)
				return
			}
			// The next connection logs on, on the generation it had.
			fs.failReset.Store(false)
			p2 := acceptPeer(t, l)
			p2.expect("A", "34", "2")
			p2.send("A", 0, "98", "0", "108", "30")
			p2.send("D", 0, "11", "IN1")
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.logons != 1 || len(rec.logouts) != 0 {
				t.Errorf("OnLogon %d times, OnLogout %d times; want once and never", rec.logons, len(rec.logouts))
			}
		})
	}
}

// An acceptor whose reset for a Logon with ResetSeqNumFlag fails neither
// answers nor logs on.
func TestStoreResetAcceptorFails(t *testing.T) {
	for _, tc := range resetFailures {
		t.Run(tc.name, func(t *testing.T) {
			fs := &resetStore{Store: memstore.New(), err: tc.err, commit: tc.commit}
			fs.failReset.Store(true)
			l := mem.Listen("acceptor")
			a := session.NewAcceptor(session.AcceptorOptions{})
			rec := newRecorder()
			// Named as the peer's counterparty: the peer sends as ACC.
			s, err := a.Add(session.Config{
				ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
				Handler: rec,
				Store:   fs,
			})
			if err != nil {
				t.Fatal(err)
			}
			a.Listen(l)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); a.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })

			p := dialPeer(t, l)
			p.send("A", 0, "98", "0", "108", "30", "141", "Y")
			p.expectSilentEnd()
			if st := s.State(); st == session.StateLoggedOn {
				t.Errorf("State = %s after the reset failed", st)
			}
			if tc.halt {
				expectHalted(t, s, tc.err)
				p2 := dialPeer(t, l)
				p2.send("A", 1, "98", "0", "108", "30", "141", "Y")
				p2.expectSilentEnd()
				return
			}
			fs.failReset.Store(false)
			p2 := dialPeer(t, l)
			p2.send("A", 0, "98", "0", "108", "30", "141", "Y")
			p2.expect("A", "34", "1", "141", "Y")
			p2.send("D", 0, "11", "IN1")
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.logons != 1 {
				t.Errorf("OnLogon %d times, want once", rec.logons)
			}
		})
	}
}

// A reset asked for on an established connection that fails is not
// answered.
func TestStoreActiveResetFails(t *testing.T) {
	for _, tc := range resetFailures {
		t.Run(tc.name, func(t *testing.T) {
			fs := &resetStore{Store: memstore.New(), err: tc.err, commit: tc.commit}
			l := mem.Listen("acceptor")
			s, _, p := logOn(t, l, l, func(c *session.Config) { c.Store = fs })
			fs.failReset.Store(true)
			p.send("A", 1, "98", "0", "108", "30", "141", "Y")
			p.expectSilentEnd()
			if tc.halt {
				expectHalted(t, s, tc.err)
				expectNoRedial(t, l)
				return
			}
			if err := s.Err(); err != nil {
				t.Errorf("Err = %v, want nil", err)
			}
		})
	}
}

// A reset on disconnection that fails is made before the next connection,
// which is refused while it still fails.
func TestStoreResetOnDisconnectFails(t *testing.T) {
	for _, tc := range resetFailures {
		t.Run(tc.name, func(t *testing.T) {
			fs := &resetStore{Store: memstore.New(), err: tc.err, commit: tc.commit}
			l := mem.Listen("acceptor")
			s, _, p := logOn(t, l, l, func(c *session.Config) { c.Store = fs; c.ResetOnDisconnect = true })
			if _, err := s.Send(context.Background(), order("O1")); err != nil {
				t.Fatal(err)
			}
			p.expect("D", "34", "2")
			fs.failReset.Store(true)
			p.conn.Close()
			waitFor(t, func() bool { return fs.resets.Load() >= 1 })
			if tc.halt {
				waitFor(t, func() bool { return s.Err() != nil })
				expectHalted(t, s, tc.err)
				expectNoRedial(t, l)
				return
			}
			p2 := acceptPeer(t, l)
			p2.expectSilentEnd()
			fs.failReset.Store(false)
			p3 := acceptPeer(t, l)
			p3.expect("A", "34", "1")
		})
	}
}

// The state reload failing after a reset halts the session: it does not
// guess the state.
func TestStoreOpenAfterResetFails(t *testing.T) {
	fs := &resetStore{Store: memstore.New()}
	l := mem.Listen("acceptor")
	s, _, p := logOn(t, l, l, func(c *session.Config) { c.Store = fs })
	fs.failOpen.Store(true)
	p.send("A", 1, "98", "0", "108", "30", "141", "Y")
	p.expectSilentEnd()
	if err := s.Err(); err == nil {
		t.Error("Err = nil, want the reload failure")
	}
	if st := s.State(); st != session.StateStopped {
		t.Errorf("State = %s, want stopped", st)
	}
	if got := s.NextSenderSeqNum(); got != 2 {
		t.Errorf("NextSenderSeqNum = %d, want 2, unchanged", got)
	}
	expectNoRedial(t, l)
}

// senderStore makes SetNextSender fail.
type senderStore struct {
	*memstore.Store
	err error
}

func (f *senderStore) SetNextSender(context.Context, uint64, uint64) error { return f.err }

// SetNextSenderSeqNum halts the session if the store may have moved
// NextSender: the numbers below may be taken.
func TestStoreSetNextSenderUncertain(t *testing.T) {
	fs := &senderStore{Store: memstore.New(), err: store.ErrUnknownOutcome}
	l := mem.Listen("acceptor")
	s, _, p := logOn(t, l, l, func(c *session.Config) { c.Store = fs })
	if err := s.SetNextSenderSeqNum(10); !errors.Is(err, store.ErrUnknownOutcome) {
		t.Fatalf("SetNextSenderSeqNum = %v, want ErrUnknownOutcome", err)
	}
	p.expectSilentEnd()
	expectHalted(t, s, store.ErrUnknownOutcome)
	expectNoRedial(t, l)
}

// A Sync that does not return within WriteTimeout drops the connection,
// without halting: the next Sync covers its messages again.
func TestStoreSyncTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	for _, tc := range []struct {
		name string
		err  error // what the Sync cancelled returns
	}{
		{"context error", nil},
		{"unknown outcome", store.ErrUnknownOutcome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSyncStore()
			st.hangErr = tc.err
			t.Cleanup(func() { close(st.release) })
			d := &durableDialer{l: mem.Listen("acceptor"), st: st}
			s, _, p := logOn(t, d, d.l, func(c *session.Config) { c.Store = st; c.WriteTimeout = timeout })
			st.hang.Store(true)
			if _, err := s.Send(context.Background(), order("O1")); err != nil {
				t.Fatal(err)
			}
			p.expectSilentEnd()
			// Checked every half WriteTimeout, cancelled once seen running
			// three times in a row.
			if took := time.Duration(st.hungFor.Load()); took < timeout || took > timeout*3/2+150*time.Millisecond {
				t.Errorf("Sync cancelled after %v, want between %v and about %v", took, timeout, timeout*3/2)
			}
			if err := s.Err(); err != nil {
				t.Errorf("Err = %v, want nil", err)
			}
			st.hang.Store(false)
			p2 := acceptPeer(t, d.l)
			p2.expect("A", "34", "3")
			d.verify(t)
		})
	}
}

// A Sync the session cancels still halts it if the store says it moved on:
// that is no outcome of the cancellation.
func TestStoreSyncCancelledDivergenceHalts(t *testing.T) {
	for _, structural := range divergences {
		for _, err := range withUnknownOutcome(structural) {
			t.Run(err.Error(), func(t *testing.T) {
				st := newSyncStore()
				st.hangErr = err
				t.Cleanup(func() { close(st.release) })
				d := &durableDialer{l: mem.Listen("acceptor"), st: st}
				s, _, p := logOn(t, d, d.l, func(c *session.Config) { c.Store = st; c.WriteTimeout = 100 * time.Millisecond })
				st.hang.Store(true)
				if _, err := s.Send(context.Background(), order("O1")); err != nil {
					t.Fatal(err)
				}
				p.expectSilentEnd()
				waitFor(t, func() bool { return s.Err() != nil })
				expectHalted(t, s, structural)
				expectNoRedial(t, d.l)
			})
		}
	}
}

// divergences are the store errors telling the store moved on, or was
// closed.
var divergences = []error{store.ErrSeqMismatch, store.ErrGenerationGone, store.ErrBackwards, store.ErrClosed}

// withUnknownOutcome returns err, alone, joined with ErrUnknownOutcome, and
// that wrapped.
func withUnknownOutcome(err error) []error {
	joined := errors.Join(store.ErrUnknownOutcome, err)
	return []error{err, joined, fmt.Errorf("wrapped: %w", joined)}
}

// Syncs slower than half WriteTimeout, but each shorter than it, are never
// cancelled.
func TestStoreSyncSlowNotCancelled(t *testing.T) {
	const timeout = 300 * time.Millisecond
	st := newSyncStore()
	d := &durableDialer{l: mem.Listen("acceptor"), st: st}
	s, _, p := logOn(t, d, d.l, func(c *session.Config) { c.Store = st; c.WriteTimeout = timeout })
	st.delay.Store(int64(timeout * 6 / 10))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 6 {
		id := fmt.Sprintf("O%d", i)
		r, err := s.Send(ctx, order(id))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Wait(ctx, session.Written); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		p.expect("D", "11", id)
	}
	if st := s.State(); st != session.StateLoggedOn {
		t.Errorf("State = %s, want logged on", st)
	}
	d.verify(t)
}

// A Sync the session cancels as its connection drops is no uncertain
// outcome, whatever the store returns: the batch is not sent, and the next
// Sync covers it again.
func TestStoreSyncCancelledByDisconnect(t *testing.T) {
	st := newSyncStore()
	st.hangErr = store.ErrUnknownOutcome
	t.Cleanup(func() { close(st.release) })
	d := &durableDialer{l: mem.Listen("acceptor"), st: st}
	s, rec, p := logOn(t, d, d.l, func(c *session.Config) { c.Store = st })
	st.hang.Store(true)
	if _, err := s.Send(context.Background(), order("O1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return st.hung.Load() == 1 })
	p.conn.Close()
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	st.hang.Store(false)
	p2 := acceptPeer(t, d.l)
	p2.expect("A", "34", "3")
	if err := s.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
	d.verify(t)
}

// Stopping a session that resets on disconnection reports no error: its
// connection, and the reset it makes, are done before the store is synced
// and closed.
func TestStoreStopAfterResetOnDisconnect(t *testing.T) {
	for range 20 {
		s, _, p := loggedOn(t, func(c *session.Config) { c.ResetOnDisconnect = true })
		if _, err := s.Send(context.Background(), order("O1")); err != nil {
			t.Fatal(err)
		}
		p.expect("D")
		done := make(chan error, 1)
		go func() { done <- s.Stop(context.Background()) }()
		p.expect("5")
		p.send("5", 0)
		if err := <-done; err != nil {
			t.Fatalf("Stop = %v", err)
		}
	}
}

// appendStore makes Append fail on demand.
type appendStore struct {
	*memstore.Store
	fail atomic.Bool
}

func (f *appendStore) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	if f.fail.Load() {
		return errors.New("disk full")
	}
	return f.Store.Append(ctx, gen, seq, raw)
}

// An acceptor that cannot store its Logon answer does not log on.
func TestStoreLogonAnswerNotStored(t *testing.T) {
	fs := &appendStore{Store: memstore.New()}
	fs.fail.Store(true)
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	rec := newRecorder()
	s, err := a.Add(session.Config{
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler: rec,
		Store:   fs,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expectSilentEnd()
	if st := s.State(); st == session.StateLoggedOn {
		t.Errorf("State = %s without a Logon answer", st)
	}
	fs.fail.Store(false)
	p2 := dialPeer(t, l)
	p2.send("A", 0, "98", "0", "108", "30")
	p2.expect("A", "34", "1")
	p2.send("D", 0, "11", "IN1")
	rec.wait(t, func() bool { return len(rec.apps) == 1 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logons != 1 || len(rec.logouts) != 0 {
		t.Errorf("OnLogon %d times, OnLogout %d times; want once and never", rec.logons, len(rec.logouts))
	}
}

// A reset on an established connection whose Logon answer cannot be stored
// is not answered, and the counterparty's Logon not taken as processed.
func TestStoreActiveResetAnswerNotStored(t *testing.T) {
	fs := &appendStore{Store: memstore.New()}
	l := mem.Listen("acceptor")
	s, _, p := logOn(t, l, l, func(c *session.Config) { c.Store = fs })
	fs.fail.Store(true)
	p.send("A", 1, "98", "0", "108", "30", "141", "Y")
	p.expectSilentEnd()
	if got := s.NextTargetSeqNum(); got != 1 {
		t.Errorf("NextTargetSeqNum = %d, want 1: the Logon was not answered", got)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
	fs.fail.Store(false)
	p2 := acceptPeer(t, l)
	p2.expect("A", "34", "1")
}

// A new schedule window's reset failing refuses the connection, or halts
// the session if its outcome is uncertain.
func TestStoreResetScheduleWindowFails(t *testing.T) {
	for _, tc := range resetFailures {
		t.Run(tc.name, func(t *testing.T) {
			fs := &resetStore{Store: memstore.New(), err: tc.err, commit: tc.commit}
			fs.failReset.Store(true)
			now := time.Now()
			l := mem.Listen("acceptor")
			s, _ := startInitiator(t, l, func(c *session.Config) {
				c.Store = fs
				c.Schedule = windows{{Start: now.Add(-time.Hour), End: now.Add(time.Hour), ID: "w1"}}
			})
			p := acceptPeer(t, l)
			p.expectSilentEnd()
			if tc.halt {
				expectHalted(t, s, tc.err)
				expectNoRedial(t, l)
				return
			}
			fs.failReset.Store(false)
			p2 := acceptPeer(t, l)
			p2.expect("A", "34", "1")
		})
	}
}

// genTargetStore makes SetNextTarget fail for one generation.
type genTargetStore struct {
	*memstore.Store
	failGen atomic.Uint64
}

func (f *genTargetStore) SetNextTarget(ctx context.Context, gen, next uint64) error {
	if gen == f.failGen.Load() {
		return errors.New("disk full")
	}
	return f.Store.SetNextTarget(ctx, gen, next)
}

// A new schedule window does not wait for the previous window's received
// messages to be recorded: its reset discards them.
func TestStoreWindowResetBeforeRecording(t *testing.T) {
	now := time.Now()
	fs := &genTargetStore{Store: memstore.New()}
	l := mem.Listen("acceptor")
	s, _, p := logOn(t, l, l, func(c *session.Config) {
		c.Store = fs
		c.Schedule = windows{
			{Start: now.Add(-time.Hour), End: now.Add(500 * time.Millisecond), ID: "w1"},
			{Start: now.Add(600 * time.Millisecond), End: now.Add(time.Hour), ID: "w2"},
		}
	})
	r, err := s.Send(context.Background(), order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	fs.failGen.Store(r.Generation())
	p.sendOrders(100)
	p.expectEnd(5 * time.Second)
	// Connections within the first window are refused: the second
	// window's logs on.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p2 := acceptPeer(t, l)
		select {
		case ev := <-p2.frames:
			if ev.err != nil {
				continue
			}
			m, err := fix.Parse(ev.frame)
			if err != nil || string(m.MsgType()) != "A" || string(m.Get(34)) != "1" {
				t.Fatalf("got %s, want a Logon at 1", visible(ev.frame))
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("no Logon")
		}
	}
	t.Fatal("the second window never logged on")
}
