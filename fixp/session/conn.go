package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport"
)

// conn is a connection a session is established on: a reader goroutine,
// which delivers what it reads, and a writer goroutine, which writes the
// session's queue and the heartbeats.
type conn struct {
	s *Session
	t transport.Conn
	r *sofh.Reader
	// peerKeepalive is the peer's interval: its silence beyond it, times
	// the leniency, ends the connection.
	peerKeepalive time.Duration
	// number numbers the session's connections, from 1.
	number uint64
	// finSent is set once our FinishedSending is written on this
	// connection, the writer's: only then is it the heartbeat, which would
	// overtake messages queued before it otherwise.
	finSent bool
	// out holds application messages, and their Sequences, the writer
	// writes at once once the queue has no more (flush); outFrames are
	// its frames, for the observer.
	out       []byte
	outFrames []outFrame
	outSince  time.Time // when out's first frame was buffered
	// wrote is set once bytes reach the transport, for the writer's
	// heartbeat timer.
	wrote bool
	// finRecvQueued is set while a FinishedReceiving answer is queued and
	// not written: repetitions of the peer's FinishedSending share it.
	finRecvQueued atomic.Bool

	closing    chan struct{} // closed once the connection is closing
	closeOnce  sync.Once
	writerDone chan struct{}
	unbound    chan struct{} // closed once the session is unbound from it
	wake       chan struct{} // wakes the writer for urgent

	mu sync.Mutex
	// urgent is a frame the writer writes before anything else, then
	// closes the connection: a Terminate on an error, or answering one.
	urgent []byte
	// ours is our Terminate, queued or written; peers the peer's, when it
	// terminated first.
	ours, peers *fixp.TerminatedError
	// written is set as our graceful Terminate is being written: what
	// follows is ignored, but the answer; sent once its Write returned;
	// answered once the answer came, the connection closing when sent
	// (closeWhenSent).
	written, sent, answered, closeWhenSent bool
	// cause is the error that closed the connection, if any.
	cause error

	// The writer's: the sequence number the peer gives our next
	// application message, 0 if it does not know it (no Sequence yet).
	outNext uint64
	// The reader's: the sequence number of the peer's next application
	// message, 0 if not known yet, and the recovery of what it missed.
	inNext uint64
	rec    recovery

	// retransmitting is the job answering the peer's RetransmitRequest, 0
	// if none: it asks one at a time (§4.6.4). Each job clears only its
	// own: an earlier one ending late leaves the next in place.
	retransmitting atomic.Uint64
	jobs           atomic.Uint64

	// The reader's finalization: the peer's FinishedSending received, then
	// answered; our final Terminate sent.
	peerFinishing, peerAnswered, finalTerminate bool
	// answers counts the answers to the peer's requests queued and not
	// yet written: a peer asking faster than it reads is cut off.
	answers atomic.Int64

	// Decoding buffers of the reader.
	seq  codec.Sequence
	hb   codec.UnsequencedHeartbeat
	mt   codec.MessageTemplate
	term codec.Terminate
	ap   codec.Applied
	nap  codec.NotApplied
}

func newConn(s *Session, t transport.Conn, r *sofh.Reader, peerKeepalive time.Duration) *conn {
	return &conn{
		s:             s,
		t:             t,
		r:             r,
		peerKeepalive: peerKeepalive,
		closing:       make(chan struct{}),
		writerDone:    make(chan struct{}),
		unbound:       make(chan struct{}),
		wake:          make(chan struct{}, 1),
	}
}

// close closes the connection, recording cause if it is the first.
func (c *conn) close(cause error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.cause = cause
		c.mu.Unlock()
		close(c.closing)
		c.t.Close()
	})
}

// closeAfter closes the connection after d, unless it closes before: the
// bound of a write that may be stuck.
func (c *conn) closeAfter(d time.Duration) {
	t := time.AfterFunc(d, func() { c.close(nil) })
	go func() {
		<-c.closing
		t.Stop()
	}()
}

func (c *conn) isClosing() bool {
	select {
	case <-c.closing:
		return true
	default:
		return false
	}
}

