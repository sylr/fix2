package session_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// logonsSeen records the Logons a session receives, and its log.
type logonsSeen struct {
	mu  sync.Mutex
	in  []fix.Message
	log strings.Builder
}

func (o *logonsSeen) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.log.Write(b)
}

func (o *logonsSeen) logged(text string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Contains(o.log.String(), text)
}

func (o *logonsSeen) OnInbound(_ *session.Session, f session.Frame) {
	m, err := fix.Parse(f.Raw)
	if err != nil || string(m.MsgType()) != "A" {
		return
	}
	o.mu.Lock()
	o.in = append(o.in, *m.Clone())
	o.mu.Unlock()
}

func (o *logonsSeen) OnOutbound(*session.Session, session.Frame) {}

func (o *logonsSeen) first(t *testing.T) *fix.Message {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.in) == 0 {
		t.Fatal("no Logon received")
	}
	return &o.in[0]
}

// resetOnLogonPair logs a fix2 initiator, which does not ask for a reset, on
// to a fix2 acceptor with ResetOnLogon, each starting at the
// sequence numbers given, UnannouncedResetOnLogon if unannounced. It
// returns once the initiator received the Logon
// answer, which seen holds.
func resetOnLogonPair(t *testing.T, begin string, unannounced bool, accAt, iniAt uint64) (ini, acc *session.Session, ir, ar *recorder, seen *logonsSeen) {
	t.Helper()
	dicts := session.Dictionaries{}
	if begin == session.FIXT11 {
		dicts.DefaultApplVerID = "9"
	}
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	ar = newRecorder()
	acc, err := a.Add(session.Config{
		ID:                      session.SessionID{BeginString: begin, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler:                 ar,
		Dictionaries:            dicts,
		ResetOnLogon:            true,
		UnannouncedResetOnLogon: unannounced,
	})
	if err != nil {
		t.Fatal(err)
	}
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: time.Hour})
	ir = newRecorder()
	seen = &logonsSeen{}
	ini, err = i.Add(session.Config{
		ID:           session.SessionID{BeginString: begin, SenderCompID: "INI", TargetCompID: "ACC"},
		Handler:      ir,
		Dictionaries: dicts,
		Observer:     seen,
		Logger:       slog.New(slog.NewTextHandler(seen, nil)),
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []struct {
		s *session.Session
		n uint64
	}{{acc, accAt}, {ini, iniAt}} {
		if err := at.s.SetNextSenderSeqNum(at.n); err != nil {
			t.Fatal(err)
		}
		if err := at.s.SetNextTargetSeqNum(at.n); err != nil {
			t.Fatal(err)
		}
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(ctx) }()
	go func() { defer wg.Done(); i.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	waitFor(t, func() bool {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		return len(seen.in) > 0
	})
	return ini, acc, ir, ar, seen
}

// exchange sends an order each way, and checks both arrive with the
// sequence numbers given and no one logs out.
func exchange(t *testing.T, begin string, ini, acc *session.Session, ir, ar *recorder, iniNext, accNext uint64) {
	t.Helper()
	ir.wait(t, func() bool { return ir.logons == 1 })
	ar.wait(t, func() bool { return ar.logons == 1 })
	ctx := context.Background()
	if _, err := ini.Send(ctx, fix.NewBuilder(begin, "D").SetString(11, "TO-ACC").SetString(55, "X")); err != nil {
		t.Fatal(err)
	}
	if _, err := acc.Send(ctx, fix.NewBuilder(begin, "D").SetString(11, "TO-INI").SetString(55, "X")); err != nil {
		t.Fatal(err)
	}
	ar.wait(t, func() bool { return len(ar.apps) == 1 || len(ar.logouts) > 0 })
	ir.wait(t, func() bool { return len(ir.apps) == 1 || len(ir.logouts) > 0 })
	time.Sleep(20 * time.Millisecond)
	for _, r := range []*recorder{ir, ar} {
		r.mu.Lock()
		if len(r.logouts) > 0 {
			t.Errorf("logged out: %v", r.logouts)
		}
		r.mu.Unlock()
	}
	if n, m := ini.NextSenderSeqNum(), acc.NextTargetSeqNum(); n != iniNext || m != iniNext {
		t.Errorf("initiator's next MsgSeqNum %d, acceptor expects %d, want %d", n, m, iniNext)
	}
	if n, m := acc.NextSenderSeqNum(), ini.NextTargetSeqNum(); n != accNext || m != accNext {
		t.Errorf("acceptor's next MsgSeqNum %d, initiator expects %d, want %d", n, m, accNext)
	}
}

// By default, an acceptor with ResetOnLogon whose sequence
// numbers had moved announces the reset it makes, though the initiator did
// not ask for one: the initiator resets too, and both restart at 1.
func TestResetOnLogon_StrictAnnouncedWhenSequenceMoved(t *testing.T) {
	for _, begin := range []string{session.FIX42, session.FIX44, session.FIXT11} {
		for _, iniAt := range []uint64{1, 5} {
			t.Run(fmt.Sprintf("%s/initiator at %d", begin, iniAt), func(t *testing.T) {
				ini, acc, ir, ar, seen := resetOnLogonPair(t, begin, false, 5, iniAt)
				answer := seen.first(t)
				if got := string(answer.Get(34)); got != "1" {
					t.Errorf("Logon answer MsgSeqNum %s, want 1", got)
				}
				if got := string(answer.Get(141)); got != "Y" {
					t.Errorf("Logon answer ResetSeqNumFlag %q, want Y", got)
				}
				// The initiator's next message is 1, as is the acceptor's
				// after its Logon answer, 1.
				exchange(t, begin, ini, acc, ir, ar, 2, 3)
			})
		}
	}
}

// By default, an acceptor with ResetOnLogon at the start of its
// sequence still announces the reset to an initiator whose Logon shows it
// expects the sequences to go on: both restart at 1.
func TestResetOnLogon_StrictAnnouncedToStaleInitiator(t *testing.T) {
	for _, begin := range []string{session.FIX42, session.FIX44, session.FIXT11} {
		t.Run(begin, func(t *testing.T) {
			ini, acc, ir, ar, seen := resetOnLogonPair(t, begin, false, 1, 5)
			answer := seen.first(t)
			if got := string(answer.Get(34)); got != "1" {
				t.Errorf("Logon answer MsgSeqNum %s, want 1", got)
			}
			if got := string(answer.Get(141)); got != "Y" {
				t.Errorf("Logon answer ResetSeqNumFlag %q, want Y", got)
			}
			exchange(t, begin, ini, acc, ir, ar, 2, 3)
		})
	}
}

// With UnannouncedResetOnLogon, an acceptor with ResetOnLogon
// answers as QuickFIX does, without
// ResetSeqNumFlag, whatever its sequence numbers were: an initiator at the
// start of its sequence goes on, its Logon counted; one that expected the
// acceptor's sequence to go on logs out, MsgSeqNum too low.
func TestResetOnLogon_UnannouncedNotAnnounced(t *testing.T) {
	for _, begin := range []string{session.FIX42, session.FIX44, session.FIXT11} {
		t.Run(begin+"/initiator at 1", func(t *testing.T) {
			ini, acc, ir, ar, seen := resetOnLogonPair(t, begin, true, 5, 1)
			answer := seen.first(t)
			if got := string(answer.Get(34)); got != "1" {
				t.Errorf("Logon answer MsgSeqNum %s, want 1", got)
			}
			if v := answer.Get(141); v != nil {
				t.Errorf("Logon answer ResetSeqNumFlag %q, want none", v)
			}
			exchange(t, begin, ini, acc, ir, ar, 3, 3)
		})
		t.Run(begin+"/initiator at 5", func(t *testing.T) {
			_, _, ir, _, seen := resetOnLogonPair(t, begin, true, 5, 5)
			if v := seen.first(t).Get(141); v != nil {
				t.Errorf("Logon answer ResetSeqNumFlag %q, want none", v)
			}
			waitFor(t, func() bool { return seen.logged("MsgSeqNum too low, expecting 5 but received 1") })
			ir.mu.Lock()
			defer ir.mu.Unlock()
			if ir.logons != 0 {
				t.Error("initiator logged on")
			}
		})
	}
}

// An acceptor with ResetOnLogon at the start of its sequence has nothing to
// announce, in either mode: its Logon answer carries no ResetSeqNumFlag.
func TestResetOnLogon_FreshNotAnnounced(t *testing.T) {
	for _, mode := range resetModes {
		for _, begin := range []string{session.FIX40, session.FIX42, session.FIX44, session.FIXT11} {
			t.Run(mode.name+"/"+begin, func(t *testing.T) {
				ini, acc, ir, ar, seen := resetOnLogonPair(t, begin, mode.unannounced, 1, 1)
				if v := seen.first(t).Get(141); v != nil {
					t.Errorf("Logon answer ResetSeqNumFlag %q, want none", v)
				}
				// Both Logons count: the next messages are 2.
				exchange(t, begin, ini, acc, ir, ar, 3, 3)
			})
		}
	}
}

// FIX 4.0 has no ResetSeqNumFlag to announce a reset with: an acceptor with
// ResetOnLogon answers at 1 without it, and its Logon counts, in either mode.
func TestResetOnLogon_FIX40CannotAnnounce(t *testing.T) {
	for _, mode := range resetModes {
		t.Run(mode.name, func(t *testing.T) {
			ini, acc, ir, ar, seen := resetOnLogonPair(t, session.FIX40, mode.unannounced, 5, 1)
			answer := seen.first(t)
			if got := string(answer.Get(34)); got != "1" {
				t.Errorf("Logon answer MsgSeqNum %s, want 1", got)
			}
			if v := answer.Get(141); v != nil {
				t.Errorf("Logon answer ResetSeqNumFlag %q, want none", v)
			}
			exchange(t, session.FIX40, ini, acc, ir, ar, 3, 3)
		})
	}
}

// resetModes are the ways an acceptor with ResetOnLogon may answer: by
// default, or as QuickFIX's acceptance tests expect.
var resetModes = []struct {
	name        string
	unannounced bool
}{
	{"announced", false},
	{"unannounced", true},
}
