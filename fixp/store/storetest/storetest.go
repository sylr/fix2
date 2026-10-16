// Package storetest is the conformance suite of FIXP stores: a store
// passes Run before a session uses it.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/store"
)

// Run runs the suite against stores open returns, a new empty one each
// call. reopen, if not nil, abandons a store and opens it again on the same
// data, for stores that persist: what was synced, and what Create
// recorded, must be read back. It should lose what a crash would, without
// closing the store gracefully, if the store allows it.
func Run(t *testing.T, open func(t *testing.T) store.Store, reopen func(t *testing.T, s store.Store) store.Store) {
	t.Run("Concurrent", func(t *testing.T) { testConcurrent(t, open(t)) })
	t.Run("Create", func(t *testing.T) { testCreate(t, open(t)) })
	t.Run("Outbound", func(t *testing.T) { testOutbound(t, open(t)) })
	t.Run("Inbound", func(t *testing.T) { testInbound(t, open(t)) })
	t.Run("Finalize", func(t *testing.T) { testFinalize(t, open(t)) })
	t.Run("Retire", func(t *testing.T) { testRetire(t, open(t)) })
	t.Run("Cancelled", func(t *testing.T) { testCancelled(t, open(t)) })
	if reopen != nil {
		t.Run("Reopen", func(t *testing.T) { testReopen(t, open(t), reopen) })
	}
}

var ctx = context.Background()

