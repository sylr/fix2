package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/359
//
// Initiator.Stop panicked on a channel already closed, or never made: when
// stopped twice, or before Start. Stopping an initiator's session before
// Run, after Run, or twice must be harmless.
func TestIssue359_StopBeforeRun(t *testing.T) {
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger()})
	s, err := i.Add(iniConfig(newRecorder(), nil), mem.Listen("acceptor"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop before Run: %v", err)
	}
	if err := i.Remove(context.Background(), iniID); err != nil {
		t.Errorf("Remove before Run: %v", err)
	}
	if err := i.Remove(context.Background(), iniID); err == nil {
		t.Error("second Remove succeeded, want a not found error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := i.Run(ctx); err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestIssue359_StopAfterRun(t *testing.T) {
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
	cancel()
	cancel()
	p.expect("5")
	p.send("5", 0)
	<-done
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop after Run: %v", err)
	}
	// Run again: its session is stopped for good, Run stops it once more
	// when ctx is done.
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := i.Run(ctx); err != nil {
		t.Errorf("Run again: %v", err)
	}
	if err := i.Remove(context.Background(), iniID); err != nil {
		t.Errorf("Remove after Run: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logons != 1 {
		t.Errorf("%d logons, want 1", rec.logons)
	}
}
