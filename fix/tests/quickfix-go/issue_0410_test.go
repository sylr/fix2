package quickfixgo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/410
//
// A Logon with ResetSeqNumFlag (141=Y) reset the store before the application
// refused it as unauthenticated: anyone knowing the CompIDs could wipe a
// session's sequence numbers. fix2 authenticates a Logon (Handler.Authenticate)
// before acting on it: a refused one must leave the store as it was.

// i410Handler refuses Logons while refuse is set.
type i410Handler struct {
	*recorder
	refuse atomic.Bool
}

func (h *i410Handler) Authenticate(context.Context, *session.LogonRequest) error {
	if h.refuse.Load() {
		return errors.New("bad credentials")
	}
	return nil
}

// i410LoggedOn returns an acceptor logged on with a scripted initiator, having
// received messages 1 to 3 and sent 1 and 2.
func i410LoggedOn(t *testing.T) (*session.Session, *i410Handler, *mem.Listener, *peer) {
	t.Helper()
	h := &i410Handler{recorder: newRecorder()}
	s, _, l := startAcceptor(t, func(c *session.Config) { c.Handler = h })
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A", "34", "1")
	h.wait(t, "logon", func() bool { return h.logons == 1 })
	p.send("D", 0, "11", "1")
	p.send("D", 0, "11", "2")
	h.wait(t, "orders", func() bool { return len(h.apps) == 2 })
	if _, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "8").SetString(11, "1")); err != nil {
		t.Fatal(err)
	}
	p.expect("8", "34", "2")
	return s, h, l, p
}

func TestIssue410_RefusedResetLogonKeepsStore(t *testing.T) {
	s, h, l, p := i410LoggedOn(t)
	p.conn.Close()
	h.wait(t, "logout", func() bool { return len(h.logouts) == 1 })

	h.refuse.Store(true)
	p = dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30", "141", "Y")
	select {
	case ev := <-p.frames:
		if ev.err == nil {
			t.Fatalf("got %s, want the connection closed", visible(ev.frame))
		}
	case <-time.After(timeout):
		t.Fatal("connection still open")
	}
	if got, want := s.NextSenderSeqNum(), uint64(3); got != want {
		t.Errorf("NextSenderSeqNum = %d, want %d", got, want)
	}
	if got, want := s.NextTargetSeqNum(), uint64(4); got != want {
		t.Errorf("NextTargetSeqNum = %d, want %d", got, want)
	}
}

func TestIssue410_RefusedResetLogonWhileLoggedOnKeepsStore(t *testing.T) {
	s, h, _, p := i410LoggedOn(t)
	h.refuse.Store(true)
	// At MsgSeqNum 1, as FIX requires of a reset: any other is refused before
	// the handler is asked.
	p.send("A", 1, "98", "0", "108", "30", "141", "Y")
	p.expect("5", "34", "3")
	if got, want := s.NextTargetSeqNum(), uint64(4); got != want {
		t.Errorf("NextTargetSeqNum = %d, want %d", got, want)
	}
}
