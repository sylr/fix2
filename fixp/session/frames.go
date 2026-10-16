package session

import (
	"encoding/binary"
	"time"
	"unicode/utf8"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/sofh"
)

// encoder is what the codec's messages and Outgoing share.
type encoder interface {
	AppendTo(dst []byte) ([]byte, error)
}

// frame encodes m as a SOFH frame of SBE 1.0 little-endian.
func frame(m encoder) ([]byte, error) {
	b, err := m.AppendTo(make([]byte, sofh.HeaderLen, 64))
	if err != nil {
		return nil, err
	}
	if err := sofh.PutHeader(b, sofh.SBE10LittleEndian, len(b)-sofh.HeaderLen); err != nil {
		return nil, err
	}
	return b, nil
}

// mustFrame encodes a session message, which cannot fail: its data fields
// are bounded before (credentials, reasons).
func mustFrame(m encoder) []byte {
	b, err := frame(m)
	if err != nil {
		panic("fixp/session: encoding a session message: " + err.Error())
	}
	return b
}

// readHeader reads the standard header of an application message, whose
// length must hold one.
func readHeader(msg []byte) Header {
	return Header{
		BlockLength: binary.LittleEndian.Uint16(msg[0:]),
		TemplateID:  binary.LittleEndian.Uint16(msg[2:]),
		SchemaID:    binary.LittleEndian.Uint16(msg[4:]),
		Version:     binary.LittleEndian.Uint16(msg[6:]),
	}
}

// nanos returns t as FIXP timestamps are: nanoseconds since the Unix epoch.
func nanos(t time.Time) uint64 { return uint64(t.UnixNano()) }

// fromNanos is nanos's inverse.
func fromNanos(n uint64) time.Time { return time.Unix(0, int64(n)) }

// millis returns a keepalive interval as the wire holds it.
func millis(d time.Duration) uint32 { return uint32(d / time.Millisecond) }

// truncate cuts a reason to n bytes at most, on a rune boundary.
func truncate(s string, n int) []byte {
	if len(s) <= n {
		return []byte(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return []byte(s[:n])
}

// printable returns a received reason fit for a log or an error: n bytes
// at most, invalid UTF-8 replaced.
func printable(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string([]rune(string(b)))
}

func sequenceFrame(next uint64) []byte {
	return mustFrame(&codec.Sequence{NextSeqNo: next})
}

func heartbeatFrame() []byte { return mustFrame(&codec.UnsequencedHeartbeat{}) }

func terminateFrame(id fixp.SessionID, code fixp.TerminationCode, reason []byte) []byte {
	return mustFrame(&codec.Terminate{SessionId: id, Code: codec.TerminationCode(code), Reason: reason})
}
