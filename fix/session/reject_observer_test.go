package session_test

import (
	"testing"
	"time"

	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/session"
)

// rejects is a MessageObserver that is a RejectObserver.
type rejects chan session.Rejected

func (rejects) OnInbound(*session.Session, session.Frame)  {}
func (rejects) OnOutbound(*session.Session, session.Frame) {}
func (r rejects) OnRejected(_ *session.Session, x session.Rejected) {
	r <- x
}

func (r rejects) next(t *testing.T) session.Rejected {
	t.Helper()
	select {
	case x := <-r:
		return x
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection observed")
	}
	return session.Rejected{}
}

// A RejectObserver learns whether a message rejected counted as received:
// a SendingTime too far off counts in sequence, the session logging out,
// but not ahead of a gap, where the message is asked for again.
func TestRejectObserver(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seq     int
		counted bool
	}{
		{"in sequence", 2, true},
		{"ahead of a gap", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := make(rejects, 4)
			_, _, p := loggedOn(t, func(c *session.Config) {
				c.MaxLatency = 500 * time.Millisecond
				c.Observer = obs
			})
			p.sendWithHeader("D", "ACC", tc.seq, time.Now().Add(-time.Minute), "11", "STALE")
			r := obs.next(t)
			if r.Seq != uint64(tc.seq) || r.Generation != 1 || r.Reason != dictionary.ReasonSendingTimeAccuracyProblem {
				t.Errorf("observed %+v, want seq %d, generation 1, SendingTime accuracy", r, tc.seq)
			}
			if r.Counted != tc.counted {
				t.Errorf("Counted = %v, want %v", r.Counted, tc.counted)
			}
		})
	}
}

// A Reject the handler asks for is not told: the handler knows. The message
// after it, rejected by the session, is.
func TestRejectObserverNotHandlerRejects(t *testing.T) {
	obs := make(rejects, 4)
	_, _, p := loggedOn(t, func(c *session.Config) {
		c.Handler = rejecting{c.Handler.(*recorder)}
		c.MaxLatency = 500 * time.Millisecond
		c.Observer = obs
	})
	p.send("D", 2, "11", "IN", "54", "7")
	p.expect("3", "45", "2", "373", "5")
	p.sendWithHeader("D", "ACC", 3, time.Now().Add(-time.Minute), "11", "STALE")
	if r := obs.next(t); r.Seq != 3 || !r.Counted {
		t.Errorf("observed %+v, want 3 counted", r)
	}
}

// A message rejected without a valid MsgSeqNum is told, numbered 0, not
// counted.
func TestRejectObserverNoSeq(t *testing.T) {
	obs := make(rejects, 4)
	_, _, p := loggedOn(t, func(c *session.Config) { c.Observer = obs })
	p.writeRaw(rawFrame("35=4|49=ACC|56=INI|34=-1|52="+sendingTime()+"|", ""))
	p.expect("3", "371", "36")
	if r := obs.next(t); r.Seq != 0 || r.Counted {
		t.Errorf("observed %+v, want 0 not counted", r)
	}
}
