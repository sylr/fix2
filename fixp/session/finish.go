package session

import (
	"context"
	"fmt"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
)

// Finalization (§4.7, docs/fixp.md): each side finishes its flow with
// FinishedSending, which the other acknowledges with FinishedReceiving once
// it holds every message of the flow. Once both flows are finished either
// side terminates, and the Terminate exchange completed, the session is
// finalized: until then it may be established again, for the peer to hear
// what it missed.

// updateFinish changes the flows' finalization with f, which reports
// whether it changed anything, and records it durably: every change goes
// through it, serialized.
func (s *Session) updateFinish(f func(out, in *store.Finish) bool) error {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	s.mu.Lock()
	out, in, st := s.outFin, s.inFin, s.st
	s.mu.Unlock()
	if !f(&out, &in) {
		return nil
	}
	ctx := s.storeCtx()
	defer ctx.cancel()
	err := st.SetFinish(ctx, out, in)
	if err == nil {
		err = st.Sync(ctx)
	}
	if err != nil {
		err = fmt.Errorf("fixp/session: recording the finalization: %w", err)
		s.halt(err)
		return err
	}
	s.mu.Lock()
	s.outFin, s.inFin = out, in
	s.mu.Unlock()
	return nil
}

// outFinishing reports whether our flow is finishing or finished.
func (s *Session) outFinishing() bool {
	out, _ := s.finishes()
	return out.Finishing
}

// finishes returns the flows' finalization.
func (s *Session) finishes() (out, in store.Finish) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outFin, s.inFin
}

// Finish finishes our flow: FinishedSending, stored, then repeated until
// the peer's FinishedReceiving (§4.7). Send fails after it. Finish returns
// once FinishedSending is stored and queued, after every message Send
// accepted, without waiting for the answer: Handler.OnFinalized tells when
// the session is over. From a callback, it does not wait for a Send storing
// its message: it fails with ErrQueueFull then. Finishing a flow finishing
// already does nothing.
func (s *Session) Finish(ctx context.Context) error {
	if inCallback(ctx) {
		select {
		case s.assign <- struct{}{}:
		default:
			return ErrQueueFull
		}
	} else {
		select {
		case s.assign <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return ErrStopped
		}
	}
	defer func() { <-s.assign }()
	switch s.State() {
	case StateStopped, StateFinalized:
		return ErrStopped
	case StateUnnegotiated, StateNegotiating:
		return ErrUnnegotiated
	}
	return s.finishOutbound()
}

// finishOutbound finishes our flow, under s.assign: what Send accepted is
// assigned and queued before its FinishedSending.
func (s *Session) finishOutbound() error {
	var last uint64
	if out, _ := s.Flows(); out.Sequenced() {
		last = s.q.nextSeq() - 1
	}
	changed := false
	err := s.updateFinish(func(out, in *store.Finish) bool {
		if out.Finishing {
			return false
		}
		*out = store.Finish{Finishing: true, LastSeqNo: last}
		changed = true
		return true
	})
	if err != nil {
		return err
	}
	if changed {
		s.q.wakeRoom() // a Send waiting for room fails now
		s.logInfo("finishing", "last_seq_no", last)
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c != nil {
			s.q.pushFinish(c, s.finishedSendingFrame(), false)
		}
	}
	return nil
}

// finishedSendingFrame is our FinishedSending: with LastSeqNo on a
// sequenced flow, without otherwise (§4.7.1).
func (s *Session) finishedSendingFrame() []byte {
	out, _ := s.finishes()
	fs := codec.FinishedSending{SessionId: s.ID()}
	if flow, _ := s.Flows(); flow.Sequenced() {
		fs.LastSeqNo = codec.Some(out.LastSeqNo)
	}
	return mustFrame(&fs)
}

// startFinish resumes finalization on a new connection: our FinishedSending
// again if unanswered, the Terminate if both flows finished.
func (c *conn) startFinish() error {
	s := c.s
	out, _ := s.finishes()
	if out.Finishing && !out.Finished {
		s.q.pushFinish(c, s.finishedSendingFrame(), false)
	}
	return c.maybeTerminate()
}

// onFinishedSending handles the peer's FinishedSending: its flow ends at
// LastSeqNo; once every message up to it is delivered or skipped, the
// handler is told, our flow finishes too, and FinishedReceiving answers.
func (c *conn) onFinishedSending(h codec.MessageHeader, body []byte) error {
	s := c.s
	var fs codec.FinishedSending
	if _, err := fs.DecodeBody(h, body); err != nil {
		return protocolf("FinishedSending: %v", err)
	}
	if fs.SessionId != s.ID() {
		return protocolf("FinishedSending of another session")
	}
	c.inNext = 0 // a session message ending the peer's numbering
	_, flow := s.Flows()
	var last uint64
	if flow.Sequenced() {
		if !fs.LastSeqNo.Valid {
			return protocolf("FinishedSending of an %s flow without LastSeqNo", flow)
		}
		last = fs.LastSeqNo.Value
		if last < c.highestKnown() {
			return protocolf("FinishedSending's LastSeqNo %d below the messages received", last)
		}
	}
	_, in := s.finishes()
	switch {
	case in.Finishing && in.LastSeqNo != last:
		return protocolf("FinishedSending's LastSeqNo %d, %d before", last, in.LastSeqNo)
	case in.Finished:
		// Our answer did not reach it: again.
		c.queueFinishedReceiving()
		return nil
	case !in.Finishing:
		if err := s.updateFinish(func(_, in *store.Finish) bool {
			*in = store.Finish{Finishing: true, LastSeqNo: last}
			return true
		}); err != nil {
			return errHalted
		}
	}
	c.peerFinishing = true
	return c.completePeerFlow()
}

