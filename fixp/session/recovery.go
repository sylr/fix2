package session

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/sofh"
)

// The recovery of the peer's messages on a sequenced inbound flow
// (docs/fixp.md, Recovery), the reader's: on a Recoverable flow, messages
// after a gap are held, the gap asked for (RetransmitRequest), one request
// at a time, and filled by Retransmission batches; on an Idempotent flow,
// a gap is reported (NotApplied) and passed.

// heldMsg is a message received after a gap, kept until the gap is filled.
type heldMsg struct {
	msg []byte // a copy
	in  Inbound
}

// request is our outstanding RetransmitRequest.
type request struct {
	ts          uint64 // its Timestamp, which the answer carries
	from, count uint64
}

// recovery is the state of a connection's inbound recovery.
type recovery struct {
	held      []heldMsg // ascending sequence numbers, above the frontier + 1
	heldBytes int
	// last is the highest sequence number the peer is known to have
	// sent: what is missing up to it is asked for.
	last uint64
	req  *request
	// batch is the Retransmission being read: its next sequence number,
	// and the messages left.
	batch     bool
	batchNext uint64
	batchLeft uint64
	// limit bounds the count of our requests, lowered when the peer
	// answers RequestLimitExceeded.
	limit uint64
	// lastTS is the Timestamp of our last request: each is unique.
	lastTS uint64
	// pending is a gap the establishment found, handled once established.
	pendingFrom, pendingTo uint64
}

// terminateError ends the connection with a Terminate of its code.
type terminateError struct {
	code   fixp.TerminationCode
	reason string
}

func (e *terminateError) Error() string { return e.reason }

// highestKnown returns the highest sequence number received or known to
// have been sent, held messages included.
func (c *conn) highestKnown() uint64 {
	h := max(c.s.inbound, c.rec.last)
	if n := len(c.rec.held); n > 0 {
		h = max(h, c.rec.held[n-1].in.Seq)
	}
	return h
}

// gap handles the peer's messages from to next-1, never received, which a
// Sequence or the establishment's NextSeqNo revealed.
func (c *conn) gap(from, next uint64) error {
	_, in := c.s.Flows()
	switch in {
	case fixp.Recoverable:
		c.rec.last = max(c.rec.last, next-1)
		return c.recover()
	case fixp.Idempotent:
		return c.notApplied(from, next-1)
	}
	return nil
}

// recover asks for what is missing, unless a request is outstanding.
func (c *conn) recover() error {
	s := c.s
	r := &c.rec
	if r.req != nil || r.batch {
		return nil
	}
	from := s.inbound + 1
	if from > r.last {
		return nil
	}
	to := r.last
	if len(r.held) > 0 && r.held[0].in.Seq-1 < to {
		to = r.held[0].in.Seq - 1
	}
	if r.limit == 0 {
		r.limit = uint64(s.cfg.RetransmitBatch)
	}
	count := min(to-from+1, r.limit, math.MaxUint32)
	ts := max(nanos(time.Now()), r.lastTS+1)
	r.lastTS = ts
	r.req = &request{ts: ts, from: from, count: count}
	s.q.pushSession(c, mustFrame(&codec.RetransmitRequest{
		SessionId: s.ID(), Timestamp: ts, FromSeqNo: from, Count: uint32(count),
	}), entryEndsNumbering)
	return nil
}

// onRecoverable handles an application message on a Recoverable inbound
// flow, retransmitted or not.
func (c *conn) onRecoverable(h codec.MessageHeader, msg, body []byte, inb Inbound) error {
	s := c.s
	r := &c.rec
	switch seq := inb.Seq; {
	case seq <= s.inbound:
		// A duplicate: delivered already.
	case seq == s.inbound+1:
		if err := c.deliverRecoverable(h, msg, body, inb); err != nil {
			return err
		}
		if err := c.drainHeld(); err != nil {
			return err
		}
	default:
		c.hold(msg, inb)
	}
	if r.batch {
		r.batchLeft--
		if r.batchLeft == 0 {
			// The batch is over; real-time messages follow a Sequence.
			r.batch, r.req = false, nil
			c.inNext = 0
		}
	}
	return c.progress()
}

