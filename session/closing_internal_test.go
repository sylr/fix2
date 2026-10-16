package session

import (
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session/transport/mem"
)

// Closing after a stalled handler's Logout, the phase still established and
// the counterparty's Logout not received, what it sends is still handled, but
// nothing is sent after our Logout: no Heartbeat, Reject, BusinessMessageReject
// or replay, none counted, no MsgSeqNum used up, and the connection not ended
// for it.
func TestClosingRefusesOutput(t *testing.T) {
	s := fenceSession(t, nil)
	a, b := mem.Pipe()
	defer b.Close()
	tc := newHeldConn(a)
	defer close(tc.release)
	c, err := s.attach(tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fix2.Parse(fix2.NewBuilder(FIX44, "D").SetString(49, "B").SetString(56, "A").SetInt(34, 2).
		SetString(52, string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds))).Build())
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phaseEstablished
	s.sendLogoutLocked(c, "application not processing messages")
	s.closeAfterFlushLocked(c, &LogoutError{Text: "handler stalled"})
	next := s.st.NextSender
	s.sendAdminLocked(&header{msgType: "0"}, nil)
	s.rejectLocked(&m, dictionary.ReasonOther, noTag, "refused")
	s.businessRejectLocked(&m, BusinessRejectOther, "", "refused")
	s.startReplayLocked(c, 1, 1, false)
	if n := s.st.NextSender; n != next {
		t.Errorf("NextSender = %d, want %d", n, next)
	}
	if n := s.stats.RejectsSent.Load(); n != 0 {
		t.Errorf("RejectsSent = %d, want 0", n)
	}
	if c.replay != nil || c.closed {
		t.Errorf("replay %v, closed %v: want neither", c.replay, c.closed)
	}
}
