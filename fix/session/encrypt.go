package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// Encrypter seals and opens the SecureData (91) of a session's messages, for
// an EncryptMethod (98) other than 0, with keys the counterparties agreed
// beforehand, out of band, as FIX 4.4 has it: "The previously agreed upon
// encryption methodology is declared in the Logon message" (unverified: no
// FIX 4.4 text is pinned). FIX 4.x only: FIXT 1.1 deprecated it.
//
// Each side's Logon declares Method, the other's must declare the same: an
// acceptor refuses a Logon declaring another, or none, disconnecting without
// Logout (SL §4.3.1), and an initiator disconnects on such an answer. A
// session without Encrypter declares 0 and refuses any other (SL §4.3.3:
// "EncyptionMethod(98) must be present in the Logon(35=A) message and set to
// “0” when application layer encryption is not in use.").
//
// Once the Logons agreed, every message but a Logon, reset ones included, and
// those without a body, has its body sealed into SecureData (91), its
// standard header in clear, as FIX 4.4 recommends: "it is recommended but not
// required that all fields within the message body be encrypted"
// (unverified). Messages received are opened alike, the header fields found
// in SecureData merged with the clear ones, which they must equal: "The clear
// (unencrypted) fields can be repeated within the SecureData field to serve as
// an integrity check of the clear data" (unverified). One that does not open,
// differs, or has body fields in clear, is rejected with SessionRejectReason
// 7, Decryption problem, and counted. The store keeps messages in clear, and
// the Observer sees them so, both ways: a resend is sealed when written.
//
// On one session, Seal runs on the connection's writer goroutine and Open on
// its reader: they may run at the same time, never two Seals, nor two Opens.
type Encrypter interface {
	// Method returns the EncryptMethod (98) the session's Logons declare: a
	// positive integer, which Config.Validate checks.
	Method() string
	// Seal appends to dst the sealed form of plain, the fields of a
	// message's body, tag=value<SOH> each, and returns the result. An error
	// closes the connection, the messages it was to write unwritten. It
	// must authenticate what it seals: what Open returns is trusted.
	Seal(dst, plain []byte) ([]byte, error)
	// Open appends to dst the fields sealed holds, as Seal was given them,
	// and returns the result. An error rejects the message.
	Open(dst, sealed []byte) ([]byte, error)
}

// encryptMethod returns the EncryptMethod (98) our Logons declare, which the
// counterparty's must declare too: the Encrypter's, 0 (none) without one.
func (s *Session) encryptMethod() string {
	if s.cfg.Encrypter == nil {
		return "0"
	}
	return s.cfg.Encrypter.Method()
}

// seal returns buf, the frames the writer is about to write, which end at the
// offsets frames give, as they go on the wire once the Logon exchange agreed
// on encryption, and where each ends there: every one but a Logon, and those
// without a body, has its body sealed into SecureData (91), which ends its
// standard header, BodyLength and CheckSum written again. The header stays in
// clear. It runs on the writer, which owns what it returns until its next
// call; an error is the Encrypter's.
func (c *conn) seal(buf []byte, frames []pendingFrame) ([]byte, []int, error) {
	out, ends := reusable(c.wire, c.s.cfg.MaxPendingBytes), c.wireEnds[:0]
	start := 0
	for _, f := range frames {
		var err error
		if out, err = c.sealFrame(out, buf[start:f.end]); err != nil {
			return nil, nil, fmt.Errorf("sealing message %d: %w", f.seq, err)
		}
		ends = append(ends, len(out))
		start = f.end
	}
	c.wire, c.wireEnds = out, ends
	return out, ends, nil
}

// sealFrame appends frame, one of ours, to dst, sealed as seal says.
func (c *conn) sealFrame(dst, frame []byte) ([]byte, error) {
	s := c.s
	m := &c.wmsg
	if err := fix.ParseInto(m, frame, s.dicts.parse); err != nil {
		return dst, err
	}
	fields := m.Fields()
	last := len(fields) - 1 // CheckSum
	body := 3
	for body < last && s.dicts.inHeader(fields[body].Tag) {
		body++
	}
	if body == last || bytes.Equal(fields[2].Value, msgLogon) {
		return append(dst, frame...), nil
	}
	from := fieldsLen(fields[:2])                      // MsgType's
	bodyStart := len(frame) - fieldsLen(fields[body:]) // the body's first field
	bodyEnd := len(frame) - fieldsLen(fields[last:])
	var err error
	if c.sealed, err = s.cfg.Encrypter.Seal(c.sealed[:0], frame[bodyStart:bodyEnd]); err != nil {
		return dst, err
	}
	start := len(dst)
	dst = fix.AppendTagValue(dst, 8, fields[0].Value)
	dst = append(dst, "9="...)
	// BodyLength comes first: reserve its widest form, shrink it after.
	lenPos := len(dst)
	dst = append(dst, "0000000000\x01"...)
	headerStart := len(dst)
	dst = append(dst, frame[from:bodyStart]...)
	dst = fix.AppendTagInt(dst, tagSecureDataLen, int64(len(c.sealed)))
	dst = fix.AppendTagValue(dst, tagSecureData, c.sealed)
	d := appendDecimal(nil, uint64(len(dst)-headerStart))
	copy(dst[lenPos:], d)
	dst[lenPos+len(d)] = 1
	copy(dst[lenPos+len(d)+1:], dst[headerStart:])
	dst = dst[:len(dst)-(10-len(d))]
	return fix.AppendTrailer(dst, start), nil
}

