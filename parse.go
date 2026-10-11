package fix2

import (
	"bytes"
	"fmt"
	"io"
	"math"
)

const (
	soh         = 0x01 // FIX field delimiter
	minMsgLen   = 25   // minimum plausible FIX message length
	tagBeginStr = 8
	tagBodyLen  = 9
	tagMsgType  = 35
	tagCheckSum = 10

	// String forms of the tags read with parseNextExpectedTag.
	tagBodyLenStr  = "9"
	tagCheckSumStr = "10"

	// maxHeaderLen bounds the "8=...|9=...|" prefix ParseFromReader reads
	// byte by byte before it knows the BodyLength.
	maxHeaderLen = 64
)

// MaxBodyLength is the largest BodyLength (tag 9) ParseFromReader accepts.
// It bounds the buffer allocated for the body before any of it is read, so a
// peer cannot make the reader allocate arbitrary memory. Parse is not limited:
// its input is already in memory.
const MaxBodyLength = 1 << 20

// Parse parses a FIX message from data with the default profile (see
// DefaultProfile). All field values are zero-copy slices into data. The
// caller must not modify data while the returned Message is in use.
// With SIMD, the checksum is one vector sum over the message after parsing
// it, while its bytes are in cache; otherwise it is summed field by field as
// the parser goes (see bulkChecksum).
func Parse(data []byte) (Message, error) {
	var msg Message
	err := parseInto(&msg, data, defaultProfile.kinds)
	return msg, err
}

// ParseInto parses a FIX message from data into m, reading DATA fields as p
// defines them (the default profile if p is nil). m is reset first and its
// field storage reused, so parsing into the same Message repeatedly does not
// allocate. Field values are zero-copy slices into data, valid while data is
// not modified and until m is parsed into again. A copy of m made after it
// grew beyond 32 fields shares that storage, so parse into m only once such
// copies are no longer used. On error, m holds the fields parsed so far.
func ParseInto(m *Message, data []byte, p *Profile) error {
	if p == nil {
		p = defaultProfile
	}
	return parseInto(m, data, p.kinds)
}

