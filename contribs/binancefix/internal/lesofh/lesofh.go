// Package lesofh reads and writes the framing header of Binance's "FIX SBE"
// API: the Simple Open Framing Header with both its fields little-endian,
// as Binance's documentation requires ("The two fields within the SOFH MUST
// be encoded in little-endian"), where SOFH 1.0 makes them big-endian.
// fix2's sofh package keeps the standard's byte order; this one follows
// its discipline otherwise: messageLength counts the 6-byte header, a frame
// is bounded by a maximum length, and a malformed header cannot be skipped.
//
// Binance accepts one encodingType, 0xEB50 (SBE 1.0 little-endian), which
// goes on the wire as the bytes 50 EB. A frame of any other is refused.
package lesofh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/sylr/fix2/sofh"
)

// HeaderLen is the length of the framing header.
const HeaderLen = sofh.HeaderLen

// Encoding is the only encodingType Binance accepts and sends.
const Encoding = sofh.SBE10LittleEndian

const (
	// DefaultMaxLength is the longest frame a Reader accepts by default,
	// header included: Binance's messages are a few hundred bytes, an
	// order list's a few kilobytes.
	DefaultMaxLength = 64 << 10
	// DefaultBufferSize is the initial size of a Reader's buffer.
	DefaultBufferSize = 4096
	// maxEmptyReads is how many consecutive reads returning no data and no
	// error a Reader tolerates before failing with io.ErrNoProgress.
	maxEmptyReads = 100
)

var (
	// ErrShortLength reports a messageLength shorter than the header it
	// includes: sofh's.
	ErrShortLength = sofh.ErrShortLength
	// ErrLongLength reports a messageLength beyond the limit: sofh's.
	ErrLongLength = sofh.ErrLongLength
	// ErrEncoding reports an encodingType other than 0xEB50.
	ErrEncoding = errors.New("lesofh: encodingType other than 0xEB50 (SBE little-endian)")
)

// ParseHeader decodes the framing header at the start of b, which must hold
// HeaderLen bytes, and returns the frame length, header included.
func ParseHeader(b []byte) (int, error) {
	_ = b[HeaderLen-1]
	n := binary.LittleEndian.Uint32(b)
	enc := sofh.Encoding(binary.LittleEndian.Uint16(b[4:]))
	if n < HeaderLen {
		return 0, fmt.Errorf("%w: %d", ErrShortLength, n)
	}
	if enc != Encoding {
		return 0, fmt.Errorf("%w: %#04x", ErrEncoding, uint16(enc))
	}
	if uint64(n) > math.MaxInt {
		return 0, fmt.Errorf("%w: %d bytes", ErrLongLength, n)
	}
	return int(n), nil
}

// PutHeader writes into b[:HeaderLen] the framing header of a message of
// msgLen bytes, header excluded.
func PutHeader(b []byte, msgLen int) error {
	_ = b[HeaderLen-1]
	if msgLen < 0 || uint64(msgLen) > math.MaxUint32-HeaderLen {
		return fmt.Errorf("%w: a message of %d bytes", ErrLongLength, msgLen)
	}
	binary.LittleEndian.PutUint32(b, uint32(HeaderLen+msgLen))
	binary.LittleEndian.PutUint16(b[4:], uint16(Encoding))
	return nil
}

// Reserve appends to dst room for a framing header, to be written with
// PutHeader once the message after it is appended, and returns dst.
func Reserve(dst []byte) []byte {
	return append(dst, make([]byte, HeaderLen)...)
}

// AppendFrame appends to dst the frame of msg: its header, then msg.
func AppendFrame(dst []byte, msg []byte) ([]byte, error) {
	n := len(dst)
	dst = slices.Grow(dst, HeaderLen+len(msg))[:n+HeaderLen]
	if err := PutHeader(dst[n:], len(msg)); err != nil {
		return dst[:n], err
	}
	return append(dst, msg...), nil
}

// Reader reads frames one at a time from a byte stream into a buffer it
// owns and reuses, however the stream splits or merges them. A Reader is not
// safe for concurrent use.
type Reader struct {
	r      io.Reader
	buf    []byte
	start  int   // first unread byte
	end    int   // end of buffered data
	err    error // read error, returned once buffered data is used up
	failed error // a malformed header: every later Next returns it
	max    int
	empty  int // consecutive reads returning no data and no error
}

// NewReader returns a Reader reading from r frames of at most maxLength
// bytes, header included, DefaultMaxLength if maxLength <= 0.
func NewReader(r io.Reader, maxLength int) *Reader {
	if maxLength <= 0 {
		maxLength = DefaultMaxLength
	}
	maxLength = max(maxLength, HeaderLen)
	return &Reader{r: r, max: maxLength, buf: make([]byte, min(DefaultBufferSize, maxLength))}
}

// Next returns the next frame's message, header excluded, valid until the
// next call to Next.
//
// A frame whose header is malformed (ErrShortLength, ErrEncoding) or too
// long (ErrLongLength) cannot be skipped: Next returns that error, and
// keeps returning it. Any other error is the stream's: io.EOF once it ends
// between frames, io.ErrUnexpectedEOF if it ends inside one, a read error
// otherwise; it too is returned by every later call. Deadlines are not
// timers here: a timeout ends the stream like any read error.
func (r *Reader) Next() ([]byte, error) {
	if r.failed != nil {
		return nil, r.failed
	}
	if err := r.fill(HeaderLen); err != nil {
		if r.end > r.start {
			return nil, unexpected(err)
		}
		return nil, err
	}
	n, err := ParseHeader(r.buf[r.start:r.end])
	if err == nil && n > r.max {
		err = fmt.Errorf("%w: %d bytes, %d at most", ErrLongLength, n, r.max)
	}
	if err != nil {
		r.failed = err
		return nil, err
	}
	if err := r.fill(n); err != nil {
		return nil, unexpected(err)
	}
	msg := r.buf[r.start+HeaderLen : r.start+n : r.start+n]
	r.start += n
	return msg, nil
}

// fill ensures n bytes are buffered from r.start, moving them to the front,
// or to a larger buffer for a frame the buffer cannot hold.
func (r *Reader) fill(n int) error {
	for r.end-r.start < n {
		if r.err != nil {
			return r.err
		}
		if n > len(r.buf)-r.start {
			if n > len(r.buf) {
				nb := make([]byte, min(max(n, 2*len(r.buf)), max(n, r.max)))
				r.end = copy(nb, r.buf[r.start:r.end])
				r.buf = nb
			} else {
				r.end = copy(r.buf, r.buf[r.start:r.end])
			}
			r.start = 0
		}
		m, err := r.r.Read(r.buf[r.end:])
		r.end += m
		switch {
		case err != nil:
			r.err = err
		case m == 0:
			r.empty++
			if r.empty >= maxEmptyReads {
				r.err = io.ErrNoProgress
			}
		default:
			r.empty = 0
		}
	}
	return nil
}

// unexpected turns io.EOF in the middle of a frame into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