// fail terminates the connection on an error, without waiting for the
// answer (§7.3.1): Terminate UnspecifiedError, ahead of what is queued,
// then the connection closes.
func (c *conn) fail(reason string, err error) {
	c.failCode(fixp.TerminationUnspecifiedError, reason, err)
}

// failCode is fail with another termination code.
func (c *conn) failCode(code fixp.TerminationCode, reason string, err error) {
	s := c.s
	c.mu.Lock()
	if c.ours != nil && c.urgent != nil || c.peers != nil {
		c.mu.Unlock()
		return
	}
	r := truncate(reason, s.cfg.MaxReasonLength)
	c.ours = &fixp.TerminatedError{Code: code, Reason: string(r), Err: err}
	c.urgent = terminateFrame(s.ID(), code, r)
	c.mu.Unlock()
	s.logWarn("terminating", "reason", reason, "err", err)
	notify(c.wake)
	// The writer may be stuck writing: bound the wait.
	c.closeAfter(s.cfg.WriteTimeout)
}

// answer answers the peer's Terminate, ahead of what is queued, then the
// connection closes: we send nothing more (§4.4.1).
func (c *conn) answer(code fixp.TerminationCode, reason string) {
	s := c.s
	c.mu.Lock()
	if c.written {
		// Ours is written, or being written: theirs crossed it, and
		// answers it; we send no other (§6.3).
		c.answered = true
		sent := c.sent
		if !sent {
			c.closeWhenSent = true
		}
		c.mu.Unlock()
		if sent {
			c.close(nil)
		}
		return
	}
	c.peers = &fixp.TerminatedError{Code: code, Reason: reason, ByPeer: true}
	c.urgent = terminateFrame(s.ID(), code, nil)
	c.mu.Unlock()
	notify(c.wake)
	c.closeAfter(s.cfg.WriteTimeout)
}

// terminate queues our graceful Terminate, after what is queued: the
// connection closes on the answer, or one keepalive interval after it is
// written (§4.4.1, §4.4.2).
func (c *conn) terminate(reason string) {
	s := c.s
	r := truncate(reason, s.cfg.MaxReasonLength)
	c.mu.Lock()
	if c.ours != nil || c.peers != nil {
		c.mu.Unlock()
		return
	}
	c.ours = &fixp.TerminatedError{Code: fixp.TerminationFinished, Reason: string(r)}
	c.mu.Unlock()
	s.q.pushSession(c, terminateFrame(s.ID(), fixp.TerminationFinished, r), entryTerminate)
}

// terminating reports whether our graceful Terminate is written, its answer
// awaited: messages are then ignored, but the answer. Until it is written,
// what the peer sends is handled.
func (c *conn) terminating() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written
}

// result is what the session tells its handler of the connection's end.
func (c *conn) result() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.peers != nil:
		return c.peers
	case c.ours != nil:
		return c.ours
	case c.cause != nil:
		return c.cause
	}
	return errors.New("fixp/session: connection closed")
}

func (c *conn) hasUrgent() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.urgent != nil
}

func (c *conn) takeUrgent() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.urgent
	c.urgent = nil
	return f
}

// write writes a frame, bounded by the write timeout.
func (c *conn) write(f []byte) error { return c.writeSeq(f, 0, false) }

// outFrame is a frame of conn.out.
type outFrame struct {
	f   []byte
	seq uint64
	app bool
}

// maxOut is what conn.out holds before it is written.
const maxOut = 64 << 10

// buffer adds a frame to what flush writes, writing it all once it holds
// maxOut, or is a quarter of a keepalive interval old: an application
// message, of sequence number seq, or its Sequence.
func (c *conn) buffer(f []byte, seq uint64, app bool) error {
	if len(c.outFrames) == 0 {
		c.outSince = time.Now()
	}
	c.out = append(c.out, f...)
	c.outFrames = append(c.outFrames, outFrame{f, seq, app})
	if len(c.out) >= maxOut || time.Since(c.outSince) >= c.s.cfg.KeepaliveInterval/4 {
		return c.flush()
	}
	return nil
}