// parseInto parses data into msg, classifying DATA fields with kinds (see
// Profile).
func parseInto(msg *Message, data []byte, kinds []int32) error {
	msg.reset()

	if len(data) < minMsgLen {
		return &ParseError{Err: ErrMessageTooShort, Offset: 0}
	}

	cursor := 0
	var csum uint32
	csumPos := 0

	// --- Tag 8: BeginString ---
	if data[0] != '8' || data[1] != '=' {
		return &ParseError{Err: fmt.Errorf("%w: %w: expected \"%d=\" got %q", ErrInvalidBeginTag, ErrInvalidTag, tagBeginStr, data[:2]), Offset: 0}
	}
	sohPos := findByte(data, 2, soh)
	if sohPos < 0 {
		return &ParseError{Err: fmt.Errorf("%w: %w: SOH not found", ErrInvalidBeginTag, ErrInvalidTag), Offset: 0}
	}
	beginVal := data[2:sohPos]
	if !bytes.HasPrefix(beginVal, []byte("FIX.")) && !bytes.HasPrefix(beginVal, []byte("FIXT.")) {
		return &ParseError{Err: fmt.Errorf("%w: %w: expected \"FIX.\" or \"FIXT.\" got %q", ErrInvalidBeginTag, ErrInvalidTag, beginVal), Offset: 0}
	}
	msg.append(Field{Tag: tagBeginStr, Value: beginVal})
	cursor = sohPos + 1
	if !bulkChecksum {
		csum += checksumRange(data, csumPos, cursor)
		csumPos = cursor
	}

	// --- Tag 9: BodyLength ---
	tag, eqPos, err := parseNextExpectedTag(data, cursor, tagBodyLenStr)
	if err != nil {
		return &ParseError{Err: fmt.Errorf("%w: %w", ErrInvalidBodyLen, err), Offset: cursor}
	}
	if tag != tagBodyLen {
		return &ParseError{Err: fmt.Errorf("%w: %w: expected \"%d=\" got \"%d=\"", ErrInvalidBodyLen, ErrInvalidTag, tagBodyLen, tag), Offset: cursor}
	}
	sohPos = findByte(data, eqPos+1, soh)
	if sohPos < 0 {
		return &ParseError{Err: fmt.Errorf("%w: %w: SOH not found", ErrInvalidBodyLen, ErrInvalidTag), Offset: cursor}
	}
	bodyLenVal := data[eqPos+1 : sohPos]
	bodyLen, ok := parseUint(bodyLenVal)
	if !ok {
		return &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	msg.append(Field{Tag: tagBodyLen, Value: bodyLenVal})
	cursor = sohPos + 1
	if !bulkChecksum {
		csum += checksumRange(data, csumPos, cursor)
		csumPos = cursor
	}

	// Compute body region. Compare by subtraction: bodyStart+bodyLen can
	// overflow for a BodyLength close to MaxInt.
	bodyStart := cursor
	if bodyLen > len(data)-bodyStart {
		return &ParseError{Err: ErrBodyLenMismatch, Offset: cursor}
	}
	bodyEnd := bodyStart + bodyLen

	// --- Parse body fields ---
	var next dataField // DATA field announced by the previous LENGTH field
	for cursor < bodyEnd {
		tag, eqPos, err = parseNextTag(data, cursor)
		if err != nil {
			return err
		}
		kind := 0
		if tag < len(kinds) {
			kind = int(kinds[tag])
		}
		if next.tag != 0 || kind == isDataTag {
			if sohPos, err = next.end(data, tag, cursor, eqPos+1, bodyEnd); err != nil {
				return err
			}
		} else {
			sohPos = findByte(data, eqPos+1, soh)
			if sohPos < 0 || sohPos >= bodyEnd {
				return fieldEndError(sohPos, cursor)
			}
		}
		value := data[eqPos+1 : sohPos]
		next.tag = 0
		if kind > 0 {
			if next, err = announceData(tag, kind, value, cursor); err != nil {
				return err
			}
		}
		msg.append(Field{Tag: tag, Value: value})
		cursor = sohPos + 1
		if !bulkChecksum {
			csum += checksumRange(data, csumPos, cursor)
			csumPos = cursor
		}
	}
	if next.tag != 0 {
		return next.missing(cursor)
	}

	// Verify tag 35 (MsgType) is the third field (index 2).
	if msg.Count() < 3 || msg.fields[2].Tag != tagMsgType {
		return &ParseError{Err: ErrMissingMsgType, Offset: bodyStart}
	}

	// --- Tag 10: CheckSum (trailer) ---
	tag, eqPos, err = parseNextExpectedTag(data, cursor, tagCheckSumStr)
	if err != nil {
		return &ParseError{Err: fmt.Errorf("%w: %w", ErrInvalidCheckSum, err), Offset: cursor}
	}
	if tag != tagCheckSum {
		return &ParseError{Err: fmt.Errorf("%w: %w: expected \"%d=\" got \"%d=\"", ErrInvalidCheckSum, ErrInvalidTag, tagCheckSum, tag), Offset: cursor}
	}
	sohPos = findByte(data, eqPos+1, soh)
	if sohPos < 0 {
		return &ParseError{Err: fmt.Errorf("%w: %w: SOH not found", ErrInvalidCheckSum, ErrInvalidTag), Offset: cursor}
	}
	csVal := data[eqPos+1 : sohPos]
	if len(csVal) != 3 {
		return &ParseError{Err: fmt.Errorf("%w: %w: expected 3 bytes, got %d", ErrInvalidCheckSum, ErrInvalidTag, len(csVal)), Offset: cursor}
	}
	expected, ok := parseUint(csVal)
	if !ok || expected > 255 {
		return &ParseError{Err: fmt.Errorf("%w: %w: expected 0-255, got %d", ErrInvalidCheckSum, ErrInvalidTag, expected), Offset: cursor}
	}
	msg.append(Field{Tag: tagCheckSum, Value: csVal})

	// Validate the checksum, accumulated field by field or summed at once.
	if bulkChecksum {
		csum = checksumBlock(data[:bodyEnd])
	}
	computed := csum % 256
	if int(computed) != expected {
		return &ParseError{Err: fmt.Errorf("%w: expected %d, got %d", ErrChecksumMismatch, expected, int(computed)), Offset: cursor}
	}

	return nil
}

// checksumRange returns the sum of data[from:to] as uint32.
func checksumRange(data []byte, from, to int) uint32 {
	var sum uint32
	for i := from; i < to; i++ {
		sum += uint32(data[i])
	}
	return sum
}

// ParseFromReader reads a single FIX message from r and parses it.
// It reads the header byte-by-byte to determine BodyLength without
// over-reading, then uses io.ReadFull for the remaining body and trailer.
// Parsing is done inline from the two buffers (header, rest), each
// checksummed separately — no full-message buffer is assembled.
func ParseFromReader(r io.Reader) (Message, error) {
	// Read tags 8 and 9 (terminated by SOH each) byte by byte so we
	// never consume bytes belonging to the next message on a stream.
	header := make([]byte, 0, 32)
	var b [1]byte
	sohCount := 0
	for sohCount < 2 {
		if len(header) == maxHeaderLen {
			return Message{}, &ParseError{Err: fmt.Errorf("%w: no BeginString and BodyLength in the first %d bytes", ErrMessageTooLarge, maxHeaderLen), Offset: len(header)}
		}
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return Message{}, &ParseError{Err: fmt.Errorf("%w: %w", ErrMessageTooShort, err), Offset: len(header)}
		}
		header = append(header, b[0])
		if b[0] == soh {
			sohCount++
		}
	}

	var msg Message
	msg.init()

	// --- Tag 8: BeginString (from header) ---
	if len(header) < 4 || header[0] != '8' || header[1] != '=' {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: expected \"%d=\" got %q", ErrInvalidBeginTag, ErrInvalidTag, tagBeginStr, header[:2]), Offset: 0}
	}
	firstSOH := findByte(header, 2, soh)
	if firstSOH < 0 {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: SOH not found", ErrInvalidBeginTag, ErrInvalidTag), Offset: 0}
	}
	beginVal := header[2:firstSOH]
	if !bytes.HasPrefix(beginVal, []byte("FIX.")) && !bytes.HasPrefix(beginVal, []byte("FIXT.")) {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: expected \"FIX.\" or \"FIXT.\" got %q", ErrInvalidBeginTag, ErrInvalidTag, beginVal), Offset: 0}
	}
	msg.append(Field{Tag: tagBeginStr, Value: beginVal})

	// --- Tag 9: BodyLength (from header) ---
	cursor := firstSOH + 1
	if len(header) < cursor+4 { // need at least "9=X\x01"
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	tag, eqPos, err := parseNextExpectedTag(header, cursor, tagBodyLenStr)
	if err != nil {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w", ErrInvalidBodyLen, err), Offset: cursor}
	}
	if tag != tagBodyLen {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: expected \"%d=\" got \"%d=\"", ErrInvalidBodyLen, ErrInvalidTag, tagBodyLen, tag), Offset: cursor}
	}
	sohPos := findByte(header, eqPos+1, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: SOH not found", ErrInvalidBodyLen, ErrInvalidTag), Offset: cursor}
	}
	bodyLenVal := header[eqPos+1 : sohPos]
	bodyLen, ok := parseUint(bodyLenVal)
	if !ok {
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	if bodyLen > MaxBodyLength {
		return msg, &ParseError{Err: fmt.Errorf("%w: BodyLength %d exceeds %d", ErrMessageTooLarge, bodyLen, MaxBodyLength), Offset: cursor}
	}
	msg.append(Field{Tag: tagBodyLen, Value: bodyLenVal})

	// Checksum the entire header.
	csum := checksumBlock(header)

	// Read body + trailer "10=XXX\x01" (7 bytes) in one call.
	rest := make([]byte, bodyLen+7)
	if _, err := io.ReadFull(r, rest); err != nil {
		return Message{}, &ParseError{Err: fmt.Errorf("%w: %w", ErrBodyLenMismatch, err), Offset: len(header)}
	}

	// --- Parse body fields from rest[0:bodyLen] ---
	cursor = 0
	bodyEnd := bodyLen
	var next dataField // DATA field announced by the previous LENGTH field
	for cursor < bodyEnd {
		tag, eqPos, err = parseNextTag(rest, cursor)
		if err != nil {
			return msg, withOffset(err, len(header))
		}
		kind := 0
		if tag < len(defaultProfile.kinds) {
			kind = int(defaultProfile.kinds[tag])
		}
		if next.tag != 0 || kind == isDataTag {
			if sohPos, err = next.end(rest, tag, cursor, eqPos+1, bodyEnd); err != nil {
				return msg, withOffset(err, len(header))
			}
		} else {
			sohPos = findByte(rest, eqPos+1, soh)
			if sohPos < 0 || sohPos >= bodyEnd {
				return msg, fieldEndError(sohPos, len(header)+cursor)
			}
		}
		value := rest[eqPos+1 : sohPos]
		next.tag = 0
		if kind > 0 {
			if next, err = announceData(tag, kind, value, cursor); err != nil {
				return msg, withOffset(err, len(header))
			}
		}
		msg.append(Field{Tag: tag, Value: value})
		if !bulkChecksum {
			csum += checksumRange(rest, cursor, sohPos+1)
		}
		cursor = sohPos + 1
	}
	if next.tag != 0 {
		return msg, withOffset(next.missing(cursor), len(header))
	}

	// Verify tag 35 (MsgType) is the third field (index 2).
	if msg.Count() < 3 || msg.fields[2].Tag != tagMsgType {
		return msg, &ParseError{Err: ErrMissingMsgType, Offset: len(header)}
	}

	// --- Tag 10: CheckSum (trailer from rest[bodyLen:]) ---
	cursor = bodyLen
	tag, eqPos, err = parseNextExpectedTag(rest, cursor, tagCheckSumStr)
	if err != nil {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w", ErrInvalidCheckSum, err), Offset: len(header) + cursor}
	}
	if tag != tagCheckSum {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: expected \"%d=\" got \"%d=\"", ErrInvalidCheckSum, ErrInvalidTag, tagCheckSum, tag), Offset: len(header) + cursor}
	}
	sohPos = findByte(rest, eqPos+1, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: SOH not found", ErrInvalidCheckSum, ErrInvalidTag), Offset: len(header) + cursor}
	}
	csVal := rest[eqPos+1 : sohPos]
	if len(csVal) != 3 {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: expected 3 bytes, got %d", ErrInvalidCheckSum, ErrInvalidTag, len(csVal)), Offset: len(header) + cursor}
	}
	expected, ok := parseUint(csVal)
	if !ok || expected > 255 {
		return msg, &ParseError{Err: fmt.Errorf("%w: %w: expected 0-255, got %d", ErrInvalidCheckSum, ErrInvalidTag, expected), Offset: len(header) + cursor}
	}
	msg.append(Field{Tag: tagCheckSum, Value: csVal})

	// Validate checksum: sum of header + body, mod 256, the body accumulated
	// field by field or summed at once.
	if bulkChecksum {
		csum += checksumBlock(rest[:bodyLen])
	}
	if int(csum%256) != expected {
		return msg, &ParseError{Err: fmt.Errorf("%w: expected %d, got %d", ErrChecksumMismatch, expected, int(csum%256)), Offset: len(header) + bodyLen}
	}

	return msg, nil
}

