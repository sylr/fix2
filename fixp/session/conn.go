package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
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
	// written is set once our graceful Terminate is written: what follows
	// is ignored, but the answer.
	written bool
	// cause is the error that closed the connection, if any.
	cause error

	// The writer's: the sequence number the peer gives our next
	// application message, 0 if it does not know it (no Sequence yet).
	outNext uint64
	// The reader's: the sequence number of the peer's next application
	// message, 0 if not known yet.
	inNext uint64

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
	s := c.s
	c.mu.Lock()
	if c.ours != nil && c.urgent != nil || c.peers != nil {
		c.mu.Unlock()
		return
	}
	r := truncate(reason, s.cfg.MaxReasonLength)
	c.ours = &fixp.TerminatedError{Code: fixp.TerminationUnspecifiedError, Reason: string(r), Err: err}
	c.urgent = terminateFrame(s.ID(), fixp.TerminationUnspecifiedError, r)
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

func (c *conn) takeUrgent() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.urgent
	c.urgent = nil
	return f
}

// write writes a frame, bounded by the write timeout.
func (c *conn) write(f []byte) error {
	c.t.SetWriteDeadline(time.Now().Add(c.s.cfg.WriteTimeout))
	_, err := c.t.Write(f)
	if err == nil {
		c.s.stats.BytesSent.Add(uint64(len(f)))
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
			c.write(f)
			c.close(nil)
			return
		}
		if c.isClosing() {
			return
		}
		if !terminated {
			if e, ok := s.q.pop(c); ok {
				if e.kind == entryTerminate {
					// Before the write: its answer may come before Write
					// returns, and must be read as the answer.
					c.mu.Lock()
					c.written = true
					c.mu.Unlock()
				}
				if err := c.writeEntry(e); err != nil {
					c.close(err)
					return
				}
				if e.kind == entryTerminate {
					terminated = true
					c.closeAfter(ka)
				}
				idle.Reset(ka)
				continue
			}
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
	if e.kind != entryApp {
		return c.write(e.frame)
	}
	if e.seq != 0 {
		if err := s.syncTo(e.seq); err != nil {
			return err
		}
		if c.outNext != e.seq {
			if err := c.write(sequenceFrame(e.seq)); err != nil {
				return err
			}
		}
	}
	if err := c.write(e.frame); err != nil {
		return err
	}
	if e.seq != 0 {
		c.outNext = e.seq + 1
	}
	s.stats.MessagesSent.Add(1)
	return nil
}

// heartbeat writes the heartbeat of our flow (§4.5): a Sequence on a
// sequenced flow, an UnsequencedHeartbeat otherwise.
func (c *conn) heartbeat() error {
	s := c.s
	var f []byte
	var next uint64
	if out, _ := s.Flows(); out.Sequenced() {
		next = s.q.nextToWrite()
		f = sequenceFrame(next)
	} else {
		f = heartbeatFrame()
	}
	if err := c.write(f); err != nil {
		return err
	}
	if next != 0 {
		c.outNext = next
	}
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
		err = c.handle(enc, msg, time.Now())
		var pe *protocolError
		var he *handlerError
		switch {
		case err == nil:
			continue
		case errors.Is(err, errStop), errors.Is(err, errHalted):
		case errors.As(err, &pe):
			c.fail(pe.reason, err)
		case errors.As(err, &he):
			c.fail("application error", err)
		default:
			c.fail("unspecified error", err)
		}
		return
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
			c.close(nil)
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
		c.answer(fixp.TerminationCode(c.term.Code), printable(c.term.Reason, c.s.cfg.MaxReasonLength))
		return errStop
	case codec.RetransmitRequestTemplateID, codec.RetransmissionTemplateID, codec.RestransmitRejectTemplateID:
		return c.onRetransmission(h)
	case codec.FinishedSendingTemplateID, codec.FinishedReceivingTemplateID:
		return c.onFinish(h)
	}
	return protocolf("unexpected session message, template %d", h.TemplateId)
}

// onSequence handles the peer's Sequence (§4.5, §7.4.1).
func (c *conn) onSequence(next uint64) error {
	s := c.s
	_, in := s.Flows()
	if !in.Sequenced() {
		return protocolf("Sequence on an %s flow", in)
	}
	expected := s.inbound + 1
	switch {
	case next < expected:
		return protocolf("Sequence %d below the next sequence number expected, %d", next, expected)
	case next > expected:
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
	if !in.Sequenced() {
		return c.deliver(h, msg, body, inb)
	}
	if c.inNext == 0 {
		return protocolf("application message before a Sequence")
	}
	inb.Seq = c.inNext
	c.inNext++
	if inb.Seq <= s.inbound {
		return nil // a duplicate (§4.8)
	}
	if in == fixp.Idempotent {
		// Admitted, durably, before the handler sees it: at most once.
		if err := s.setInbound(inb.Seq, true); err != nil {
			return errHalted
		}
	}
	if err := c.deliver(h, msg, body, inb); err != nil {
		return err
	}
	if in == fixp.Recoverable {
		if err := s.setInbound(inb.Seq, false); err != nil {
			return errHalted
		}
	}
	return nil
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
	switch {
	case next < expected:
		return protocolf("NextSeqNo %d below the next sequence number expected, %d: the peer lost messages it sent", next, expected)
	case next > expected:
		return c.gap(expected, next)
	}
	c.inNext = next
	return nil
}

// errUnsupported is what M5 answers to what M6 and M7 implement.
func errUnsupported(what string) error {
	return protocolf("%s is not supported yet", what)
}

// gap handles the peer's messages from to next-1, never received. (M6)
func (c *conn) gap(from, next uint64) error {
	return errUnsupported(fmt.Sprintf("recovering messages %d to %d", from, next-1))
}

// onRetransmission handles the retransmission messages. (M6)
func (c *conn) onRetransmission(h codec.MessageHeader) error {
	if out, _ := c.s.Flows(); h.TemplateId == codec.RetransmitRequestTemplateID && out != fixp.Recoverable {
		return protocolf("RetransmitRequest on an %s flow", out)
	}
	return errUnsupported("retransmission")
}

// onFinish handles finalization. (M7)
func (c *conn) onFinish(h codec.MessageHeader) error {
	return errUnsupported("finalization")
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

// storeCtx returns the context of a store call.
func (s *Session) storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cfg.StoreTimeout)
}
