package session_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport/mem"
)

// gatedStore blocks its Open number at until release is called.
type gatedStore struct {
	*closeStore
	at      int32
	entered chan struct{}
	gate    chan struct{}
	release func()
}

// newGatedStore returns a gatedStore released at the latest when the test
// ends.
func newGatedStore(t *testing.T, at int32) *gatedStore {
	g := &gatedStore{closeStore: &closeStore{Store: memstore.New()}, at: at, entered: make(chan struct{}), gate: make(chan struct{})}
	g.release = sync.OnceFunc(func() { close(g.gate) })
	t.Cleanup(g.release)
	return g
}

func (g *gatedStore) Open(ctx context.Context) (store.State, error) {
	st, err := g.closeStore.Open(ctx)
	if g.opens.Load() == g.at {
		close(g.entered)
		<-g.gate
	}
	return st, err
}

// manager is an Initiator or an Acceptor, serving on a mem.Listener.
type manager struct {
	add    func(session.Config) (*session.Session, error)
	remove func(context.Context, session.SessionID) error
	run    func(context.Context) error
	// logOn connects a peer to the session added and logs it on.
	logOn func(t *testing.T) *peer
}

var managers = []struct {
	name string
	new  func() manager
}{
	{"initiator", func() manager {
		l := mem.Listen("acceptor")
		i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
		return manager{
			add:    func(cfg session.Config) (*session.Session, error) { return i.Add(cfg, l) },
			remove: i.Remove,
			run:    i.Run,
			logOn: func(t *testing.T) *peer {
				p := acceptPeer(t, l)
				p.expect("A")
				p.send("A", 0, "98", "0", "108", "30")
				return p
			},
		}
	}},
	{"acceptor", func() manager {
		l := mem.Listen("acceptor")
		a := session.NewAcceptor(session.AcceptorOptions{})
		a.Listen(l)
		return manager{
			add:    a.Add,
			remove: a.Remove,
			run:    a.Run,
			logOn: func(t *testing.T) *peer {
				p := dialPeer(t, l)
				p.send("A", 0, "98", "0", "108", "30")
				p.expect("A")
				return p
			},
		}
	}},
}

// An Add of a session's ID, made as the session is removed with a callback
// stuck, never closes the store the removed session still uses: it is
// refused without opening it.
func TestAddConcurrentWithRemove(t *testing.T) {
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	for _, tc := range managers {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.new()
			st := newGatedStore(t, 2)
			h := newBlockingApp(t)
			cfg := session.Config{ID: id, Handler: h, Store: st, LogoutTimeout: 100 * time.Millisecond}
			s, err := m.add(cfg)
			if err != nil {
				t.Fatal(err)
			}
			runUntilCleanup(t, m.run)
			p := m.logOn(t)
			h.wait(t, func() bool { return h.logons == 1 })
			p.sendOrders(1)
			h.waitEntered(t)

			added := make(chan error, 1)
			go func() { _, err := m.add(cfg); added <- err }()
			var addErr error
			opened := false
			select {
			case addErr = <-added:
			case <-st.entered:
				opened = true
			case <-time.After(5 * time.Second):
				t.Fatal("Add neither returned nor opened the store")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			m.remove(ctx, id)
			cancel()
			st.release()
			if opened {
				t.Error("Add opened the store of the session added")
				addErr = <-added
			}
			if addErr == nil {
				t.Fatal("Add of a session added succeeded")
			}
			if st.closed.Load() {
				t.Error("refused Add closed the store the removed session still uses")
			}
			select {
			case <-s.Done():
				t.Error("Done closed while FromApp is stuck")
			default:
			}

			h.unblock()
			waitDone(t, s)
			if n := st.opens.Load(); n != 1 {
				t.Errorf("store opened %d times, want once: by the removed session alone", n)
			}
			if late := st.lateCalls(); len(late) > 0 {
				t.Errorf("store used after Close: %v", late)
			}
		})
	}
}

// Add reserves its ID before opening the store: another Add of the ID
// meanwhile is refused without opening its own, and the first succeeds.
func TestAddReservesID(t *testing.T) {
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	for _, tc := range managers {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.new()
			first := newGatedStore(t, 1)
			added := make(chan error, 1)
			go func() {
				_, err := m.add(session.Config{ID: id, Handler: session.BaseHandler{}, Store: first})
				added <- err
			}()
			select {
			case <-first.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first Add did not open its store")
			}

			second := &closeStore{Store: memstore.New()}
			cfg := session.Config{ID: id, Handler: session.BaseHandler{}, Store: second}
			if _, err := m.add(cfg); err == nil || !strings.Contains(err.Error(), "being added") {
				t.Errorf("Add of an ID being added = %v, want it refused", err)
			}
			first.release()
			if err := <-added; err != nil {
				t.Errorf("first Add: %v", err)
			}
			if first.closed.Load() {
				t.Error("store of the session added closed")
			}
			if _, err := m.add(cfg); err == nil || !strings.Contains(err.Error(), "already added") {
				t.Errorf("Add of an ID added = %v, want it refused", err)
			}
			if n := second.opens.Load(); n != 0 || second.closed.Load() {
				t.Errorf("refused Adds used their store: opened %d times, closed %v", n, second.closed.Load())
			}
		})
	}
}
