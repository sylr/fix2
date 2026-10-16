package quickfixgo

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/717
//
// Stopping an initiator or an acceptor never closed the sessions' stores,
// leaking their files or database connections. Ending Run, or removing a
// session, must close its store, once.

// i717Store is a memstore counting its Closes.
type i717Store struct {
	*memstore.Store
	closes atomic.Int32
}

func (s *i717Store) Close() error {
	s.closes.Add(1)
	return s.Store.Close()
}

func newI717Store() *i717Store { return &i717Store{Store: memstore.New()} }

func TestIssue717_InitiatorRunClosesStore(t *testing.T) {
	st := newI717Store()
	rec := newRecorder()
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), ReconnectMin: 20 * time.Millisecond})
	if _, err := i.Add(iniConfig(rec, func(c *session.Config) { c.Store = st }), l); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); i.Run(ctx) }()
	p := acceptPeer(t, l)
	p.logon()
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	if n := st.closes.Load(); n != 0 {
		t.Fatalf("store closed %d times while running", n)
	}
	cancel()
	p.expect("5")
	p.send("5", 0)
	<-done
	if n := st.closes.Load(); n != 1 {
		t.Errorf("store closed %d times once Run returned, want 1", n)
	}
}

func TestIssue717_AcceptorRunClosesStores(t *testing.T) {
	static, dynamic := newI717Store(), newI717Store()
	rec := newRecorder()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{
		Logger: testLogger(),
		Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			cfg := accConfig(rec, func(c *session.Config) { c.ID, c.Store = req.ID, dynamic })
			return &cfg, nil
		},
	})
	if _, err := a.Add(accConfig(newRecorder(), func(c *session.Config) { c.Store = static })); err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	conn, err := l.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := newPeer(t, conn, session.FIX44, "DYN", "ACC")
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	cancel()
	p.expect("5")
	p.send("5", 0)
	<-done
	if n := static.closes.Load(); n != 1 {
		t.Errorf("configured session's store closed %d times once Run returned, want 1", n)
	}
	if n := dynamic.closes.Load(); n != 1 {
		t.Errorf("dynamic session's store closed %d times once Run returned, want 1", n)
	}
}

func TestIssue717_RemoveClosesStore(t *testing.T) {
	st := newI717Store()
	a := session.NewAcceptor(session.AcceptorOptions{Logger: testLogger()})
	if _, err := a.Add(accConfig(newRecorder(), func(c *session.Config) { c.Store = st })); err != nil {
		t.Fatal(err)
	}
	run(t, a.Run)
	if err := a.Remove(context.Background(), accID); err != nil {
		t.Fatal(err)
	}
	if n := st.closes.Load(); n != 1 {
		t.Errorf("store closed %d times once removed, want 1", n)
	}
}
