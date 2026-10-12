package fix2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

const (
	// DefaultFramerBufferSize is the initial size of a Framer's buffer.
	DefaultFramerBufferSize = 4096

	// maxBeginStringLen bounds the BeginString value a frame header may have
	// ("FIXT.1.1" is 8 bytes).
	maxBeginStringLen = 16
	// maxBodyLenDigits bounds the digits of BodyLength.
	maxBodyLenDigits = 9
	// trailerLen is the length of "10=NNN\x01".
	trailerLen = 7
	// maxEmptyReads is how many consecutive reads returning no data and no
	// error a Framer tolerates before failing with io.ErrNoProgress.
	maxEmptyReads = 100
	// garbageReport is how many skipped bytes a Framer accumulates before
	// reporting them, when no frame start is found.
	garbageReport = 4096
)

// frameStart is how every frame begins: tag 8 with a FIX or FIXT BeginString.
var frameStart = []byte("8=FIX")

// GarbledError reports bytes a Framer skipped because they did not form a
// frame: garbage before a frame, or a frame whose header or trailer is
// malformed. The Framer stays usable: the next call to Next resumes after
// the skipped bytes. FIX requires garbled messages to be ignored without
// changing sequence numbers.
type GarbledError struct {
	Skipped int   // bytes discarded
	Err     error // why, wrapping a fix2 sentinel such as ErrInvalidBodyLen
}

func (e *GarbledError) Error() string {
	return fmt.Sprintf("fix2: garbled input, %d bytes skipped: %v", e.Skipped, e.Err)
}

func (e *GarbledError) Unwrap() error { return e.Err }

// FramerOptions configures a Framer. The zero value is valid.
type FramerOptions struct {
	// MaxBodyLength is the largest BodyLength accepted, MaxBodyLength if 0.
	// A larger one is garbled: the Framer never buffers more than this plus
	// the header and trailer.
	MaxBodyLength int
	// BufferSize is the initial buffer size, DefaultFramerBufferSize if 0.
	BufferSize int
}

// Framer reads FIX messages one at a time from a byte stream, such as a
// network connection, without reading byte by byte: it reads into a buffer
// it owns and reuses, finds each frame from its BeginString and BodyLength,
// and checks the frame ends with a CheckSum field where BodyLength says.
// The frame's fields, DATA fields included, are left to ParseInto, which also
// verifies the checksum. A Framer is not safe for concurrent use.
type Framer struct {
	r       io.Reader
	buf     []byte
	start   int   // first unread byte
	end     int   // end of buffered data
	err     error // read error to return once buffered data is used up
	maxBody int
	skipped int // garbage skipped and not reported yet
	empty   int // consecutive reads returning no data and no error

	// [checked, checkedEnd) are buffered bytes where skipToCheckSum found no
	// CheckSum field start: it does not search them again for the malformed
	// frames found inside.
	checked, checkedEnd int
	searched            int // bytes skipToCheckSum searched, for tests
	moved               int // bytes fill moved in or to a new buffer, for tests
}

// NewFramer returns a Framer reading from r.
func NewFramer(r io.Reader, opts FramerOptions) *Framer {
	size := opts.BufferSize
	if size <= 0 {
		size = DefaultFramerBufferSize
	}
	maxBody := opts.MaxBodyLength
	if maxBody <= 0 {
		maxBody = MaxBodyLength
	}
	return &Framer{r: r, buf: make([]byte, size), maxBody: maxBody}
}

// SetMaxBodyLength changes the largest BodyLength accepted from the next
// frame on, MaxBodyLength if n <= 0: an acceptor reads a connection's first
// message with a small limit, then raises it once the peer is known.
func (f *Framer) SetMaxBodyLength(n int) {
	if n <= 0 {
		n = MaxBodyLength
	}
	f.maxBody = n
}

// Next returns the next frame: the raw bytes of one message, from "8=" to
// the SOH ending its CheckSum. The slice is only valid until the next call
// to Next. Bytes that do not form a frame are skipped and reported by a
// *GarbledError, after which Next may be called again. Any other error is
// the reader's (io.EOF once the stream ends between frames,
// io.ErrUnexpectedEOF if it ends inside one) and is final.
func (f *Framer) Next() ([]byte, error) {
	for {
		frame, garbled, err := f.next()
		if err != nil {
			if f.skipped > 0 {
				// Report the garbage before the read error ends the stream.
				return nil, f.garbled(fmt.Errorf("%w: bytes before the end of the stream", ErrInvalidBeginTag))
			}
			return nil, err
		}
		if garbled != nil {
			return nil, garbled
		}
		if frame != nil {
			return frame, nil
		}
	}
}