// open returns m, received on c, and frame, its bytes, as the session handles
// them: once the Logon exchange agreed on encryption, a message but a Logon
// has its SecureData (91) opened, the header fields found in it merged with
// the clear ones (FIX 4.4: "The clear (unencrypted) fields can be repeated
// within the SecureData field to serve as an integrity check of the clear
// data", unverified), the other fields its body, and its frame is built
// anew, parsed again into a message of the reader's. One without SecureData
// has no body. It returns instead why m cannot be made out, its frame and m
// left as they are: SecureData Open refuses, or whose fields do not parse, a
// header field in it differing from the clear one, a body field in clear.
// FIX 4.4 recommends, without requiring, that the whole body be encrypted:
// fix2 requires it, fields in clear and sealed leaving their order unknown.
// A signature, SignatureLength (93) and Signature (89), stays in clear, last.
func (c *conn) open(m *fix.Message, frame []byte) (*fix.Message, []byte, string) {
	s := c.s
	if !c.encrypted || bytes.Equal(m.MsgType(), msgLogon) {
		return m, frame, ""
	}
	fields := m.Fields()
	fields = fields[3 : len(fields)-1] // after MsgType, before CheckSum
	var sealed []byte
	for _, f := range fields {
		switch {
		case f.Tag == tagSecureData:
			sealed = f.Value
		case f.Tag == tagSignatureLength || f.Tag == tagSignature:
		case !s.dicts.inHeader(f.Tag):
			return m, frame, fmt.Sprintf("tag %d in clear: the body must be in SecureData (91)", f.Tag)
		}
	}
	if sealed == nil {
		return m, frame, ""
	}
	plain, err := s.cfg.Encrypter.Open(c.plain[:0], sealed)
	if plain != nil {
		c.plain = plain
	}
	if err != nil {
		return m, frame, "SecureData (91) does not open: " + err.Error()
	}
	// The fields sealed, parsed behind a header of their own.
	c.body = append(fix.AppendTagValue(c.body[:0], tagMsgType, m.MsgType()), plain...)
	c.inner = wrapFrame(c.inner[:0], m.BeginString(), c.body)
	if err := s.cfg.parseOptions().ParseInto(&c.smsg, c.inner, s.dicts.parse); err != nil {
		return m, frame, "SecureData (91) does not hold fields: " + err.Error()
	}
	inner := c.smsg.Fields()
	inner = inner[3 : len(inner)-1]
	body := fix.AppendTagValue(c.body[:0], tagMsgType, m.MsgType())
	for _, f := range fields {
		switch f.Tag {
		case tagSecureDataLen, tagSecureData, tagSignatureLength, tagSignature:
		default:
			body = fix.AppendTagValue(body, f.Tag, f.Value)
		}
	}
	for _, f := range inner {
		if !s.dicts.inHeader(f.Tag) {
			continue
		}
		if v := m.Get(f.Tag); v != nil {
			if !bytes.Equal(v, f.Value) {
				return m, frame, fmt.Sprintf("tag %d in SecureData (91) differs from the one in clear", f.Tag)
			}
			continue
		}
		body = fix.AppendTagValue(body, f.Tag, f.Value)
	}
	for _, f := range inner {
		if !s.dicts.inHeader(f.Tag) {
			body = fix.AppendTagValue(body, f.Tag, f.Value)
		}
	}
	for _, f := range fields {
		if f.Tag == tagSignatureLength || f.Tag == tagSignature {
			body = fix.AppendTagValue(body, f.Tag, f.Value)
		}
	}
	c.body = body
	c.opened = wrapFrame(c.opened[:0], m.BeginString(), body)
	if err := s.cfg.parseOptions().ParseInto(&c.omsg, c.opened, s.dicts.parse); err != nil {
		return m, frame, "SecureData (91) does not hold fields: " + err.Error()
	}
	return &c.omsg, c.opened, ""
}

// wrapFrame appends to dst the frame of BeginString begin whose fields from
// MsgType on are body.
func wrapFrame(dst, begin, body []byte) []byte {
	start := len(dst)
	dst = fix.AppendTagValue(dst, 8, begin)
	dst = fix.AppendTagInt(dst, 9, int64(len(body)))
	return fix.AppendTrailer(append(dst, body...), start)
}

// undecryptable handles m, received on c, which open could not make out, for
// why: as a message parsed only up to a malformed tag (see unreadableLocked),
// since SecureData (91) may hide any of its fields, it is rejected with
// SessionRejectReason 7, Decryption problem, about SecureData, and counted if
// it is the one expected, not acted on (SL §4.5.4), a MsgSeqNum (34) it hides
// logging out. The Observer does not see it. Before the session is logged on
// it reports false: m is handled as any other message then, a Logout or
// Reject refusing our Logon, anything else ending the connection.
func (c *conn) undecryptable(m *fix.Message, why string, read, now time.Time) bool {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == phaseAwaitingLogon || s.phase == phaseLogonSent {
		return false
	}
	if s.c != c || c.closed {
		return true
	}
	c.lastRead = read
	if c.peerLoggedOut {
		// As a message whole (see handleFrame).
		s.log.LogAttrs(context.Background(), slog.LevelInfo, "undecryptable message after the counterparty's Logout ignored")
	} else {
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "message not decrypted", slog.String("reason", why))
		s.unreadableLocked(c, m, dictionary.ReasonDecryptionProblem, tagSecureData, now)
	}
	if !c.closed {
		s.armLocked(c)
	}
	return true
}

// fieldsLen returns the bytes fields take in a frame, tag=value<SOH> each.
func fieldsLen(fields []fix.Field) int {
	n := 0
	for _, f := range fields {
		n += len(f.Value) + 3 // a digit, '=' and SOH
		for t := f.Tag; t >= 10; t /= 10 {
			n++
		}
	}
	return n
}

// validMethod reports whether v is an EncryptMethod (98) other than 0: a
// positive integer, as Logons compare it, without sign or leading zeros.
func validMethod(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n > 0 && strconv.Itoa(n) == v
}