// progress asks for what is still missing, and answers the peer's
// FinishedSending once nothing is.
func (c *conn) progress() error {
	if err := c.recover(); err != nil {
		return err
	}
	return c.completePeerFlow()
}

// deliverRecoverable delivers the next message, and records it delivered
// once the handler returned.
func (c *conn) deliverRecoverable(h codec.MessageHeader, msg, body []byte, inb Inbound) error {
	if err := c.deliver(h, msg, body, inb); err != nil {
		return err
	}
	if err := c.s.setInbound(inb.Seq, false); err != nil {
		return errHalted
	}
	return nil
}

// hold keeps a message received after a gap, or drops it, to be asked for
// again, beyond the bounds.
func (c *conn) hold(msg []byte, inb Inbound) {
	s := c.s
	r := &c.rec
	r.last = max(r.last, inb.Seq)
	i := len(r.held)
	for i > 0 && r.held[i-1].in.Seq > inb.Seq {
		i--
	}
	if i > 0 && r.held[i-1].in.Seq == inb.Seq {
		return // held already
	}
	if len(r.held) >= s.cfg.MaxHeldMessages || r.heldBytes+len(msg) > s.cfg.MaxHeldBytes {
		return // asked for again: r.last covers it
	}
	r.held = append(r.held, heldMsg{})
	copy(r.held[i+1:], r.held[i:])
	r.held[i] = heldMsg{msg: append([]byte(nil), msg...), in: inb}
	r.heldBytes += len(msg)
}

// drainHeld delivers the held messages that follow the frontier.
func (c *conn) drainHeld() error {
	s := c.s
	r := &c.rec
	for len(r.held) > 0 && r.held[0].in.Seq <= s.inbound+1 {
		m := r.held[0]
		r.held[0] = heldMsg{}
		r.held = r.held[1:]
		r.heldBytes -= len(m.msg)
		if m.in.Seq <= s.inbound {
			continue
		}
		h, body, _ := codec.ReadHeader(m.msg)
		if err := c.deliverRecoverable(h, m.msg, body, m.in); err != nil {
			return err
		}
	}
	if len(r.held) == 0 {
		r.held = nil
	}
	return nil
}

// onRetransmission handles a Retransmission: the next count application
// messages are the peer's from NextSeqNo on, answering our request.
func (c *conn) onRetransmission(h codec.MessageHeader, body []byte) error {
	r := &c.rec
	if _, in := c.s.Flows(); in != fixp.Recoverable {
		return protocolf("Retransmission on an %s flow", in)
	}
	var rt codec.Retransmission
	if _, err := rt.DecodeBody(h, body); err != nil {
		return protocolf("Retransmission: %v", err)
	}
	if r.req == nil || r.batch || rt.RequestTimestamp != r.req.ts {
		return protocolf("Retransmission answering no request of ours")
	}
	if rt.SessionId != c.s.ID() || rt.NextSeqNo != r.req.from || uint64(rt.Count) > r.req.count {
		return protocolf("Retransmission of %d from %d, %d from %d asked", rt.Count, rt.NextSeqNo, r.req.count, r.req.from)
	}
	c.inNext = 0
	if rt.Count == 0 {
		r.req = nil
		return c.recover()
	}
	r.batch, r.batchNext, r.batchLeft = true, rt.NextSeqNo, uint64(rt.Count)
	return nil
}