func meta(role store.Role) store.Meta {
	return store.Meta{
		ID:           fixp.NewSessionID(),
		Role:         role,
		ClientFlow:   fixp.Idempotent,
		ServerFlow:   fixp.Recoverable,
		Owner:        "owner",
		NegotiatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testCreate(t *testing.T, s store.Store) {
	defer s.Close()
	m := meta(store.Server)
	ss, err := s.Create(ctx, m)
	must(t, err)
	st := ss.State()
	if st.Meta != m || st.NextOutbound != 1 || st.Inbound != 0 || !st.EstablishedAt.IsZero() || !st.FinalizedAt.IsZero() {
		t.Fatalf("new session: %+v", st)
	}
	if _, err := s.Create(ctx, m); !errors.Is(err, store.ErrExists) {
		t.Errorf("Create again: %v", err)
	}
	if _, err := s.Open(ctx, fixp.NewSessionID()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Open unknown: %v", err)
	}
	opened, err := s.Open(ctx, m.ID)
	must(t, err)
	if opened.State().Meta != m {
		t.Errorf("opened: %+v", opened.State())
	}
	at := time.Unix(1_700_000_100, 0).UTC()
	must(t, ss.SetEstablished(ctx, at))
	must(t, ss.SetEstablished(ctx, at.Add(time.Hour)))
	if got := ss.State().EstablishedAt; !got.Equal(at) {
		t.Errorf("EstablishedAt %v, the first, %v", got, at)
	}
	m2 := meta(store.Server)
	_, err = s.Create(ctx, m2)
	must(t, err)
	states, err := s.Sessions(ctx)
	must(t, err)
	if len(states) != 2 {
		t.Errorf("%d sessions", len(states))
	}
	must(t, ss.Close())
}

func testOutbound(t *testing.T, s store.Store) {
	defer s.Close()
	ss, err := s.Create(ctx, meta(store.Server))
	must(t, err)
	if err := ss.Append(ctx, 2, []byte("x")); !errors.Is(err, store.ErrSequence) {
		t.Errorf("Append 2 first: %v", err)
	}
	msg := []byte("msg-1")
	must(t, ss.Append(ctx, 1, msg))
	msg[0] = 'X' // borrowed: the store copied it
	for seq := uint64(2); seq <= 5; seq++ {
		must(t, ss.Append(ctx, seq, fmt.Appendf(nil, "msg-%d", seq)))
	}
	if n := ss.State().NextOutbound; n != 6 {
		t.Fatalf("NextOutbound %d", n)
	}
	expectRange(t, ss, 1, 5)
	if err := ss.Range(ctx, 1, 6, func(uint64, []byte) error { return nil }); !errors.Is(err, store.ErrSequence) {
		t.Errorf("Range beyond NextOutbound: %v", err)
	}
	stop := errors.New("stop")
	calls := 0
	if err := ss.Range(ctx, 2, 5, func(uint64, []byte) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Errorf("Range stopped: %v after %d calls", err, calls)
	}

	must(t, ss.Evict(ctx, 3))
	calls = 0
	if err := ss.Range(ctx, 2, 5, func(uint64, []byte) error { calls++; return nil }); !errors.Is(err, store.ErrEvicted) || calls != 0 {
		t.Errorf("Range over evicted: %v after %d calls", err, calls)
	}
	expectRange(t, ss, 3, 5)

	// A snapshot: evicting during Range changes nothing it reports.
	var seqs []uint64
	must(t, ss.Range(ctx, 3, 5, func(seq uint64, m []byte) error {
		if seq == 3 {
			must(t, ss.Evict(ctx, 6))
		}
		seqs = append(seqs, seq)
		if want := fmt.Sprintf("msg-%d", seq); string(m) != want {
			t.Errorf("%d during eviction: %q", seq, m)
		}
		return nil
	}))
	if fmt.Sprint(seqs) != "[3 4 5]" {
		t.Errorf("during eviction: %v", seqs)
	}

	// An Idempotent flow's sequence number, without messages.
	idem, err := s.Create(ctx, meta(store.Client))
	must(t, err)
	must(t, idem.SetNextOutbound(ctx, 10))
	if err := idem.SetNextOutbound(ctx, 9); !errors.Is(err, store.ErrSequence) {
		t.Errorf("SetNextOutbound backwards: %v", err)
	}
	must(t, idem.Sync(ctx))
	if n := idem.State().NextOutbound; n != 10 {
		t.Errorf("NextOutbound %d", n)
	}
}

// expectRange checks Range reports every message from begin to end, each
// once, in order, as Append stored it: "msg-<seq>".
func expectRange(t *testing.T, ss store.Session, begin, end uint64) {
	t.Helper()
	var got, want []string
	for seq := begin; seq <= end; seq++ {
		want = append(want, fmt.Sprintf("%d:msg-%d", seq, seq))
	}
	must(t, ss.Range(ctx, begin, end, func(seq uint64, m []byte) error {
		got = append(got, fmt.Sprintf("%d:%s", seq, m))
		return nil
	}))
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Range %d-%d: %q, want %q", begin, end, got, want)
	}
}

func testInbound(t *testing.T, s store.Store) {
	defer s.Close()
	ss, err := s.Create(ctx, meta(store.Client))
	must(t, err)
	must(t, ss.SetInbound(ctx, 7))
	must(t, ss.SetInbound(ctx, 7))
	if err := ss.SetInbound(ctx, 6); !errors.Is(err, store.ErrSequence) {
		t.Errorf("SetInbound backwards: %v", err)
	}
	if n := ss.State().Inbound; n != 7 {
		t.Errorf("Inbound %d", n)
	}
}

func testFinalize(t *testing.T, s store.Store) {
	defer s.Close()
	m := meta(store.Server)
	ss, err := s.Create(ctx, m)
	must(t, err)
	t1 := time.Unix(1_700_000_200, 0).UTC()
	out := store.Finish{Finishing: true, LastSeqNo: 9}
	must(t, ss.SetFinish(ctx, out, store.Finish{}))
	if st := ss.State(); st.OutboundFinish != out || !st.FinalizedAt.IsZero() {
		t.Fatalf("finishing: %+v", st)
	}
	out.Finished = true
	in := store.Finish{Finishing: true, LastSeqNo: 4, Finished: true}
	must(t, ss.SetFinish(ctx, out, in))
	// Both finished, not finalized: that is the engine's to say.
	if st := ss.State(); st.OutboundFinish != out || st.InboundFinish != in || !st.FinalizedAt.IsZero() {
		t.Fatalf("finished: %+v", st)
	}
	must(t, ss.Finalize(ctx, t1))
	must(t, ss.Finalize(ctx, t1.Add(time.Hour)))
	if st := ss.State(); !st.FinalizedAt.Equal(t1) || st.Retired {
		t.Fatalf("finalized: %+v", st)
	}
	states, err := s.Sessions(ctx)
	must(t, err)
	if len(states) != 0 {
		t.Errorf("finalized sessions listed: %+v", states)
	}
	// Refused until forgotten.
	if _, err := s.Create(ctx, m); !errors.Is(err, store.ErrExists) {
		t.Errorf("Create of a finalized ID: %v", err)
	}
	must(t, s.Forget(ctx, t1))
	if _, err := s.Open(ctx, m.ID); err != nil {
		t.Errorf("forgotten too early: %v", err)
	}
	must(t, s.Forget(ctx, t1.Add(time.Second)))
	if _, err := s.Open(ctx, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("not forgotten: %v", err)
	}
	if _, err := s.Create(ctx, m); err != nil {
		t.Errorf("Create of a forgotten ID: %v", err)
	}
}

func testRetire(t *testing.T, s store.Store) {
	defer s.Close()
	ss, err := s.Create(ctx, meta(store.Server))
	must(t, err)
	t1 := time.Unix(1_700_000_300, 0).UTC()
	must(t, ss.Retire(ctx, t1))
	must(t, ss.Retire(ctx, t1.Add(time.Hour)))
	if st := ss.State(); !st.Retired || !st.FinalizedAt.Equal(t1) {
		t.Fatalf("retired: %+v", st)
	}
	states, err := s.Sessions(ctx)
	must(t, err)
	if len(states) != 0 {
		t.Errorf("retired sessions listed")
	}
}

func testCancelled(t *testing.T, s store.Store) {
	defer s.Close()
	ss, err := s.Create(ctx, meta(store.Server))
	must(t, err)
	done, cancel := context.WithCancel(ctx)
	cancel()
	// Failed or not, the write happened whole or not at all.
	ss.Append(done, 1, []byte("msg-1"))
	switch n := ss.State().NextOutbound; n {
	case 1:
		if err := ss.Range(ctx, 1, 1, func(uint64, []byte) error { return nil }); !errors.Is(err, store.ErrSequence) {
			t.Errorf("Append not done, but Range: %v", err)
		}
	case 2:
		expectRange(t, ss, 1, 1)
	default:
		t.Errorf("NextOutbound %d", n)
	}
	if _, err := s.Create(done, meta(store.Server)); err == nil {
		t.Log("Create succeeded despite a done ctx")
	}
}

// sameState reports whether two states are equal, times compared as
// instants.
func sameState(a, b store.State) bool {
	if !a.NegotiatedAt.Equal(b.NegotiatedAt) || !a.EstablishedAt.Equal(b.EstablishedAt) || !a.FinalizedAt.Equal(b.FinalizedAt) {
		return false
	}
	a.NegotiatedAt, a.EstablishedAt, a.FinalizedAt = time.Time{}, time.Time{}, time.Time{}
	b.NegotiatedAt, b.EstablishedAt, b.FinalizedAt = time.Time{}, time.Time{}, time.Time{}
	return a == b
}

func testReopen(t *testing.T, s store.Store, reopen func(t *testing.T, s store.Store) store.Store) {
	// A session written to, synced.
	m := meta(store.Server)
	ss, err := s.Create(ctx, m)
	must(t, err)
	must(t, ss.SetEstablished(ctx, time.Unix(1_700_000_400, 0).UTC()))
	must(t, ss.Append(ctx, 1, []byte("msg-1")))
	must(t, ss.Append(ctx, 2, []byte("msg-2")))
	must(t, ss.SetInbound(ctx, 3))
	must(t, ss.SetFinish(ctx, store.Finish{Finishing: true, LastSeqNo: 2}, store.Finish{Finishing: true, LastSeqNo: 3}))
	must(t, ss.Sync(ctx))
	want := ss.State()

	// Sessions finalized and retired: their IDs stay refused.
	fin, err := s.Create(ctx, meta(store.Server))
	must(t, err)
	done := store.Finish{Finishing: true, Finished: true}
	must(t, fin.SetFinish(ctx, done, done))
	must(t, fin.Finalize(ctx, time.Unix(1_700_000_500, 0).UTC()))
	must(t, fin.Sync(ctx))
	ret, err := s.Create(ctx, meta(store.Server))
	must(t, err)
	must(t, ret.Retire(ctx, time.Unix(1_700_000_600, 0).UTC()))
	must(t, ret.Sync(ctx))
	finState, retState := fin.State(), ret.State()

	// A session created, never synced: Create is durable by itself.
	created := meta(store.Client)
	_, err = s.Create(ctx, created)
	must(t, err)

	s = reopen(t, s)
	defer s.Close()
	ss, err = s.Open(ctx, m.ID)
	must(t, err)
	if got := ss.State(); !sameState(got, want) {
		t.Fatalf("reopened\n%+v\nwant\n%+v", got, want)
	}
	expectRange(t, ss, 1, 2)
	for _, w := range []store.State{finState, retState} {
		got, err := s.Open(ctx, w.ID)
		must(t, err)
		if !sameState(got.State(), w) {
			t.Errorf("reopened\n%+v\nwant\n%+v", got.State(), w)
		}
		if _, err := s.Create(ctx, w.Meta); !errors.Is(err, store.ErrExists) {
			t.Errorf("Create of a finalized ID after reopening: %v", err)
		}
	}
	if _, err := s.Open(ctx, created.ID); err != nil {
		t.Errorf("Create not durable: %v", err)
	}
	states, err := s.Sessions(ctx)
	must(t, err)
	if len(states) != 2 {
		t.Errorf("%d sessions not finalized after reopening, want 2", len(states))
	}
}

func testConcurrent(t *testing.T, s store.Store) {
	defer s.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ss, err := s.Create(ctx, meta(store.Server))
			if err != nil {
				t.Error(err)
				return
			}
			for seq := uint64(1); seq <= 50; seq++ {
				if err := ss.Append(ctx, seq, fmt.Appendf(nil, "msg-%d", seq)); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Sessions(ctx); err != nil {
					t.Error(err)
					return
				}
			}
			expectRange(t, ss, 1, 50)
		})
	}
	wg.Wait()

	// One session from several goroutines, as the engine calls it.
	ss, err := s.Create(ctx, meta(store.Server))
	must(t, err)
	var mu sync.Mutex // orders the Appends, which must be in sequence
	next := uint64(1)
	for g := range 4 {
		wg.Go(func() {
			for i := range 50 {
				var err error
				switch g {
				case 0:
					mu.Lock()
					err = ss.Append(ctx, next, fmt.Appendf(nil, "msg-%d", next))
					next++
					mu.Unlock()
				case 1:
					err = ss.SetInbound(ctx, uint64(i))
				case 2:
					err = ss.Sync(ctx)
				case 3:
					if n := ss.State().NextOutbound; n > 1 {
						err = ss.Range(ctx, 1, n-1, func(uint64, []byte) error { return nil })
					}
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	expectRange(t, ss, 1, 50)
}
