package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// plain hides the optional interfaces of the store it embeds.
type plain struct{ store.Store }

// syncSpy counts the Syncs it is asked for, and implements no Truncater.
type syncSpy struct {
	store.Store
	syncs atomic.Int32
}

func (s *syncSpy) Sync(context.Context, uint64, uint64) error {
	s.syncs.Add(1)
	return nil
}

// truncOnly implements Truncater, not Syncer.
type truncOnly struct{ *memstore.Store }

func (truncOnly) Sync() {} // shadows memstore's: not a Syncer

// The wrapper implements exactly the optional interfaces of the store it
// wraps.
func TestWrapStore_Interfaces(t *testing.T) {
	ev, err := OpenEvents(t.TempDir(), "events-t.jsonl", "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		inner           store.Store
		syncs, truncate bool
	}{
		{"neither", plain{memstore.New()}, false, false},
		{"syncer", &syncSpy{Store: plain{memstore.New()}}, true, false},
		{"truncater", truncOnly{memstore.New()}, false, true},
		{"both", memstore.New(), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, innerSyncs := tc.inner.(store.Syncer)
			_, innerTruncates := tc.inner.(store.Truncater)
			if innerSyncs != tc.syncs || innerTruncates != tc.truncate {
				t.Fatalf("test store: Syncer %v, Truncater %v", innerSyncs, innerTruncates)
			}
			w := wrapStore(tc.inner, ev, false)
			if _, ok := w.(store.Syncer); ok != tc.syncs {
				t.Errorf("wrapper Syncer = %v, want %v", ok, tc.syncs)
			}
			if _, ok := w.(store.Truncater); ok != tc.truncate {
				t.Errorf("wrapper Truncater = %v, want %v", ok, tc.truncate)
			}
		})
	}
}

// The no-sync mutant reports Syncs done without asking the store for them;
// otherwise each is forwarded.
func TestWrapStore_NoSync(t *testing.T) {
	for _, noSync := range []bool{false, true} {
		dir := t.TempDir()
		ev, err := OpenEvents(dir, "events-t.jsonl", "t")
		if err != nil {
			t.Fatal(err)
		}
		spy := &syncSpy{Store: plain{memstore.New()}}
		w := wrapStore(spy, ev, noSync).(store.Syncer)
		if err := w.Sync(context.Background(), 1, 1); err != nil {
			t.Fatal(err)
		}
		want := int32(1)
		if noSync {
			want = 0
		}
		if got := spy.syncs.Load(); got != want {
			t.Errorf("no-sync %v: %d Syncs reached the store, want %d", noSync, got, want)
		}
		var ends int
		for _, e := range events(t, dir) {
			if e.Call == "sync" && e.Phase == "end" && e.Err == "" {
				ends++
			}
		}
		if ends != 1 {
			t.Errorf("no-sync %v: %d successful Syncs logged, want 1", noSync, ends)
		}
	}
}

// The digest is the message's application fields: a resend's header does
// not change it, an application field does.
func TestDigest(t *testing.T) {
	raw := func(seq int, possDup bool, qty string) []byte {
		b := fix.NewBuilder(session.FIX44, "D").SetString(49, "INI").SetString(56, "ACC").SetInt(34, seq)
		if possDup {
			b.SetString(43, "Y").SetString(122, "20261018-10:00:00.000").SetString(52, "20261018-11:00:00.000")
		} else {
			b.SetString(52, "20261018-10:00:00.000")
		}
		return b.SetString(11, "O1").SetString(55, "FIX2").SetString(54, "1").SetString(38, qty).SetString(40, "1").Build()
	}
	first, resent, other := digestRaw(raw(2, false, "100")), digestRaw(raw(2, true, "100")), digestRaw(raw(2, false, "101"))
	if first == "" || first != resent {
		t.Errorf("digest %q, resent %q: want the same", first, resent)
	}
	if other == first {
		t.Errorf("digest unchanged by another quantity: %q", other)
	}
}

// Range logs the holes it reported, in runs.
func TestWrapStore_RangeHoles(t *testing.T) {
	dir := t.TempDir()
	ev, err := OpenEvents(dir, "events-t.jsonl", "t")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w := wrapStore(memstore.New(), ev, false)
	if _, err := w.Open(ctx); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 6; seq++ {
		if err := w.Append(ctx, 1, seq, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.(store.Truncater).Truncate(ctx, 1, 4); err != nil {
		t.Fatal(err)
	}
	if err := w.Range(ctx, 1, 1, 6, func(uint64, []byte, store.Hole) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var got []HoleRun
	for _, e := range events(t, dir) {
		if e.Call == "range" && e.Phase == "end" {
			got = e.Holes
		}
	}
	if len(got) != 1 || got[0].From != 1 || got[0].To != 3 || got[0].Kind != store.HoleRetention.String() {
		t.Errorf("holes %+v, want 1 to 3 retention", got)
	}
}