// dataField is the DATA field announced by the LENGTH field just parsed (see
// dataFields). FIX requires it to come next; its value is read as exactly
// length bytes, which may include SOH. Other fields end at the next SOH.
type dataField struct {
	lengthTag int // the announcing LENGTH tag
	tag       int // expected DATA tag, 0 if none
	length    int // its size in bytes
}

// end returns the index of the SOH ending the value of field tag, which
// starts at valStart, when a DATA field is involved: tag must be the announced
// DATA tag, and a DATA tag must have been announced. The value must fit, with
// its SOH, before bodyEnd; the length comes from the peer, so it is compared
// by subtraction. cursor is the start of the field, for the error offset.
func (d dataField) end(data []byte, tag, cursor, valStart, bodyEnd int) (int, error) {
	if d.tag == 0 {
		return 0, &ParseError{Err: fmt.Errorf("%w: DATA tag %d without its LENGTH field right before it", ErrMalformedField, tag), Offset: cursor}
	}
	if tag != d.tag {
		return 0, d.missing(cursor)
	}
	if d.length >= bodyEnd-valStart || data[valStart+d.length] != soh {
		return 0, &ParseError{Err: fmt.Errorf("%w: DATA tag %d does not end after its declared %d bytes", ErrMalformedField, d.tag, d.length), Offset: cursor}
	}
	return valStart + d.length, nil
}

