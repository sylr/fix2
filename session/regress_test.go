package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport/mem"
	"github.com/sylr/fix2/tests/dicts/fix44"
	"github.com/sylr/fix2/tests/dicts/fix50sp2"
	"github.com/sylr/fix2/tests/dicts/fixt11"
)

// A message written, then gap-filled at the counterparty's request when the
// handler vetoes its resend, was read before the request: a probe answered
// after acknowledges it. The GapFill itself never counts as its write.
func TestReceipt_WrittenThenVetoed(t *testing.T) {
	var rec *recorder
	s, _, p := loggedOn(t, func(c *session.Config) {
		rec = c.Handler.(*recorder)
		c.Handler = noResend{rec}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.Send(ctx, order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	p.send("2", 0, "7", "2", "16", "0")
	p.expect("4")
	// Another message and a probe: answering it acknowledges what was
	// written, not the gap-filled message.
	r2, _ := s.Send(ctx, order("O2"))
	p.expect("D", "11", "O2")
	done := make(chan error, 1)
	go func() { done <- r2.Wait(ctx, session.Acknowledged) }()
	tr := p.expect("1")
	p.send("0", 0, "112", string(tr.Get(112)))
	if err := <-done; err != nil {
		t.Fatalf("second message: %v", err)
	}
	if err := r.Wait(ctx, session.Acknowledged); err != nil {
		t.Errorf("message written before the probe: Wait = %v, want acknowledged", err)
	}
}

// Two sessions of this package with NextExpectedMsgSeqNum log on to each
// other.
func TestNextExpectedBothSides(t *testing.T) {
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	ar := newRecorder()
	if _, err := a.Add(session.Config{
		ID:                    session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler:               ar,
		NextExpectedMsgSeqNum: true,
	}); err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()
	_, ir := startInitiator(t, l, func(c *session.Config) { c.NextExpectedMsgSeqNum = true })
	ir.wait(t, func() bool { return ir.logons == 1 })
	ar.wait(t, func() bool { return ar.logons == 1 })
}

// Receipts follow the store's generation, whatever it is when the session
// starts; a level reached stays reached once the session stops.
func TestReceipt_RestoredGenerationAndStop(t *testing.T) {
	st := memstore.New()
	gen, _ := st.Open(context.Background())
	if err := st.Reset(context.Background(), gen.Generation, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	s, _, p := loggedOn(t, func(c *session.Config) { c.Store = st })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.Send(ctx, order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Generation() != 2 {
		t.Errorf("Generation = %d, want 2", r.Generation())
	}
	if err := r.Wait(ctx, session.Written); err != nil {
		t.Fatalf("Wait(Written) = %v", err)
	}
	p.expect("D")
	done := make(chan error, 1)
	go func() { done <- r.Wait(ctx, session.Acknowledged) }()
	tr := p.expect("1")
	p.send("0", 0, "112", string(tr.Get(112)))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	go func() {
		p.expect("5")
		p.send("5", 0)
	}()
	s.Stop(ctx)
	if err := r.Wait(ctx, session.Acknowledged); err != nil {
		t.Errorf("after Stop: Wait = %v, want the acknowledgement kept", err)
	}
}

// A handler stuck past MaxDeliveryStall disconnects the session, which
// connects again once the handler returns.
type stuck struct {
	*recorder
	release chan struct{}
	once    sync.Once
}

func (h *stuck) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	h.once.Do(func() { <-h.release })
	return h.recorder.FromApp(ctx, s, m, in)
}

func TestStallAndRecover(t *testing.T) {
	h := &stuck{recorder: newRecorder(), release: make(chan struct{})}
	l := mem.Listen("acceptor")
	s, _ := startInitiator(t, l, func(c *session.Config) {
		c.Handler = h
		c.DeliveryQueue = 1
		c.MaxDeliveryStall = 300 * time.Millisecond
	})
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	for i := range 5 {
		p.send("D", 0, "11", "O"+string(rune('0'+i)))
	}
	p.expect("5") // "application not processing messages"
	p.expectEnd(2 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for s.State() != session.StateStalled {
		if time.Now().After(deadline) {
			t.Fatalf("state %s, want stalled", s.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(h.release)
	p2 := acceptPeer(t, l) // connects again once unstalled
	p2.expect("A")
}

// An in-sequence PossDup message without OrigSendingTime is rejected, and
// counts as received.
func TestPossDupInSequenceWithoutOrigSendingTime(t *testing.T) {
	_, rec, p := loggedOn(t, nil)
	p.send("D", 0, "43", "Y", "11", "DUP")
	p.expect("3", "45", "2", "371", "122", "373", "1")
	p.send("D", 0, "11", "NEXT")
	rec.wait(t, func() bool { return len(rec.apps) == 1 })
	if rec.apps[0] != "NEXT" {
		t.Errorf("delivered %v", rec.apps)
	}
}

// LastMsgSeqNumProcessed never claims a message the handler has not
// processed.
func TestLastMsgSeqNumProcessed(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	_, _, p := loggedOn(t, func(c *session.Config) {
		c.Handler = &stuck{recorder: c.Handler.(*recorder), release: release}
		c.LastMsgSeqNumProcessed = true
	})
	p.send("D", 0, "11", "BLOCKED") // seq 2, its handler blocks
	p.send("1", 0, "112", "PING")   // seq 3
	p.expect("0", "112", "PING", "369", "1")
}

// failingStore makes Append fail with an uncertain outcome.
type failingStore struct {
	store.Store
	fail bool
}

func (f *failingStore) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	if f.fail {
		return store.ErrUnknownOutcome
	}
	return f.Store.Append(ctx, gen, seq, raw)
}

func TestUncertainStoreHalts(t *testing.T) {
	fs := &failingStore{Store: memstore.New()}
	s, _, p := loggedOn(t, func(c *session.Config) { c.Store = fs })
	fs.fail = true
	if _, err := s.Send(context.Background(), order("O1")); !errors.Is(err, store.ErrUnknownOutcome) {
		t.Fatalf("Send = %v, want ErrUnknownOutcome", err)
	}
	p.expectEnd(2 * time.Second)
	if st := s.State(); st != session.StateStopped {
		t.Errorf("State = %s, want stopped until the store is checked", st)
	}
}

// Run stops its sessions when it ends.
func TestRunStopsSessions(t *testing.T) {
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	s, err := a.Add(session.Config{
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "INI"},
		Handler: newRecorder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if st := s.State(); st != session.StateStopped {
		t.Errorf("after Run: %s, want stopped", st)
	}
}

// refusingRejects is a recorder whose application refuses every
// BusinessMessageReject, as a generated Dispatch without a handler for it
// does.
type refusingRejects struct{ *recorder }

func (r refusingRejects) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	if string(m.MsgType()) == "j" {
		return fix2.ErrUnsupportedMessageType
	}
	return r.recorder.FromApp(ctx, s, m, in)
}

// A Reject or BusinessMessageReject, invalid or refused by the handler, is
// not rejected in turn: the counterparty would reject our reject, and the
// two would loop. A TestRequest sent after it is answered with nothing
// before its Heartbeat.
func TestRejectsAreNotRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*session.Config)
		send   []string // MsgType, then fields
	}{
		{"invalid Reject", func(c *session.Config) {
			c.Dictionaries.Session = fix44.Dictionary()
		}, []string{"3", "58", "no RefSeqNum"}},
		{"invalid BusinessMessageReject", func(c *session.Config) {
			c.Dictionaries.Session = fix44.Dictionary()
		}, []string{"j", "45", "1", "58", "no RefMsgType nor BusinessRejectReason"}},
		{"BusinessMessageReject refused by the handler", func(c *session.Config) {
			c.Handler = refusingRejects{c.Handler.(*recorder)}
		}, []string{"j", "45", "1", "372", "D", "380", "3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec, p := loggedOn(t, tc.change)
			p.send(tc.send[0], 0, tc.send[1:]...)
			// An order delivered after it: the handler is done with it.
			p.send("D", 0, "11", "O1", "55", "X", "54", "1",
				"60", string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds)), "38", "1", "40", "1")
			rec.wait(t, func() bool { return len(rec.apps) == 1 })
			p.send("1", 0, "112", "SYNC")
			p.expect("0", "112", "SYNC")
			if n := s.Stats().RejectsSent.Load(); n != 0 {
				t.Errorf("%d rejects sent", n)
			}
		})
	}
}

// Two FIXT sessions whose FIX 5.0 SP2 dictionary has no
// BusinessMessageReject, though FIXT 1.1's MsgType lists it: one receiving a
// BusinessMessageReject does not reject it as an unsupported message type,
// which the other would reject in turn, and so on forever.
func TestUnsupportedBusinessMessageRejectNotRejected(t *testing.T) {
	dicts := session.Dictionaries{
		Session:          fixt11.Dictionary(),
		Apps:             map[string]*dictionary.Dict{fix50sp2.Version.ApplVerID: fix50sp2.Dictionary()},
		DefaultApplVerID: fix50sp2.Version.ApplVerID,
	}
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	acc, err := a.Add(session.Config{
		ID:           session.SessionID{BeginString: session.FIXT11, SenderCompID: "ACC", TargetCompID: "INI"},
		Dictionaries: dicts,
		Handler:      newRecorder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	i := session.NewInitiator(session.InitiatorOptions{})
	ir := newRecorder()
	ini, err := i.Add(session.Config{
		ID:           session.SessionID{BeginString: session.FIXT11, SenderCompID: "INI", TargetCompID: "ACC"},
		Dictionaries: dicts,
		Handler:      ir,
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { a.Run(ctx) })
	wg.Go(func() { i.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	ir.wait(t, func() bool { return ir.logons == 1 })

	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	r, err := acc.Send(wctx, fix2.NewBuilder(session.FIXT11, "j").
		SetString(45, "1").SetString(372, "D").SetString(380, "3").SetString(58, "Unsupported Message Type"))
	if err != nil {
		t.Fatal(err)
	}
	// Acknowledged, the initiator read it: it would have rejected it then.
	if err := r.Wait(wctx, session.Acknowledged); err != nil {
		t.Fatalf("Wait(Acknowledged): %v", err)
	}
	if n := ini.Stats().RejectsSent.Load(); n != 0 {
		t.Errorf("the initiator sent %d rejects", n)
	}
	if n := acc.Stats().RejectsSent.Load(); n != 0 {
		t.Errorf("the acceptor sent %d rejects", n)
	}
}