// next makes one step: it returns a frame, a garbled report, or neither when
// it only skipped garbage that is not worth reporting yet.
func (f *Framer) next() (frame []byte, garbled *GarbledError, err error) {
	// Find a frame start.
	if err := f.fill(len(frameStart)); err != nil {
		// The stream ended: skip garbage up to a partial frame start, which
		// makes the end unexpected.
		rest := f.buf[f.start:f.end]
		for i := range rest {
			if bytes.HasPrefix(frameStart, rest[i:]) {
				f.skip(i)
				return nil, nil, unexpected(err)
			}
		}
		f.skip(len(rest))
		return nil, nil, err
	}
	if !bytes.HasPrefix(f.buf[f.start:f.end], frameStart) {
		i := bytes.Index(f.buf[f.start+1:f.end], frameStart)
		if i < 0 {
			// Keep the tail: it may be the beginning of a frame start.
			n := f.end - f.start - (len(frameStart) - 1)
			f.skip(n)
			if f.skipped >= garbageReport {
				return nil, f.garbled(fmt.Errorf("%w: no frame start in the bytes received", ErrInvalidBeginTag)), nil
			}
			return nil, nil, nil
		}
		f.skip(i + 1)
		return nil, f.garbled(fmt.Errorf("%w: bytes before the frame start", ErrInvalidBeginTag)), nil
	}
	if f.skipped > 0 {
		return nil, f.garbled(fmt.Errorf("%w: bytes before the frame start", ErrInvalidBeginTag)), nil
	}

	// Header: "8=" BeginString SOH "9=" BodyLength SOH.
	headerEnd, bodyLen, herr := f.header()
	if herr != nil {
		if errors.Is(herr, errNeedMore) {
			return nil, nil, unexpected(f.err)
		}
		f.resync()
		return nil, f.garbled(herr), nil
	}
	frameLen := headerEnd + bodyLen + trailerLen
	if err := f.fill(frameLen); err != nil {
		return nil, nil, unexpected(err)
	}
	if !isTrailer(f.buf[f.start+headerEnd+bodyLen : f.start+frameLen]) {
		f.skipToCheckSum(headerEnd + bodyLen - 1)
		return nil, f.garbled(fmt.Errorf("%w: no CheckSum field after the %d bytes BodyLength declares", ErrBodyLenMismatch, bodyLen)), nil
	}
	frame = f.buf[f.start : f.start+frameLen]
	f.start += frameLen
	return frame, nil, nil
}

// errNeedMore reports that the reader ended before a complete header.
var errNeedMore = errors.New("need more data")

// header parses the frame header at f.start, reading more as needed. It
// returns the header length and BodyLength.
func (f *Framer) header() (int, int, error) {
	// BeginString: "8=FIX" already buffered, then up to SOH.
	i := 2
	for {
		if f.start+i >= f.end && f.fill(i+1) != nil {
			return 0, 0, errNeedMore
		}
		c := f.buf[f.start+i]
		if c == soh {
			break
		}
		if i-2 >= maxBeginStringLen {
			return 0, 0, fmt.Errorf("%w: BeginString longer than %d bytes", ErrInvalidBeginTag, maxBeginStringLen)
		}
		i++
	}
	if begin := f.buf[f.start+2 : f.start+i]; !bytes.HasPrefix(begin, []byte("FIX.")) && !bytes.HasPrefix(begin, []byte("FIXT.")) {
		return 0, 0, fmt.Errorf("%w: BeginString %q is not FIX.x or FIXT.x", ErrInvalidBeginTag, begin)
	}
	i++ // past SOH
	if f.fill(i+2) != nil {
		return 0, 0, errNeedMore
	}
	if f.buf[f.start+i] != '9' || f.buf[f.start+i+1] != '=' {
		return 0, 0, fmt.Errorf("%w: BodyLength is not the second field", ErrInvalidBodyLen)
	}
	i += 2
	bodyLen, digits := 0, 0
	for {
		if f.start+i >= f.end && f.fill(i+1) != nil {
			return 0, 0, errNeedMore
		}
		c := f.buf[f.start+i]
		if c == soh {
			break
		}
		if !isDigit(c) || digits == maxBodyLenDigits {
			return 0, 0, fmt.Errorf("%w: BodyLength is not a number of at most %d digits", ErrInvalidBodyLen, maxBodyLenDigits)
		}
		bodyLen = bodyLen*10 + int(c-'0')
		digits++
		i++
	}
	if digits == 0 {
		return 0, 0, fmt.Errorf("%w: empty BodyLength", ErrInvalidBodyLen)
	}
	if bodyLen > f.maxBody {
		return 0, 0, fmt.Errorf("%w: BodyLength %d exceeds %d", ErrMessageTooLarge, bodyLen, f.maxBody)
	}
	return i + 1, bodyLen, nil
}

