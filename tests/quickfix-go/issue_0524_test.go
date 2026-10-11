package quickfixgo

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/524
//
// Disconnecting a dynamic session could deadlock: its event loop, the only
// reader of a channel its timers wrote to, had already returned. A dynamic
// session whose counterparty hangs up must log out, take the counterparty's
// next Logon, and be removed without blocking.
func TestIssue524_DisconnectDynamicSession(t *testing.T) {
	rec := newRecorder()
	var resolved atomic.Int32
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{
		Logger: testLogger(),
		Resolve: func(context.Context, *session.LogonRequest) (*session.Config, error) {
			resolved.Add(1)
			cfg := accConfig(rec, nil)
			return &cfg, nil
		},
	})
	a.Listen(l)
	run(t, a.Run)

	// Hang up, log on again on the same session, hang up once more.
	for n := 1; n <= 2; n++ {
		p := dialPeer(t, l)
		p.seq = n
		p.send("A", 0, "98", "0", "108", "1")
		p.expect("A")
		rec.wait(t, "logon", func() bool { return rec.logons == n })
		p.conn.Close()
		rec.wait(t, "logout", func() bool { return len(rec.logouts) == n })
	}
	if n := resolved.Load(); n != 1 {
		t.Fatalf("Resolve called %d times, want once: the session stays until removed", n)
	}
	removed := make(chan error, 1)
	go func() { removed <- a.Remove(context.Background(), accID) }()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("Remove: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove of a disconnected dynamic session blocked")
	}

	// Removed: the next Logon resolves a new session.
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "1")
	p.expect("A", "34", "1")
	rec.wait(t, "logon", func() bool { return rec.logons == 3 })
	if n := resolved.Load(); n != 2 {
		t.Errorf("Resolve called %d times, want twice", n)
	}
}