// flush writes what buffer holds, in one write. The frames written whole
// are observed, and the store may evict what precedes them (retention):
// a frame buffered is not written yet.
func (c *conn) flush() error {
	if len(c.out) == 0 {
		return nil
	}
	s := c.s
	n, err := c.rawWrite(c.out)
	var last uint64 // the last application message written whole
	off := 0
	for _, of := range c.outFrames {
		off += len(of.f)
		if off > n {
			break
		}
		c.observeOut(of.f, of.seq, false)
		if of.app {
			s.stats.MessagesSent.Add(1)
			last = max(last, of.seq)
		}
	}
	if last != 0 {
		s.retain(last)
	}
	c.discard()
	return err
}

// discard drops what buffer holds: taken out of the queue, its messages
// are as written and lost, to be retransmitted or reported NotApplied.
func (c *conn) discard() {
	clear(c.outFrames)
	c.out, c.outFrames = c.out[:0], c.outFrames[:0]
}

// rawWrite writes b, bounded by the write timeout, and counts what it
// wrote.
func (c *conn) rawWrite(b []byte) (int, error) {
	c.t.SetWriteDeadline(time.Now().Add(c.s.cfg.WriteTimeout))
	n, err := c.t.Write(b)
	if n > 0 {
		c.s.stats.BytesSent.Add(uint64(n))
		c.wrote = true
	}
	return n, err
}

// writeSeq writes a frame, an application message's of sequence number
// seq for the observer, after what buffer holds.
func (c *conn) writeSeq(f []byte, seq uint64, retransmission bool) error {
	if err := c.flush(); err != nil {
		return err
	}
	return c.writeNow(f, seq, retransmission)
}

// writeNow writes a frame, before what buffer holds if anything.
func (c *conn) writeNow(f []byte, seq uint64, retransmission bool) error {
	n, err := c.rawWrite(f)
	if n == len(f) {
		c.observeOut(f, seq, retransmission)
	}
	return err
}

// writeLoop writes the session's queue and heartbeats until the
// connection closes.
func (c *conn) writeLoop() {
	defer close(c.writerDone)
	s := c.s
	ka := s.cfg.KeepaliveInterval
	idle := time.NewTimer(ka)
	defer idle.Stop()
	terminated := false // our graceful Terminate is written: nothing follows
	for {
		if f := c.takeUrgent(); f != nil {
			// First, before what buffer holds, which the connection
			// ending loses: as if the write had failed.
			c.discard()
			c.writeNow(f, 0, false)
			c.close(nil)
			return
		}
		if c.isClosing() {
			c.flush() // what was taken out of the queue, as if written before
			return
		}
		if !terminated {
			if e, ok := s.q.pop(c); ok {
				if e.kind == entryTerminate {
					// What precedes it is written first, the peer's
					// answers to it read as such.
					if err := c.flush(); err != nil {
						c.close(err)
						return
					}
					// Before its write: its answer may come before Write
					// returns, and must be read as the answer. Unless the
					// peer's Terminate, or an error, came meanwhile: its
					// urgent frame goes instead, one Terminate alone.
					c.mu.Lock()
					if c.urgent != nil {
						c.mu.Unlock()
						continue
					}
					c.written = true
					c.mu.Unlock()
				}
				if err := c.writeEntry(e); err != nil {
					c.close(err)
					return
				}
				if e.kind == entryTerminate {
					terminated = true
					c.mu.Lock()
					c.sent = true
					closeNow := c.closeWhenSent
					c.mu.Unlock()
					if closeNow {
						c.close(nil) // answered while we wrote it
						return
					}
					c.closeAfter(ka)
				}
				if e.finishing {
					c.finSent = true // the heartbeat may be FinishedSending now
				}
				if c.wrote {
					idle.Reset(ka)
					c.wrote = false
				}
				continue
			}
		}
		// The queue has no more: what buffer holds goes.
		if err := c.flush(); err != nil {
			c.close(err)
			return
		}
		if c.wrote {
			idle.Reset(ka)
			c.wrote = false
		}
		select {
		case <-s.q.signal:
		case <-c.wake:
		case <-idle.C:
			if !terminated {
				if err := c.heartbeat(); err != nil {
					c.close(err)
					return
				}
			}
			idle.Reset(ka)
			c.wrote = false
		case <-c.closing:
			return
		}
	}
}

