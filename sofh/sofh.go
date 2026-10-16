// Package sofh reads and writes the Simple Open Framing Header (SOFH 1.0)
// of the FIX Trading Community: the 6 bytes in front of a message telling
// its length and its encoding, so that a stream of messages can be split
// without decoding them, whatever their encoding.
//
// The header is a 4-byte Message_Length, the length of the message
// including the header, then a 2-byte Encoding_Type, both big-endian.
// Some implementations count Message_Length without the header (the FIXP
// reference implementation, Silverflash, does): their frames are not
// SOFH's, and this package does not read them.
//
// A malformed header cannot be skipped: nothing in a binary stream tells
// where the next frame starts. The Reader fails, and the connection is to
// be closed.
package sofh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
)

// HeaderLen is the length of the framing header.
const HeaderLen = 6

// Encoding is a SOFH Encoding_Type.
type Encoding uint16

// The encodings SOFH 1.0 registers. FIX FAST takes 0xFA01 to 0xFAFF, private
// encodings 0x0001 to 0x00FF.
const (
	SBE10BigEndian    Encoding = 0x5BE0
	SBE10LittleEndian Encoding = 0xEB50
	GPB10             Encoding = 0x4700
	ASN1PER10         Encoding = 0xA500
	ASN1BER10         Encoding = 0xA501
	ASN1OER10         Encoding = 0xA502
	TagValue          Encoding = 0xF000
	FIXMLSchema10     Encoding = 0xF100
	JSON              Encoding = 0xF500
	BSON              Encoding = 0xFB00
)

// Private reports whether e is a private, user-defined encoding.
func (e Encoding) Private() bool { return e >= 0x0001 && e <= 0x00FF }

func (e Encoding) String() string {
	switch e {
	case SBE10BigEndian:
		return "SBE 1.0 big-endian"
	case SBE10LittleEndian:
		return "SBE 1.0 little-endian"
	case GPB10:
		return "GPB 1.0"
	case ASN1PER10:
		return "ASN.1 PER 1.0"
	case ASN1BER10:
		return "ASN.1 BER 1.0"
	case ASN1OER10:
		return "ASN.1 OER 1.0"
	case TagValue:
		return "FIX tagvalue"
	case FIXMLSchema10:
		return "FIXML schema 1.0"
	case JSON:
		return "FIX JSON"
	case BSON:
		return "FIX BSON"
	}
	switch {
	case e.Private():
		return fmt.Sprintf("private 0x%04X", uint16(e))
	case e >= 0xFA01 && e <= 0xFAFF:
		return fmt.Sprintf("FIX FAST 0x%04X", uint16(e))
	}
	return fmt.Sprintf("0x%04X", uint16(e))
}

var (
	// ErrShortLength reports a Message_Length shorter than the header it
	// includes.
	ErrShortLength = errors.New("sofh: message length shorter than the framing header")
	// ErrLongLength reports a Message_Length beyond the limit the reader or
	// writer was given.
	ErrLongLength = errors.New("sofh: message length beyond the limit")
)

// ParseHeader decodes the framing header at the start of b, which must hold
// HeaderLen bytes. It returns the frame length, header included, and the
// encoding. A frame length below HeaderLen is ErrShortLength.
func ParseHeader(b []byte) (frameLen int, enc Encoding, err error) {
	_ = b[HeaderLen-1]
	n := binary.BigEndian.Uint32(b)
	enc = Encoding(binary.BigEndian.Uint16(b[4:]))
	if n < HeaderLen {
		return 0, enc, fmt.Errorf("%w: %d", ErrShortLength, n)
	}
	if uint64(n) > math.MaxInt {
		return 0, enc, fmt.Errorf("%w: %d bytes", ErrLongLength, n)
	}
	return int(n), enc, nil
}

// PutHeader writes into b[:HeaderLen] the framing header of a message of
// msgLen bytes, header excluded, in encoding enc. A frame longer than 2^32-1
// bytes is ErrLongLength.
func PutHeader(b []byte, enc Encoding, msgLen int) error {
	_ = b[HeaderLen-1]
	if err := checkMsgLen(msgLen); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(b, uint32(HeaderLen+uint64(msgLen)))
	binary.BigEndian.PutUint16(b[4:], uint16(enc))
	return nil
}

// checkMsgLen reports whether a message of n bytes fits a frame.
func checkMsgLen(n int) error {
	if n < 0 || uint64(n) > math.MaxUint32-HeaderLen {
		return fmt.Errorf("%w: a message of %d bytes", ErrLongLength, n)
	}
	return nil
}

// AppendFrame appends to dst the frame of msg in encoding enc: its header,
// then msg. msg may share dst's memory.
func AppendFrame(dst []byte, enc Encoding, msg []byte) ([]byte, error) {
	if err := checkMsgLen(len(msg)); err != nil {
		return dst, err
	}
	n := len(dst)
	dst = slices.Grow(dst, HeaderLen+len(msg))[:n+HeaderLen+len(msg)]
	// The message first: the header would overwrite it, were it in dst
	// past n. copy handles the overlap.
	copy(dst[n+HeaderLen:], msg)
	PutHeader(dst[n:], enc, len(msg))
	return dst, nil
}
