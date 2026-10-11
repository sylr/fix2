// Package storetest checks that a store.Store keeps the contract the session
// relies on. A Store implementation calls Run from its tests.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/session/store"
)

// Run runs the conformance tests against stores made by newStore, which must
// return a new, empty store each time.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	open := func(t *testing.T) (store.Store, store.State) {
		t.Helper()
		s := newStore(t)
		t.Cleanup(func() { s.Close() })
		st, err := s.Open(context.Background())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s, st
	}
	ctx := context.Background()

	t.Run("OpenEmpty", func(t *testing.T) {
		_, st := open(t)
		if st.Generation != 1 || st.NextSender != 1 || st.NextTarget != 1 {
			t.Errorf("Open on an empty store = %+v, want generation 1 and both sequence numbers at 1", st)
		}
	})

	t.Run("AppendAndRange", func(t *testing.T) {
		s, st := open(t)
		var want [][]byte
		for seq := uint64(1); seq <= 100; seq++ {
			raw := fmt.Appendf(nil, "message %d %s", seq, make([]byte, seq%13))
			if err := s.Append(ctx, st.Generation, seq, raw); err != nil {
				t.Fatalf("Append %d: %v", seq, err)
			}
			want = append(want, append([]byte(nil), raw...))
			raw[0] = 'X' // the store keeps its own copy
		}
		got := collect(t, s, st.Generation, 1, 100)
		if len(got) != 100 {
			t.Fatalf("Range 1..100 returned %d records, want 100", len(got))
		}
		for i, r := range got {
			if r.hole != store.HoleNone || string(r.raw) != string(want[i]) {
				t.Fatalf("Range %d = %q (%v), want %q", i+1, r.raw, r.hole, want[i])
			}
		}
		if st2 := state(t, s); st2.NextSender != 101 {
			t.Errorf("NextSender = %d, want 101", st2.NextSender)
		}
	})

	t.Run("AppendContract", func(t *testing.T) {
		s, st := open(t)
		g := st.Generation
		if err := s.Append(ctx, g, 1, []byte("one")); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, g, 2, []byte("two")); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, g, 1, []byte("one")); err != nil {
			t.Errorf("retrying a successful Append: %v", err)
		}
		if err := s.Append(ctx, g, 1, []byte("ONE")); !errors.Is(err, store.ErrSeqMismatch) {
			t.Errorf("overwriting message 1: err = %v, want ErrSeqMismatch", err)
		}
		if err := s.Append(ctx, g, 4, []byte("four")); !errors.Is(err, store.ErrSeqMismatch) {
			t.Errorf("skipping 3: err = %v, want ErrSeqMismatch", err)
		}
		if err := s.Append(ctx, g+1, 3, []byte("three")); !errors.Is(err, store.ErrGenerationGone) {
			t.Errorf("wrong generation: err = %v, want ErrGenerationGone", err)
		}
	})

	t.Run("NextTarget", func(t *testing.T) {
		s, st := open(t)
		if err := s.SetNextTarget(ctx, st.Generation, 10); err != nil {
			t.Fatal(err)
		}
		if err := s.SetNextTarget(ctx, st.Generation, 10); err != nil {
			t.Errorf("same NextTarget again: %v", err)
		}
		if err := s.SetNextTarget(ctx, st.Generation, 9); !errors.Is(err, store.ErrBackwards) {
			t.Errorf("NextTarget backwards: err = %v, want ErrBackwards", err)
		}
		if err := s.SetNextTarget(ctx, st.Generation+1, 11); !errors.Is(err, store.ErrGenerationGone) {
			t.Errorf("wrong generation: err = %v, want ErrGenerationGone", err)
		}
		if st2 := state(t, s); st2.NextTarget != 10 {
			t.Errorf("NextTarget = %d, want 10", st2.NextTarget)
		}
	})

	t.Run("NextSenderHoles", func(t *testing.T) {
		s, st := open(t)
		g := st.Generation
		mustAppend(t, s, g, 1, "one")
		if err := s.SetNextSender(ctx, g, 4); err != nil {
			t.Fatal(err)
		}
		if err := s.SetNextSender(ctx, g, 3); !errors.Is(err, store.ErrBackwards) {
			t.Errorf("NextSender backwards: err = %v, want ErrBackwards", err)
		}
		mustAppend(t, s, g, 4, "four")
		got := collect(t, s, g, 1, 4)
		want := []record{{1, "one", store.HoleNone}, {2, "", store.HoleNotStored}, {3, "", store.HoleNotStored}, {4, "four", store.HoleNone}}
		checkRecords(t, got, want)
	})

	t.Run("RangeBounds", func(t *testing.T) {
		s, st := open(t)
		g := st.Generation
		for seq := uint64(1); seq <= 5; seq++ {
			mustAppend(t, s, g, seq, fmt.Sprint(seq))
		}
		if got := collect(t, s, g, 3, 1000); len(got) != 3 || got[0].seq != 3 || got[2].seq != 5 {
			t.Errorf("Range 3..1000 = %v, want 3..5", got)
		}
		if got := collect(t, s, g, 0, 1); len(got) != 1 || got[0].seq != 1 {
			t.Errorf("Range 0..1 = %v, want 1", got)
		}
		if got := collect(t, s, g, 4, 2); len(got) != 0 {
			t.Errorf("Range 4..2 = %v, want nothing", got)
		}
		stop := errors.New("stop")
		n := 0
		err := s.Range(ctx, g, 1, 5, func(uint64, []byte, store.Hole) error { n++; return stop })
		if !errors.Is(err, stop) || n != 1 {
			t.Errorf("Range stopping on the first callback: err %v after %d calls", err, n)
		}
		if err := s.Range(ctx, g+1, 1, 5, func(uint64, []byte, store.Hole) error { return nil }); !errors.Is(err, store.ErrGenerationGone) {
			t.Errorf("Range of another generation: err = %v, want ErrGenerationGone", err)
		}
	})

	t.Run("Reset", func(t *testing.T) {
		s, st := open(t)
		g := st.Generation
		mustAppend(t, s, g, 1, "one")
		if err := s.SetNextTarget(ctx, g, 7); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC)
		if err := s.Reset(ctx, g, at, "w2"); err != nil {
			t.Fatal(err)
		}
		if err := s.Reset(ctx, g, at, "w2"); err != nil {
			t.Errorf("repeating a Reset: %v", err)
		}
		if err := s.Append(ctx, g, 2, []byte("late")); !errors.Is(err, store.ErrGenerationGone) {
			t.Errorf("Append to the previous generation: err = %v, want ErrGenerationGone", err)
		}
		st2 := state(t, s)
		if st2.Generation != g+1 || st2.NextSender != 1 || st2.NextTarget != 1 || !st2.CreatedAt.Equal(at) || st2.WindowID != "w2" {
			t.Errorf("state after Reset = %+v", st2)
		}
		mustAppend(t, s, g+1, 1, "new one")
		checkRecords(t, collect(t, s, g+1, 1, 1), []record{{1, "new one", store.HoleNone}})
		// Two generations on, resetting the first is stale.
		if err := s.Reset(ctx, g+1, at, "w3"); err != nil {
			t.Fatal(err)
		}
		if err := s.Reset(ctx, g, at, "w3"); !errors.Is(err, store.ErrGenerationGone) {
			t.Errorf("Reset of an older generation: err = %v, want ErrGenerationGone", err)
		}
	})

	t.Run("ResetDuringRange", func(t *testing.T) {
		s, st := open(t)
		g := st.Generation
		for seq := uint64(1); seq <= 3; seq++ {
			mustAppend(t, s, g, seq, "m")
		}
		err := s.Range(ctx, g, 1, 3, func(seq uint64, _ []byte, _ store.Hole) error {
			if seq == 1 {
				// The callback runs without the store's locks: it may call it.
				return s.Reset(ctx, g, time.Now(), "")
			}
			return nil
		})
		if !errors.Is(err, store.ErrGenerationGone) {
			t.Errorf("Range across a Reset: err = %v, want ErrGenerationGone", err)
		}
		// A Reset from the last callback is reported too.
		s2, st2 := open(t)
		mustAppend(t, s2, st2.Generation, 1, "m")
		err = s2.Range(ctx, st2.Generation, 1, 1, func(uint64, []byte, store.Hole) error {
			return s2.Reset(ctx, st2.Generation, time.Now(), "")
		})
		if !errors.Is(err, store.ErrGenerationGone) {
			t.Errorf("Range with a Reset in its last callback: err = %v, want ErrGenerationGone", err)
		}
	})

	t.Run("ConcurrentAppendAndRange", func(t *testing.T) {
		s, st := open(t)
		g := st.Generation
		const n = 2000
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := uint64(1); seq <= n; seq++ {
				if err := s.Append(ctx, g, seq, fmt.Appendf(nil, "m%d", seq)); err != nil {
					t.Errorf("Append %d: %v", seq, err)
					return
				}
			}
		}()
		for range 50 {
			err := s.Range(ctx, g, 1, n, func(seq uint64, raw []byte, hole store.Hole) error {
				if hole != store.HoleNone || string(raw) != fmt.Sprintf("m%d", seq) {
					return fmt.Errorf("message %d = %q (%v)", seq, raw, hole)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		wg.Wait()
	})

	if _, ok := newStore(t).(store.Truncater); ok {
		t.Run("Truncate", func(t *testing.T) {
			s, st := open(t)
			g := st.Generation
			for seq := uint64(1); seq <= 5; seq++ {
				mustAppend(t, s, g, seq, fmt.Sprint(seq))
			}
			if err := s.(store.Truncater).Truncate(ctx, g, 3); err != nil {
				t.Fatal(err)
			}
			checkRecords(t, collect(t, s, g, 1, 5), []record{
				{1, "", store.HoleRetention}, {2, "", store.HoleRetention},
				{3, "3", store.HoleNone}, {4, "4", store.HoleNone}, {5, "5", store.HoleNone},
			})
			// A retry of a truncated Append still succeeds.
			if err := s.Append(ctx, g, 1, []byte("1")); err != nil {
				t.Errorf("retrying a truncated Append: %v", err)
			}
			// A Range keeps its snapshot across a Truncate.
			var got []record
			err := s.Range(ctx, g, 3, 5, func(seq uint64, raw []byte, hole store.Hole) error {
				got = append(got, record{seq, string(raw), hole})
				if seq == 3 {
					return s.(store.Truncater).Truncate(ctx, g, 6)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			checkRecords(t, got, []record{{3, "3", store.HoleNone}, {4, "4", store.HoleNone}, {5, "5", store.HoleNone}})
		})

		t.Run("TruncateRepeatedly", func(t *testing.T) {
			s, st := open(t)
			g := st.Generation
			big := make([]byte, 100<<10)
			for seq := uint64(1); seq <= 50; seq++ {
				if err := s.Append(ctx, g, seq, big); err != nil {
					t.Fatal(err)
				}
				if err := s.(store.Truncater).Truncate(ctx, g, seq+1); err != nil {
					t.Fatal(err)
				}
			}
			checkRecords(t, collect(t, s, g, 49, 50), []record{{49, "", store.HoleRetention}, {50, "", store.HoleRetention}})
		})
	}

	t.Run("Closed", func(t *testing.T) {
		s, st := open(t)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, st.Generation, 1, []byte("x")); err == nil {
			t.Error("Append on a closed store succeeded")
		}
	})
}

type record struct {
	seq  uint64
	raw  string
	hole store.Hole
}

func collect(t *testing.T, s store.Store, gen, begin, end uint64) []record {
	t.Helper()
	var out []record
	err := s.Range(context.Background(), gen, begin, end, func(seq uint64, raw []byte, hole store.Hole) error {
		out = append(out, record{seq, string(raw), hole})
		return nil
	})
	if err != nil {
		t.Fatalf("Range %d..%d: %v", begin, end, err)
	}
	return out
}

func checkRecords(t *testing.T, got, want []record) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Range = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Range[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func mustAppend(t *testing.T, s store.Store, gen, seq uint64, raw string) {
	t.Helper()
	if err := s.Append(context.Background(), gen, seq, []byte(raw)); err != nil {
		t.Fatalf("Append %d: %v", seq, err)
	}
}

// state returns the store's state through Open, which a store answers again
// with its current state.
func state(t *testing.T, s store.Store) store.State {
	t.Helper()
	st, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open to read the state: %v", err)
	}
	return st
}