// writeEntry writes a queued frame: an application message on a sequenced
// flow after the store holds it durably, and after a Sequence if the peer
// would number it otherwise.
func (c *conn) writeEntry(e entry) error {
	s := c.s
	if e.answer {
		defer c.answers.Add(-1)
	}
	if e.finRecv {
		defer c.finRecvQueued.Store(false)
	}
	switch e.kind {
	case entryRetransmit:
		return c.retransmit(e.job)
	case entryEndsNumbering:
		// A session message but Sequence and Retransmission ends the
		// numbering (§3.5.4): the next application message follows a
		// Sequence.
		c.outNext = 0
		return c.write(e.frame)
	case entrySession, entryTerminate:
		return c.write(e.frame)
	}
	if e.seq != 0 {
		if err := s.syncTo(e.seq); err != nil {
			return err
		}
		// Out of the queue, written or not: from now on only a
		// retransmission sends it, which must not find it beyond what
		// was sent (the write may fail, the connection ending). The
		// store evicts once it is written (flush).
		if e.seq > s.written.Load() {
			s.written.Store(e.seq) // the writer's alone
		}
		if c.outNext != e.seq {
			if err := c.buffer(sequenceFrame(e.seq), 0, false); err != nil {
				return err
			}
		}
	}
	// Written with the messages queued behind it, once the queue has no
	// more: one write for a burst.
	if err := c.buffer(e.frame, e.seq, true); err != nil {
		return err
	}
	if e.seq != 0 {
		c.outNext = e.seq + 1
	}
	return nil
}

// heartbeat writes the heartbeat of our flow (§4.5): a Sequence on a
// sequenced flow, an UnsequencedHeartbeat otherwise.
func (c *conn) heartbeat() error {
	s := c.s
	var f []byte
	var next uint64
	fin, _ := s.finishes()
	switch out, _ := s.Flows(); {
	case c.finSent || fin.Finished:
		// Finishing, or finished and waiting for the peer's flow: the
		// heartbeat is FinishedSending (§4.7.1, §7.5.2), once the first
		// is out: before, it would overtake what Send queued before it.
		f = s.finishedSendingFrame()
	case out.Sequenced():
		next = s.q.nextToWrite()
		f = sequenceFrame(next)
	default:
		f = heartbeatFrame()
	}
	if err := c.write(f); err != nil {
		return err
	}
	c.outNext = next
	s.stats.HeartbeatsSent.Add(1)
	return nil
}

// errStop ends the read loop without an error: the connection is
// terminating.
var errStop = errors.New("stop")

// protocolError is a protocol violation by the peer, with its reason.
type protocolError struct{ reason string }

func (e *protocolError) Error() string { return "fixp: protocol violation: " + e.reason }
func (e *protocolError) Unwrap() error { return fixp.ErrProtocol }

func protocolf(format string, args ...any) error {
	return &protocolError{fmt.Sprintf(format, args...)}
}

// handlerError is an error a handler returned for a message.
type handlerError struct{ err error }

func (e *handlerError) Error() string { return "fixp/session: handler: " + e.err.Error() }
func (e *handlerError) Unwrap() error { return e.err }

// errHalted ends the read loop of a session whose store failed.
var errHalted = errors.New("fixp/session: halted")

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

// readLoop reads and handles frames until the connection ends.
func (c *conn) readLoop() {
	s := c.s
	lapse := time.Duration(float64(c.peerKeepalive) * s.cfg.Leniency)
	for {
		c.t.SetReadDeadline(time.Now().Add(lapse))
		enc, msg, err := c.r.Next()
		if c.isClosing() {
			return
		}
		switch {
		case err == nil:
		case isTimeout(err):
			c.fail("keepalive interval lapsed", fixp.ErrKeepaliveLapsed)
			return
		case errors.Is(err, sofh.ErrShortLength), errors.Is(err, sofh.ErrLongLength):
			c.fail("malformed frame", fmt.Errorf("%w: %w", fixp.ErrProtocol, err))
			return
		default:
			c.close(err)
			return
		}
		s.stats.BytesReceived.Add(uint64(sofh.HeaderLen + len(msg)))
		c.observeIn(enc, msg)
		if err := c.handle(enc, msg, time.Now()); err != nil {
			c.failOn(err)
			return
		}
	}
}

