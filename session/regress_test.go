package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
	"github.com/sylr/fix2/session/transport/mem"
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
