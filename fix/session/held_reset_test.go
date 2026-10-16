package session_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// eventRecorder records, in the order the handler is told, the ClOrdID of
// each application message ("app ID") and each range reported lost ("loss
// B-E").
type eventRecorder struct {
	*recorder
	events []string // guarded by recorder.mu
}

func (r *eventRecorder) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
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
	return string(fix.AppendUTCTimestamp(nil, time.Now().Add(-time.Second), fix.Milliseconds))
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

// targetLog is a Store recording the NextTarget values it is set to.
type targetLog struct {
	*memstore.Store
	mu   sync.Mutex
	sets []uint64
}

func (st *targetLog) SetNextTarget(ctx context.Context, gen, next uint64) error {
	st.mu.Lock()
	st.sets = append(st.sets, next)
	st.mu.Unlock()
	return st.Store.SetNextTarget(ctx, gen, next)
}

// highest returns the highest NextTarget set so far.
func (st *targetLog) highest() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Max(append([]uint64{0}, st.sets...))
}

// lossGate is a Handler whose OnRecoveryLoss waits for release.
type lossGate struct {
	*recorder
	entered, release chan struct{}
}

func (h *lossGate) OnRecoveryLoss(context.Context, *session.Session, uint64, uint64, error) {
	close(h.entered)
	<-h.release
}

