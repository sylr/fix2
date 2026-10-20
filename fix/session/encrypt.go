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
// FIX 4.4 text is pinned). FIX 4.x only: SL §4.8.4, note 12: "The use of
// SecureData(91) and its associated length field SecureDataLen(90) were
// deprecated with the introduction of the FIXT.1.1 Session Layer
// specification."
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
	// and returns the result. An error rejects the message, as does a
	// result, or the message rebuilt with it, longer than fix.MaxBodyLength,
	// the most the session reads of a message: Open itself bounds what it
	// allocates.
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
// clear, and so does a signature ending the body, SignatureLength (93) and
// Signature (89), after SecureData. It runs on the writer, which owns what it returns until its next
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
	// A signature, SignatureLength (93) and Signature (89), ends the message,
	// never within SecureData: it stays in clear, after it.
	end := last
	for end > body && (fields[end-1].Tag == tagSignatureLength || fields[end-1].Tag == tagSignature) {
		end--
	}
	if end == body || bytes.Equal(fields[2].Value, msgLogon) {
		return append(dst, frame...), nil
	}
	from := fieldsLen(fields[:2])                      // MsgType's
	bodyStart := len(frame) - fieldsLen(fields[body:]) // the body's first field
	bodyEnd := len(frame) - fieldsLen(fields[end:])    // the signature's, or CheckSum's
	trailer := frame[bodyEnd : len(frame)-fieldsLen(fields[last:])]
	sealed, err := s.cfg.Encrypter.Seal(reusable(c.sealed, maxPooledBuffer), frame[bodyStart:bodyEnd])
	if err != nil {
		return dst, err
	}
	c.sealed = sealed
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
	dst = append(dst, trailer...)
	var digits [20]byte
	d := appendDecimal(digits[:0], uint64(len(dst)-headerStart))
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
// has no body. It returns instead the tag and why m cannot be made out, its
// frame and m left as they are: SecureData Open refuses, whose fields do not
// parse, or which opens to more than fix.MaxBodyLength, a header field in it
// differing from its clear one, the occurrences of a tag compared in order,
// a body field in clear. FIX 4.4 recommends, without requiring, that the
// whole body be encrypted: fix2 requires it, fields in clear and sealed
// leaving their order unknown. A signature, SignatureLength (93) and
// Signature (89), stays in clear, last.
func (c *conn) open(m *fix.Message, frame []byte) (*fix.Message, []byte, int, string) {
	s := c.s
	if !c.encrypted || bytes.Equal(m.MsgType(), msgLogon) {
		return m, frame, 0, ""
	}
	fields := m.Fields()
	fields = fields[3 : len(fields)-1] // after MsgType, before CheckSum
	var sealed []byte
	found := false
	for _, f := range fields {
		switch {
		case f.Tag == tagSecureData && found:
			// Which one would the message be? Both removed, validation
			// would see neither.
			return m, frame, tagSecureData, "SecureData (91) more than once"
		case f.Tag == tagSecureData:
			sealed, found = f.Value, true
		case f.Tag == tagSignatureLength || f.Tag == tagSignature:
		case !s.dicts.inHeader(f.Tag):
			return m, frame, f.Tag, fmt.Sprintf("tag %d in clear: the body must be in SecureData (91)", f.Tag)
		}
	}
	if !found {
		return m, frame, 0, ""
	}
	plain, err := s.cfg.Encrypter.Open(reusable(c.plain, maxPooledBuffer), sealed)
	if plain != nil {
		c.plain = plain
	}
	switch {
	case err != nil:
		return m, frame, tagSecureData, "SecureData (91) does not open: " + err.Error()
	case len(plain) > fix.MaxBodyLength:
		return m, frame, tagSecureData, fmt.Sprintf("SecureData (91) opens to %d bytes, more than %d", len(plain), fix.MaxBodyLength)
	}
	// The fields sealed, parsed behind a header of their own.
	c.body = append(fix.AppendTagValue(reusable(c.body, maxPooledBuffer), tagMsgType, m.MsgType()), plain...)
	c.inner = wrapFrame(reusable(c.inner, maxPooledBuffer), m.BeginString(), c.body)
	if err := s.cfg.parseOptions().ParseInto(&c.smsg, c.inner, s.dicts.parse); err != nil {
		return m, frame, tagSecureData, "SecureData (91) does not hold fields: " + err.Error()
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
	// The clear values of each tag, and the occurrences of each seen inside,
	// once one is: the n-th inside is compared with the n-th in clear, a
	// repeated group (NoHops) matching entry by entry. A field FIX keeps in
	// clear may be repeated inside, never be there alone; SecureData itself,
	// the signature and CheckSum are never inside.
	var clear map[int][][]byte
	var seen map[int]int
	for _, f := range inner {
		switch f.Tag {
		case tagSecureDataLen, tagSecureData, tagSignatureLength, tagSignature, 10:
			return m, frame, tagSecureData, fmt.Sprintf("tag %d in SecureData (91), where it never is", f.Tag)
		}
		if !s.dicts.inHeader(f.Tag) {
			continue
		}
		if clear == nil {
			clear, seen = make(map[int][][]byte), make(map[int]int)
			for _, g := range m.Fields() {
				clear[g.Tag] = append(clear[g.Tag], g.Value)
			}
		}
		n := seen[f.Tag]
		seen[f.Tag] = n + 1
		if vs := clear[f.Tag]; n < len(vs) {
			if !bytes.Equal(vs[n], f.Value) {
				return m, frame, tagSecureData, fmt.Sprintf("tag %d in SecureData (91) differs from the one in clear", f.Tag)
			}
			continue
		}
		if alwaysClear(f.Tag) {
			return m, frame, f.Tag, fmt.Sprintf("tag %d in SecureData (91) only, where it is always in clear", f.Tag)
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
	if len(body) > fix.MaxBodyLength {
		return m, frame, tagSecureData, fmt.Sprintf("SecureData (91) opens to a message of %d bytes, more than %d", len(body), fix.MaxBodyLength)
	}
	c.opened = wrapFrame(reusable(c.opened, maxPooledBuffer), m.BeginString(), body)
	if err := s.cfg.parseOptions().ParseInto(&c.omsg, c.opened, s.dicts.parse); err != nil {
		// Not reached: every field was parsed with this profile, and is
		// written as it was read, a LENGTH field and its DATA field kept
		// together (see newDicts). Kept rather than handing on a message
		// half parsed.
		return m, frame, tagSecureData, "SecureData (91) does not hold fields: " + err.Error()
	}
	return &c.omsg, c.opened, 0, ""
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
// SessionRejectReason 7, Decryption problem, about tag, and counted if
// it is the one expected, not acted on (SL §4.5.4), a MsgSeqNum (34) it hides
// logging out. The Observer does not see it. Before the session is logged on
// it reports false: m is handled as any other message then, a Logout or
// Reject refusing our Logon, anything else ending the connection.
func (c *conn) undecryptable(m *fix.Message, tag int, why string, read, now time.Time) bool {
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
		s.unreadableLocked(c, m, dictionary.ReasonDecryptionProblem, tag, now)
	}
	if !c.closed {
		s.armLocked(c)
	}
	return true
}

// checkSealable checks that body, of a message of msgType SendRaw is given,
// can be sealed: its fields parse, and none is SecureDataLen (90) or
// SecureData (91), which sealing writes. Otherwise the writer would fail to
// seal it, or the counterparty reject it.
func (s *Session) checkSealable(msgType string, body []byte) error {
	b := append(fix.AppendTagValue(nil, tagMsgType, msgType), body...)
	var m fix.Message
	if err := fix.ParseInto(&m, wrapFrame(nil, []byte(s.id.BeginString), b), s.dicts.parse); err != nil {
		return fmt.Errorf("body cannot be sealed: %w", err)
	}
	for _, f := range m.Fields() {
		if f.Tag == tagSecureDataLen || f.Tag == tagSecureData {
			return &fix.ReservedTagError{Tag: f.Tag}
		}
	}
	return nil
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

// alwaysClear reports whether FIX keeps tag out of SecureData (91): SL §8.5
// has SenderCompID (49) and TargetCompID (56) "(Always unencrypted)", and
// "SecureDataLen(90) must be present and unencrypted if SecureData(91) is
// present in the message", §8.6 "SignatureLength(93) must not be included
// as part of the encrypted content in SecureData(91)", as Signature (89);
// BeginString (8), BodyLength (9), MsgType (35) and CheckSum (10) frame the
// message.
func alwaysClear(tag int) bool {
	switch tag {
	case 8, 9, tagMsgType, tagSenderCompID, tagTargetCompID, tagSecureDataLen, tagSecureData, tagSignatureLength, tagSignature, 10:
		return true
	}
	return false
}

// validMethod reports whether v is an EncryptMethod (98) other than 0: a
// positive integer, as Logons compare it, without sign or leading zeros.
func validMethod(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n > 0 && strconv.Itoa(n) == v
}
