package quickfixgo

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/schedule"
	"github.com/sylr/fix2/session/transport/mem"
)

// https://github.com/quickfixgo/quickfix/issues/476
//
// After Initiator.Stop a session's event loop never exited: its stopped
// flag was never set, so its goroutine and timers lived on. Once Run
// returns, no goroutine of the sessions it ran may be left.

// i476Schedule is a window open for the whole test, so that the schedule's
// goroutine runs too.
type i476Schedule struct{ w schedule.Window }

func (s i476Schedule) String() string { return "issue 476" }

func (s i476Schedule) Window(time.Time) (schedule.Window, bool, bool) { return s.w, true, true }

// i476Goroutines returns the stacks of the goroutines running fix2's session
// code, by goroutine header.
func i476Goroutines() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := make(map[string]string)
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "github.com/sylr/fix2/session") {
			id, _, _ := strings.Cut(g, " [")
			out[id] = g
		}
	}
	return out
}

func TestIssue476_NoGoroutineLeftAfterRun(t *testing.T) {
	before := i476Goroutines()
	now := time.Now()
	sched := i476Schedule{schedule.Window{Start: now.Add(-time.Hour), End: now.Add(time.Hour), ID: "w"}}
	withSchedule := func(c *session.Config) { c.Schedule = sched }

	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{Logger: testLogger()})
	ar := newRecorder()
	if _, err := a.Add(accConfig(ar, withSchedule)); err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	i := session.NewInitiator(session.InitiatorOptions{Logger: testLogger(), ReconnectMin: 20 * time.Millisecond})
	ir := newRecorder()
	if _, err := i.Add(iniConfig(ir, withSchedule), l); err != nil {
		t.Fatal(err)
	}
	actx, acancel := context.WithCancel(context.Background())
	ictx, icancel := context.WithCancel(context.Background())
	adone, idone := make(chan struct{}), make(chan struct{})
	go func() { defer close(adone); a.Run(actx) }()
	go func() { defer close(idone); i.Run(ictx) }()
	ir.wait(t, "initiator logon", func() bool { return ir.logons == 1 })
	ar.wait(t, "acceptor logon", func() bool { return ar.logons == 1 })
	if len(i476Goroutines()) <= len(before) {
		t.Fatal("no session goroutine seen while logged on")
	}

	icancel()
	<-idone
	ir.wait(t, "initiator logout", func() bool { return len(ir.logouts) == 1 })
	acancel()
	<-adone

	// Goroutines winding down when Run returned get a moment to exit.
	var left []string
	deadline := time.Now().Add(time.Second)
	for {
		left = left[:0]
		for id, g := range i476Goroutines() {
			if _, ok := before[id]; !ok {
				left = append(left, g)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, g := range left {
		t.Errorf("goroutine left after Run:\n%s", g)
	}
}
