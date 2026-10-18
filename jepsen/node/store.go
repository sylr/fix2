package main

import (
	"context"
	"sync"
	"time"

	"github.com/sylr/fix2/fix/session/store"
)

// loggedStore logs each call to the store it wraps, its start and its end,
// so the checkers know what reached the disk: an Append completed before a
// Sync that returned nil began is durable, with the SQLite store.
type loggedStore struct {
	inner  store.Store
	ev     *Events
	noSync bool // mutant: Sync reports success without syncing

	mu       sync.Mutex
	appended map[uint64]uint64 // per generation, the last Append completed
}

// wrapStore wraps inner, keeping the optional interfaces it implements.
func wrapStore(inner store.Store, ev *Events, noSync bool) store.Store {
	l := &loggedStore{inner: inner, ev: ev, noSync: noSync, appended: map[uint64]uint64{}}
	_, syncs := inner.(store.Syncer)
	_, truncates := inner.(store.Truncater)
	switch {
	case syncs && truncates:
		return syncingTruncating{l}
	case syncs:
		return syncing{l}
	case truncates:
		return truncating{l}
	}
	return l
}

type syncing struct{ *loggedStore }

func (s syncing) Sync(ctx context.Context, gen, upToSeq uint64) error {
	return s.sync(ctx, gen, upToSeq)
}

type truncating struct{ *loggedStore }

func (t truncating) Truncate(ctx context.Context, gen, before uint64) error {
	return t.truncate(ctx, gen, before)
}

type syncingTruncating struct{ *loggedStore }

func (s syncingTruncating) Sync(ctx context.Context, gen, upToSeq uint64) error {
	return s.sync(ctx, gen, upToSeq)
}

func (s syncingTruncating) Truncate(ctx context.Context, gen, before uint64) error {
	return s.truncate(ctx, gen, before)
}

// call logs begin, runs fn, and logs its end with its error.
func (l *loggedStore) call(begin Event, fn func() error) error {
	begin.Kind, begin.Phase = "store", "begin"
	l.ev.Log(begin)
	err := fn()
	end := begin
	end.Phase = "end"
	if err != nil {
		end.Err = err.Error()
	}
	l.ev.Log(end)
	return err
}

func (l *loggedStore) Open(ctx context.Context) (store.State, error) {
	l.ev.Log(Event{Kind: "store", Call: "open", Phase: "begin"})
	st, err := l.inner.Open(ctx)
	end := Event{Kind: "store", Call: "open", Phase: "end", Gen: st.Generation, Next: st.NextSender, Target: st.NextTarget}
	if err != nil {
		end.Err = err.Error()
	}
	l.ev.Log(end)
	return st, err
}

// Append logs its end and moves the generation's frontier at once, under
// mu: a Sync whose begin is logged after an Append's end has it below its
// cutoff.
func (l *loggedStore) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	ev := Event{Kind: "store", Phase: "begin", Call: "append", Gen: gen, Seq: seq, Hash: hash(raw)}
	if id := clOrdID(raw); id != "" {
		ev.ClOrdID, ev.Digest = id, digestRaw(raw)
	}
	l.ev.Log(ev)
	err := l.inner.Append(ctx, gen, seq, raw)
	ev.Phase = "end"
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		ev.Err = err.Error()
	} else {
		l.appended[gen] = max(l.appended[gen], seq)
	}
	l.ev.Log(ev)
	return err
}

func (l *loggedStore) SetNextTarget(ctx context.Context, gen, next uint64) error {
	return l.call(Event{Call: "set-next-target", Gen: gen, Target: next}, func() error {
		return l.inner.SetNextTarget(ctx, gen, next)
	})
}

func (l *loggedStore) SetNextSender(ctx context.Context, gen, next uint64) error {
	return l.call(Event{Call: "set-next-sender", Gen: gen, Next: next}, func() error {
		return l.inner.SetNextSender(ctx, gen, next)
	})
}

// Range logs the holes it reported, in runs: which messages a resend could
// not find.
func (l *loggedStore) Range(ctx context.Context, gen, begin, end uint64, fn func(seq uint64, raw []byte, hole store.Hole) error) error {
	ev := Event{Kind: "store", Call: "range", Phase: "begin", Gen: gen, Begin: begin, End: end}
	l.ev.Log(ev)
	var holes []HoleRun
	err := l.inner.Range(ctx, gen, begin, end, func(seq uint64, raw []byte, hole store.Hole) error {
		if hole != store.HoleNone {
			if n := len(holes); n > 0 && holes[n-1].To+1 == seq && holes[n-1].Kind == hole.String() {
				holes[n-1].To = seq
			} else {
				holes = append(holes, HoleRun{From: seq, To: seq, Kind: hole.String()})
			}
		}
		return fn(seq, raw, hole)
	})
	ev.Phase, ev.Holes = "end", holes
	if err != nil {
		ev.Err = err.Error()
	}
	l.ev.Log(ev)
	return err
}

// Reset logs the generation it started.
func (l *loggedStore) Reset(ctx context.Context, expectGen uint64, createdAt time.Time, windowID string) error {
	ev := Event{Kind: "store", Call: "reset", Phase: "begin", Gen: expectGen}
	l.ev.Log(ev)
	err := l.inner.Reset(ctx, expectGen, createdAt, windowID)
	ev.Phase = "end"
	if err != nil {
		ev.Err = err.Error()
	} else {
		ev.NewGen = expectGen + 1
	}
	l.ev.Log(ev)
	return err
}

func (l *loggedStore) Close() error {
	return l.call(Event{Call: "close"}, l.inner.Close)
}

// sync logs, with the Sync's begin, the last Append of gen completed then:
// the SQLite store's Sync flushes every commit before it, past upToSeq.
// Its begin is logged with the cutoff under mu, the inner Sync run without.
func (l *loggedStore) sync(ctx context.Context, gen, upToSeq uint64) error {
	l.mu.Lock()
	ev := Event{Kind: "store", Phase: "begin", Call: "sync", Gen: gen, UpTo: upToSeq, Cutoff: l.appended[gen]}
	l.ev.Log(ev)
	l.mu.Unlock()
	var err error
	if !l.noSync {
		err = l.inner.(store.Syncer).Sync(ctx, gen, upToSeq)
	}
	ev.Phase = "end"
	if err != nil {
		ev.Err = err.Error()
	}
	l.ev.Log(ev)
	return err
}

func (l *loggedStore) truncate(ctx context.Context, gen, before uint64) error {
	return l.call(Event{Call: "truncate", Gen: gen, Begin: before}, func() error {
		return l.inner.(store.Truncater).Truncate(ctx, gen, before)
	})
}