// The messages a Reset gives up are not recorded as processed before
// OnRecoveryLoss has told of them: a crash in between would lose the
// notification for good.
func TestResetPastHeld_LossBeforeCommit(t *testing.T) {
	st := &targetLog{Store: memstore.New()}
	h := &lossGate{recorder: newRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	l := mem.Listen("acceptor")
	s, _ := startInitiator(t, l, func(c *session.Config) { c.Handler = h; c.Store = st })
	released := false
	t.Cleanup(func() {
		if !released {
			close(h.release)
		}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 101, "98", "0", "108", "30")
	h.wait(t, func() bool { return h.logons == 1 })
	p.expect("2", "7", "1", "16", "0")
	p.resetTo(102, 102)
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("OnRecoveryLoss not called")
	}
	waitFor(t, func() bool { return s.NextTargetSeqNum() == 102 })
	if n := st.highest(); n > 1 {
		t.Errorf("NextTarget %d recorded before OnRecoveryLoss(1, 100) returned", n)
	}
	released = true
	close(h.release)
	waitFor(t, func() bool { return st.highest() == 102 })
}

// recordingFails logs an initiator recording through fs on to a peer whose
// Logon comes at 3, ahead of a gap, sends it n orders, then makes recording
// fail and resets past them all: the connection drops amid their delivery.
// It returns once the handler is told.
func recordingFails(t *testing.T, n int, change func(*session.Config)) (*session.Session, *eventRecorder, *targetStore, *mem.Listener) {
	t.Helper()
	fs := &targetStore{Store: memstore.New(), err: errors.New("disk full")}
	l := mem.Listen("acceptor")
	rec := &eventRecorder{recorder: newRecorder()}
	s, _ := startInitiator(t, l, func(c *session.Config) {
		c.Store = fs
		c.Handler = rec
		c.DeliveryQueue = 2
		if change != nil {
			change(c)
		}
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 3, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	p.expect("2", "7", "1", "16", "0")
	for i := range n {
		p.order(4+i, fmt.Sprint("O", i))
	}
	fs.fail.Store(true)
	p.resetTo(n+4, n+5)
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	return s, rec, fs, l
}

// expectHeld checks that rec was told of no more than a batch of the
// orders recordingFails sends: OnLogout goes past the messages held, and
// once it was told, what was processed before the failure is all there is
// until recording works again.
func expectHeld(t *testing.T, rec *eventRecorder) {
	t.Helper()
	const batch = 64
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n := len(rec.apps); n > batch {
		t.Errorf("%d orders processed while recording failed, want at most %d", n, batch)
	}
}

// heldEvents are what the handler is told of the n orders recordingFails
// sends, then of extra.
func heldEvents(n int, extra ...string) []string {
	want := []string{"loss 1-2"}
	for i := range n {
		want = append(want, fmt.Sprint("app O", i))
	}
	return append(want, extra...)
}

// A failure to record the messages processed holds the delivery of the
// others, those a Reset releases from the held ones included, past a batch.
// They were received, and are not asked for again: once recording works
// again, and the counterparty reconnects, they are delivered, whatever it
// does with the messages that are asked for.
func TestResetPastHeld_RecordingFails(t *testing.T) {
	const n = 200
	for _, tc := range []struct {
		name   string
		answer func(p *peer) // to our ResendRequest for 205 to 209
		extra  []string
	}{
		{"counterparty resends", func(p *peer) {
			for seq := n + 5; seq < n+10; seq++ {
				p.send("D", seq, "11", fmt.Sprint("R", seq), "55", "X", "43", "Y", "122", origTime())
			}
		}, []string{"app R205", "app R206", "app R207", "app R208", "app R209"}},
		{"counterparty gap-fills", func(p *peer) {
			p.send("4", n+5, "123", "Y", "36", fmt.Sprint(n+10), "43", "Y", "122", origTime())
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec, fs, l := recordingFails(t, n, nil)
			expectHeld(t, rec)
			if next := s.NextTargetSeqNum(); next != n+5 {
				t.Errorf("NextTargetSeqNum = %d, want %d: the orders received are not asked for again", next, n+5)
			}
			// No connection until recording works again.
			p2 := acceptPeer(t, l)
			p2.expectSilentEnd()
			fs.fail.Store(false)
			p3 := acceptPeer(t, l)
			p3.expect("A")
			// Recorded, they are delivered.
			rec.wait(t, func() bool { return len(rec.apps) == n })
			// The counterparty sent 205 to 209 meanwhile, which were lost.
			p3.send("A", n+10, "98", "0", "108", "30")
			p3.expect("2", "7", fmt.Sprint(n+5), "16", "0")
			tc.answer(p3)
			p3.order(n+11, "AFTER")
			want := heldEvents(n, append(tc.extra, "app AFTER")...)
			rec.waitEvents(t, want...)
			waitFor(t, func() bool { return fs.persisted() == uint64(n+12) })
		})
	}
}

// A sequence reset after a failure to record the messages processed
// delivers those held, of the generation reset: the counterparty will not
// send them again.
func TestResetPastHeld_RecordingFailsThenReset(t *testing.T) {
	const n = 200
	t.Run("ResetOnDisconnect", func(t *testing.T) {
		_, rec, fs, l := recordingFails(t, n, func(c *session.Config) { c.ResetOnDisconnect = true })
		rec.wait(t, func() bool { return len(rec.apps) == n })
		fs.fail.Store(false)
		p := acceptPeer(t, l)
		p.expect("A", "34", "1")
		p.send("A", 1, "98", "0", "108", "30")
		p.order(2, "AFTER")
		rec.waitEvents(t, heldEvents(n, "app AFTER")...)
		waitFor(t, func() bool { return fs.persisted() == 3 })
	})
	t.Run("counterparty's ResetSeqNumFlag", func(t *testing.T) {
		_, rec, fs, l := recordingFails(t, n, nil)
		expectHeld(t, rec)
		fs.fail.Store(false)
		p := acceptPeer(t, l)
		p.expect("A")
		p.send("A", 1, "98", "0", "108", "30", "141", "Y")
		p.order(2, "AFTER")
		rec.waitEvents(t, heldEvents(n, "app AFTER")...)
		waitFor(t, func() bool { return fs.persisted() == 3 })
	})
}

// Stopping a session whose recording fails delivers the messages held
// behind the failure: they may be delivered again after a restart, not
// lost.
func TestResetPastHeld_StopWhileRecordingFails(t *testing.T) {
	const n = 200
	s, rec, _, _ := recordingFails(t, n, nil)
	expectHeld(t, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Stop(ctx) }()
	rec.waitEvents(t, heldEvents(n)...)
	if err := <-done; err != nil {
		t.Logf("Stop = %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed")
	}
}

// A Reset past many messages held between gaps looks at each once.
func TestResetPastHeld_ScanBounded(t *testing.T) {
	const n = 1000
	s, rec, p := heldGap(t, 2, nil)
	// Held at 4, 6, ..., after the Logon's marker at 2: a gap before each.
	for i := range n {
		p.order(4+2*i, fmt.Sprint("O", i))
	}
	p.resetTo(2*n+3, 2*n+4)
	rec.wait(t, func() bool { return len(rec.apps) == n })
	if got, bound := session.HeldScanned(s), uint64(2*(n+1)); got > bound {
		t.Errorf("%d held messages looked at for %d held, want at most %d", got, n+1, bound)
	}
}

// A message dropped for want of bytes among those held is reported lost;
// one after it that fits is kept.
func TestResetPastHeld_DroppedForBytes(t *testing.T) {
	_, rec, p := heldGap(t, 3, func(c *session.Config) { c.MaxStashBytes = 400 })
	p.order(4, "A")
	p.send("D", 5, "11", "B", "55", "X", "58", strings.Repeat("x", 500)) // no room for its bytes
	p.order(6, "C")
	p.resetTo(7, 8)
	rec.waitEvents(t, "loss 1-2", "app A", "loss 5-5", "app C")
}
