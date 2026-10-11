package quickfixgo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/532
//
// A dynamic session could be stopped from several places of the acceptor,
// and a second stop deadlocked on a channel nobody read any more. Removing a
// dynamic session, stopping it, and ending Run, all at once, must all return.
func TestIssue532_SeveralStopsOfDynamicSession(t *testing.T) {
	rec := newRecorder()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{
		Logger: testLogger(),
		Resolve: func(context.Context, *session.LogonRequest) (*session.Config, error) {
			cfg := accConfig(rec, nil)
			return &cfg, nil
		},
	})
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	s := a.Session(accID)
	if s == nil {
		t.Fatal("no dynamic session")
	}

	var wg sync.WaitGroup
	wg.Go(func() { a.Remove(context.Background(), accID) })
	for range 3 {
		wg.Go(func() { s.Stop(context.Background()) })
	}
	wg.Go(func() { cancel(); <-done })
	p.expect("5")
	p.send("5", 0)
	stopped := make(chan struct{})
	go func() { defer close(stopped); wg.Wait() }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stops of a dynamic session deadlocked")
	}
	if st := s.State(); st != session.StateStopped {
		t.Errorf("State = %s, want stopped", st)
	}
	if a.Session(accID) != nil {
		t.Error("dynamic session still in the acceptor after Remove")
	}
	rec.wait(t, "logout", func() bool { return len(rec.logouts) == 1 })
}
