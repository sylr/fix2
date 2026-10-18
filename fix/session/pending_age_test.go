package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session"
)

// A message left waiting past MaxPendingAge disconnects the counterparty
// then, not at the session's next deadline, here the heartbeat 30s away.
func TestMaxPendingAge_Prompt(t *testing.T) {
	st := newSyncStore()
	s, rec, _ := loggedOn(t, func(c *session.Config) {
		c.Store = st
		c.MaxPendingAge = 500 * time.Millisecond
	})
	// The writer syncs O1 for 3s: O2 waits behind it.
	st.delay.Store(int64(3 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Send(ctx, order("O1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	queued := time.Now()
	if _, err := s.Send(ctx, order("O2")); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, func() bool { return len(rec.logouts) == 1 })
	waited := time.Since(queued)
	rec.mu.Lock()
	reason := rec.logouts[0]
	rec.mu.Unlock()
	if !errors.Is(reason, session.ErrSlowConsumer) {
		t.Fatalf("logged out for %v, want ErrSlowConsumer", reason)
	}
	if waited < 400*time.Millisecond || waited > 2500*time.Millisecond {
		t.Errorf("found too old after %v, want MaxPendingAge, 500ms", waited)
	}
}
