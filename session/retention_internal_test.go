package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// ackForTest records an acknowledgement as from a message the counterparty
// sent after the previous one.
func (s *Session) ackForTest(seq uint64, source int) {
	s.ackedLocked(seq, source, s.ret.ackFrom+1)
}

// retentionSession returns a session with retention r, as if it had sent
// messages up to next-1.
func retentionSession(t *testing.T, r Retention, next uint64) *Session {
	t.Helper()
	s := fenceSession(t, func(c *Config) {
		c.Retention = r
		c.MaxPendingMessages, c.MaxHeldMessages = 5, 5
	})
	s.st.NextSender = next
	return s
}

func TestRetention_EvictBefore(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		r     Retention
		acked uint64 // 0 for no acknowledgement
		want  uint64
	}{
		{"no ack", Retention{Keep: 10}, 0, 1},
		{"no ack, max", Retention{Keep: 10, Max: 30}, 0, 71},
		{"ack", Retention{}, 50, 51},
		{"ack within keep", Retention{Keep: 60}, 50, 41},
		{"ack beyond keep", Retention{Keep: 10}, 50, 51},
		{"max over ack", Retention{Keep: 30, Max: 40}, 50, 61},
		{"keep above next", Retention{Keep: 1000}, 50, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.r.Interval = time.Minute
			s := retentionSession(t, tc.r, 101)
			s.mu.Lock()
			defer s.mu.Unlock()
			if tc.acked > 0 {
				s.ackForTest(tc.acked, tagLastMsgSeqNumProcessed)
			}
			if got := s.evictBeforeLocked(now); got != tc.want {
				t.Errorf("evictBefore = %d, want %d", got, tc.want)
			}
		})
	}
}

// The last acknowledgement counts, even lower than an earlier one, but a
// BeginSeqNo never moves it forward; one above our last message is ignored;
// a reset forgets them.
func TestRetention_Acked(t *testing.T) {
	s := retentionSession(t, Retention{Interval: time.Minute}, 101)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, step := range []struct {
		seq    uint64
		source int
		want   uint64
	}{
		{80, tagLastMsgSeqNumProcessed, 81},
		{30, tagBeginSeqNo, 31},
		{50, tagBeginSeqNo, 31},
		{101, tagLastMsgSeqNumProcessed, 31}, // above our last message
		{60, tagNextExpectedMsgSeqNum, 61},
		{70, tagLastMsgSeqNumProcessed, 71},
	} {
		s.ackForTest(step.seq, step.source)
		if got := s.evictBeforeLocked(time.Now()); got != step.want {
			t.Errorf("after %d from %d: evictBefore = %d, want %d", step.seq, step.source, got, step.want)
		}
	}
	s.ret.newGeneration()
	if s.ret.ackKnown || s.ret.floor != 1 {
		t.Errorf("after a reset: %+v", s.ret)
	}
}

// What a replay, running or queued, will read stays.
func TestRetention_KeepsReplays(t *testing.T) {
	s := retentionSession(t, Retention{Interval: time.Minute}, 101)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackForTest(80, tagLastMsgSeqNumProcessed)
	s.c = &conn{replay: &replayJob{begin: 60, end: 70}, replayQueue: []replayJob{{begin: 20, end: 25}}}
	defer func() { s.c = nil }()
	if got := s.evictBeforeLocked(time.Now()); got != 20 {
		t.Errorf("evictBefore = %d, want 20", got)
	}
}

// MinAge keeps the messages stored since the last mark at least MinAge old.
func TestRetention_MinAge(t *testing.T) {
	s := retentionSession(t, Retention{Interval: time.Second, MinAge: 3 * time.Second}, 11)
	s.mu.Lock()
	defer s.mu.Unlock()
	t0 := time.Now()
	steps := []struct {
		next uint64
		want uint64
	}{
		{11, 1},  // t0: no mark old enough
		{21, 1},  // t0+1s
		{31, 1},  // t0+2s
		{41, 11}, // t0+3s: the messages below 11 were stored at t0
		{51, 21},
	}
	for i, st := range steps {
		s.st.NextSender = st.next
		s.ackForTest(st.next-1, tagLastMsgSeqNumProcessed)
		if got := s.evictBeforeLocked(t0.Add(time.Duration(i) * time.Second)); got != st.want {
			t.Errorf("step %d: evictBefore = %d, want %d", i, got, st.want)
		}
	}
	if n := len(s.ret.marks); n > 4 {
		t.Errorf("%d marks kept, want at most 4", n)
	}
}

