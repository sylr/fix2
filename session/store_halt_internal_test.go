package session

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
)

// storeErrors are store errors, and whether they halt a session.
var storeErrors = []struct {
	name string
	err  error
	halt bool
}{
	{"generation gone", store.ErrGenerationGone, true},
	{"closed", store.ErrClosed, true},
	{"wrapped", fmt.Errorf("store: %w", store.ErrSeqMismatch), true},
	{"joined with a cancellation", errors.Join(context.Canceled, store.ErrBackwards), true},
	{"joined with an unknown outcome", errors.Join(store.ErrUnknownOutcome, store.ErrGenerationGone), true},
	{"unknown outcome", store.ErrUnknownOutcome, false},
	{"transient", errors.New("disk busy"), false},
}

// truncateStore fails Truncate with err.
type truncateStore struct {
	*memstore.Store
	err error
}

func (f truncateStore) Truncate(context.Context, uint64, uint64) error { return f.err }

// An eviction told the store moved on halts the session; any other failure
// leaves it running, retried at the next eviction.
func TestStoreHalt_Truncate(t *testing.T) {
	for _, tc := range storeErrors {
		t.Run(tc.name, func(t *testing.T) {
			s, c, _ := replaySession(t, func(c *Config) {
				c.Store = truncateStore{Store: memstore.New(), err: tc.err}
				c.Retention = Retention{Interval: time.Hour}
			})
			sendWritten(t, s, "A", "")
			sendWritten(t, s, "B", "")
			s.mu.Lock()
			s.ackForTest(2, tagLastMsgSeqNumProcessed)
			s.evictLocked(time.Now())
			halted, closed := s.halted != nil, c.closed
			s.mu.Unlock()
			if halted != tc.halt || closed != tc.halt {
				t.Errorf("halted = %t, connection closed = %t, want %t", halted, closed, tc.halt)
			}
		})
	}
}

// rangeStore fails Range with err, after calling before if not nil.
type rangeStore struct {
	*memstore.Store
	err    error
	before func()
}

func (f rangeStore) Range(context.Context, uint64, uint64, uint64, func(uint64, []byte, store.Hole) error) error {
	if f.before != nil {
		f.before()
	}
	return f.err
}

// A replay told the store moved on halts the session; any other failure to
// read only logs out.
func TestStoreHalt_Range(t *testing.T) {
	for _, tc := range storeErrors {
		t.Run(tc.name, func(t *testing.T) {
			s, c, _ := replaySession(t, func(c *Config) {
				c.Store = rangeStore{Store: memstore.New(), err: tc.err}
			})
			r := sendWritten(t, s, "A", "")
			replayDone(t, s, c, r.seq, r.seq)
			s.mu.Lock()
			halted, closed := s.halted != nil, c.closed
			s.mu.Unlock()
			if halted != tc.halt {
				t.Errorf("halted = %t, want %t", halted, tc.halt)
			}
			if tc.halt && !closed {
				t.Error("halted, the connection still open")
			}
			if !tc.halt && s.State() != StateLoggingOut {
				t.Errorf("State() = %s, want %s", s.State(), StateLoggingOut)
			}
		})
	}
}

// A replay of a generation a reset ended meanwhile does not halt the
// session, though the store says that generation is gone.
func TestStoreHalt_RangeOfAGenerationLeft(t *testing.T) {
	var s *Session
	ms := memstore.New()
	s, c, _ := replaySession(t, func(c *Config) {
		c.Store = rangeStore{Store: ms, err: store.ErrGenerationGone, before: func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			ctx := context.Background()
			if err := ms.Reset(ctx, s.st.Generation, time.Now(), ""); err != nil {
				t.Error(err)
			}
			st, err := ms.Open(ctx)
			if err != nil {
				t.Error(err)
			}
			s.st = st
		}}
	})
	r := sendWritten(t, s, "A", "")
	replayDone(t, s, c, r.seq, r.seq)
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

// targetFailStore fails SetNextTarget with err once failing is set.
type targetFailStore struct {
	*memstore.Store
	err     error
	failing *atomic.Bool
}

func (f targetFailStore) SetNextTarget(ctx context.Context, gen, next uint64) error {
	if f.failing.Load() {
		return f.err
	}
	return f.Store.SetNextTarget(ctx, gen, next)
}

// A halt while disconnected wakes the receipts waited for: they can never
// be acknowledged.
func TestStoreHalt_WakesReceiptsWhileDisconnected(t *testing.T) {
	failing := new(atomic.Bool)
	s, c, _ := replaySession(t, func(c *Config) {
		c.Store = targetFailStore{Store: memstore.New(), err: store.ErrGenerationGone, failing: failing}
	})
	r := sendWritten(t, s, "A", "")
	s.mu.Lock()
	s.disconnectLocked(c, errors.New("gone"))
	s.mu.Unlock()
	waited := make(chan error, 1)
	go func() { waited <- r.Wait(context.Background(), Acknowledged) }()
	// Let it wait.
	time.Sleep(10 * time.Millisecond)
	failing.Store(true)
	if err := s.SetNextTargetSeqNum(100); !errors.Is(err, store.ErrGenerationGone) {
		t.Fatalf("SetNextTargetSeqNum: %v", err)
	}
	select {
	case err := <-waited:
		if !errors.Is(err, ErrUnknownDelivery) {
			t.Errorf("Wait: %v, want %v", err, ErrUnknownDelivery)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait still blocked after the halt")
	}
}
