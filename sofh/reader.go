package sofh

import (
	"errors"
	"fmt"
	"io"
)

const (
	// DefaultMaxLength is the longest frame a Reader accepts by default,
	// header included.
	DefaultMaxLength = 1 << 20
	// DefaultBufferSize is the initial size of a Reader's buffer.
	DefaultBufferSize = 4096
	// maxEmptyReads is how many consecutive reads returning no data and no
	// error a Reader tolerates before failing with io.ErrNoProgress.
	maxEmptyReads = 100
)

// ReaderOptions configures a Reader. The zero value is valid.
type ReaderOptions struct {
	// MaxLength is the longest frame accepted, header included,
	// DefaultMaxLength if 0. A longer one is ErrLongLength. The Reader's
	// buffer never grows beyond it.
	MaxLength int
	// BufferSize is the initial buffer size, DefaultBufferSize if 0, and
	// MaxLength if that is less.
	BufferSize int
}

// Reader reads SOFH frames one at a time from a byte stream, such as a
// network connection, into a buffer it owns and reuses, however the stream
// splits or merges them. A Reader is not safe for concurrent use.
type Reader struct {
	r      io.Reader
	buf    []byte
	start  int   // first unread byte
	end    int   // end of buffered data
	err    error // read error to return once buffered data is used up
	failed error // a malformed header: every later Next returns it
	max    int
	empty  int // consecutive reads returning no data and no error
	moved  int // bytes fill moved in or to a new buffer, for tests

	// timeout came with the bytes completing the header, in this Next:
	// returned unless they complete the frame too, never read past.
	timeout error
}

// NewReader returns a Reader reading from r.
func NewReader(r io.Reader, opts ReaderOptions) *Reader {
	size := opts.BufferSize
	if size <= 0 {
		size = DefaultBufferSize
	}
	rd := &Reader{r: r}
	rd.SetMaxLength(opts.MaxLength)
	rd.buf = make([]byte, max(min(size, rd.max), HeaderLen))
	return rd
}

// SetMaxLength changes the longest frame accepted, header included, from
// the next frame on, DefaultMaxLength if n <= 0: a server reads a
// connection's first message with a small limit, then raises it once the
// peer is known. Lowering it keeps the buffer the Reader has, and what it
// buffered.
func (r *Reader) SetMaxLength(n int) {
	if n <= 0 {
		n = DefaultMaxLength
	}
	r.max = max(n, HeaderLen)
}

// Next returns the next frame's encoding and message, header excluded. The
// message is only valid until the next call to Next.
//
// A frame whose header is malformed (ErrShortLength) or too long
// (ErrLongLength) cannot be skipped: Next returns that error, and keeps
// returning it. Any other error is the reader's: io.EOF once the stream
// ends between frames, io.ErrUnexpectedEOF if it ends inside one. A read
// error reporting a timeout, as os.ErrDeadlineExceeded does, ends nothing:
// Next returns it, keeping what it read of the frame, and the next call
// resumes once the deadline is moved, a deadline serving as a timer. Any
// other read error is returned again by every later call.
func (r *Reader) Next() (Encoding, []byte, error) {
	if r.failed != nil {
		return 0, nil, r.failed
	}
	r.timeout = nil
	if err := r.fill(HeaderLen); err != nil {
		if r.end > r.start {
			return 0, nil, unexpected(err)
		}
		return 0, nil, err
	}
	n, enc, err := ParseHeader(r.buf[r.start:r.end])
	if err == nil && n > r.max {
		err = fmt.Errorf("%w: %d bytes, %d at most", ErrLongLength, n, r.max)
	}
	if err != nil {
		r.failed = err
		return enc, nil, err
	}
	if err := r.fill(n); err != nil {
		return enc, nil, unexpected(err)
	}
	msg := r.buf[r.start+HeaderLen : r.start+n]
	r.start += n
	return enc, msg, nil
}

// fill ensures n bytes are buffered from r.start, moving the buffered bytes
// to the front, or to a larger buffer for a frame the buffer cannot hold.
// It returns the read error if the stream ends first.
func (r *Reader) fill(n int) error {
	for r.end-r.start < n {
		if r.err != nil {
			return r.err
		}
		if r.timeout != nil {
			// The header came with a timeout: the body is not read for
			// now, a reader need not time out again before the caller
			// moves its deadline.
			return r.timeout
		}
		if n > len(r.buf)-r.start {
			r.moved += r.end - r.start
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
		case isTimeout(err):
			// Not kept beyond this Next: the stream goes on. What came
			// with it counts first: enough for the frame, it is returned;
			// enough for its header only, the timeout is returned without
			// reading again (timeout).
			r.empty = 0
			if r.end-r.start >= n {
				r.timeout = err
				return nil
			}
			return err
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

// isTimeout reports whether err reports a timeout: a deadline passed, the
// stream still usable once it is moved (net.Conn).
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	// Declared past the nil check: given to errors.As, t is allocated where
	// it is declared, which every read without an error would pay.
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// unexpected turns io.EOF in the middle of a frame into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