// failOn ends the connection on an error of the reader's.
func (c *conn) failOn(err error) {
	var pe *protocolError
	var he *handlerError
	var te *terminateError
	switch {
	case errors.Is(err, errStop), errors.Is(err, errHalted):
	case errors.As(err, &te):
		c.failCode(te.code, te.reason, nil)
	case errors.As(err, &pe):
		c.fail(pe.reason, err)
	case errors.As(err, &he):
		c.fail("application error", err)
	default:
		c.fail("unspecified error", err)
	}
}

// handle handles a frame read.
func (c *conn) handle(enc sofh.Encoding, msg []byte, received time.Time) error {
	kind, h, body, err := codec.Classify(enc, msg)
	if err != nil {
		return protocolf("%v", err)
	}
	if c.terminating() {
		// Waiting for the answer to our Terminate: the rest is ignored.
		if kind == codec.Session && h.TemplateId == codec.TerminateTemplateID {
			// The answer, if it is one: of this session, well-formed.
			if err := c.term.DecodeBody(h, body); err != nil || c.term.SessionId != c.s.ID() {
				c.close(nil)
				return errStop
			}
			// Ours may still be on its way, its Write not returned:
			// closing now would cut it, the peer seeing only EOF.
			c.mu.Lock()
			c.answered = true
			sent := c.sent
			if !sent {
				c.closeWhenSent = true
			}
			c.mu.Unlock()
			if sent {
				c.close(nil)
			}
			return errStop
		}
		return nil
	}
	switch kind {
	case codec.Ignored:
		// MessageTemplate: well-formed, then ignored. A session message,
		// it ends the numbering (§3.5.4).
		if err := c.mt.DecodeBody(h, body); err != nil {
			return protocolf("MessageTemplate: %v", err)
		}
		c.inNext = 0
		return nil
	case codec.Application:
		return c.onApplication(h, msg, body, received)
	}
	switch h.TemplateId {
	case codec.SequenceTemplateID:
		if err := c.seq.DecodeBody(h, body); err != nil {
			return protocolf("Sequence: %v", err)
		}
		return c.onSequence(c.seq.NextSeqNo)
	case codec.UnsequencedHeartbeatTemplateID:
		if err := c.hb.DecodeBody(h, body); err != nil {
			return protocolf("UnsequencedHeartbeat: %v", err)
		}
		// Any session message but those setting the numbering ends it
		// (§3.5.4).
		c.inNext = 0
		return nil
	case codec.TerminateTemplateID:
		if err := c.term.DecodeBody(h, body); err != nil {
			return protocolf("Terminate: %v", err)
		}
		if c.term.SessionId != c.s.ID() {
			return protocolf("Terminate of another session")
		}
		c.answer(fixp.TerminationCode(c.term.Code), printable(c.term.Reason, c.s.cfg.MaxReasonLength))
		return errStop
	case codec.RetransmitRequestTemplateID:
		c.inNext = 0 // a session message ending the peer's numbering (§3.5.4)
		return c.onRetransmitRequest(h, body)
	case codec.RetransmissionTemplateID:
		return c.onRetransmission(h, body)
	case codec.RestransmitRejectTemplateID:
		c.inNext = 0
		return c.onRetransmitReject(h, body)
	case codec.FinishedSendingTemplateID:
		return c.onFinishedSending(h, body)
	case codec.FinishedReceivingTemplateID:
		return c.onFinishedReceiving(h, body)
	case codec.EstablishTemplateID:
		if c.s.role == store.Server {
			return c.onEstablish(h, body)
		}
	}
	return protocolf("unexpected session message, template %d", h.TemplateId)
}

