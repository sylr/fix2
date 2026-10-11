package quickfixgo

import (
	"context"
	"sync"
	"testing"

	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/581
//
// A session stopped more than once panicked with "close of closed channel":
// its timers' Stop was not idempotent. Stopping a session again, or from
// several goroutines at once, must be harmless.
func TestIssue581_StopTwice(t *testing.T) {
	s, rec, p := loggedOn(t, nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { s.Stop(context.Background()) })
	}
	p.expect("5")
	p.send("5", 0)
	wg.Wait()
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop again: %v", err)
	}
	if st := s.State(); st != session.StateStopped {
		t.Errorf("State = %s, want stopped", st)
	}
	rec.wait(t, "logout", func() bool { return len(rec.logouts) == 1 })
}

// The acceptor's side, its Run then stopping the session once more.
func TestIssue581_AcceptorStopTwice(t *testing.T) {
	s, rec, p := acceptedPeer(t, nil)
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.Stop(context.Background()) }()
	p.expect("5")
	p.send("5", 0)
	<-stopped
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop again: %v", err)
	}
	rec.wait(t, "logout", func() bool { return len(rec.logouts) == 1 })
}
