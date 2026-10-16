package session_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// closeStore records the calls made to it once closed.
type closeStore struct {
	*memstore.Store
	closed atomic.Bool
	opens  atomic.Int32

	mu   sync.Mutex
	late []string
}

func (f *closeStore) used(call string) {
	if f.closed.Load() {
		f.mu.Lock()
		f.late = append(f.late, call)
		f.mu.Unlock()
	}
}

func (f *closeStore) lateCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.late
}

func (f *closeStore) Open(ctx context.Context) (store.State, error) {
	f.used("Open")
	f.opens.Add(1)
	return f.Store.Open(ctx)
}

func (f *closeStore) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	f.used("Append")
	return f.Store.Append(ctx, gen, seq, raw)
}

func (f *closeStore) SetNextTarget(ctx context.Context, gen, next uint64) error {
	f.used("SetNextTarget")
	return f.Store.SetNextTarget(ctx, gen, next)
}

func (f *closeStore) SetNextSender(ctx context.Context, gen, next uint64) error {
	f.used("SetNextSender")
	return f.Store.SetNextSender(ctx, gen, next)
}

func (f *closeStore) Range(ctx context.Context, gen, begin, end uint64, fn func(uint64, []byte, store.Hole) error) error {
	f.used("Range")
	return f.Store.Range(ctx, gen, begin, end, fn)
}

func (f *closeStore) Reset(ctx context.Context, gen uint64, at time.Time, win string) error {
	f.used("Reset")
	return f.Store.Reset(ctx, gen, at, win)
}

func (f *closeStore) Sync(ctx context.Context, gen, upTo uint64) error {
	f.used("Sync")
	return f.Store.Sync(ctx, gen, upTo)
}

func (f *closeStore) Close() error {
	f.used("Close")
	f.closed.Store(true)
	return f.Store.Close()
}

// blockingApp blocks FromApp until release is closed.
type blockingApp struct {
	*recorder
	entered  chan struct{}
	release  chan struct{}
	returned atomic.Int32
}

// newBlockingApp returns a blockingApp released at the latest when the test
// ends.
func newBlockingApp(t *testing.T) *blockingApp {
	h := &blockingApp{recorder: newRecorder(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(h.unblock)
	return h
}

func (b *blockingApp) unblock() {
	select {
	case <-b.release:
	default:
		close(b.release)
	}
}

// waitEntered waits for FromApp to be called.
func (b *blockingApp) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("FromApp not called")
	}
}

// waitDone waits for s's Done.
func waitDone(t *testing.T, s *session.Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed")
	}
}

func (b *blockingApp) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	defer b.returned.Add(1)
	return b.recorder.FromApp(ctx, s, m, in)
}

// Stop returns at its ctx's end though FromApp is stuck; Done tells once the
// callbacks returned, and the store is closed only then: never used after.
func TestStopWithStuckCallback(t *testing.T) {
	st := &closeStore{Store: memstore.New()}
	h := &blockingApp{entered: make(chan struct{}, 1), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(h.release)
		}
	}()
	s, _, p := loggedOn(t, func(c *session.Config) {
		h.recorder = c.Handler.(*recorder)
		c.Handler = h
		c.Store = st
	})
	p.sendOrders(3)
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("FromApp not called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Stop(ctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Stop returned after %v, its ctx ended after 300ms", d)
	}
	select {
	case <-s.Done():
		t.Fatal("Done closed while FromApp still runs")
	default:
	}

	released = true
	close(h.release)
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed once FromApp returned")
	}
	if n := h.returned.Load(); n != 3 {
		t.Errorf("Done closed with %d FromApp calls returned, want 3", n)
	}
	if !st.closed.Load() {
		t.Error("store not closed once the callbacks returned")
	}
	if late := st.lateCalls(); len(late) > 0 {
		t.Errorf("store used after Close: %v", late)
	}
}

// Done is closed by the time Stop returns when nothing is stuck, and at once
// for a session that never connected.
func TestStopDone(t *testing.T) {
	t.Run("connected", func(t *testing.T) {
		s, _, _ := loggedOn(t, nil)
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.Done():
		default:
			t.Fatal("Done not closed when Stop returned")
		}
	})
	t.Run("never connected", func(t *testing.T) {
		s, err := session.New(session.Config{
			ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
			Handler: session.BaseHandler{},
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.Done():
			t.Fatal("Done closed before Stop")
		default:
		}
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.Done():
		default:
			t.Fatal("Done not closed when Stop returned")
		}
	})
}

