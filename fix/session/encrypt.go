package session

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/sylr/fix2/fix"
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
// Seal and Open may run at the same time.
type Encrypter interface {
	// Method returns the EncryptMethod (98) the session's Logons declare: a
	// positive integer, which Config.Validate checks.
	Method() string
	// Seal appends to dst the sealed form of plain, the fields of a
	// message's body, tag=value<SOH> each, and returns the result.
	Seal(dst, plain []byte) ([]byte, error)
	// Open appends to dst the fields sealed holds, as Seal was given them,
	// and returns the result.
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
