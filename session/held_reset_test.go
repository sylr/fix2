package session_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport/mem"
)

// eventRecorder records, in the order the handler is told, the ClOrdID of
// each application message ("app ID") and each range reported lost ("loss
// B-E").
type eventRecorder struct {
	*recorder
	events []string // guarded by recorder.mu
}

func (r *eventRecorder) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	r.mu.Lock()
	r.events = append(r.events, "app "+string(m.Get(11)))
	r.mu.Unlock()
	return r.recorder.FromApp(ctx, s, m, in)
}

func (r *eventRecorder) OnRecoveryLoss(_ context.Context, _ *session.Session, begin, end uint64, _ error) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf("loss %d-%d", begin, end))
	r.mu.Unlock()
	r.got <- struct{}{}
}

// waitEvents waits for want, then checks nothing else came.
func (r *eventRecorder) waitEvents(t *testing.T, want ...string) {
	t.Helper()
	r.wait(t, func() bool { return len(r.events) >= len(want) })
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Equal(r.events, want) {
		t.Errorf("handler told %q, want %q", r.events, want)
	}
}

// heldGap logs an initiator on to a peer whose Logon comes at logonSeq, ahead
// of a gap the initiator asks for.
func heldGap(t *testing.T, logonSeq int, change func(*session.Config)) (*session.Session, *eventRecorder, *peer) {
	t.Helper()
	l := mem.Listen("acceptor")
	rec := &eventRecorder{recorder: newRecorder()}
	s, _ := startInitiator(t, l, func(c *session.Config) {
		c.Handler = rec
		if change != nil {
			change(c)
		}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", logonSeq, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	p.expect("2", "7", "1", "16", "0")
	return s, rec, p
}

// order sends a NewOrderSingle with ClOrdID id at seq.
func (p *peer) order(seq int, id string) {
	p.t.Helper()
	p.send("D", seq, "11", id, "55", "X")
}

// resetTo sends a SequenceReset-Reset to newSeqNo at seq.
func (p *peer) resetTo(seq, newSeqNo int) {
	p.t.Helper()
	p.send("4", seq, "36", fmt.Sprint(newSeqNo))
}

func origTime() string {
	return string(fix2.AppendUTCTimestamp(nil, time.Now().Add(-time.Second), fix2.Milliseconds))
}

// A Reset past messages held ahead of several gaps delivers them in
// sequence, reporting only the gaps lost, one range each.
func TestResetPastHeld_SeveralGaps(t *testing.T) {
	s, rec, p := heldGap(t, 3, nil)
	p.order(4, "A")
	p.order(6, "B")
	p.order(9, "C")
	p.resetTo(10, 11)
	rec.waitEvents(t, "loss 1-2", "app A", "loss 5-5", "app B", "loss 7-8", "app C")
	if n := s.NextTargetSeqNum(); n != 11 {
		t.Errorf("NextTargetSeqNum = %d, want 11", n)
	}
}

// Held administrative messages are processed, not delivered, nor reported
// lost; nor is the Reset's own MsgSeqNum.
func TestResetPastHeld_AdminMessages(t *testing.T) {
	s, rec, p := heldGap(t, 3, nil)
	p.send("0", 4)
	p.order(5, "A")
	p.send("0", 6)
	p.resetTo(7, 8)
	rec.waitEvents(t, "loss 1-2", "app A")
	if n := s.NextTargetSeqNum(); n != 8 {
		t.Errorf("NextTargetSeqNum = %d, want 8", n)
	}
	p.order(8, "B")
	rec.waitEvents(t, "loss 1-2", "app A", "app B")
}

// A Reset into the held range delivers those below its NewSeqNo, then those
// from it in sequence.
func TestResetPastHeld_IntoHeldRange(t *testing.T) {
	s, rec, p := heldGap(t, 3, nil)
	p.order(4, "A")
	p.order(5, "B")
	p.order(6, "C")
	p.resetTo(7, 5)
	rec.waitEvents(t, "loss 1-2", "app A", "app B", "app C")
	if n := s.NextTargetSeqNum(); n != 7 {
		t.Errorf("NextTargetSeqNum = %d, want 7", n)
	}
}

// A Reset to the first message held gives up the gap before it only.
func TestResetPastHeld_ToFirstHeld(t *testing.T) {
	s, rec, p := heldGap(t, 3, nil)
	p.order(4, "A")
	p.order(5, "B")
	p.resetTo(6, 3)
	rec.waitEvents(t, "loss 1-2", "app A", "app B")
	if n := s.NextTargetSeqNum(); n != 6 {
		t.Errorf("NextTargetSeqNum = %d, want 6", n)
	}
}

// A Reset to the MsgSeqNum expected changes nothing: the messages held wait
// for the gap to be filled, nothing is lost.
func TestResetPastHeld_ToExpected(t *testing.T) {
	s, rec, p := heldGap(t, 3, nil)
	p.order(4, "A")
	p.resetTo(5, 1)
	time.Sleep(20 * time.Millisecond)
	if n := s.NextTargetSeqNum(); n != 1 {
		t.Errorf("NextTargetSeqNum = %d, want 1", n)
	}
	p.send("4", 1, "43", "Y", "122", origTime(), "123", "Y", "36", "3")
	rec.waitEvents(t, "app A")
	if n := s.NextTargetSeqNum(); n != 5 {
		t.Errorf("NextTargetSeqNum = %d, want 5", n)
	}
}

// A message dropped for want of room among those held is reported lost.
func TestResetPastHeld_DroppedForRoom(t *testing.T) {
	_, rec, p := heldGap(t, 3, func(c *session.Config) { c.MaxStash = 2 })
	p.order(4, "A")
	p.order(5, "B") // no room: the Logon's marker and A fill it
	p.order(6, "C")
	p.resetTo(7, 8)
	rec.waitEvents(t, "loss 1-2", "app A", "loss 5-6")
}

// The messages delivered are not delivered again when the counterparty
// resends them after all.
func TestResetPastHeld_NoDuplicates(t *testing.T) {
	_, rec, p := heldGap(t, 3, nil)
	p.order(4, "A")
	p.order(5, "B")
	p.resetTo(6, 7)
	rec.waitEvents(t, "loss 1-2", "app A", "app B")
	p.send("D", 4, "43", "Y", "122", origTime(), "11", "A", "55", "X")
	p.send("D", 5, "43", "Y", "122", origTime(), "11", "B", "55", "X")
	p.order(7, "C")
	rec.waitEvents(t, "loss 1-2", "app A", "app B", "app C")
}

// Held messages more numerous than the delivery queue holds are all
// delivered, in order.
func TestResetPastHeld_SmallDeliveryQueue(t *testing.T) {
	_, rec, p := heldGap(t, 3, func(c *session.Config) { c.DeliveryQueue = 2 })
	want := []string{"loss 1-2"}
	for i := range 20 {
		id := fmt.Sprint("O", i)
		p.order(4+i, id)
		want = append(want, "app "+id)
	}
	p.resetTo(24, 25)
	rec.waitEvents(t, want...)
}