// fieldEndError reports a body field whose SOH, at sohPos, is missing (-1)
// or not strictly inside the body declared by BodyLength.
func fieldEndError(sohPos, offset int) error {
	if sohPos < 0 {
		return &ParseError{Err: ErrMalformedField, Offset: offset}
	}
	return &ParseError{Err: fmt.Errorf("%w: field runs past the end of the body", ErrBodyLenMismatch), Offset: offset}
}

// missing reports that the announced DATA field does not come next.
func (d dataField) missing(cursor int) error {
	return &ParseError{Err: fmt.Errorf("%w: LENGTH tag %d not followed by DATA tag %d", ErrMalformedField, d.lengthTag, d.tag), Offset: cursor}
}

// announceData parses the value of LENGTH tag, which announces DATA tag
// dataTag that must follow it.
func announceData(tag, dataTag int, value []byte, cursor int) (dataField, error) {
	n, ok := parseUint(value)
	if !ok {
		return dataField{}, &ParseError{Err: fmt.Errorf("%w: invalid length %q in tag %d", ErrMalformedField, value, tag), Offset: cursor}
	}
	return dataField{lengthTag: tag, tag: dataTag, length: n}, nil
}

// withOffset shifts the offset of a *ParseError by delta, e.g. from the body
// buffer of ParseFromReader to the whole message.
func withOffset(err error, delta int) error {
	if pe, ok := err.(*ParseError); ok {
		pe.Offset += delta
	}
	return err
}