// fill ensures n bytes are buffered from f.start, moving and growing the
// buffer as needed. It returns the read error if the stream ends first.
//
// It moves the buffered bytes to the front only if they are no more than
// the bytes consumed before them, or if the buffer is half as large again
// as the largest window a frame needs: then those are at least half the
// window. It grows the buffer otherwise, or malformed frames resyncing a few
// bytes apart, each needing as large a window, would move it again and
// again.
func (f *Framer) fill(n int) error {
	for f.end-f.start < n {
		if f.err != nil {
			return f.err
		}
		if f.start+n > len(f.buf) {
			f.checked, f.checkedEnd = max(0, f.checked-f.start), max(0, f.checkedEnd-f.start)
			f.moved += f.end - f.start
			// A frame's header, body and trailer, and as many bytes again
			// searched for its CheckSum (see skipToCheckSum).
			window := maxHeaderLen + 2*f.maxBody + trailerLen
			bound := window + window/2
			if n > len(f.buf) || f.end-f.start > f.start && len(f.buf) < bound {
				nb := make([]byte, max(n, min(2*len(f.buf), bound)))
				f.end = copy(nb, f.buf[f.start:f.end])
				f.buf = nb
			} else {
				f.end = copy(f.buf, f.buf[f.start:f.end])
			}
			f.start = 0
		}
		m, err := f.r.Read(f.buf[f.end:])
		f.end += m
		switch {
		case err != nil:
			f.err = err
		case m == 0:
			f.empty++
			if f.empty >= maxEmptyReads {
				f.err = io.ErrNoProgress
			}
		default:
			f.empty = 0
		}
	}
	return nil
}

// isTrailer reports whether t, 7 bytes, is a CheckSum field "10=NNN\x01".
func isTrailer(t []byte) bool {
	return t[0] == '1' && t[1] == '0' && t[2] == '=' && isDigit(t[3]) && isDigit(t[4]) && isDigit(t[5]) && t[6] == soh
}

// checkSumField starts every CheckSum field, after the SOH ending the body.
var checkSumField = []byte("\x0110=")

// skipToCheckSum skips a frame whose CheckSum is not where its BodyLength
// says, up to the end of the first CheckSum field from offset from, whatever
// its value: a BodyLength too short ends at the frame's own CheckSum, one too
// long swallows the next frame, as QuickFIX engines read them. Without a
// CheckSum within MaxBodyLength more bytes, it resyncs on the next frame
// start instead.
//
// However the stream is split into reads, each byte is searched about once:
// the search resumes where the previous read ended it, and skips the bytes
// searched for a previous malformed frame, which this one starts inside.
func (f *Framer) skipToCheckSum(from int) {
	const maxValue = 16 // bytes searched for the SOH ending a CheckSum value
	limit := from + f.maxBody + trailerLen
	// No CheckSum field starts in [first, from), offsets from f.start, which
	// fill keeps valid.
	first := from
	eof := false
	for {
		buffered := f.end - f.start
		// The bytes past the limit are not searched, however many were read:
		// which CheckSum field ends the frame does not depend on the reads.
		view := min(buffered, limit)
		lo, hi := f.checked-f.start, f.checkedEnd-f.start
		stop, gap := view, false
		switch {
		case from >= lo && from < hi:
			from = hi // searched for a previous frame
		case from < lo && lo < hi && lo+len(checkSumField)-1 < view:
			stop, gap = lo+len(checkSumField)-1, true // up to the bytes searched before
		}
		i := -1
		if from < stop {
			f.searched += stop - from
			i = bytes.Index(f.buf[f.start+from:f.start+stop], checkSumField)
		}
		if i >= 0 {
			from += i
			value := from + len(checkSumField) // offset of the value
			rest := f.buf[f.start+value : f.start+view]
			f.searched += min(len(rest), maxValue)
			if j := bytes.IndexByte(rest[:min(len(rest), maxValue)], soh); j >= 0 {
				f.skip(value + j + 1)
				return
			}
			if len(rest) >= maxValue {
				from = value // a look-alike: search further
				continue
			}
			// The value is incomplete: search it again once read.
		} else if gap {
			from = hi // none before the bytes searched before either
			continue
		} else {
			from = max(from, stop-(len(checkSumField)-1)) // the tail may begin one
		}
		if first <= hi && from >= lo {
			first, from = max(0, min(first, lo)), max(from, hi) // none before f.start matters
		}
		f.checked, f.checkedEnd = f.start+first, f.start+from
		if eof || buffered >= limit {
			f.resync()
			return
		}
		if f.fill(buffered+1) != nil {
			eof = true
		}
	}
}

// resync skips the malformed frame at f.start up to the next frame start
// buffered, or all but a tail that may begin one. A frame start inside the
// skipped frame, as in a DATA field, is only searched for because the frame
// is malformed: a well-formed frame is skipped by its BodyLength.
func (f *Framer) resync() {
	if i := bytes.Index(f.buf[f.start+1:f.end], frameStart); i >= 0 {
		f.skip(i + 1)
		return
	}
	f.skip(max(1, f.end-f.start-(len(frameStart)-1)))
}

// skip discards n buffered bytes as garbage.
func (f *Framer) skip(n int) {
	f.start += n
	f.skipped += n
}

// garbled returns the report of the garbage skipped so far and resets it.
func (f *Framer) garbled(err error) *GarbledError {
	g := &GarbledError{Skipped: f.skipped, Err: err}
	f.skipped = 0
	return g
}

// unexpected turns io.EOF in the middle of a frame into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
