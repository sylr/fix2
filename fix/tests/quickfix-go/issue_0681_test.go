package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/681
//
// Stopping an acceptor right after starting it raced with the goroutine
// starting its sessions: the race detector flagged the sessions' stop
// sync.Once. Ending Run at once, even while a session is being stopped by
// hand, must be race-free and stop every session.
func TestIssue681_StopRightAfterStartIsRaceFree(t *testing.T) {
	for range 20 {
		a := session.NewAcceptor(session.AcceptorOptions{Logger: testLogger()})
		s, err := a.Add(accConfig(newRecorder(), nil))
		if err != nil {
			t.Fatal(err)
		}
		a.Listen(mem.Listen("acceptor"))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); a.Run(ctx) }()
		stopped := make(chan struct{})
		go func() { defer close(stopped); s.Stop(context.Background()) }()
		cancel()
		for _, ch := range []chan struct{}{done, stopped} {
			select {
			case <-ch:
			case <-time.After(time.Second):
				t.Fatal("Run or Stop still running 1s after the stop")
			}
		}
		if st := s.State(); st != session.StateStopped {
			t.Fatalf("after Run: %s, want stopped", st)
		}
	}
}

// The same for an initiator, whose session dials as soon as Run starts.
func TestIssue681_InitiatorStopRightAfterStartIsRaceFree(t *testing.T) {
	for range 20 {
		i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), ReconnectMin: 20 * time.Millisecond})
		s, err := i.Add(iniConfig(newRecorder(), nil), mem.Listen("acceptor"))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); i.Run(ctx) }()
		stopped := make(chan struct{})
		go func() { defer close(stopped); s.Stop(context.Background()) }()
		cancel()
		for _, ch := range []chan struct{}{done, stopped} {
			select {
			case <-ch:
			case <-time.After(time.Second):
				t.Fatal("Run or Stop still running 1s after the stop")
			}
		}
		if st := s.State(); st != session.StateStopped {
			t.Fatalf("after Run: %s, want stopped", st)
		}
	}
}
