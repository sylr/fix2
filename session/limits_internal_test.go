package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/transport/mem"
)

// A message fits the pending queue only if the bytes waiting and its own
// stay within MaxPendingBytes, or if nothing is waiting.
func TestHasRoom(t *testing.T) {
	c := &conn{s: &Session{cfg: Config{MaxPendingMessages: 2, MaxPendingBytes: 100}}}
	if !c.hasRoom(1000) {
		t.Error("nothing waiting: no room for a message bigger than MaxPendingBytes")
	}
	c.pending = make([]byte, 90)
	c.frames = []pendingFrame{{end: 90, seq: 1}}
	if !c.hasRoom(10) {
		t.Error("90 bytes waiting: no room for 10 more")
	}
	if c.hasRoom(11) {
		t.Error("90 bytes waiting: room for 11 more")
	}
	c.pending = make([]byte, 10)
	c.frames = []pendingFrame{{end: 5, seq: 1}, {end: 10, seq: 2}}
	if c.hasRoom(1) {
		t.Error("MaxPendingMessages waiting: room for another")
	}
}

// limitSession returns an established session with MaxPendingBytes 100 on
// a connection that writes nothing, its writer not started.
func limitSession(t *testing.T) (*Session, *conn) {
	t.Helper()
	s := fenceSession(t, func(c *Config) { c.MaxPendingBytes, c.FailWhenFull = 100, true })
	a, b := mem.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	c, err := s.attach(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.phase = phaseEstablished
	s.mu.Unlock()
	return s, c
}

// order returns an order whose body is about n bytes.
func sizedOrder(n int) *fix2.Builder {
	return fix2.NewBuilder(FIX44, "D").SetString(11, "O").SetString(58, strings.Repeat("x", n))
}

// Send fails when the message does not fit what is waiting to be written.
func TestSend_PendingBytesStrict(t *testing.T) {
	s, c := limitSession(t)
	if _, err := s.Send(t.Context(), sizedOrder(500)); err != nil {
		t.Fatalf("nothing waiting, a message bigger than MaxPendingBytes: %v", err)
	}
	s.mu.Lock()
	c.pending, c.frames = make([]byte, 90), []pendingFrame{{end: 90, seq: 1}}
	s.mu.Unlock()
	if _, err := s.Send(t.Context(), sizedOrder(20)); !errors.Is(err, ErrSlowConsumer) {
		t.Errorf("90 bytes waiting, 20 more: %v, want %v", err, ErrSlowConsumer)
	}
}

// Send fails when the message does not fit what is held back during a
// replay.
func TestSend_HeldBytes(t *testing.T) {
	s, c := limitSession(t)
	s.mu.Lock()
	c.replay = &replayJob{begin: 1, end: 1}
	s.mu.Unlock()
	if _, err := s.Send(t.Context(), sizedOrder(500)); err != nil {
		t.Fatalf("nothing held, a message bigger than MaxPendingBytes: %v", err)
	}
	if _, err := s.Send(t.Context(), sizedOrder(20)); !errors.Is(err, ErrReplayInProgress) {
		t.Errorf("a message held, 20 bytes more: %v, want %v", err, ErrReplayInProgress)
	}
}

// The stash counts the bytes it keeps, within MaxStashBytes, one message
// whatever its size.
func TestStashBytes(t *testing.T) {
	s := fenceSession(t, func(c *Config) { c.MaxStashBytes = 100 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stashLocked(1, make([]byte, 150), true) {
		t.Error("an empty stash refused a message bigger than MaxStashBytes")
	}
	if s.stashLocked(2, make([]byte, 1), true) {
		t.Error("a full stash kept another message")
	}
	s.unstashLocked(1)
	if s.stashSize != 0 {
		t.Errorf("stashSize = %d after unstashing, want 0", s.stashSize)
	}
	for seq, n := range map[uint64]int{3: 60, 4: 40} {
		if !s.stashLocked(seq, make([]byte, n), true) {
			t.Errorf("message %d of %d bytes refused", seq, n)
		}
	}
	if s.stashLocked(5, make([]byte, 1), true) {
		t.Error("100 bytes kept: another byte kept")
	}
	s.clearStashLocked()
	if s.stashSize != 0 || len(s.stash) != 0 {
		t.Errorf("after clearing: %d bytes, %d messages", s.stashSize, len(s.stash))
	}
}

// The delivery queue is full at DeliveryQueue items or DeliveryQueueBytes
// of frames, one item whatever its size.
func TestDeliveryFull(t *testing.T) {
	s := &Session{cfg: Config{DeliveryQueue: 3, DeliveryQueueBytes: 100}}
	d := &delivery{s: s, wake: make(chan struct{}, 1)}
	if d.full() {
		t.Error("empty queue full")
	}
	d.push(item{kind: itemApp, frame: make([]byte, 150)})
	if !d.full() {
		t.Error("150 bytes queued: not full")
	}
	d = &delivery{s: s, wake: make(chan struct{}, 1)}
	d.push(item{kind: itemApp, frame: make([]byte, 60)})
	if d.full() {
		t.Error("60 bytes queued: full")
	}
	d.push(item{kind: itemApp, frame: make([]byte, 40)})
	if !d.full() {
		t.Error("100 bytes queued: not full")
	}
	d = &delivery{s: s, wake: make(chan struct{}, 1)}
	for range 3 {
		d.push(item{kind: itemMark})
	}
	if !d.full() {
		t.Error("DeliveryQueue items queued: not full")
	}
}

// The bytes of the messages delivered leave the queue's count.
func TestDeliverySizeDrained(t *testing.T) {
	s, _, _ := replaySession(t, nil)
	s.mu.Lock()
	for seq := uint64(1); seq <= 5; seq++ {
		frame := s.appendFrame(nil, &header{msgType: "D", seq: seq, sendingTime: time.Now()}, adminBody(nil).str(11, "O"))
		s.deliv.push(item{kind: itemApp, gen: s.st.Generation, seq: seq, frame: s.deliv.copyFrame(frame)})
	}
	s.mu.Unlock()
	select {
	case <-s.deliv.waitIdle():
	case <-time.After(5 * time.Second):
		t.Fatal("the queue never drained")
	}
	s.deliv.mu.Lock()
	defer s.deliv.mu.Unlock()
	if s.deliv.size != 0 {
		t.Errorf("size = %d once delivered, want 0", s.deliv.size)
	}
}

// A message handled already keeps a marker within MaxStash whatever
// MaxStashBytes, and without one nothing is kept in its place.
func TestStashMarkers(t *testing.T) {
	s, c, _ := replaySession(t, func(c *Config) { c.MaxStash, c.MaxStashBytes = 2, 100 })
	m := new(fix2.Message)
	if err := fix2.ParseInto(m, s.appendFrame(nil, &header{msgType: "2", seq: 9, sendingTime: time.Now()},
		adminBody(nil).int(tagBeginSeqNo, 1).int(tagEndSeqNo, 0)), nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.NextTarget = 2
	s.stashLocked(3, make([]byte, 200), true) // MaxStashBytes taken
	s.markAheadLocked(4)
	s.tooHighLocked(c, m, Inbound{Seq: 4}, nil)
	if marker := s.stash[4]; len(marker) == 0 {
		t.Errorf("message 4 handled: stash holds %q, want its marker", marker)
	}
	s.markAheadLocked(5) // MaxStash taken
	s.tooHighLocked(c, m, Inbound{Seq: 5}, nil)
	if frame, ok := s.stash[5]; ok {
		t.Errorf("message 5 handled, no room for its marker: stash holds %q", frame)
	}
}

// Rejections are kept for the last messages sent only, their Text cut.
func TestRejectionsBounded(t *testing.T) {
	var r receipts
	r.init(1<<20, 1)
	r.rejected(1, 50, 50, RejectInfo{Text: "not sent yet"})
	r.rejected(1, 1, maxRejections+2, RejectInfo{Text: "too old"})
	if len(r.rejections) != 0 {
		t.Errorf("kept %v, want none", r.rejections)
	}
	next := uint64(3 * maxRejections)
	for seq := next - 2*maxRejections; seq < next; seq++ {
		r.rejected(1, seq, next, RejectInfo{})
	}
	if n := len(r.rejections); n > maxRejections+1 {
		t.Errorf("%d rejections kept, want at most %d", n, maxRejections+1)
	}

	text := strings.Repeat("x", maxRejectText-1) + "é" + strings.Repeat("y", 100)
	m := new(fix2.Message)
	s := fenceSession(t, nil)
	if err := fix2.ParseInto(m, s.appendFrame(nil, &header{msgType: "3", seq: 1, sendingTime: time.Now()},
		adminBody(nil).int(tagRefSeqNum, 1).str(tagText, text)), nil); err != nil {
		t.Fatal(err)
	}
	if got := rejectInfo(m).Text; got != strings.Repeat("x", maxRejectText-1) {
		t.Errorf("Text of %d bytes kept, want the %d before the character cut", len(got), maxRejectText-1)
	}
}

// A buffer one oversized message grew is not kept for reuse.
func TestReusable(t *testing.T) {
	if b := reusable(make([]byte, 10, 150), 100); b == nil || len(b) != 0 {
		t.Errorf("a 150-byte buffer, MaxPendingBytes 100: %v, want it emptied", b)
	}
	if b := reusable(make([]byte, 10, 300), 100); b != nil {
		t.Errorf("a 300-byte buffer, MaxPendingBytes 100: kept")
	}
}