// Application messages not completely written stay, as do those a
// connection ended before writing, until acknowledged or resent.
func TestRetention_KeepsUnwritten(t *testing.T) {
	s := retentionSession(t, Retention{Interval: time.Minute}, 101)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackForTest(80, tagLastMsgSeqNumProcessed)
	c := &conn{
		frames:     []pendingFrame{{seq: 40, admin: true}, {seq: 45}},
		heldFrames: []pendingFrame{{seq: 60}},
	}
	s.c = c
	defer func() { s.c = nil }()
	if got := s.evictBeforeLocked(time.Now()); got != 45 {
		t.Errorf("pending 45: evictBefore = %d, want 45", got)
	}
	c.writing = 42
	if got := s.evictBeforeLocked(time.Now()); got != 42 {
		t.Errorf("writing 42: evictBefore = %d, want 42", got)
	}

	s.unsentLocked(c)
	s.c = nil
	if got := s.evictBeforeLocked(time.Now()); got != 42 {
		t.Errorf("unsent from 42: evictBefore = %d, want 42", got)
	}
	s.ackForTest(50, tagLastMsgSeqNumProcessed)
	if got := s.evictBeforeLocked(time.Now()); got != 51 {
		t.Errorf("unsent from 42, acked 50: evictBefore = %d, want 51", got)
	}
	s.ackForTest(90, tagLastMsgSeqNumProcessed)
	if got := s.evictBeforeLocked(time.Now()); got != 91 {
		t.Errorf("unsent from 51, acked 90: evictBefore = %d, want 91", got)
	}
	s.ret.deliveredLocked(91, 100)
	s.ackForTest(100, tagLastMsgSeqNumProcessed)
	if got := s.evictBeforeLocked(time.Now()); s.ret.unsentLo != 0 || got != 101 {
		t.Errorf("all delivered: unsent from %d, evictBefore = %d", s.ret.unsentLo, got)
	}
}

// A ResendRequest not completely served keeps its range, through a
// disconnection, until a later one asks for messages after it.
func TestRetention_KeepsAskedFor(t *testing.T) {
	s := retentionSession(t, Retention{Interval: time.Minute, Max: 10}, 101)
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &conn{replay: &replayJob{begin: 60, end: 100, running: true}, replayQueue: []replayJob{{begin: 70, end: 80}}}
	s.unsentLocked(c)
	if got := s.evictBeforeLocked(time.Now()); got != 60 {
		t.Errorf("replay from 60 cut short: evictBefore = %d, want 60", got)
	}
	s.ackForTest(20, tagLastMsgSeqNumProcessed)
	s.ackForTest(75, tagBeginSeqNo) // cannot raise the acknowledgement: only unsent moves
	if s.ret.acked != 20 {
		t.Errorf("acked = %d, want 20", s.ret.acked)
	}
	if got := s.evictBeforeLocked(time.Now()); got != 76 {
		t.Errorf("ResendRequest from 76: evictBefore = %d, want 76", got)
	}
}

// What a reset drops stays if the reset is refused, as does the range of a
// ResendRequest dropped from a full queue.
func TestRetention_KeepsDropped(t *testing.T) {
	s := retentionSession(t, Retention{Interval: time.Minute, Max: 2}, 101)
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &conn{frames: []pendingFrame{{seq: 95}}}
	s.c = c
	defer func() { s.c = nil }()
	s.activeResetLocked(c, nil, Inbound{})
	s.resetting = false // refused at its fence
	if got := s.evictBeforeLocked(time.Now()); got != 95 {
		t.Errorf("reset refused, 95 dropped: evictBefore = %d, want 95", got)
	}
	s.ret = retention{floor: 1}
	c.replay = &replayJob{begin: 90, end: 90}
	for i := range maxQueuedReplays {
		c.replayQueue = append(c.replayQueue, replayJob{begin: uint64(92 + 2*i), end: uint64(92 + 2*i)})
	}
	s.startReplayLocked(c, 50, 60, false)
	c.replay, c.replayQueue = nil, nil
	if got := s.evictBeforeLocked(time.Now()); got != 50 {
		t.Errorf("ResendRequest from 50 dropped: evictBefore = %d, want 50", got)
	}
}

