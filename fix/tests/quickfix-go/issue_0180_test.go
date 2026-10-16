package quickfixgo

import (
	"slices"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/180
//
// A session recovering a gap went back to its plain logged on state on any
// timer event (a heartbeat due, a TestRequest), or when leaving the
// TestRequest-pending state: it forgot the gap. Across heartbeats and
// TestRequests, the session must keep holding what came ahead of the gap,
// not ask again, and deliver everything in order once the gap is filled.
func TestIssue180_RecoveryOutlivesHeartbeats(t *testing.T) {
	s, rec, p := loggedOn(t, func(c *session.Config) { c.HeartBtInt = time.Second })
	p.seq = 5
	p.send("D", 0, "11", "O5")
	p.expect("2", "7", "2", "16", "0")
	// Quiet past a heartbeat interval: the session sends a Heartbeat, then
	// a TestRequest, which we answer.
	p.expect("0")
	tr := p.expect("1")
	p.send("0", 0, "112", string(tr.Get(112)))
	p.send("D", 0, "11", "O7")
	orig := string(fix.AppendUTCTimestamp(nil, time.Now().Add(-time.Second), fix.Milliseconds))
	for i, id := range []string{"O2", "O3", "O4"} {
		p.send("D", 2+i, "43", "Y", "122", orig, "11", id)
	}
	rec.wait(t, "the messages", func() bool { return len(rec.apps) == 5 })
	if got, want := rec.appValues(11), []string{"O2", "O3", "O4", "O5", "O7"}; !slices.Equal(got, want) {
		t.Errorf("FromApp got %q, want %q", got, want)
	}
	if n := s.NextTargetSeqNum(); n != 8 {
		t.Errorf("NextTargetSeqNum = %d, want 8", n)
	}
	// No second ResendRequest: the gap was never forgotten.
	p.expectNothing(50 * time.Millisecond)
}
