package fix2

import "bytes"

const (
	soh         = 0x01 // FIX field delimiter
	minMsgLen   = 25   // minimum plausible FIX message length
	tagBeginStr = 8
	tagBodyLen  = 9
	tagMsgType  = 35
	tagCheckSum = 10
)

// Parse parses a FIX message from data. All field values are zero-copy slices
// into data. The caller must not modify data while the returned Message is in use.
func Parse(data []byte) (Message, error) {
	var msg Message
	msg.init()

	if len(data) < minMsgLen {
		return msg, &ParseError{Err: ErrMessageTooShort, Offset: 0}
	}

	cursor := 0

	// --- Tag 8: BeginString ---
	// Must start with "8="
	if data[0] != '8' || data[1] != '=' {
		return msg, &ParseError{Err: ErrInvalidBeginTag, Offset: 0}
	}
	sohPos := findByte(data, 2, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: ErrInvalidBeginTag, Offset: 0}
	}
	beginVal := data[2:sohPos]
	if !bytes.HasPrefix(beginVal, []byte("FIX.")) && !bytes.HasPrefix(beginVal, []byte("FIXT.")) {
		return msg, &ParseError{Err: ErrInvalidBeginTag, Offset: 0}
	}
	msg.append(Field{Tag: tagBeginStr, Value: beginVal})
	cursor = sohPos + 1

	// --- Tag 9: BodyLength ---
	tag, eqPos, err := parseNextExpectedTag(data, cursor, []byte("9"))
	if err != nil {
		return msg, err
	}
	if tag != tagBodyLen {
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	sohPos = findByte(data, eqPos+1, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	bodyLenVal := data[eqPos+1 : sohPos]
	bodyLen, ok := parseUint(bodyLenVal)
	if !ok {
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	msg.append(Field{Tag: tagBodyLen, Value: bodyLenVal})
	cursor = sohPos + 1

	// Compute body region: from cursor to cursor+bodyLen.
	bodyStart := cursor
	bodyEnd := bodyStart + bodyLen
	if bodyEnd > len(data) {
		return msg, &ParseError{Err: ErrBodyLenMismatch, Offset: cursor}
	}

	// --- Parse body fields ---
	for cursor < bodyEnd {
		tag, eqPos, err = parseNextTag(data, cursor)
		if err != nil {
			return msg, err
		}
		sohPos = findByte(data, eqPos+1, soh)
		if sohPos < 0 || sohPos > bodyEnd {
			return msg, &ParseError{Err: ErrMalformedField, Offset: cursor}
		}
		msg.append(Field{Tag: tag, Value: data[eqPos+1 : sohPos]})
		cursor = sohPos + 1
	}

	// Verify tag 35 (MsgType) is the third field (index 2).
	if msg.Count() < 3 || msg.fields[2].Tag != tagMsgType {
		return msg, &ParseError{Err: ErrMissingMsgType, Offset: bodyStart}
	}

	// --- Tag 10: CheckSum (trailer) ---
	trailerStart := cursor
	tag, eqPos, err = parseNextExpectedTag(data, cursor, []byte("10"))
	if err != nil {
		return msg, err
	}
	if tag != tagCheckSum {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	sohPos = findByte(data, eqPos+1, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	csVal := data[eqPos+1 : sohPos]
	if len(csVal) != 3 {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	expected, ok := parseUint(csVal)
	if !ok || expected > 255 {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	msg.append(Field{Tag: tagCheckSum, Value: csVal})

	// Validate checksum: sum of all bytes before "10=" trailer.
	computed := checksumBlock(data[:trailerStart]) % 256
	if int(computed) != expected {
		return msg, &ParseError{Err: ErrChecksumMismatch, Offset: cursor}
	}

	return msg, nil
}

// checksumRange returns the sum of data[from:to] as uint32.
func checksumRange(data []byte, from, to int) uint32 {
	var sum uint32
	for i := from; i < to; i++ {
		sum += uint32(data[i])
	}
	return sum
}

// ParseWithChecksum is like Parse but computes the checksum incrementally
// during the single parsing pass instead of making a separate pass over the
// data. The bytes are already L1-hot from findByte, so the scalar addition
// has near-zero marginal cost.
func ParseWithChecksum(data []byte) (Message, error) {
	var msg Message
	msg.init()

	if len(data) < minMsgLen {
		return msg, &ParseError{Err: ErrMessageTooShort, Offset: 0}
	}

	cursor := 0
	var csum uint32
	csumPos := 0

	// --- Tag 8: BeginString ---
	if data[0] != '8' || data[1] != '=' {
		return msg, &ParseError{Err: ErrInvalidBeginTag, Offset: 0}
	}
	sohPos := findByte(data, 2, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: ErrInvalidBeginTag, Offset: 0}
	}
	beginVal := data[2:sohPos]
	if !bytes.HasPrefix(beginVal, []byte("FIX.")) && !bytes.HasPrefix(beginVal, []byte("FIXT.")) {
		return msg, &ParseError{Err: ErrInvalidBeginTag, Offset: 0}
	}
	msg.append(Field{Tag: tagBeginStr, Value: beginVal})
	cursor = sohPos + 1
	csum += checksumRange(data, csumPos, cursor)
	csumPos = cursor

	// --- Tag 9: BodyLength ---
	tag, eqPos, err := parseNextExpectedTag(data, cursor, []byte("9"))
	if err != nil {
		return msg, err
	}
	if tag != tagBodyLen {
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	sohPos = findByte(data, eqPos+1, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	bodyLenVal := data[eqPos+1 : sohPos]
	bodyLen, ok := parseUint(bodyLenVal)
	if !ok {
		return msg, &ParseError{Err: ErrInvalidBodyLen, Offset: cursor}
	}
	msg.append(Field{Tag: tagBodyLen, Value: bodyLenVal})
	cursor = sohPos + 1
	csum += checksumRange(data, csumPos, cursor)
	csumPos = cursor

	// Compute body region.
	bodyStart := cursor
	bodyEnd := bodyStart + bodyLen
	if bodyEnd > len(data) {
		return msg, &ParseError{Err: ErrBodyLenMismatch, Offset: cursor}
	}

	// --- Parse body fields ---
	for cursor < bodyEnd {
		tag, eqPos, err = parseNextTag(data, cursor)
		if err != nil {
			return msg, err
		}
		sohPos = findByte(data, eqPos+1, soh)
		if sohPos < 0 || sohPos > bodyEnd {
			return msg, &ParseError{Err: ErrMalformedField, Offset: cursor}
		}
		msg.append(Field{Tag: tag, Value: data[eqPos+1 : sohPos]})
		cursor = sohPos + 1
		csum += checksumRange(data, csumPos, cursor)
		csumPos = cursor
	}

	// Verify tag 35 (MsgType) is the third field (index 2).
	if msg.Count() < 3 || msg.fields[2].Tag != tagMsgType {
		return msg, &ParseError{Err: ErrMissingMsgType, Offset: bodyStart}
	}

	// --- Tag 10: CheckSum (trailer) ---
	tag, eqPos, err = parseNextExpectedTag(data, cursor, []byte("10"))
	if err != nil {
		return msg, err
	}
	if tag != tagCheckSum {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	sohPos = findByte(data, eqPos+1, soh)
	if sohPos < 0 {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	csVal := data[eqPos+1 : sohPos]
	if len(csVal) != 3 {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	expected, ok := parseUint(csVal)
	if !ok || expected > 255 {
		return msg, &ParseError{Err: ErrInvalidCheckSum, Offset: cursor}
	}
	msg.append(Field{Tag: tagCheckSum, Value: csVal})

	// Validate checksum using incrementally accumulated sum.
	computed := csum % 256
	if int(computed) != expected {
		return msg, &ParseError{Err: ErrChecksumMismatch, Offset: cursor}
	}

	return msg, nil
}

// parseNextTag parses a tag number at data[cursor:] up to '='.
// Returns the tag number and the index of '=' in data.
func parseNextTag(data []byte, cursor int) (int, int, error) {
	var eqPos int
	if data[cursor+1] == '=' {
		eqPos = cursor + 1
	} else if data[cursor+2] == '=' {
		eqPos = cursor + 2
	} else if data[cursor+3] == '=' {
		eqPos = cursor + 3
	} else if data[cursor+4] == '=' {
		eqPos = cursor + 4
	} else {
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

// parseNextTagWithTag parses a tag number at data[cursor:] up to '='.
// Returns the tag number and the index of '=' in data.
func parseNextExpectedTag(data []byte, cursor int, expectedTag []byte) (int, int, error) {
	if data[cursor+len(expectedTag)] != '=' {
		return 0, 0, &ParseError{Err: ErrFieldSeparatorNotFound, Offset: cursor}
	}
	tag, ok := parseUint(data[cursor : cursor+len(expectedTag)])
	if !ok {
		return 0, 0, &ParseError{Err: ErrInvalidTag, Offset: cursor}
	}
	return tag, cursor + len(expectedTag), nil
}

// parseUint parses an ASCII decimal integer from b without allocating.
// Returns the value and true on success.
func parseUint(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n < 0 { // overflow
			return 0, false
		}
	}
	return n, true
}
