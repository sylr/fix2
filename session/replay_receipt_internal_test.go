package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// vetoHandler does not resend the orders whose ClOrdID is VETO: a replay
// gap-fills them.
type vetoHandler struct{ BaseHandler }

func (vetoHandler) OnResend(_ context.Context, _ *Session, _ uint64, m *fix2.Message) bool {
	return string(m.Get(11)) != "VETO"
}

// failingConn writes nothing anywhere. Once armed, it accepts left writes
// more, then fails every one: with nothing written, or short, with all of
// it but the last byte.
type failingConn struct {
	transport.Conn
	mu    sync.Mutex
	armed bool
	left  int
	short bool
}

func (c *failingConn) arm(left int, short bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.left, c.short = true, left, short
}

func (c *failingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed || c.left > 0 {
		c.left--
		return len(b), nil
	}
	if c.short {
		return len(b) - 1, errors.New("broken")
	}
	return 0, errors.New("broken")
}

// replaySession returns an established session vetoing resends, on a
// failingConn, its config changed by change if not nil.
func replaySession(t *testing.T, change func(*Config)) (*Session, *conn, *failingConn) {
	t.Helper()
	s := fenceSession(t, func(c *Config) {
		c.Handler = vetoHandler{}
		if change != nil {
			change(c)
		}
	})
	a, b := mem.Pipe()
	t.Cleanup(func() { b.Close() })
	fc := &failingConn{Conn: a}
	c, err := s.attach(fc, nil)
	if err != nil {
		t.Fatal(err)
	}
	go c.run()
	s.mu.Lock()
	s.phase = phaseEstablished
	s.mu.Unlock()
	return s, c, fc
}

// sendWritten sends an order with ClOrdID id, and body text if not empty,
// and waits for it to be written.
func sendWritten(t *testing.T, s *Session, id, text string) Receipt {
	t.Helper()
	b := fix2.NewBuilder(FIX44, "D").SetString(11, id)
	if text != "" {
		b.SetString(58, text)
	}
	r, err := s.Send(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(ctx, Written); err != nil {
		t.Fatal(err)
	}
	return r
}

// replayDone resends begin to end on c, and waits until the replay ends.
func replayDone(t *testing.T, s *Session, c *conn, begin, end uint64) {
	t.Helper()
	s.mu.Lock()
	s.startReplayLocked(c, begin, end, false)
	s.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		done := c.replay == nil && len(c.replayQueue) == 0
		s.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the replay never ended")
		}
		time.Sleep(time.Millisecond)
	}
}

// A GapFill not written whole skips nothing: the counterparty may still ask
// for the messages it stands for.
func TestReplay_GapFillNotWrittenSkipsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		short bool
	}{
		{"nothing written", false},
		{"short write", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, fc := replaySession(t, nil)
			r := sendWritten(t, s, "VETO", "")
			fc.arm(0, tc.short)
			replayDone(t, s, c, r.seq, r.seq)
			if err := r.Err(); errors.Is(err, ErrSkipped) {
				t.Errorf("Err() = %v, want not skipped", err)
			}
			if n := s.stats.GapFilled.Load(); n != 0 {
				t.Errorf("GapFilled = %d, want 0", n)
			}
		})
	}
}

// A GapFill written whole before a later write of the replay fails skips
// what it stands for; the one in the failed write does not.
func TestReplay_GapFillWrittenBeforeFailedWrite(t *testing.T) {
	s, c, fc := replaySession(t, nil)
	big := strings.Repeat("x", 40000)
	first := sendWritten(t, s, "VETO", "")
	sendWritten(t, s, "A", big)
	sendWritten(t, s, "B", big)
	last := sendWritten(t, s, "VETO", "")
	// The first write: the GapFill of the first, then A and B, past
	// replayChunk; the second, the GapFill of the last, fails.
	fc.arm(1, false)
	replayDone(t, s, c, first.seq, last.seq)
	if err := first.Err(); !errors.Is(err, ErrSkipped) {
		t.Errorf("first: Err() = %v, want %v", err, ErrSkipped)
	}
	if err := last.Err(); errors.Is(err, ErrSkipped) {
		t.Errorf("last: Err() = %v, want not skipped", err)
	}
	if n := s.stats.GapFilled.Load(); n != 1 {
		t.Errorf("GapFilled = %d, want 1", n)
	}
	if n := s.stats.Resent.Load(); n != 2 {
		t.Errorf("Resent = %d, want 2", n)
	}
}

// missingStore reports message missing as missing from the store.
type missingStore struct {
	*memstore.Store
	missing uint64
}

func (m missingStore) Range(ctx context.Context, gen, begin, end uint64, fn func(uint64, []byte, store.Hole) error) error {
	return m.Store.Range(ctx, gen, begin, end, func(seq uint64, raw []byte, hole store.Hole) error {
		if seq == m.missing {
			return fn(seq, nil, store.HoleMissing)
		}
		return fn(seq, raw, hole)
	})
}

// A replay stopping at a message missing from the store still writes the
// GapFill of what came before it: those messages are skipped.
func TestReplay_GapFillBeforeMissingSkips(t *testing.T) {
	s, c, _ := replaySession(t, func(c *Config) { c.Store = missingStore{Store: memstore.New(), missing: 2} })
	vetoed := sendWritten(t, s, "VETO", "")
	if vetoed.seq != 1 {
		t.Fatalf("vetoed order sent as %d, want 1", vetoed.seq)
	}
	sendWritten(t, s, "A", "")
	replayDone(t, s, c, 1, 2)
	if err := vetoed.Err(); !errors.Is(err, ErrSkipped) {
		t.Errorf("Err() = %v, want %v", err, ErrSkipped)
	}
}

// Receipts and the notices of messages beyond retention agree: the
// messages a GapFill stands for are skipped, and those evicted lost, once
// it is written whole, not before.
func TestReplay_GapFillBeyondRetention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written bool
	}{
		{"written", true},
		{"short write", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, fc := replaySession(t, func(c *Config) { c.Retention = Retention{Interval: time.Hour} })
			first := sendWritten(t, s, "A", "")
			last := sendWritten(t, s, "B", "")
			s.mu.Lock()
			err := s.store.(store.Truncater).Truncate(context.Background(), s.st.Generation, last.seq+1)
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.written {
				fc.arm(0, true)
			}
			replayDone(t, s, c, first.seq, last.seq)
			want, lost := uint64(0), uint64(0)
			if tc.written {
				want, lost = 2, 2
			}
			for _, r := range []Receipt{first, last} {
				if skipped := errors.Is(r.Err(), ErrSkipped); skipped != tc.written {
					t.Errorf("message %d: skipped = %t, want %t", r.seq, skipped, tc.written)
				}
			}
			if n := s.stats.GapFilled.Load(); n != want {
				t.Errorf("GapFilled = %d, want %d", n, want)
			}
			if n := s.stats.BeyondRetention.Load(); n != lost {
				t.Errorf("BeyondRetention = %d, want %d", n, lost)
			}
		})
	}
}