// The batch the writer is writing stays until written, and is noted unsent
// if the connection ends first.
func TestRetention_WriterBatch(t *testing.T) {
	s := fenceSession(t, func(c *Config) { c.Retention = Retention{Interval: time.Hour} })
	a, b := mem.Pipe()
	defer b.Close()
	tc := newHeldConn(a)
	c, err := s.attach(tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.phase = phaseEstablished
	s.queueLocked(&header{msgType: "D"}, nil, false)
	s.mu.Unlock()
	<-tc.entered // the writer holds the order's write
	s.mu.Lock()
	s.ackForTest(1, tagLastMsgSeqNumProcessed)
	if got := s.evictBeforeLocked(time.Now()); got != 1 {
		t.Errorf("order being written: evictBefore = %d, want 1", got)
	}
	s.disconnectLocked(c, errors.New("test"))
	if s.ret.unsentLo != 1 || s.ret.unsentHi != 1 {
		t.Errorf("unsent = %d to %d, want 1 to 1", s.ret.unsentLo, s.ret.unsentHi)
	}
	s.mu.Unlock()
	close(tc.release)
	c.writers.Wait()
}

// Messages are evicted once at least an eighth of what stays can go.
func TestRetention_Batches(t *testing.T) {
	s := fenceSession(t, func(c *Config) { c.Retention = Retention{Interval: time.Hour} })
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	for seq := uint64(1); seq <= 100; seq++ {
		if err := s.store.Append(ctx, s.st.Generation, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	s.st.NextSender = 101
	s.ackForTest(10, tagLastMsgSeqNumProcessed)
	s.evictLocked(time.Now())
	if n := s.stats.Evicted.Load(); n != 0 {
		t.Errorf("10 of 100 acknowledged: Evicted = %d, want 0", n)
	}
	s.ackForTest(20, tagLastMsgSeqNumProcessed)
	s.evictLocked(time.Now())
	if n := s.stats.Evicted.Load(); n != 20 {
		t.Errorf("20 of 100 acknowledged: Evicted = %d, want 20", n)
	}
}

// brokenAfter fails every Write after the first n.
type brokenAfter struct {
	transport.Conn
	mu sync.Mutex
	n  int
}

func (c *brokenAfter) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		return 0, errors.New("broken")
	}
	c.n--
	return len(b), nil
}

// Messages beyond retention a GapFill covers are lost once it is written,
// even if a later write of the replay fails.
func TestRetention_LossBeforeFailedWrite(t *testing.T) {
	s := fenceSession(t, func(c *Config) { c.Retention = Retention{Interval: time.Hour} })
	a, b := mem.Pipe()
	defer b.Close()
	c, err := s.attach(&brokenAfter{Conn: a, n: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	big := fix2.AppendTagValue(nil, 58, strings.Repeat("x", 40000))
	s.mu.Lock()
	s.phase = phaseEstablished
	for seq := uint64(1); seq <= 8; seq++ {
		raw := s.appendFrame(nil, &header{msgType: "D", seq: seq, sendingTime: time.Now()}, big)
		if err := s.store.Append(ctx, s.st.Generation, seq, raw); err != nil {
			t.Fatal(err)
		}
	}
	s.st.NextSender = 9
	if err := s.store.(interface {
		Truncate(context.Context, uint64, uint64) error
	}).Truncate(ctx, s.st.Generation, 5); err != nil {
		t.Fatal(err)
	}
	// The first write: the GapFill of 1 to 4, then 5 and 6; the second, 7
	// and 8, fails.
	s.startReplayLocked(c, 1, 8, false)
	s.mu.Unlock()
	<-c.done()
	c.writers.Wait()
	if n := s.stats.BeyondRetention.Load(); n != 4 {
		t.Errorf("BeyondRetention = %d, want 4", n)
	}
}

// An acknowledgement from a message the counterparty sent before the one
// that told the last counts no more, as when a message kept ahead of a gap
// is handled after a ResendRequest that followed it.
func TestRetention_AckOrder(t *testing.T) {
	s := retentionSession(t, Retention{Interval: time.Minute}, 101)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackedLocked(80, tagLastMsgSeqNumProcessed, 10)
	s.ackedLocked(30, tagBeginSeqNo, 12)
	s.ackedLocked(90, tagLastMsgSeqNumProcessed, 11)
	if got := s.evictBeforeLocked(time.Now()); got != 31 {
		t.Errorf("evictBefore = %d, want 31", got)
	}
}

// The messages a store holds when the session starts may never have been
// written: they stay until acknowledged.
func TestRetention_HoldsStored(t *testing.T) {
	st := memstore.New()
	ctx := context.Background()
	open, err := st.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 100; seq++ {
		if err := st.Append(ctx, open.Generation, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	s := fenceSession(t, func(c *Config) {
		c.Store = st
		c.Retention = Retention{Interval: time.Minute, Max: 10}
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := s.evictBeforeLocked(time.Now()); got != 1 {
		t.Errorf("after a restart: evictBefore = %d, want 1", got)
	}
	s.ackForTest(50, tagNextExpectedMsgSeqNum)
	if got := s.evictBeforeLocked(time.Now()); got != 51 {
		t.Errorf("acknowledged up to 50: evictBefore = %d, want 51", got)
	}
}
