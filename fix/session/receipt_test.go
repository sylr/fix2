package session_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store/memstore"
	"github.com/sylr/fix2/transport/mem"
)

// loggedOn returns an initiator session logged on to a scripted peer.
func loggedOn(t *testing.T, change func(*session.Config)) (*session.Session, *recorder, *peer) {
	t.Helper()
	l := mem.Listen("acceptor")
	s, rec := startInitiator(t, l, change)
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })
	return s, rec, p
}

func order(id string) *fix.Builder {
	return fix.NewBuilder(session.FIX44, "D").SetString(11, id)
}

func TestReceipt_Levels(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.Send(ctx, order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq() != 2 {
		t.Errorf("Seq = %d, want 2", r.Seq())
	}
	if err := r.Wait(ctx, session.Written); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "11", "O1")
	done := make(chan error, 1)
	go func() { done <- r.Wait(ctx, session.Acknowledged) }()
	// Waiting for an acknowledgement probes the peer.
	tr := p.expect("1")
	id := string(tr.Get(112))
	if !strings.HasPrefix(id, "ACK-") {
		t.Fatalf("probe TestReqID %q", id)
	}
	select {
	case err := <-done:
		t.Fatalf("acknowledged before the probe was answered: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	p.send("0", 0, "112", id)
	if err := <-done; err != nil {
		t.Fatalf("Wait(Acknowledged) = %v", err)
	}
	if r.Err() != nil {
		t.Errorf("Err = %v", r.Err())
	}
}

func TestReceipt_Rejection(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	r, err := s.Send(context.Background(), order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	p.send("3", 0, "45", "2", "58", "bad order", "373", "5", "371", "11")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ri, ok := r.Rejection(); ok {
			if ri.MsgType != "3" || ri.Reason != 5 || ri.RefTagID != 11 || ri.Text != "bad order" {
				t.Errorf("Rejection = %+v", ri)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no rejection recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A message the handler does not want resent is gap-filled: its receipt
// can no longer be acknowledged.
type noResend struct{ *recorder }

func (noResend) OnResend(context.Context, *session.Session, uint64, *fix.Message) bool {
	return false
}

func TestReceipt_Skipped(t *testing.T) {
	var rec *recorder
	s, _, p := loggedOn(t, func(c *session.Config) {
		rec = c.Handler.(*recorder)
		c.Handler = noResend{rec}
	})
	r, err := s.Send(context.Background(), order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	p.send("2", 0, "7", "2", "16", "0")
	p.expect("4", "34", "2", "36", "3")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(ctx, session.Acknowledged); !errors.Is(err, session.ErrSkipped) {
		t.Errorf("Wait = %v, want ErrSkipped", err)
	}
}

func TestReceipt_UnknownAfterReset(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	r, err := s.Send(context.Background(), order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	// The peer resets the sequence numbers on the connection.
	p.seq = 1
	p.send("A", 0, "98", "0", "108", "30", "141", "Y")
	p.expect("A", "34", "1", "141", "Y")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(ctx, session.Acknowledged); !errors.Is(err, session.ErrUnknownDelivery) {
		t.Errorf("Wait = %v, want ErrUnknownDelivery", err)
	}
	if got := s.NextSenderSeqNum(); got != 2 {
		t.Errorf("NextSenderSeqNum after the reset = %d, want 2", got)
	}
}

// resetSeqs has the peer reset the sequence numbers on the connection, and
// waits for the session's answer.
func (p *peer) resetSeqs() {
	p.seq = 1
	p.send("A", 0, "98", "0", "108", "30", "141", "Y")
	p.expect("A", "34", "1", "141", "Y")
}

// A sequence reset keeps what a receipt reached: a message written stays
// written, acknowledged or not; one not acknowledged by then never will be.
func TestReceipt_SettledAcrossReset(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		name := "written"
		if acknowledged {
			name = "acknowledged"
		}
		t.Run(name, func(t *testing.T) {
			s, _, p := loggedOn(t, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			r, err := s.Send(ctx, order("O1"))
			if err != nil {
				t.Fatal(err)
			}
			p.expect("D", "11", "O1")
			if err := r.Wait(ctx, session.Written); err != nil {
				t.Fatal(err)
			}
			if acknowledged {
				done := make(chan error, 1)
				go func() { done <- r.Wait(ctx, session.Acknowledged) }()
				probe := p.expect("1")
				p.send("0", 0, "112", string(probe.Get(112)))
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			p.resetSeqs()
			if err := r.Wait(ctx, session.Written); err != nil {
				t.Errorf("Wait(Written) after the reset = %v, want nil", err)
			}
			var want error
			if !acknowledged {
				want = session.ErrUnknownDelivery
			}
			if err := r.Wait(ctx, session.Acknowledged); !errors.Is(err, want) {
				t.Errorf("Wait(Acknowledged) after the reset = %v, want %v", err, want)
			}
			if err := r.Err(); !errors.Is(err, want) {
				t.Errorf("Err after the reset = %v, want %v", err, want)
			}
		})
	}
}

// A message gap-filled stays skipped after a reset, and written if it was.
func TestReceipt_SkippedAcrossReset(t *testing.T) {
	var rec *recorder
	s, _, p := loggedOn(t, func(c *session.Config) {
		rec = c.Handler.(*recorder)
		c.Handler = noResend{rec}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := s.Send(ctx, order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	p.send("2", 0, "7", "2", "16", "0")
	p.expect("4", "34", "2", "36", "3")
	if err := r.Wait(ctx, session.Acknowledged); !errors.Is(err, session.ErrSkipped) {
		t.Fatalf("Wait = %v, want ErrSkipped", err)
	}
	p.resetSeqs()
	if err := r.Wait(ctx, session.Acknowledged); !errors.Is(err, session.ErrSkipped) {
		t.Errorf("Wait(Acknowledged) after the reset = %v, want ErrSkipped", err)
	}
	if err := r.Wait(ctx, session.Written); err != nil {
		t.Errorf("Wait(Written) after the reset = %v, want nil", err)
	}
}

// A rejection stays after a reset.
func TestReceipt_RejectionAcrossReset(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	r, err := s.Send(t.Context(), order("O1"))
	if err != nil {
		t.Fatal(err)
	}
	p.expect("D")
	p.send("3", 0, "45", "2", "58", "bad order", "373", "5", "371", "11")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := r.Rejection(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no rejection recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.resetSeqs()
	ri, ok := r.Rejection()
	if !ok || ri.MsgType != "3" || ri.Reason != 5 || ri.RefTagID != 11 || ri.Text != "bad order" {
		t.Errorf("Rejection after the reset = %+v, %v", ri, ok)
	}
}

// Generations are kept within MaxTrackedReceipts, the oldest forgotten
// first: their receipts report ErrReceiptUntracked, as receipts too old do
// within a generation.
func TestReceipt_ResetsBounded(t *testing.T) {
	s, _, p := loggedOn(t, func(c *session.Config) { c.MaxTrackedReceipts = 2 })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var rs []session.Receipt
	for i := range 3 {
		r, err := s.Send(ctx, order(fmt.Sprint("O", i)))
		if err != nil {
			t.Fatal(err)
		}
		p.expect("D")
		if err := r.Wait(ctx, session.Written); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
		p.resetSeqs()
	}
	if err := rs[0].Wait(ctx, session.Written); !errors.Is(err, session.ErrReceiptUntracked) {
		t.Errorf("Wait(Written) of the first generation = %v, want ErrReceiptUntracked", err)
	}
	if err := rs[2].Wait(ctx, session.Written); err != nil {
		t.Errorf("Wait(Written) of the last generation = %v, want nil", err)
	}
	if err := rs[2].Wait(ctx, session.Acknowledged); !errors.Is(err, session.ErrUnknownDelivery) {
		t.Errorf("Wait(Acknowledged) of the last generation = %v, want ErrUnknownDelivery", err)
	}
}

// stuckStore resets nothing: its generation stays.
type stuckStore struct{ *memstore.Store }

func (stuckStore) Reset(context.Context, uint64, time.Time, string) error { return nil }

// A store whose generation does not move on with a reset halts the session:
// the receipts of the generation ended would read those of the next.
func TestReceipt_ResetKeepingGeneration(t *testing.T) {
	s, _, p := loggedOn(t, func(c *session.Config) { c.Store = stuckStore{memstore.New()} })
	p.seq = 1
	p.send("A", 0, "98", "0", "108", "30", "141", "Y")
	deadline := time.Now().Add(5 * time.Second)
	for s.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the session did not halt")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Err(); !strings.Contains(err.Error(), "not a later one") {
		t.Errorf("Err = %v, want the generation not moving on", err)
	}
	if st := s.State(); st != session.StateStopped {
		t.Errorf("State = %s, want stopped", st)
	}
}

func TestReceipt_WaitInHandler(t *testing.T) {
	var got error
	h := &waitingHandler{recorder: newRecorder(), result: make(chan error, 1)}
	l := mem.Listen("acceptor")
	s, _ := startInitiator(t, l, func(c *session.Config) { c.Handler = h })
	h.s = s
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	p.send("D", 0, "11", "IN")
	select {
	case got = <-h.result:
	case <-time.After(5 * time.Second):
		t.Fatal("handler not called")
	}
	if !errors.Is(got, session.ErrWaitInHandler) {
		t.Errorf("Wait from FromApp = %v, want ErrWaitInHandler", got)
	}
}

type waitingHandler struct {
	*recorder
	s      *session.Session
	result chan error
}

func (h *waitingHandler) FromApp(ctx context.Context, s *session.Session, m *fix.Message, in session.Inbound) error {
	r, err := s.Send(ctx, order("REPLY"))
	if err != nil {
		h.result <- err
		return nil
	}
	h.result <- r.Wait(ctx, session.Acknowledged)
	return nil
}

func TestSend_Errors(t *testing.T) {
	l := mem.Listen("acceptor")
	s, _ := startInitiator(t, l, nil)
	if _, err := s.Send(context.Background(), order("O1")); !errors.Is(err, session.ErrNotLoggedOn) {
		t.Errorf("Send before logon = %v, want ErrNotLoggedOn", err)
	}
	p := acceptPeer(t, l)
	p.expect("A")
	p.send("A", 0, "98", "0", "108", "30")
	deadline := time.Now().Add(5 * time.Second)
	for s.State() != session.StateLoggedOn && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var rerr *fix.ReservedTagError
	if _, err := s.Send(context.Background(), order("O1").SetInt(34, 99)); !errors.As(err, &rerr) || rerr.Tag != 34 {
		t.Errorf("Send with MsgSeqNum set = %v, want a ReservedTagError", err)
	}
	var serr *fix.SOHValueError
	if _, err := s.Send(context.Background(), order("O1\x0158=forged")); !errors.As(err, &serr) || serr.Tag != 11 {
		t.Errorf("Send with SOH in ClOrdID = %v, want a SOHValueError", err)
	}
	// Neither sequenced nor sent.
	if _, err := s.Send(context.Background(), order("O2")); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "2", "11", "O2")
	if _, err := s.SendRaw(context.Background(), "0", nil); err == nil {
		t.Error("SendRaw of a Heartbeat succeeded: the session sends those")
	}
}

// A handler's RejectError rejects the message with the reason and Text.
type rejecting struct{ *recorder }

func (rejecting) FromApp(context.Context, *session.Session, *fix.Message, session.Inbound) error {
	return &session.RejectError{Reason: dictionary.ReasonValueIsIncorrect, Tag: 54, Text: "Side 7 is not traded here"}
}

func TestHandlerReject(t *testing.T) {
	_, _, p := loggedOn(t, func(c *session.Config) { c.Handler = rejecting{c.Handler.(*recorder)} })
	p.send("D", 0, "11", "IN", "54", "7")
	rej := p.expect("3", "45", "2", "371", "54", "372", "D", "373", "5")
	if got := string(rej.Get(58)); !strings.HasPrefix(got, "Value is incorrect (out of range) for this tag: Side 7") {
		t.Errorf("Text = %q", got)
	}
}

// An application message type the handler does not support is answered
// with a BusinessMessageReject.
type unsupported struct{ *recorder }

func (unsupported) FromApp(_ context.Context, _ *session.Session, m *fix.Message, _ session.Inbound) error {
	return fmt.Errorf("%w: %s", fix.ErrUnsupportedMessageType, m.MsgType())
}

// A handler's BusinessRejectError whose RefID holds SOH.
type sohRefID struct{ *recorder }

func (sohRefID) FromApp(context.Context, *session.Session, *fix.Message, session.Inbound) error {
	return &session.BusinessRejectError{Reason: session.BusinessRejectUnknownID, RefID: "O1\x0158=forged", Text: "unknown order"}
}

// A RefID holding SOH would end BusinessRejectRefID (379) early, the rest
// read as fields of their own: the BusinessMessageReject goes without it.
func TestBusinessRejectRefIDWithSOH(t *testing.T) {
	_, _, p := loggedOn(t, func(c *session.Config) { c.Handler = sohRefID{c.Handler.(*recorder)} })
	p.send("D", 0, "11", "O1")
	rej := p.expect("j", "45", "2", "372", "D", "380", "1", "58", "unknown order")
	if rej.Get(379) != nil {
		t.Errorf("BusinessRejectRefID sent: %q", rej.Get(379))
	}
	n := 0
	for _, f := range rej.Fields() {
		if f.Tag == 58 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d Text fields, want 1", n)
	}
	// The session goes on.
	p.send("0", 0)
	p.send("1", 0, "112", "T1")
	p.expect("0", "112", "T1")
}

// A ToAdmin setting a value holding SOH cancels the Logon, as a ToAdmin
// error does: nothing is sent.
func TestToAdminSOH(t *testing.T) {
	l := mem.Listen("acceptor")
	startInitiator(t, l, func(c *session.Config) { c.Handler = user{c.Handler.(*recorder), "a\x0158=forged"} })
	p := acceptPeer(t, l)
	select {
	case ev := <-p.frames:
		if ev.err == nil {
			t.Fatalf("got %s, want no Logon", visible(ev.frame))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connection still open")
	}
}

// The identities the session writes in every header cannot hold SOH.
func TestConfigSOH(t *testing.T) {
	_, err := session.New(session.Config{
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC", TargetSubID: "a\x01b"},
		Handler: session.BaseHandler{},
	})
	if err == nil || !strings.Contains(err.Error(), "ID.TargetSubID") {
		t.Errorf("New = %v, want an error about ID.TargetSubID", err)
	}
}

func TestUnsupportedMessageType(t *testing.T) {
	_, _, p := loggedOn(t, func(c *session.Config) { c.Handler = unsupported{c.Handler.(*recorder)} })
	p.send("8", 0, "11", "X")
	rej := p.expect("j", "45", "2", "372", "8", "380", "3")
	if got := string(rej.Get(58)); !strings.HasPrefix(got, "Unsupported Message Type") {
		t.Errorf("Text = %q", got)
	}
}
