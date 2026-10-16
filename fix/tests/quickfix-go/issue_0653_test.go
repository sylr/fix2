package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/653
//
// Initiator.Stop blocked while Dial hung on an unresponsive counterparty,
// until the dial timed out: the dialer had no context to cancel. Stopping,
// or removing the session, must cancel a dial in progress.

// i653Dialer is a dialer hanging until its context is done, telling dialing
// of each dial.
func i653Dialer(dialing chan<- struct{}) transport.Dialer {
	return transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		dialing <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
}

func TestIssue653_StopCancelsDial(t *testing.T) {
	dialing := make(chan struct{}, 10)
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), DialTimeout: time.Minute})
	if _, err := i.Add(iniConfig(newRecorder(), nil), i653Dialer(dialing)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); i.Run(ctx) }()
	<-dialing
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run still running 1s after its context was cancelled: blocked on Dial")
	}
}

func TestIssue653_RemoveCancelsDial(t *testing.T) {
	dialing := make(chan struct{}, 10)
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), DialTimeout: time.Minute})
	s, err := i.Add(iniConfig(newRecorder(), nil), i653Dialer(dialing))
	if err != nil {
		t.Fatal(err)
	}
	run(t, i.Run)
	<-dialing
	removed := make(chan error, 1)
	go func() { removed <- i.Remove(context.Background(), iniID) }()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("Remove: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove still running after 1s: blocked on Dial")
	}
	if st := s.State(); st != session.StateStopped {
		t.Errorf("after Remove: %s, want stopped", st)
	}
	if i.Session(iniID) != nil {
		t.Error("session still in the initiator after Remove")
	}
}

// The acceptor's side: a connection that never sends its Logon must not
// hold Run for LogonTimeout once its context is done.
func TestIssue653_AcceptorStopDoesNotWaitForPendingLogon(t *testing.T) {
	a := session.NewAcceptor(session.AcceptorOptions{Logger: testLogger(), LogonTimeout: time.Minute})
	if _, err := a.Add(accConfig(newRecorder(), nil)); err != nil {
		t.Fatal(err)
	}
	l := mem.Listen("acceptor")
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	p := dialPeer(t, l) // silent
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run still running 1s after its context was cancelled: waiting for a connection's Logon")
	}
	p.expectEnd()
}
