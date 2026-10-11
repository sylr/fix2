package session_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/schedule"
	"github.com/sylr/fix2/session/transport/mem"
)

// windows is a Schedule of fixed windows.
type windows []schedule.Window

func (ws windows) String() string { return "test windows" }

func (ws windows) Window(t time.Time) (schedule.Window, bool, bool) {
	for _, w := range ws {
		if t.Before(w.End) {
			return w, !t.Before(w.Start), true
		}
	}
	return schedule.Window{}, false, false
}

func TestScheduleWindows(t *testing.T) {
	now := time.Now()
	sched := windows{
		{Start: now.Add(300 * time.Millisecond), End: now.Add(1500 * time.Millisecond), ID: "w1"},
		{Start: now.Add(2 * time.Second), End: now.Add(4 * time.Second), ID: "w2"},
	}
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	ar := newRecorder()
	acc, err := a.Add(session.Config{
		ID:       session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler:  ar,
		Schedule: sched,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond})
	ir := newRecorder()
	ini, err := i.Add(session.Config{
		ID:       session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler:  ir,
		Schedule: sched,
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(ctx) }()
	go func() { defer wg.Done(); i.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()

	// Nothing before the first window.
	time.Sleep(150 * time.Millisecond)
	if st := ini.State(); st != session.StateDisconnected {
		t.Fatalf("before the window: initiator %s", st)
	}
	ir.wait(t, func() bool { return ir.logons == 1 })
	if since := time.Since(now); since < 300*time.Millisecond {
		t.Errorf("logged on %v after the start, before the window", since)
	}
	// The window ends: logged out, with the reason.
	ar.wait(t, func() bool { return len(ar.logouts) == 1 })
	if since := time.Since(now); since < 1500*time.Millisecond {
		t.Errorf("logged out after %v, before the window ended", since)
	}
	ar.mu.Lock()
	reason := ar.logouts[0]
	ar.mu.Unlock()
	if reason == nil || !strings.Contains(reason.Error(), "session window ended") {
		t.Errorf("logout reason %v, want the window's end", reason)
	}
	// The second window starts afresh.
	ir.wait(t, func() bool { return ir.logons == 2 })
	if ini.NextTargetSeqNum() > 3 || acc.NextTargetSeqNum() > 3 {
		t.Errorf("second window: next target %d and %d, want a new sequence", ini.NextTargetSeqNum(), acc.NextTargetSeqNum())
	}
}