// completePeerFlow answers the peer's FinishedSending once its flow is
// complete: on a Recoverable flow every message up to LastSeqNo delivered
// or skipped, on an Idempotent one the gap up to it reported (§4.7.1,
// §7.5.3).
func (c *conn) completePeerFlow() error {
	s := c.s
	if !c.peerFinishing || c.peerAnswered {
		return nil
	}
	_, flow := s.Flows()
	_, in := s.finishes()
	switch flow {
	case fixp.Recoverable:
		if s.inbound < in.LastSeqNo {
			c.rec.last = max(c.rec.last, in.LastSeqNo)
			return c.recover() // answered once complete
		}
	case fixp.Idempotent:
		if s.inbound < in.LastSeqNo {
			if err := c.notApplied(s.inbound+1, in.LastSeqNo); err != nil {
				return err
			}
		}
	}
	c.peerAnswered = true
	if err := s.handler.OnPeerFinished(s.cbCtx, s); err != nil {
		return &handlerError{fmt.Errorf("finishing: %w", err)}
	}
	// Our flow finishes before the answer, behind every message Send
	// accepted: we send none after FinishedReceiving (§6.3).
	s.assign <- struct{}{}
	err := s.finishOutbound()
	if err == nil {
		err = s.updateFinish(func(_, in *store.Finish) bool {
			in.Finished = true
			return true
		})
	}
	if err == nil {
		c.queueFinishedReceiving()
	}
	<-s.assign
	if err != nil {
		return errHalted
	}
	s.logInfo("peer finished", "last_seq_no", in.LastSeqNo)
	return c.maybeTerminate()
}

// queueFinishedReceiving queues our FinishedReceiving, unless one is
// queued already: a peer repeating FinishedSending without reading gets
// one answer, not as many.
func (c *conn) queueFinishedReceiving() {
	if c.finRecvQueued.CompareAndSwap(false, true) {
		c.s.q.pushFinish(c, mustFrame(&codec.FinishedReceiving{SessionId: c.s.ID()}), true)
	}
}

// onFinishedReceiving handles the peer's acknowledgement of our flow.
func (c *conn) onFinishedReceiving(h codec.MessageHeader, body []byte) error {
	s := c.s
	var fr codec.FinishedReceiving
	if _, err := fr.DecodeBody(h, body); err != nil {
		return protocolf("FinishedReceiving: %v", err)
	}
	if fr.SessionId != s.ID() {
		return protocolf("FinishedReceiving of another session")
	}
	c.inNext = 0
	out, _ := s.finishes()
	if !out.Finishing {
		return protocolf("FinishedReceiving of a flow not finishing")
	}
	if !out.Finished {
		if err := s.updateFinish(func(out, _ *store.Finish) bool {
			out.Finished = true
			return true
		}); err != nil {
			return errHalted
		}
	}
	return c.maybeTerminate()
}

// maybeTerminate terminates once both flows are finished: either side may
// (§7.5.1); both doing so at once, each takes the other's as its answer.
func (c *conn) maybeTerminate() error {
	s := c.s
	out, in := s.finishes()
	if !out.Finished || !in.Finished || c.finalTerminate {
		return nil
	}
	c.finalTerminate = true
	s.Terminate(context.Background(), "finished")
	return nil
}

// finishedExchange reports whether the connection ended with a Terminate
// exchange, both flows finished: the peer heard our FinishedReceiving,
// the session is over.
func (c *conn) finishedExchange() bool {
	out, in := c.s.finishes()
	if !out.Finished || !in.Finished {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Ours written whole and answered, or the peer's, sent once it heard
	// our FinishedReceiving.
	return c.answered && c.sent || c.peers != nil && c.peers.Code == fixp.TerminationFinished
}

// finalize finalizes the session, durably, once its connection is gone.
func (s *Session) finalize() {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	ctx := s.storeCtx()
	err := st.Finalize(ctx, time.Now())
	if err == nil {
		err = st.Sync(ctx)
	}
	ctx.cancel()
	if err != nil {
		s.halt(fmt.Errorf("fixp/session: finalizing: %w", err))
		return
	}
	s.mu.Lock()
	if s.state == StateStopped {
		s.mu.Unlock()
		return
	}
	s.state = StateFinalized
	s.mu.Unlock()
	s.logInfo("finalized")
	s.doneOnce.Do(func() { close(s.done) })
	s.handler.OnFinalized(s.cbCtx, s)
	if s.onFinalized != nil {
		s.onFinalized(s)
	}
}
