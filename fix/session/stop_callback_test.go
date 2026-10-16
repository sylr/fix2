package session_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// stopper stops its session from one of its callbacks, once, with the
// callback's ctx, which never ends: the test cancels it only if the stop
// is stuck.
type stopper struct {
	*recorder
	in      string // the callback stopping: OnLogon, FromAdmin or FromApp
	stop    func(context.Context, *session.Session) error
	once    atomic.Bool
	cancels chan context.CancelFunc
	stopped chan error
}

func (h *stopper) stopFrom(ctx context.Context, s *session.Session, in string) {
	if h.in != in || h.once.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	h.cancels <- cancel
	h.stopped <- h.stop(ctx, s)
}

func (h *stopper) OnLogon(ctx context.Context, s *session.Session) {
	h.recorder.OnLogon(ctx, s)
	h.stopFrom(ctx, s, "OnLogon")
}

func (h *stopper) FromAdmin(ctx context.Context, s *session.Session, m *fix.Message, _ session.Inbound) {
	if string(m.MsgType()) == "0" {
		h.stopFrom(ctx, s, "FromAdmin")
	}
}

func (h *stopper) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
	h.recorder.FromApp(ctx, s, m, in)
	h.stopFrom(ctx, s, "FromApp")
	return nil
}

// Stop, or an Initiator's Remove, called from one of the session's own
// callbacks with a ctx that never ends does not wait for that callback: it
// returns, the session logs out, and Done closes once the callback
// returned.
func TestStopFromCallback(t *testing.T) {
	for _, in := range []string{"OnLogon", "FromAdmin", "FromApp"} {
		for _, how := range []string{"Stop", "Remove"} {
			t.Run(how+" from "+in, func(t *testing.T) {
				l := mem.Listen("acceptor")
				i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond})
				h := &stopper{recorder: newRecorder(), in: in, cancels: make(chan context.CancelFunc, 1), stopped: make(chan error, 1)}
				h.stop = func(ctx context.Context, s *session.Session) error {
					if how == "Remove" {
						return i.Remove(ctx, s.ID())
					}
					return s.Stop(ctx)
				}
				s, err := i.Add(session.Config{
					ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
					Handler:       h,
					LogoutTimeout: 100 * time.Millisecond,
				}, l)
				if err != nil {
					t.Fatal(err)
				}
				runUntilCleanup(t, i.Run)
				p := acceptPeer(t, l)
				p.expect("A")
				p.send("A", 0, "98", "0", "108", "30")
				switch in {
				case "FromAdmin":
					p.send("0", 0)
				case "FromApp":
					p.sendOrders(1)
				}

				var cancel context.CancelFunc
				select {
				case cancel = <-h.cancels:
				case <-time.After(5 * time.Second):
					t.Fatalf("%s not called", in)
				}
				defer cancel()
				select {
				case err := <-h.stopped:
					if err != nil {
						t.Errorf("%s from %s: %v", how, in, err)
					}
				case <-time.After(2 * time.Second):
					t.Errorf("%s from %s waits for %s to return", how, in, in)
					cancel()
					select {
					case <-h.stopped:
					case <-time.After(5 * time.Second):
						t.Fatalf("%s from %s stuck once its ctx ended", how, in)
					}
				}
				p.expect("5")
				waitDone(t, s)
				if i.Session(s.ID()) != nil && how == "Remove" {
					t.Error("session not removed")
				}
			})
		}
	}
}