// onRetransmitReject handles the peer's refusal of our request.
func (c *conn) onRetransmitReject(h codec.MessageHeader, body []byte) error {
	s := c.s
	r := &c.rec
	if _, in := s.Flows(); in != fixp.Recoverable {
		return protocolf("RetransmitReject on an %s flow", in)
	}
	var rj codec.RestransmitReject
	if _, err := rj.DecodeBody(h, body); err != nil {
		return protocolf("RetransmitReject: %v", err)
	}
	if r.req == nil || r.batch || rj.RequestTimestamp != r.req.ts || rj.SessionId != s.ID() {
		return protocolf("RetransmitReject answering no request of ours")
	}
	req := r.req
	reason := printable(rj.Reason, s.cfg.MaxReasonLength)
	switch fixp.RetransmitRejectCode(rj.Code) {
	case fixp.RetransmitOutOfRange:
		// Lost: skipped only with the handler's consent.
		if err := s.handler.OnLost(s.cbCtx, s, req.from, req.count); err != nil {
			return &handlerError{fmt.Errorf("messages %d to %d lost (%s): %w", req.from, req.from+req.count-1, reason, err)}
		}
		s.logWarn("messages lost, skipped", "from", req.from, "count", req.count, "reason", reason)
		r.req = nil
		// Durable: a restart must not ask the handler again.
		if err := s.setInbound(req.from+req.count-1, true); err != nil {
			return errHalted
		}
		if err := c.drainHeld(); err != nil {
			return err
		}
	case fixp.RetransmitRequestLimitExceeded:
		if req.count == 1 {
			return protocolf("RetransmitReject RequestLimitExceeded for one message")
		}
		r.req = nil
		r.limit = req.count / 2
	default:
		// InvalidSession, our request carrying our own ID, or unknown.
		return protocolf("RetransmitReject %s: %s", fixp.RetransmitRejectCode(rj.Code), reason)
	}
	return c.progress()
}

// notApplied reports the peer's messages from to to, never received on an
// Idempotent flow (§4.8.2): NotApplied on our flow if sequenced, the
// handler told, and the admitted frontier past them, durably.
func (c *conn) notApplied(from, to uint64) error {
	s := c.s
	// One NotApplied reports a gap: one it cannot is no peer's mistake.
	if to-from+1 > math.MaxUint32 {
		return protocolf("a gap of %d messages, from %d", to-from+1, from)
	}
	// Our flow finishing, no application message follows its
	// FinishedSending: the gap goes unreported to the peer, as on an
	// unsequenced return flow.
	if out, _ := s.Flows(); out.Sequenced() && !s.outFinishing() {
		_, err := s.sendInternal(mustFrame(&codec.NotApplied{FromSeqNo: from, Count: uint32(to - from + 1)}))
		switch {
		case errors.Is(err, errInternalFull):
			// Not reported, so not passed: the next establishment
			// finds the gap again.
			return protocolf("gaps faster than NotApplied can be written")
		case errors.Is(err, fixp.ErrNoFlow):
			// Finish won the assignment meanwhile: unreported, as above.
		case err != nil:
			return errHalted
		}
	}
	s.handler.OnGap(s.cbCtx, s, from, to-from+1)
	if err := s.setInbound(to, true); err != nil {
		return errHalted
	}
	return nil
}

// onRetransmitRequest answers the peer's request on our Recoverable flow
// (§4.6): a batch of at most RetransmitBatch messages, in the writer's
// order, or a RetransmitReject.
func (c *conn) onRetransmitRequest(h codec.MessageHeader, body []byte) error {
	s := c.s
	if out, _ := s.Flows(); out != fixp.Recoverable {
		return protocolf("RetransmitRequest on an %s flow", out)
	}
	var rr codec.RetransmitRequest
	if _, err := rr.DecodeBody(h, body); err != nil {
		return protocolf("RetransmitRequest: %v", err)
	}
	// The answers wait for the writer: a peer asking faster than it reads
	// them would fill memory.
	if c.answers.Load() >= maxAnswers {
		return protocolf("requests faster than their answers are read")
	}
	reject := func(code fixp.RetransmitRejectCode, reason string) error {
		c.answers.Add(1)
		s.q.pushAnswer(c, mustFrame(&codec.RestransmitReject{
			SessionId: rr.SessionId, RequestTimestamp: rr.Timestamp,
			Code: codec.RetransmitRejectCode(code), Reason: []byte(reason),
		}))
		return nil
	}
	if rr.SessionId != s.ID() {
		return reject(fixp.RetransmitInvalidSession, "")
	}
	id := c.jobs.Add(1)
	if !c.retransmitting.CompareAndSwap(0, id) {
		return &terminateError{fixp.TerminationReRequestInProgress, "RetransmitRequest while one is in progress"}
	}
	written := s.written.Load()
	// A request reaching beyond what was sent, in part even, is refused
	// whole (§7.4.5.1, §7.4.5.2).
	if rr.Count == 0 || rr.FromSeqNo == 0 || rr.FromSeqNo > written || uint64(rr.Count)-1 > written-rr.FromSeqNo {
		c.retransmitting.CompareAndSwap(id, 0)
		return reject(fixp.RetransmitOutOfRange, "beyond what was sent")
	}
	count := min(uint64(rr.Count), written-rr.FromSeqNo+1, uint64(s.cfg.RetransmitBatch))
	c.answers.Add(1)
	s.q.pushRetransmit(c, retransmitJob{id: id, ts: rr.Timestamp, from: rr.FromSeqNo, count: uint32(count)})
	return nil
}

