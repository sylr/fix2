package quickfixgo

import (
	"slices"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/407
//
// After a reconnection the counterparty's Logon came too high: the initiator
// asked for the gap, then received a market data snapshot and an update,
// which it held, then a SequenceReset-Reset past them instead of the resend.
// The held messages never reached FromApp: the snapshot, never sent again,
// was lost. Messages received ahead of the gap must be delivered once a
// SequenceReset-Reset gives up on it.
func TestIssue407_HeldMessagesDeliveredAfterResetPastThem(t *testing.T) {
	s, rec, p := i407Gap(t)
	// As in the issue's log: the Reset follows the held messages and moves
	// past itself.
	p.send("4", 0, "43", "Y", "122", i407Orig(), "123", "N", "36", "8")
	rec.wait(t, "the held messages", func() bool { return len(rec.apps) == 2 })
	if got, want := rec.appValues(35), []string{"X", "W"}; !slices.Equal(got, want) {
		t.Errorf("FromApp got MsgTypes %q, want %q", got, want)
	}
	if n := s.NextTargetSeqNum(); n != 8 {
		t.Errorf("NextTargetSeqNum = %d, want 8", n)
	}
}

// A SequenceReset-Reset to the first message held: the gap before it is
// given up, the held messages are delivered.
func TestIssue407_HeldMessagesDeliveredAfterResetToThem(t *testing.T) {
	s, rec, p := i407Gap(t)
	p.send("4", 1, "43", "Y", "122", i407Orig(), "123", "N", "36", "5")
	rec.wait(t, "the held messages", func() bool { return len(rec.apps) == 2 })
	if got, want := rec.appValues(35), []string{"X", "W"}; !slices.Equal(got, want) {
		t.Errorf("FromApp got MsgTypes %q, want %q", got, want)
	}
	if n := s.NextTargetSeqNum(); n != 7 {
		t.Errorf("NextTargetSeqNum = %d, want 7", n)
	}
}

// i407Gap logs on an initiator whose counterparty's Logon comes too high (4
// when 1 is expected), then sends a snapshot (5) and an update (6), which the
// initiator holds while it asks for 1 to 3.
func i407Gap(t *testing.T) (*session.Session, *recorder, *peer) {
	t.Helper()
	s, rec, l := startInitiator(t, nil)
	p := acceptPeer(t, l)
	p.seq = 4
	p.logon()
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	p.expect("2", "7", "1", "16", "0")
	p.send("X", 0, "262", "0", "268", "1", "279", "0", "269", "1", "55", "BTCCAD", "270", "12000", "271", "1")
	p.send("W", 0, "55", "BTCCAD", "268", "1", "269", "0", "270", "10000", "271", "1", "262", "0")
	p.expectNothing(20 * time.Millisecond)
	if n := len(rec.appValues(35)); n != 0 {
		t.Fatalf("%d messages delivered ahead of the gap", n)
	}
	return s, rec, p
}

// i407Orig is the OrigSendingTime of a SequenceReset, a second ago.
func i407Orig() string {
	return string(fix.AppendUTCTimestamp(nil, time.Now().Add(-time.Second), fix.Milliseconds))
}
