package session

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// heldConn holds its first Write until release, recording every write.
type heldConn struct {
	transport.Conn
	entered, release chan struct{}
	once             sync.Once
	mu               sync.Mutex
	writes           [][]byte
}

func newHeldConn(c transport.Conn) *heldConn {
	return &heldConn{Conn: c, entered: make(chan struct{}), release: make(chan struct{})}
}

func (c *heldConn) Write(b []byte) (int, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	c.mu.Lock()
	c.writes = append(c.writes, bytes.Clone(b))
	c.mu.Unlock()
	return len(b), nil
}

func (c *heldConn) written() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func fenceSession(t *testing.T, change func(*Config)) *Session {
	t.Helper()
	cfg := Config{ID: SessionID{BeginString: FIX44, SenderCompID: "A", TargetCompID: "B"}, Handler: BaseHandler{}}
	if change != nil {
		change(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.Stop(ctx)
	})
	return s
}

// A probe the old writer's last write triggers is old-generation traffic:
// the first message after the reset is the Logon at sequence number 1.
func TestFence_NoStaleProbeAfterReset(t *testing.T) {
	s := fenceSession(t, func(c *Config) { c.AckProbe = AckProbeEager })
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
	s.activeResetLocked(c, nil, Inbound{})
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.fenceReset(c); close(done) }()
	for {
		s.mu.Lock()
		stopping := c.writerStop
		s.mu.Unlock()
		if stopping {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(tc.release)
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for len(tc.written()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("nothing written after the reset")
		}
		time.Sleep(time.Millisecond)
	}
	f := fix.NewFramer(bytes.NewReader(tc.written()[1]), fix.FramerOptions{})
	raw, err := f.Next()
	if err != nil {
		t.Fatal(err)
	}
	m, err := fix.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.MsgType()) != "A" || string(m.Get(34)) != "1" {
		t.Errorf("first message after the reset: MsgType %s seq %s, want the Logon at 1", m.MsgType(), m.Get(34))
	}
}

// A new connection waits for the previous one to be done, so a reset on
// disconnection, made once its writer is joined, cannot hit the new one.
func TestFence_NewConnectionWaitsForPrevious(t *testing.T) {
	s := fenceSession(t, func(c *Config) { c.ResetOnDisconnect = true })
	a, b := mem.Pipe()
	tc := newHeldConn(a)
	c, err := s.attach(tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.phase = phaseEstablished
	s.queueLocked(&header{msgType: "D"}, nil, false)
	gen := s.st.Generation
	s.mu.Unlock()
	<-tc.entered
	ran := make(chan struct{})
	go func() { c.run(); close(ran) }()
	b.Close()
	<-c.done()

	attached := make(chan *conn, 1)
	a2, b2 := mem.Pipe()
	defer b2.Close()
	go func() {
		c2, err := s.attach(a2, nil)
		if err != nil {
			t.Error(err)
		}
		attached <- c2
	}()
	select {
	case <-attached:
		t.Fatal("attached while the previous connection's writer still runs")
	case <-time.After(50 * time.Millisecond):
	}
	close(tc.release)
	<-ran
	c2 := <-attached
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Generation != gen+1 || s.st.NextSender != 1 || s.c != c2 {
		t.Errorf("generation %d (was %d), next sender %d: want the reset made before the new connection", s.st.Generation, gen, s.st.NextSender)
	}
}

// Removing frames from the pending queue keeps the others intact.
func TestRemovePending(t *testing.T) {
	c := &conn{s: &Session{cfg: Config{MaxPendingMessages: 10, MaxPendingBytes: 100}}}
	c.pending = []byte("AAABBBBCC")
	c.frames = []pendingFrame{{end: 3, seq: 1}, {end: 7, seq: 2}, {end: 9, seq: 3}}
	c.removePendingLocked(1, 1)
	if string(c.pending) != "BBBBCC" || len(c.frames) != 2 || c.frames[0].end != 4 || c.frames[1].end != 6 {
		t.Errorf("after removing 1: %q %+v", c.pending, c.frames)
	}
	c.removePendingLocked(2, 2)
	if string(c.pending) != "CC" || len(c.frames) != 1 || c.frames[0].end != 2 || c.frames[0].seq != 3 {
		t.Errorf("after removing 2: %q %+v", c.pending, c.frames)
	}
}

// Session-level frames stay when their range is replayed: the replay only
// gap-fills them.
func TestRemovePendingKeepsAdmin(t *testing.T) {
	c := &conn{s: &Session{cfg: Config{MaxPendingMessages: 10, MaxPendingBytes: 100}}}
	c.pending = []byte("AAABBBBCC")
	c.frames = []pendingFrame{{end: 3, seq: 1}, {end: 7, seq: 2, admin: true}, {end: 9, seq: 3}}
	c.removePendingLocked(1, 3)
	if string(c.pending) != "BBBB" || len(c.frames) != 1 || c.frames[0].end != 4 || c.frames[0].seq != 2 {
		t.Errorf("after removing 1 to 3: %q %+v", c.pending, c.frames)
	}
}

// A GapFill moving straight past messages kept ahead of the gap drops them.
func TestAdvancePrunesAfterJump(t *testing.T) {
	s := fenceSession(t, nil)
	c := &conn{s: s}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c = c
	s.st.NextTarget = 2
	s.stash = map[uint64]keptMsg{5: {frame: []byte("x")}}
	s.advanceLocked(c, 6)
	if len(s.stash) != 0 {
		t.Errorf("stash after the jump: %v", s.stash)
	}
	s.c = nil
}
