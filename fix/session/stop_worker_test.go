package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session"
)

// workerApp hands the ctx of its OnLogon, once returned, to a worker.
type workerApp struct {
	*blockingApp
	ctxs chan context.Context
}

func (w *workerApp) OnLogon(ctx context.Context, s *session.Session) {
	w.blockingApp.OnLogon(ctx, s)
	w.ctxs <- ctx
}

// A worker given a callback's ctx, calling Stop once the callback returned,
// is not in a callback: Stop waits for Done, or for its ctx's deadline.
func TestStopFromWorkerWithCallbackCtx(t *testing.T) {
	t.Run("waits for Done", func(t *testing.T) {
		h := &workerApp{blockingApp: newBlockingApp(t), ctxs: make(chan context.Context, 1)}
		s, _, p := loggedOn(t, func(c *session.Config) {
			h.recorder = c.Handler.(*recorder)
			c.Handler = h
			c.LogoutTimeout = 100 * time.Millisecond
		})
		ctx := <-h.ctxs
		// FromApp called: OnLogon returned.
		p.sendOrders(1)
		h.waitEntered(t)
		h.unblock()
		if err := s.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
		select {
		case <-s.Done():
		default:
			t.Error("Stop returned before Done closed, called with the ctx of a callback that returned")
		}
	})
	t.Run("honours the deadline", func(t *testing.T) {
		h := &workerApp{blockingApp: newBlockingApp(t), ctxs: make(chan context.Context, 1)}
		s, _, p := loggedOn(t, func(c *session.Config) {
			h.recorder = c.Handler.(*recorder)
			c.Handler = h
			c.LogoutTimeout = 100 * time.Millisecond
		})
		ctx := <-h.ctxs
		// FromApp called, and stuck: OnLogon returned.
		p.sendOrders(1)
		h.waitEntered(t)
		ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		s.Stop(ctx)
		if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
			t.Errorf("Stop returned after %v, its ctx's deadline 300ms away, FromApp stuck", d)
		}
		h.unblock()
		waitDone(t, s)
	})
}
