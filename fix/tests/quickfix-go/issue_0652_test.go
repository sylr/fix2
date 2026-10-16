package quickfixgo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/schedule"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// https://github.com/quickfixgo/quickfix/issues/652
//
// The session reset only once it saw itself out of session: a process frozen
// over the gap between two windows never did, and ran the new window on the
// old sequence numbers. A new window must start afresh however its start was
// noticed: with no gap to see, or after the process missed the boundary.
func TestIssue652_BackToBackWindowsReset(t *testing.T) {
	sched := i652Schedule{base: time.Now(), length: time.Second}
	s, rec, l := startInitiator(t, func(c *session.Config) { c.Schedule = sched })
	p := acceptPeer(t, l)
	p.logon()
	rec.wait(t, "logon", func() bool { return rec.logons == 1 })
	if time.Since(sched.base) > sched.length/2 {
		t.Skip("logged on too late in the first window to exchange an order within it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := s.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "2")
	p.send("8", 0, "11", "O1")
	// The window ends: logged out, then on again in the next one, afresh.
	p.skipUntil("5")
	p.send("5", 0)
	p2 := acceptPeer(t, l)
	p2.expect("A", "34", "1")
}

// A store left by an earlier window, as a process frozen or down over the
// boundary finds it, starts afresh too.
func TestIssue652_StaleWindowResets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st := memstore.New()
	if _, err := st.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Reset(ctx, 1, time.Now().Add(-25*time.Hour), "i652 yesterday"); err != nil {
		t.Fatal(err)
	}
	for seq := range uint64(3) {
		if err := st.Append(ctx, 2, seq+1, raw("8=FIX.4.4|35=0|34="+fmt.Sprint(seq+1))); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetNextTarget(ctx, 2, 4); err != nil {
		t.Fatal(err)
	}
	sched := i652Schedule{base: time.Now(), length: time.Hour}
	_, _, l := startInitiator(t, func(c *session.Config) {
		c.Schedule = sched
		c.Store = st
	})
	p := acceptPeer(t, l)
	p.expect("A", "34", "1")
}

// i652Schedule has windows of length one after the other from base, without
// a gap between them.
type i652Schedule struct {
	base   time.Time
	length time.Duration
}

func (s i652Schedule) Window(t time.Time) (schedule.Window, bool, bool) {
	k := int64(0)
	if t.After(s.base) {
		k = int64(t.Sub(s.base) / s.length)
	}
	start := s.base.Add(time.Duration(k) * s.length)
	w := schedule.Window{Start: start, End: start.Add(s.length), ID: fmt.Sprintf("i652 #%d", k)}
	return w, w.Contains(t), true
}

func (i652Schedule) String() string { return "i652" }
