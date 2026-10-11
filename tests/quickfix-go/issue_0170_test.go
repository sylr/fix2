package quickfixgo

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/170
//
// Stopping an initiator did not wait for the answer to its Logout: the
// counterparty's Logout went unread, and the next logon had to ask for it
// again. Stopping must send a Logout and wait for the answer, which the
// store then counts as received.
func TestIssue170_StopWaitsForLogoutAnswer(t *testing.T) {
	st := i170Store{memstore.New()}
	change := func(c *session.Config) {
		c.Store = st
		c.LogoutTimeout = 2 * time.Second
	}
	rec := newRecorder()
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond})
	s, err := i.Add(iniConfig(rec, change), l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); i.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	p := acceptPeer(t, l)
	p.logon()
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })

	cancel()
	p.expect("5", "34", "2")
	select {
	case <-stopped:
		t.Fatal("stopped without waiting for the Logout's answer")
	case <-time.After(100 * time.Millisecond):
	}
	p.send("5", 0)
	select {
	case <-stopped:
	case <-time.After(timeout):
		t.Fatal("still stopping after the Logout's answer")
	}
	if next := s.NextTargetSeqNum(); next != 3 {
		t.Errorf("NextTargetSeqNum = %d after the answer, want 3", next)
	}

	// The next run logs on where the last left off, asking for nothing.
	_, rec2, l2 := startInitiator(t, change)
	p2 := acceptPeer(t, l2)
	p2.seq = 3
	m := p2.expect("A", "34", "3")
	p2.send("A", 0, "98", "0", "108", string(m.Get(108)))
	rec2.wait(t, "logon", func() bool { return rec2.logons == 1 })
	p2.expectNothing(100 * time.Millisecond)
}

// i170Store is a memstore surviving its session, as a file store does a
// restart.
type i170Store struct{ *memstore.Store }

func (i170Store) Close() error { return nil }
