package session

import (
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
)

// A message kept ahead of a gap whose header fails when its turn comes, and
// which logs out counted, leaves those kept after it unprocessed and
// uncounted: they are asked for again once logged on (see
// checkHeaderLocked). Kept with the time it was received, one whose CompIDs
// and SendingTime were checked then cannot fail them since: the stash is
// filled here, with a SenderCompID that is wrong.
func TestHeldLogoutStopsDrain(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill func(s *Session, c *conn)
	}{
		{"missing message", func(s *Session, c *conn) { s.advanceLocked(c, 3) }},
		{"reset past them", func(s *Session, c *conn) { s.resetPastLocked(c, 6) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, _ := replaySession(t, nil)
			received := time.Now()
			frame := func(sender string, seq int, msgType string) []byte {
				return fix2.NewBuilder(FIX44, msgType).SetString(tagSenderCompID, sender).SetString(tagTargetCompID, "A").
					SetInt(tagMsgSeqNum, seq).SetString(tagSendingTime, string(fix2.AppendUTCTimestamp(nil, received, fix2.Milliseconds))).
					SetString(11, "MUST-STAY-HELD").Build()
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.st.NextTarget = 2
			s.keepLocked(3, keptMsg{frame: frame("X", 3, "0"), received: received})
			s.keepLocked(4, keptMsg{frame: frame("B", 4, "D"), received: received})
			tc.fill(s, c)
			if s.phase != phaseLogoutSent {
				t.Errorf("phase = %s, want %s", s.phase, phaseLogoutSent)
			}
			if s.st.NextTarget != 4 {
				t.Errorf("NextTarget = %d, want 4: the message with the wrong CompID counted, the one after it not", s.st.NextTarget)
			}
			if _, ok := s.stash[4]; !ok {
				t.Error("the message kept after the one logging out was processed")
			}
		})
	}
}
