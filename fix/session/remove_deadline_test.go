package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// blockedAdmin blocks FromAdmin on a Heartbeat until release is closed.
type blockedAdmin struct {
	*recorder
	entered chan struct{}
	release chan struct{}
}

func (h *blockedAdmin) FromAdmin(_ context.Context, _ *session.Session, m *fix.Message, _ session.Inbound) {
	if string(m.MsgType()) == "0" {
		close(h.entered)
		<-h.release
	}
}

// Initiator.Remove returns at its ctx's deadline though FromAdmin, run by
// the connection's goroutine, is stuck; Done tells once it returned.
func TestRemoveHonorsDeadlineWithReaderCallback(t *testing.T) {
	h := &blockedAdmin{recorder: newRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(h.release)
		}
	}()
	l := mem.Listen("acceptor")
	i := session.NewInitiator(session.InitiatorOptions{})
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	s, err := i.Add(session.Config{ID: id, Handler: h, LogoutTimeout: 100 * time.Millisecond}, l)
	if err != nil {
		t.Fatal(err)
	}
	runUntilCleanup(t, i.Run)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	h.wait(t, func() bool { return h.logons == 1 })
	p.send("0", 0)
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("FromAdmin not called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	removed := make(chan error, 1)
	go func() { removed <- i.Remove(ctx, id) }()
	select {
	case err := <-removed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Remove = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(600 * time.Millisecond):
		t.Error("Remove still waiting 500ms past its deadline while FromAdmin is stuck")
	}
	select {
	case <-s.Done():
		t.Error("Done closed while FromAdmin is stuck")
	default:
	}
	released = true
	close(h.release)
	waitDone(t, s)
}
