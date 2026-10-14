package session_test

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport/mem"
)

// hangStore makes Append hang, once hang is set, until unblock is called.
type hangStore struct {
	*memstore.Store
	hang    atomic.Bool
	release chan struct{}
	once    sync.Once
}

func newHangStore() *hangStore {
	return &hangStore{Store: memstore.New(), release: make(chan struct{})}
}

// hangingSession returns a session logged on through hs, with StoreTimeout
// 100ms, and its peer. hs is unblocked when the test ends, before the
// session's cleanup, which would wait for the write that hangs.
func hangingSession(t *testing.T, hs *hangStore) (*session.Session, *peer) {
	t.Helper()
	s, _, p := loggedOn(t, func(c *session.Config) {
		c.Store = hs
		c.StoreTimeout = 100 * time.Millisecond
	})
	t.Cleanup(hs.unblock)
	return s, p
}

func (h *hangStore) unblock() { h.once.Do(func() { close(h.release) }) }

func (h *hangStore) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	if h.hang.Load() {
		<-h.release
	}
	return h.Store.Append(ctx, gen, seq, raw)
}

// A store write that hangs halts the session after StoreTimeout without
// waiting for it: State and Err tell it, the counterparty sees the
// connection close, Stop returns when its ctx ends. Once the write returns,
// the session stays halted.
func TestStoreTimeout(t *testing.T) {
	hs := newHangStore()
	s, p := hangingSession(t, hs)
	hs.hang.Store(true)
	sent := make(chan error, 1)
	go func() {
		_, err := s.Send(context.Background(), order("O1"))
		sent <- err
	}()
	p.expectEnd(5 * time.Second)
	if st := s.State(); st != session.StateStopped {
		t.Errorf("State() = %s, want %s", st, session.StateStopped)
	}
	if err := s.Err(); !errors.Is(err, session.ErrStoreTimeout) {
		t.Errorf("Err() = %v, want %v", err, session.ErrStoreTimeout)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		if !errors.Is(err, session.ErrStoreTimeout) {
			t.Errorf("Stop: %v, want %v", err, session.ErrStoreTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited for the store write that hangs")
	}

	hs.unblock()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("Send never returned once the store answered")
	}
	if err := s.Err(); !errors.Is(err, session.ErrStoreTimeout) {
		t.Errorf("once the write returned: Err() = %v, want %v", err, session.ErrStoreTimeout)
	}
	if st := s.State(); st != session.StateStopped {
		t.Errorf("once the write returned: State() = %s, want %s", st, session.StateStopped)
	}
	// The Stop that returned goes on.
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session never stopped once the store answered")
	}
}

// Stop returns when its own Logout hangs in the store, and the session
// stops once the write returns.
func TestStoreTimeout_StopsLogoutHanging(t *testing.T) {
	hs := newHangStore()
	s, _ := hangingSession(t, hs)
	hs.hang.Store(true)
	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		if !errors.Is(err, session.ErrStoreTimeout) {
			t.Errorf("Stop: %v, want %v", err, session.ErrStoreTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited for its Logout's store write")
	}
	hs.unblock()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session never stopped once the store answered")
	}
}

// Store writes that answer within StoreTimeout halt nothing.
func TestStoreTimeout_Answering(t *testing.T) {
	s, _, p := loggedOn(t, func(c *session.Config) { c.StoreTimeout = 50 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline := time.Now().Add(300 * time.Millisecond) // six timeouts' worth of checks
	for i := 0; time.Now().Before(deadline); i++ {
		if _, err := s.Send(ctx, order("O")); err != nil {
			t.Fatal(err)
		}
		p.expect("D")
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

// Stop called again and again while a store write hangs returns at once,
// starting no further stop.
func TestStoreTimeout_RepeatedStop(t *testing.T) {
	hs := newHangStore()
	s, _ := hangingSession(t, hs)
	hs.hang.Store(true)
	go s.Send(context.Background(), order("O1"))
	for s.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	before := runtime.NumGoroutine()
	for range 100 {
		if err := s.Stop(context.Background()); !errors.Is(err, session.ErrStoreTimeout) {
			t.Fatalf("Stop: %v, want %v", err, session.ErrStoreTimeout)
		}
	}
	if n := runtime.NumGoroutine(); n > before+5 {
		t.Errorf("%d goroutines after 100 Stops, %d before", n, before)
	}
	hs.unblock()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session never stopped once the store answered")
	}
}

// Initiator.Remove returns once the session's store write is found to
// hang, not waiting for the connection loop the write holds.
func TestStoreTimeout_InitiatorRemove(t *testing.T) {
	hs := newHangStore()
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	rec := newRecorder()
	s, err := i.Add(session.Config{ID: id, Handler: rec, Store: hs, StoreTimeout: 100 * time.Millisecond, LogoutTimeout: 200 * time.Millisecond}, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); i.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	t.Cleanup(hs.unblock) // first: the initiator's cleanup waits for the write
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	hs.hang.Store(true)
	go s.Send(context.Background(), order("O1"))
	removed := make(chan error, 1)
	go func() { removed <- i.Remove(context.Background(), id) }()
	select {
	case err := <-removed:
		if !errors.Is(err, session.ErrStoreTimeout) {
			t.Errorf("Remove: %v, want %v", err, session.ErrStoreTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Remove waited for the store write that hangs")
	}
}

// blockingLog blocks every record with message msg until release closes, as
// a logger writing to the disk that hangs would.
type blockingLog struct {
	msg     string
	release chan struct{}
}

func (b blockingLog) Enabled(context.Context, slog.Level) bool { return true }
func (b blockingLog) WithAttrs([]slog.Attr) slog.Handler       { return b }
func (b blockingLog) WithGroup(string) slog.Handler            { return b }
func (b blockingLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == b.msg {
		<-b.release
	}
	return nil
}

// A logger that hangs keeps neither the connection open nor the session
// from halting once a store write is found to hang.
func TestStoreTimeout_LoggerHangs(t *testing.T) {
	hs := newHangStore()
	logged := make(chan struct{})
	s, _, p := loggedOn(t, func(c *session.Config) {
		c.Store = hs
		c.StoreTimeout = 100 * time.Millisecond
		c.Logger = slog.New(blockingLog{msg: "session halted: a store write has run longer than StoreTimeout", release: logged})
	})
	t.Cleanup(hs.unblock)
	t.Cleanup(func() { close(logged) })
	hs.hang.Store(true)
	go s.Send(context.Background(), order("O1"))
	p.expectEnd(5 * time.Second)
	if err := s.Err(); !errors.Is(err, session.ErrStoreTimeout) {
		t.Errorf("Err() = %v, want %v", err, session.ErrStoreTimeout)
	}
}
