package session

import (
	"time"

	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/sofh"
)

// Frame is a frame as it went over an established connection, for an
// Observer.
type Frame struct {
	// Msg is the message, its SBE header included, borrowed for the call:
	// it must be neither changed nor kept. The credentials of a Negotiate,
	// a NegotiationResponse or an Establish are zeroed in it, whenever
	// they come.
	Msg      []byte
	Encoding sofh.Encoding
	// Header is its SBE header, zero if Msg is shorter than one.
	Header Header
	// Seq is an application message's sequence number on a sequenced flow,
	// as far as known when observed, 0 otherwise.
	Seq uint64
	// Retransmission is set for a message of a Retransmission batch.
	Retransmission bool
	// Connection numbers the session's connections, from 1.
	Connection uint64
	// Time is when it was read, or written.
	Time time.Time
}

// Observer sees every frame of a session's established connections, such
// as for an audit log: those of the handshake are not. OnInbound runs on
// the connection's reader before the frame is handled, a frame then found
// malformed included; OnOutbound on its writer once the frame is written.
// They may run concurrently, must not block, and must call none of the
// session's methods but its getters (ID, State, Flows, NextSeqNo, Stats).
type Observer interface {
	OnInbound(s *Session, f Frame)
	OnOutbound(s *Session, f Frame)
}

// redact returns msg, or a copy of it whose credentials are zeroed if it
// carries some.
func redact(msg []byte) []byte {
	h, _, err := codec.ReadHeader(msg)
	if err != nil || h.SchemaId != codec.SchemaID {
		return msg
	}
	var creds []byte
	c := append([]byte(nil), msg...)
	switch h.TemplateId {
	case codec.NegotiateTemplateID:
		var m codec.Negotiate
		if _, err := m.Decode(c); err != nil {
			return zeroBody(c)
		}
		creds = m.Credentials
	case codec.NegotiationResponseTemplateID:
		var m codec.NegotiationResponse
		if _, err := m.Decode(c); err != nil {
			return zeroBody(c)
		}
		creds = m.Credentials
	case codec.EstablishTemplateID:
		var m codec.Establish
		if _, err := m.Decode(c); err != nil {
			return zeroBody(c)
		}
		creds = m.Credentials
	default:
		return msg
	}
	clear(creds) // aliases c
	return c
}

// zeroBody zeroes what follows the header of a message too malformed to
// find its credentials in.
func zeroBody(c []byte) []byte {
	if len(c) > codec.HeaderSize {
		clear(c[codec.HeaderSize:])
	}
	return c
}

// observe builds the Frame of msg.
func (c *conn) frameOf(enc sofh.Encoding, msg []byte, seq uint64, retransmission bool) Frame {
	f := Frame{Msg: redact(msg), Encoding: enc, Seq: seq, Retransmission: retransmission, Connection: c.number, Time: time.Now()}
	if len(msg) >= codec.HeaderSize {
		f.Header = readHeader(msg)
	}
	return f
}

// observeIn shows a frame read to the observer, before it is handled.
func (c *conn) observeIn(enc sofh.Encoding, msg []byte) {
	s := c.s
	o := s.cfg.Observer
	if o == nil {
		return
	}
	var seq uint64
	retrans := false
	if _, in := s.Flows(); in.Sequenced() && len(msg) >= codec.HeaderSize {
		if h := readHeader(msg); h.SchemaID != codec.SchemaID || h.TemplateID == codec.AppliedTemplateID || h.TemplateID == codec.NotAppliedTemplateID {
			if c.rec.batch != nil {
				seq, retrans = c.rec.batchNext, true
			} else {
				seq = c.inNext
			}
		}
	}
	o.OnInbound(s, c.frameOf(enc, msg, seq, retrans))
}

// observeOut shows a frame written to the observer.
func (c *conn) observeOut(frame []byte, seq uint64, retransmission bool) {
	s := c.s
	o := s.cfg.Observer
	if o == nil || len(frame) < sofh.HeaderLen {
		return
	}
	_, enc, err := sofh.ParseHeader(frame)
	if err != nil {
		return
	}
	o.OnOutbound(s, c.frameOf(enc, frame[sofh.HeaderLen:], seq, retransmission))
}