// maxAnswers bounds the answers to the peer's requests queued and not
// written.
const maxAnswers = 64

// retransmitJob is a batch to retransmit.
type retransmitJob struct {
	id    uint64 // its conn.retransmitting
	ts    uint64
	from  uint64
	count uint32
}

// errInterrupted stops a retransmission for a Terminate.
var errInterrupted = errors.New("fixp/session: retransmission interrupted")

// errWrite wraps a write error out of a store's Range.
type errWrite struct{ err error }

func (e errWrite) Error() string { return e.err.Error() }

// retransmit writes a batch, the writer's: Retransmission then the
// messages from the store, or RetransmitReject OutOfRange if the store no
// longer holds them. Real-time messages after it follow a Sequence.
func (c *conn) retransmit(j retransmitJob) error {
	s := c.s
	defer c.retransmitting.CompareAndSwap(j.id, 0)
	c.outNext = 0
	s.mu.Lock()
	st, id := s.st, s.id
	s.mu.Unlock()
	ctx := s.storeCtx()
	defer ctx.cancel()
	headed := false
	var buf []byte
	last := j.from + uint64(j.count) - 1
	err := st.Range(ctx, j.from, last, func(seq uint64, msg []byte) error {
		// A Terminate, ours on an error or the answer to the peer's,
		// goes before the rest of the batch.
		if c.isClosing() || c.hasUrgent() {
			return errInterrupted
		}
		if !headed {
			if err := c.write(mustFrame(&codec.Retransmission{
				SessionId: id, RequestTimestamp: j.ts, NextSeqNo: j.from, Count: j.count,
			})); err != nil {
				return errWrite{err}
			}
			headed = true
		}
		var err error
		if buf, err = sofh.AppendFrame(buf[:0], sofh.SBE10LittleEndian, msg); err != nil {
			return err
		}
		if seq == last {
			// The peer may ask for more as soon as it reads the last
			// one: before this Write returns, but not before the rest.
			c.retransmitting.CompareAndSwap(j.id, 0)
		}
		if err := c.writeSeq(buf, seq, true); err != nil {
			return errWrite{err}
		}
		s.stats.MessagesRetransmitted.Add(1)
		return nil
	})
	var we errWrite
	switch {
	case err == nil, errors.Is(err, errInterrupted):
		return nil
	case errors.As(err, &we):
		return we.err
	case errors.Is(err, store.ErrEvicted) && !headed:
		c.retransmitting.CompareAndSwap(j.id, 0) // answered once written
		return c.write(mustFrame(&codec.RestransmitReject{
			SessionId: id, RequestTimestamp: j.ts, Code: codec.RetransmitRejectCodeOutOfRange, Reason: []byte("no longer available"),
		}))
	}
	err = fmt.Errorf("fixp/session: reading messages %d to %d to retransmit: %w", j.from, j.from+uint64(j.count)-1, err)
	s.halt(err)
	return err
}
