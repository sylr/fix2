package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
)

// closeReadGate is a store whose Range waits for its ctx at MsgSeqNum at, or
// after its last message if at is past it; with returnNil, it then returns
// nil, a read completing as its ctx is cancelled.
type closeReadGate struct {
	*memstore.Store
	at        uint64
	returnNil bool
	entered   chan struct{}
	once      sync.Once
}

func (g *closeReadGate) Range(ctx context.Context, gen, begin, end uint64, fn func(uint64, []byte, store.Hole) error) error {
	wait := func() error {
		g.once.Do(func() { close(g.entered) })
		<-ctx.Done()
		if g.returnNil {
			return nil
		}
		return ctx.Err()
	}
	err := g.Store.Range(ctx, gen, begin, end, func(seq uint64, raw []byte, hole store.Hole) error {
		if seq == g.at {
			return wait()
		}
		return fn(seq, raw, hole)
	})
	if err == nil && g.at > end {
		return wait()
	}
	return err
}

// A replay the close deadline cuts short keeps from eviction what it did not
// resend, Range returning nil as its ctx is cancelled included: only the
// chunks written count as resent, the receipts of the originals stay
// Written, and an acknowledgement then lets eviction go on.
func TestCloseDeadlineKeepsRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		at        uint64
		returnNil bool
		resent    uint64
	}{
		{"before first message", 1, false, 0},
		{"after first complete chunk", 3, false, 2},
		{"success races cancellation with buffered tail", 4, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &closeReadGate{Store: memstore.New(), at: tc.at, returnNil: tc.returnNil, entered: make(chan struct{})}
			s, c, _ := replaySession(t, func(cfg *Config) {
				cfg.Store = gate
				cfg.Retention = Retention{Interval: time.Hour, Max: 1}
				cfg.LogoutTimeout = 40 * time.Millisecond
				cfg.Handler = BaseHandler{}
			})
			var receipts []Receipt
			for range 3 {
				receipts = append(receipts, sendWritten(t, s, "ORDER", strings.Repeat("x", 40000)))
			}
			s.mu.Lock()
			s.startReplayLocked(c, 1, 3, false)
			s.mu.Unlock()
			select {
			case <-gate.entered:
			case <-time.After(time.Second):
				t.Fatal("Range not waiting")
			}
			s.mu.Lock()
			s.sendLogoutLocked(c, "peer logs out")
			s.phase = phaseLogoutSent
			s.closeAfterFlushLocked(c, &LogoutError{ByCounterparty: true, Text: "peer logs out"})
			s.mu.Unlock()
			select {
			case <-c.finished:
			case <-time.After(time.Second):
				t.Fatal("replay not cut short by the close deadline")
			}
			s.mu.Lock()
			if s.ret.unsentLo != 1 || s.ret.unsentHi < 3 {
				t.Errorf("unsent %d..%d, want 1..3 at least", s.ret.unsentLo, s.ret.unsentHi)
			}
			s.evictLocked(time.Now())
			floor := s.ret.floor
			s.mu.Unlock()
			if floor != 1 {
				t.Errorf("floor = %d, want 1: evicted what was not resent", floor)
			}
			if got := s.stats.Resent.Load(); got != tc.resent {
				t.Errorf("Resent = %d, want %d", got, tc.resent)
			}
			var messages int
			err := gate.Store.Range(context.Background(), 1, 1, 3, func(_ uint64, _ []byte, hole store.Hole) error {
				if hole != store.HoleNone {
					t.Errorf("hole %v, want none: evicted what was not resent", hole)
				}
				messages++
				return nil
			})
			if err != nil || messages != 3 {
				t.Errorf("%d messages kept, error %v, want 3", messages, err)
			}
			for _, r := range receipts {
				if err := r.Wait(context.Background(), Written); err != nil {
					t.Errorf("Wait(Written) = %v, want nil", err)
				}
				if errors.Is(r.Err(), ErrSkipped) {
					t.Error("receipt skipped by the replay cut short")
				}
			}
			// Retention still works: acknowledged, they are evicted.
			s.mu.Lock()
			s.ackForTest(s.st.NextSender-1, tagLastMsgSeqNumProcessed)
			s.evictLocked(time.Now())
			after := s.ret.floor
			s.mu.Unlock()
			if after <= floor {
				t.Errorf("floor = %d after the acknowledgement, want past %d", after, floor)
			}
			if err := s.Err(); err != nil {
				t.Errorf("Err = %v, want nil", err)
			}
		})
	}
}