// onEstablish answers an Establish on the connection the session is
// established on: redundant, it is rejected AlreadyEstablished, the
// binding staying (§4.3.3, §7.2.5.2). One for another session is a
// protocol violation: a connection carries one session.
func (c *conn) onEstablish(h codec.MessageHeader, body []byte) error {
	s := c.s
	var e codec.Establish
	err := e.DecodeBody(h, body)
	clear(body) // its credentials, as they were read
	if err != nil {
		return protocolf("Establish: %v", err)
	}
	if fixp.SessionID(e.SessionId) != s.ID() {
		return protocolf("Establish for another session, %s", fixp.SessionID(e.SessionId))
	}
	// Any session message but those setting the numbering ends it (§3.5.4).
	c.inNext = 0
	if c.answers.Load() >= maxAnswers {
		return protocolf("requests faster than their answers are read")
	}
	c.answers.Add(1)
	s.q.pushAnswer(c, mustFrame(&codec.EstablishmentReject{
		SessionId: e.SessionId, RequestTimestamp: e.Timestamp,
		Code: codec.EstablishmentRejectCodeAlreadyEstablished, Reason: truncate("established on this connection already", s.cfg.MaxReasonLength),
	}))
	return nil
}

// onSequence handles the peer's Sequence (§4.5, §7.4.1).
func (c *conn) onSequence(next uint64) error {
	s := c.s
	_, in := s.Flows()
	if !in.Sequenced() {
		return protocolf("Sequence on an %s flow", in)
	}
	if c.rec.batch {
		return protocolf("Sequence before the end of a Retransmission")
	}
	// Expected: the next of the real-time numbering, or past everything
	// known once it ended.
	expected := c.inNext
	if expected == 0 {
		expected = c.highestKnown() + 1
	}
	_, fin := s.finishes()
	switch {
	case next < expected:
		return protocolf("Sequence %d below the next sequence number expected, %d", next, expected)
	case fin.Finishing:
		// Its flow ended: its heartbeat is FinishedSending, a Sequence
		// would resume the flow (§7.5.5).
		return protocolf("Sequence after FinishedSending")
	case next > expected:
		c.inNext = next
		return c.gap(expected, next)
	}
	c.inNext = next
	return nil
}

// onApplication handles an application message.
func (c *conn) onApplication(h codec.MessageHeader, msg, body []byte, received time.Time) error {
	s := c.s
	_, in := s.Flows()
	if in == fixp.None {
		return protocolf("application message on a None flow")
	}
	inb := Inbound{Header: readHeader(msg), Received: received}
	s.stats.MessagesReceived.Add(1)
	_, fin := s.finishes()
	if !in.Sequenced() {
		if fin.Finishing {
			return protocolf("application message after FinishedSending")
		}
		return c.deliver(h, msg, body, inb)
	}
	if c.rec.batch {
		inb.Seq, inb.Retransmission = c.rec.batchNext, true
		c.rec.batchNext++
		if fin.Finishing && inb.Seq > fin.LastSeqNo {
			return protocolf("retransmitted message %d beyond FinishedSending's LastSeqNo %d", inb.Seq, fin.LastSeqNo)
		}
		return c.onRecoverable(h, msg, body, inb)
	}
	if c.inNext == 0 {
		return protocolf("application message before a Sequence")
	}
	inb.Seq = c.inNext
	c.inNext++
	if fin.Finishing && inb.Seq > fin.LastSeqNo {
		return protocolf("application message %d after FinishedSending's LastSeqNo %d", inb.Seq, fin.LastSeqNo)
	}
	if in == fixp.Recoverable {
		return c.onRecoverable(h, msg, body, inb)
	}
	if inb.Seq <= s.inbound {
		return nil // a duplicate (§4.8)
	}
	if inb.Seq > s.inbound+1 {
		// Numbered past what was admitted: the numbers between are a gap.
		if err := c.notApplied(s.inbound+1, inb.Seq-1); err != nil {
			return err
		}
	}
	// Admitted, durably, before the handler sees it: at most once.
	if err := s.setInbound(inb.Seq, true); err != nil {
		return errHalted
	}
	return c.deliver(h, msg, body, inb)
}