// A session removed while a callback is stuck keeps its ID until Done: Add
// refuses it meanwhile, without opening the store the old session still
// uses, and accepts it once the callback returned.
func TestRemoveWhileCallbackStuck(t *testing.T) {
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	for _, tc := range []struct {
		name string
		// start runs an initiator or an acceptor of cfg, logs its session
		// on to a peer, and returns the session, the peer, and the
		// initiator's or acceptor's Remove and Add.
		start func(t *testing.T, cfg session.Config) (*session.Session, *peer, func(context.Context) error, func(session.Config) error)
	}{
		{"initiator", func(t *testing.T, cfg session.Config) (*session.Session, *peer, func(context.Context) error, func(session.Config) error) {
			l := mem.Listen("acceptor")
			i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
			s, err := i.Add(cfg, l)
			if err != nil {
				t.Fatal(err)
			}
			runUntilCleanup(t, i.Run)
			p := acceptPeer(t, l)
			p.expect("A")
			p.send("A", 0, "98", "0", "108", "30")
			return s, p, func(ctx context.Context) error { return i.Remove(ctx, id) },
				func(cfg session.Config) error { _, err := i.Add(cfg, l); return err }
		}},
		{"acceptor", func(t *testing.T, cfg session.Config) (*session.Session, *peer, func(context.Context) error, func(session.Config) error) {
			l := mem.Listen("acceptor")
			a := session.NewAcceptor(session.AcceptorOptions{})
			s, err := a.Add(cfg)
			if err != nil {
				t.Fatal(err)
			}
			a.Listen(l)
			runUntilCleanup(t, a.Run)
			p := dialPeer(t, l)
			p.send("A", 0, "98", "0", "108", "30")
			p.expect("A")
			return s, p, func(ctx context.Context) error { return a.Remove(ctx, id) },
				func(cfg session.Config) error { _, err := a.Add(cfg); return err }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &closeStore{Store: memstore.New()}
			h := newBlockingApp(t)
			cfg := session.Config{ID: id, Handler: h, Store: st, LogoutTimeout: 200 * time.Millisecond}
			s, p, remove, add := tc.start(t, cfg)
			h.wait(t, func() bool { return h.logons == 1 })
			p.sendOrders(3)
			h.waitEntered(t)

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			remove(ctx)
			if err := add(cfg); err == nil || !strings.Contains(err.Error(), "still stopping") {
				t.Errorf("Add while the removed session stops = %v, want it refused", err)
			}
			if n := st.opens.Load(); n != 1 {
				t.Errorf("store opened %d times, want once: by the removed session alone", n)
			}

			h.unblock()
			waitDone(t, s)
			cfg.Store = memstore.New()
			if err := add(cfg); err != nil {
				t.Errorf("Add once the removed session stopped: %v", err)
			}
			if late := st.lateCalls(); len(late) > 0 {
				t.Errorf("store used after Close: %v", late)
			}
		})
	}
}

// runUntilCleanup runs run until the test ends.
func runUntilCleanup(t *testing.T, run func(context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// A Stop called again, or at once with another, waits for Done as the first
// does.
func TestStopAgainWaitsForDone(t *testing.T) {
	h := newBlockingApp(t)
	s, _, p := loggedOn(t, func(c *session.Config) {
		h.recorder = c.Handler.(*recorder)
		c.Handler = h
	})
	p.sendOrders(1)
	h.waitEntered(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Stop(ctx)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.Stop(context.Background())
	}()
	select {
	case <-stopped:
		t.Fatal("Stop called again returned before Done closed")
	case <-time.After(200 * time.Millisecond):
	}
	h.unblock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop called again did not return once Done closed")
	}
	waitDone(t, s)
}

// toAdminBlocker blocks ToAdmin of a Logon until release is closed.
type toAdminBlocker struct {
	session.BaseHandler
	entered chan struct{}
	release chan struct{}
}

func (b toAdminBlocker) ToAdmin(_ context.Context, _ *session.Session, msgType string, _ *fix.Builder) error {
	if msgType == "A" {
		close(b.entered)
		<-b.release
	}
	return nil
}

// An acceptor's ToAdmin, run for a Logon before the connection is attached
// to the session, is a callback Done waits for.
func TestDoneWaitsForAcceptorToAdmin(t *testing.T) {
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	h := toAdminBlocker{entered: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(h.release)
		}
	}()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	s, err := a.Add(session.Config{ID: id, Handler: h})
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	runUntilCleanup(t, a.Run)
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ToAdmin not called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	a.Remove(ctx, id)
	select {
	case <-s.Done():
		t.Fatal("Done closed while ToAdmin runs")
	default:
	}
	released = true
	close(h.release)
	waitDone(t, s)
	p.expectSilentEnd()
}

// The messages stored are made durable before the store is closed, though
// Stop's ctx ended before its Sync could: they are synced again once the
// stuck callback returned.
func TestStopSyncsAgainOnceCallbacksReturn(t *testing.T) {
	st := newSyncStore()
	h := newBlockingApp(t)
	l := mem.Listen("acceptor")
	s, _, p := logOn(t, l, l, func(c *session.Config) {
		h.recorder = c.Handler.(*recorder)
		c.Handler = h
		c.Store = st
	})
	p.sendOrders(1)
	h.waitEntered(t)
	// Stop's Logout is stored, but its Syncs wait for their ctx to end.
	st.hang.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); err == nil {
		t.Error("Stop = nil, want its Sync's failure")
	}
	st.hang.Store(false)
	h.unblock()
	waitDone(t, s)
	if got, want := st.closedAt(), s.NextSenderSeqNum(); got != want {
		t.Errorf("durable NextSender when closed = %d, want %d", got, want)
	}
}