// parseNextTag parses a tag number at data[cursor:] up to '='.
// Returns the tag number and the index of '=' in data.
func parseNextTag(data []byte, cursor int) (int, int, error) {
	// Fast path for tags of 1-4 digits, only when all four probes are in bounds.
	var eqPos int
	switch {
	case cursor+4 >= len(data):
		eqPos = findByte(data, cursor, '=')
	case data[cursor+1] == '=':
		eqPos = cursor + 1
	case data[cursor+2] == '=':
		eqPos = cursor + 2
	case data[cursor+3] == '=':
		eqPos = cursor + 3
	case data[cursor+4] == '=':
		eqPos = cursor + 4
	default:
		eqPos = findByte(data, cursor, '=')
	}
	if eqPos < 0 || eqPos == cursor {
		return 0, 0, &ParseError{Err: ErrInvalidTag, Offset: cursor}
	}
	tag, ok := parseUint(data[cursor:eqPos])
	if !ok {
		return 0, 0, &ParseError{Err: ErrInvalidTag, Offset: cursor}
	}
	return tag, eqPos, nil
}

// parseNextExpectedTag parses a tag of len(expectedTag) digits at data[cursor:]
// followed by '='. Returns the tag number and the index of '=' in data. Errors
// are bare sentinels; callers wrap them in a ParseError with their own context.
func parseNextExpectedTag(data []byte, cursor int, expectedTag string) (int, int, error) {
	eqPos := cursor + len(expectedTag)
	if eqPos >= len(data) || data[eqPos] != '=' {
		return 0, 0, ErrFieldSeparatorNotFound
	}
	tag, ok := parseUint(data[cursor:eqPos])
	if !ok {
		return 0, 0, ErrInvalidTag
	}
	return tag, eqPos, nil
}

// parseUint parses an ASCII decimal integer from b without allocating.
// Returns the value and true on success, false on a non-digit byte or if the
// value does not fit in an int.
func parseUint(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	if len(b) <= maxSafeDigits {
		for _, c := range b {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int(c - '0')
		if n > (math.MaxInt-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}
