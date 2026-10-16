package quickfixgo

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/436
//
// The acceptor's session maps were read and written from each connection's
// goroutine without a lock: many sessions logging on at once panicked with
// a concurrent map access. Configured and dynamic sessions logging on
// together, while sessions are added, removed and listed, must be race-free.
func TestIssue436_ManySessionsLogOnAtOnce(t *testing.T) {
	const n = 40 // half configured, half resolved
	rec := newRecorder()
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{
		Logger: testLogger(),
		Resolve: func(_ context.Context, req *session.LogonRequest) (*session.Config, error) {
			cfg := accConfig(rec, func(c *session.Config) { c.ID = req.ID })
			return &cfg, nil
		},
	})
	sender := func(k int) string { return fmt.Sprintf("INI%d", k) }
	for k := range n / 2 {
		if _, err := a.Add(accConfig(rec, func(c *session.Config) { c.ID = accID; c.ID.TargetCompID = sender(k) })); err != nil {
			t.Fatal(err)
		}
	}
	a.Listen(l)
	run(t, a.Run)

	peers := make([]*peer, n)
	logons := make([][]byte, n)
	for k := range peers {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		conn, err := l.Dial(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		peers[k] = newPeer(t, conn, session.FIX44, sender(k), "ACC")
		logons[k] = peers[k].build("A", 0, "98", "0", "108", "30")
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)
	for k, p := range peers {
		wg.Go(func() {
			<-start
			if _, err := p.conn.Write(logons[k]); err != nil {
				errs <- err
			}
		})
	}
	// Meanwhile, sessions come and go, and are listed.
	wg.Go(func() {
		<-start
		for k := range 20 {
			id := accID
			id.TargetCompID = fmt.Sprintf("OTHER%d", k)
			if _, err := a.Add(accConfig(newRecorder(), func(c *session.Config) { c.ID = id })); err != nil {
				errs <- err
				continue
			}
			a.Sessions()
			a.Session(accID)
			if err := a.Remove(context.Background(), id); err != nil {
				errs <- err
			}
		}
	})
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for k, p := range peers {
		p.expect("A", "56", sender(k))
	}
	rec.wait(t, "every logon", func() bool { return rec.logons == n })
	if got := len(a.Sessions()); got != n {
		t.Errorf("%d sessions, want %d", got, n)
	}
}
