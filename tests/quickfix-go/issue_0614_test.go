package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/614
//
// Sessions were registered in a global registry no stop ever cleared: a new
// initiator for the SessionID of a stopped one failed with "Duplicate
// SessionID". Once stopped or removed, a SessionID must be free again.
func TestIssue614_NewInitiatorAfterStop(t *testing.T) {
	l := mem.Listen("acceptor")
	opts := session.InitiatorOptions{Logger: testLogger(), ReconnectMin: 20 * time.Millisecond}
	for n := range 2 {
		rec := newRecorder()
		i := session.NewInitiator(opts)
		if _, err := i.Add(iniConfig(rec, nil), l); err != nil {
			t.Fatalf("initiator %d: Add: %v", n+1, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); i.Run(ctx) }()
		p := acceptPeer(t, l)
		p.logon()
		rec.wait(t, "logon", func() bool { return rec.logons == 1 })
		cancel()
		p.expect("5")
		p.send("5", 0)
		<-done
	}
}

// The same initiator: a removed session's ID can be added again.
func TestIssue614_AddAfterRemove(t *testing.T) {
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), ReconnectMin: 20 * time.Millisecond})
	run(t, i.Run)
	for n := range 2 {
		rec := newRecorder()
		if _, err := i.Add(iniConfig(rec, nil), l); err != nil {
			t.Fatalf("session %d: Add: %v", n+1, err)
		}
		p := acceptPeer(t, l)
		p.logon()
		rec.wait(t, "logon", func() bool { return rec.logons == 1 })
		removed := make(chan error, 1)
		go func() { removed <- i.Remove(context.Background(), iniID) }()
		p.expect("5")
		p.send("5", 0)
		if err := <-removed; err != nil {
			t.Fatalf("session %d: Remove: %v", n+1, err)
		}
	}
}
