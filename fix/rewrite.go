package fix

import (
	"fmt"
	"sync"
)

// messagePool holds the scratch Messages of RewriteForResend: a Message
// always escapes, its field slice pointing at its own inline buffer.
var messagePool = sync.Pool{New: func() any { return new(Message) }}

const (
	tagMsgSeqNum       = 34
	tagPossDupFlag     = 43
	tagSendingTime     = 52
	tagOrigSendingTime = 122
	tagSignatureLength = 93
	tagSignature       = 89
)

// ErrSignedMessage reports a message carrying a signature (tags 89/93), which
// changing its header would invalidate.
var ErrSignedMessage = newError("signed message")

// standardHeaderTags are the tags of the FIX standard header, which
// RewriteForResend scans to place the fields it adds without stepping into
// the body.
var standardHeaderTags = map[int]bool{
	8: true, 9: true, 35: true, 49: true, 56: true, 115: true, 128: true,
	90: true, 91: true, 34: true, 50: true, 142: true, 57: true, 143: true,
	116: true, 144: true, 129: true, 145: true, 43: true, 97: true, 52: true,
	122: true, 212: true, 213: true, 347: true, 369: true, 627: true,
	628: true, 629: true, 630: true, 1128: true, 1129: true, 1156: true,
}

// RewriteForResend appends to dst the retransmission of raw, a complete
// message as it was sent, parsed with p (the default profile if nil): FIX
// allows only CheckSum, OrigSendingTime, SendingTime, BodyLength and
// PossDupFlag to change. PossDupFlag (43) is set to Y, OrigSendingTime (122)
// to the original SendingTime unless raw has one already, SendingTime (52)
// to sendingTime, and BodyLength and CheckSum are recomputed. A field added
// is placed in the standard header, before the first header field with a
// higher tag. The body and the other header fields are copied unchanged,
// DATA fields included. dst must not overlap raw.
//
// It returns an error, and dst unchanged, if raw does not parse or holds
// more than one message, has no SendingTime in its standard header (the
// fields before the first one that is not a standard header field) or
// repeats SendingTime, PossDupFlag or OrigSendingTime there, or is signed.
func RewriteForResend(dst, raw []byte, p *Profile, sendingTime []byte) ([]byte, error) {
	m := messagePool.Get().(*Message)
	defer messagePool.Put(m)
	if err := ParseInto(m, raw, p); err != nil {
		return dst, fmt.Errorf("fix2: RewriteForResend: %w", err)
	}
	fields := m.Fields()
	last := len(fields) - 1 // CheckSum
	orig, hasOrig, hasPossDup := []byte(nil), false, false
	headerEnd := last // index of the first field after the header
	for i, f := range fields[3:last] {
		switch f.Tag {
		case tagSignature, tagSignatureLength:
			return dst, fmt.Errorf("fix2: RewriteForResend: %w", ErrSignedMessage)
		}
		if headerEnd < last {
			continue // in the body: copied as is
		}
		if !standardHeaderTags[f.Tag] {
			headerEnd = i + 3
			continue
		}
		dup := false
		switch f.Tag {
		case tagSendingTime:
			dup, orig = orig != nil, f.Value
		case tagOrigSendingTime:
			dup, hasOrig = hasOrig, true
		case tagPossDupFlag:
			dup, hasPossDup = hasPossDup, true
		}
		if dup {
			return dst, fmt.Errorf("fix2: RewriteForResend: %w: tag %d appears more than once in the header", ErrInvalidField, f.Tag)
		}
	}
	if orig == nil {
		return dst, fmt.Errorf("fix2: RewriteForResend: %w: no SendingTime (tag 52) in the standard header", ErrInvalidField)
	}

	start := len(dst)
	dst = append(dst, "8="...)
	dst = append(dst, fields[0].Value...)
	dst = append(dst, soh, '9', '=')
	lenPos := len(dst)
	dst = append(dst, "0000000000"...)
	dst = append(dst, soh)
	bodyStart := len(dst)
	dst = AppendTagValue(dst, tagMsgType, fields[2].Value)

	// insert appends the fields to add whose tag is below limit.
	insert := func(dst []byte, limit int) []byte {
		if !hasPossDup && tagPossDupFlag < limit {
			dst = AppendTagValue(dst, tagPossDupFlag, "Y")
			hasPossDup = true
		}
		if !hasOrig && tagOrigSendingTime < limit {
			dst = AppendTagValue(dst, tagOrigSendingTime, orig)
			hasOrig = true
		}
		return dst
	}
	for i, f := range fields[3:last] {
		i += 3
		if i < headerEnd {
			dst = insert(dst, f.Tag)
		} else if i == headerEnd {
			dst = insert(dst, int(^uint(0)>>1))
		}
		switch f.Tag {
		case tagSendingTime:
			if i < headerEnd {
				dst = AppendTagValue(dst, tagSendingTime, sendingTime)
				continue
			}
		case tagPossDupFlag:
			if i < headerEnd {
				dst = AppendTagValue(dst, tagPossDupFlag, "Y")
				continue
			}
		}
		dst = appendField(dst, f)
	}
	if headerEnd == last {
		dst = insert(dst, int(^uint(0)>>1))
	}

	bodyLen := len(dst) - bodyStart
	var digits [20]byte
	n := appendUint(digits[:0], bodyLen)
	copy(dst[lenPos:], n)
	dst[lenPos+len(n)] = soh
	copy(dst[lenPos+len(n)+1:], dst[bodyStart:])
	dst = dst[:len(dst)-(10-len(n))]
	return AppendTrailer(dst, start), nil
}

// appendUint appends v in decimal to dst.
func appendUint(dst []byte, v int) []byte {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return append(dst, b[i:]...)
}
