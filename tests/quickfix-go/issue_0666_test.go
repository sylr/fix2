package quickfixgo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/666
//
// Initiator.Stop hung forever, also while the initiator waited to reconnect
// after a failed attempt. Stopping must return promptly whatever the session
// is doing: logged on, waiting between connection attempts, or logging on to
// a counterparty that never answers.

// i666Stop cancels ctx and checks done, closed when Run returns, closes
// within d.
func i666Stop(t *testing.T, cancel context.CancelFunc, done <-chan struct{}, d time.Duration) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("Run still running %v after its context was cancelled", d)
	}
}

func TestIssue666_StopWhileWaitingToReconnect(t *testing.T) {
	var dials atomic.Int32
	dialer := transport.DialerFunc(func(context.Context) (transport.Conn, error) {
		dials.Add(1)
		return nil, errors.New("connection refused")
	})
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), ReconnectMin: time.Minute})
	if _, err := i.Add(iniConfig(newRecorder(), nil), dialer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); i.Run(ctx) }()
	waitFor(t, "a failed connection attempt", func() bool { return dials.Load() == 1 })
	i666Stop(t, cancel, done, time.Second)
}

func TestIssue666_StopWhileLoggedOn(t *testing.T) {
	rec := newRecorder()
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), ReconnectMin: 20 * time.Millisecond})
	s, err := i.Add(iniConfig(rec, nil), l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); i.Run(ctx) }()
	p := acceptPeer(t, l)
	p.logon()
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	// The counterparty never answers the Logout: LogoutTimeout (200ms)
	// bounds the wait.
	i666Stop(t, cancel, done, time.Second)
	p.skipUntil("5")
	if st := s.State(); st != session.StateStopped {
		t.Errorf("after Run: %s, want stopped", st)
	}
}

func TestIssue666_StopWhileLoggingOn(t *testing.T) {
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), ReconnectMin: 20 * time.Millisecond})
	if _, err := i.Add(iniConfig(newRecorder(), nil), l); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); i.Run(ctx) }()
	p := acceptPeer(t, l)
	p.expect("A") // never answered
	i666Stop(t, cancel, done, time.Second)
	p.expectEnd()
}
