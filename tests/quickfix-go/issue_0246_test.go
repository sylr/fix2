package quickfixgo

import (
	"slices"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
)

// https://github.com/quickfixgo/quickfix/issues/246
//
// A retransmission (PossDupFlag=Y) of a message already processed, its
// MsgSeqNum lower than the one expected, reached FromApp again. The engine
// must drop it, without rejecting it or logging out.
func TestIssue246_PossDupTooLowIsDropped(t *testing.T) {
	s, rec, p := loggedOn(t, nil)
	p.send("D", 0, "11", "O1")
	orig := string(fix2.AppendUTCTimestamp(nil, time.Now().Add(-time.Second), fix2.Milliseconds))
	p.send("D", 2, "43", "Y", "122", orig, "11", "O1")
	p.send("D", 0, "11", "O2")
	rec.wait(t, "O2", func() bool { return len(rec.apps) == 2 })
	p.expectNothing(50 * time.Millisecond)
	if got, want := rec.appValues(11), []string{"O1", "O2"}; !slices.Equal(got, want) {
		t.Errorf("FromApp got %q, want %q", got, want)
	}
	if n := s.NextTargetSeqNum(); n != 4 {
		t.Errorf("NextTargetSeqNum = %d, want 4", n)
	}
}
