package session_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// genLosses is an eventRecorder that is a RecoveryLossHandler: OnLoss, not
// OnRecoveryLoss, tells it of losses.
type genLosses struct {
	*eventRecorder
	losses []session.RecoveryLoss // guarded by recorder.mu
}

func (r *genLosses) OnLoss(_ context.Context, _ *session.Session, l session.RecoveryLoss) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf("gen-loss %d-%d", l.Begin, l.End))
	r.losses = append(r.losses, l)
	r.mu.Unlock()
	r.notify()
}

// A RecoveryLossHandler is told of the counterparty's messages reset past
// with their generation, in sequence, and not by OnRecoveryLoss.
func TestRecoveryLossHandler_ResetPast(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var h *genLosses
		_, rec, p := heldGap(t, 3, func(c *session.Config) {
			h = &genLosses{eventRecorder: c.Handler.(*eventRecorder)}
			c.Handler = h
		})
		p.order(4, "A")
		p.resetTo(5, 6)
		rec.waitEvents(t, "gen-loss 1-2", "app A")
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if l := h.losses[0]; l.Generation != 1 || l.Ours || l.Cause == nil {
			t.Errorf("loss = %+v, want generation 1, the counterparty's, a cause", l)
		}
	})
}

// lossHeld is a RecoveryLossHandler whose first OnLoss waits for release.
type lossHeld struct {
	*recorder
	entered, release chan struct{}
	got              chan session.RecoveryLoss
	n                int
}

func (h *lossHeld) OnLoss(_ context.Context, _ *session.Session, l session.RecoveryLoss) {
	if h.n++; h.n == 1 {
		close(h.entered)
		<-h.release
	}
	h.got <- l
}

// A loss told after a reset started another generation keeps the one it
// belongs to: queued behind another the handler is slow with, it waits
// across a ResetOnDisconnect, which does not wait for the handler as a
// reset on the connection does.
func TestRecoveryLossHandler_AfterReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &lossHeld{recorder: newRecorder(), entered: make(chan struct{}), release: make(chan struct{}), got: make(chan session.RecoveryLoss, 2)}
		l := mem.Listen("acceptor")
		startInitiator(t, l, func(c *session.Config) { c.Handler = h; c.ResetOnDisconnect = true })
		p := acceptPeer(t, l)
		p.expect("A")
		p.send("A", 3, "98", "0", "108", "30")
		h.wait(t, func() bool { return h.logons == 1 })
		p.expect("2", "7", "1", "16", "0")
		p.order(4, "A")
		p.order(6, "B")
		p.resetTo(7, 8) // losses 1-2, then 5-5
		select {
		case <-h.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("OnLoss not called")
		}
		p.conn.Close()
		// Logging on again from 1: the reset took place.
		acceptPeer(t, l).expect("A", "34", "1")
		close(h.release)
		for _, want := range [][2]uint64{{1, 2}, {5, 5}} {
			select {
			case got := <-h.got:
				if got.Generation != 1 || got.Begin != want[0] || got.End != want[1] || got.Ours {
					t.Errorf("loss = %+v, want generation 1, the counterparty's %d to %d", got, want[0], want[1])
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("loss %d-%d not told", want[0], want[1])
			}
		}
	})
}

// lossesOf is a lossRecorder that is a RecoveryLossHandler.
type lossesOf struct {
	*lossRecorder
	full []session.RecoveryLoss
}

func (r *lossesOf) OnLoss(_ context.Context, _ *session.Session, l session.RecoveryLoss) {
	r.mu.Lock()
	r.full = append(r.full, l)
	r.mu.Unlock()
	r.notify()
}

// Our messages evicted and asked for again are ours, of their generation.
func TestRecoveryLossHandler_Ours(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var h *lossesOf
		r := session.Retention{Interval: 10 * time.Millisecond, Keep: 2}
		s, _, p := retained(t, r, 10, func(c *session.Config) {
			h = &lossesOf{lossRecorder: c.Handler.(*lossRecorder)}
			c.Handler = h
		})
		p.send("0", 0, "369", "8")
		waitFor(t, func() bool { return s.Stats().Evicted.Load() == 8 })
		settle(t, s)
		p.send("2", 0, "7", "2", "16", "0")
		p.expect("4", "34", "2", "36", "9", "123", "Y", "43", "Y")
		h.wait(t, func() bool { return len(h.full) == 1 })
		h.mu.Lock()
		defer h.mu.Unlock()
		if l := h.full[0]; l.Generation != 1 || !l.Ours || l.Begin != 2 || l.End != 8 || !errors.Is(l.Cause, session.ErrBeyondRetention) {
			t.Errorf("loss = %+v, want generation 1, ours, 2 to 8, ErrBeyondRetention", l)
		}
		if len(h.losses) != 0 {
			t.Errorf("OnRecoveryLoss told %+v as well", h.losses)
		}
	})
}