// deliver hands an application message to the handler.
func (c *conn) deliver(h codec.MessageHeader, msg, body []byte, in Inbound) error {
	s := c.s
	var err error
	switch {
	case h.SchemaId == codec.SchemaID && h.TemplateId == codec.AppliedTemplateID:
		if err := c.ap.DecodeBody(h, body); err != nil {
			return protocolf("Applied: %v", err)
		}
		err = s.handler.OnApplied(s.cbCtx, s, c.ap.FromSeqNo, c.ap.Count, in)
	case h.SchemaId == codec.SchemaID && h.TemplateId == codec.NotAppliedTemplateID:
		// What an Idempotent flow's receiver reports (§4.8.2): of ours.
		if out, _ := s.Flows(); out != fixp.Idempotent {
			return protocolf("NotApplied of our %s flow", out)
		}
		if err := c.nap.DecodeBody(h, body); err != nil {
			return protocolf("NotApplied: %v", err)
		}
		err = s.handler.OnNotApplied(s.cbCtx, s, c.nap.FromSeqNo, c.nap.Count, in)
	default:
		err = s.handler.OnMessage(s.cbCtx, s, msg, in)
	}
	if err != nil {
		return &handlerError{err}
	}
	return nil
}

// checkNextSeqNo checks the NextSeqNo of the peer's Establish or
// EstablishmentAck against what was received (§4.3.2, §7.3.3), and sets
// the numbering of its next message.
func (c *conn) checkNextSeqNo(next uint64) error {
	s := c.s
	if _, in := s.Flows(); !in.Sequenced() {
		return nil
	}
	expected := s.inbound + 1
	if next < expected {
		return protocolf("NextSeqNo %d below the next sequence number expected, %d: the peer lost messages it sent", next, expected)
	}
	if next > expected {
		// Handled once established: what it sends, and the handler,
		// come after OnEstablished.
		c.rec.pendingFrom, c.rec.pendingTo = expected, next-1
	}
	c.inNext = next
	return nil
}

// startRecovery handles the gap the establishment found, if any.
func (c *conn) startRecovery() error {
	if c.rec.pendingFrom == 0 {
		return nil
	}
	from, to := c.rec.pendingFrom, c.rec.pendingTo
	c.rec.pendingFrom, c.rec.pendingTo = 0, 0
	return c.gap(from, to+1)
}

// waitWriter waits for the writer to end, closing the connection if it
// does not within the write timeout.
func (c *conn) waitWriter() {
	select {
	case <-c.writerDone:
	case <-time.After(c.s.cfg.WriteTimeout):
		c.close(nil)
		<-c.writerDone
	}
}

// storeCtx returns the context of a store call, ending after
// Config.StoreTimeout; its cancel releases it.
func (s *Session) storeCtx() *deadlineCtx {
	return &deadlineCtx{deadline: time.Now().Add(s.cfg.StoreTimeout)}
}

// deadlineCtx is a context ending at its deadline, or once canceled,
// without a timer until Done is asked for: every message makes store
// calls, which an in-memory store answers without looking at Done.
type deadlineCtx struct {
	deadline time.Time
	// end is why it ended, the first reason kept: 0 not yet,
	// endCanceled, endDeadline.
	end      atomic.Int32
	released atomic.Bool // cancel ran
	once     sync.Once
	done     chan struct{}
	timer    *time.Timer
}

const (
	endCanceled = 1
	endDeadline = 2
)

func (c *deadlineCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *deadlineCtx) Value(any) any               { return nil }

func (c *deadlineCtx) Done() <-chan struct{} {
	c.once.Do(func() {
		if c.Err() != nil {
			c.done = closedChan
			return
		}
		done := make(chan struct{})
		c.done = done
		c.timer = time.AfterFunc(time.Until(c.deadline), func() {
			c.end.CompareAndSwap(0, endDeadline)
			close(done)
		})
	})
	return c.done
}

func (c *deadlineCtx) Err() error {
	if c.end.Load() == 0 && !time.Now().Before(c.deadline) {
		c.end.CompareAndSwap(0, endDeadline)
	}
	switch c.end.Load() {
	case endCanceled:
		return context.Canceled
	case endDeadline:
		return context.DeadlineExceeded
	}
	return nil
}

// cancel ends the context, unless its deadline did first: its Done, if
// asked for, closes.
func (c *deadlineCtx) cancel() {
	if c.released.Swap(true) {
		return
	}
	if c.Err() == nil { // the deadline, passed unobserved, came first
		c.end.CompareAndSwap(0, endCanceled)
	}
	c.once.Do(func() { c.done = closedChan }) // Done, asked for later
	if c.timer != nil && c.timer.Stop() {
		close(c.done)
	}
}

var closedChan = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()
