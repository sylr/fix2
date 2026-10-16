package sofh

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// negotiate is a FIXP Negotiate the interoperability spike sent
// (docs/fixp-evidence/silverflash-spike/1-standard-sofh.wire.log): SBE 1.0
// little-endian, 52 bytes header included.
const negotiate = "00000034eb50" +
	"19000100bc0a0000" + // SBE header: block 25, template 1, schema 2748, version 0
	"5376a12d0f5241bfaa84686dbd490de6" + // SessionId
	"684d129adc19df18" + // Timestamp
	"01" + // ClientFlow Idempotent
	"0b007370696b6500746f6b656e" // Credentials "spike\x00token"

func TestParseHeader(t *testing.T) {
	n, enc, err := ParseHeader(unhex(t, negotiate))
	if err != nil || n != 52 || enc != SBE10LittleEndian {
		t.Fatalf("ParseHeader = %d, %v, %v; want 52, SBE 1.0 little-endian", n, enc, err)
	}
	for _, short := range []uint32{0, 1, 5} {
		var h [HeaderLen]byte
		binary.BigEndian.PutUint32(h[:], short)
		if _, _, err := ParseHeader(h[:]); !errors.Is(err, ErrShortLength) {
			t.Errorf("length %d: %v, want ErrShortLength", short, err)
		}
	}
	if n, _, err := ParseHeader([]byte{0, 0, 0, 6, 0xEB, 0x50}); err != nil || n != HeaderLen {
		t.Errorf("empty message: %d, %v", n, err)
	}
}

func TestAppendFrame(t *testing.T) {
	want := unhex(t, negotiate)
	got, err := AppendFrame([]byte("x"), SBE10LittleEndian, want[HeaderLen:])
	if err != nil || !bytes.Equal(got[1:], want) || got[0] != 'x' {
		t.Fatalf("AppendFrame = %x, %v, want %x", got, err, want)
	}
	var h [HeaderLen]byte
	if err := PutHeader(h[:], TagValue, -1); !errors.Is(err, ErrLongLength) {
		t.Errorf("PutHeader of a negative length: %v, want ErrLongLength", err)
	}
	if strconv.IntSize == 32 {
		return // an int cannot hold the longest message
	}
	longest := uint64(math.MaxUint32 - HeaderLen)
	if err := PutHeader(h[:], TagValue, int(longest+1)); !errors.Is(err, ErrLongLength) {
		t.Errorf("PutHeader beyond 2^32-1: %v, want ErrLongLength", err)
	}
	if err := PutHeader(h[:], TagValue, int(longest)); err != nil || binary.BigEndian.Uint32(h[:]) != math.MaxUint32 {
		t.Errorf("PutHeader of the longest frame: %x, %v", h, err)
	}
}

// The message may share the destination's memory: the header must not
// overwrite it before it is copied.
func TestAppendFrame_Overlap(t *testing.T) {
	b := make([]byte, 3, 32)
	copy(b, "abc")
	got, err := AppendFrame(b[:0], TagValue, b)
	if want := frame(TagValue, "abc"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("AppendFrame = %x, %v, want %x", got, err, want)
	}
}

func TestEncodingString(t *testing.T) {
	for enc, want := range map[Encoding]string{
		SBE10BigEndian:    "SBE 1.0 big-endian",
		SBE10LittleEndian: "SBE 1.0 little-endian",
		GPB10:             "GPB 1.0",
		ASN1PER10:         "ASN.1 PER 1.0",
		ASN1BER10:         "ASN.1 BER 1.0",
		ASN1OER10:         "ASN.1 OER 1.0",
		TagValue:          "FIX tagvalue",
		FIXMLSchema10:     "FIXML schema 1.0",
		JSON:              "FIX JSON",
		BSON:              "FIX BSON",
		0x0042:            "private 0x0042",
		0xFA10:            "FIX FAST 0xFA10",
		0x1234:            "0x1234",
	} {
		if got := enc.String(); got != want {
			t.Errorf("%#04x: %q, want %q", uint16(enc), got, want)
		}
	}
}

// frame builds a frame of msg in encoding enc.
func frame(enc Encoding, msg string) []byte {
	b, err := AppendFrame(nil, enc, []byte(msg))
	if err != nil {
		panic(err)
	}
	return b
}

type got struct {
	enc Encoding
	msg string
}

// readAll reads r until an error, returning the frames read and the error.
func readAll(t testing.TB, r io.Reader, opts ReaderOptions) ([]got, error) {
	t.Helper()
	rd := NewReader(r, opts)
	var frames []got
	for range 100000 {
		enc, msg, err := rd.Next()
		if err != nil {
			return frames, err
		}
		frames = append(frames, got{enc, string(msg)})
	}
	t.Fatal("Reader did not stop")
	return nil, nil
}

// readers returns the ways a stream is read in tests: at once, a byte at a
// time, by halves, and with the last read returning data and io.EOF.
func readers(stream []byte) map[string]io.Reader {
	return map[string]io.Reader{
		"whole":   bytes.NewReader(stream),
		"bytes":   iotest.OneByteReader(bytes.NewReader(stream)),
		"halves":  iotest.HalfReader(bytes.NewReader(stream)),
		"dataerr": iotest.DataErrReader(bytes.NewReader(stream)),
	}
}

func TestReader_FramesBackToBack(t *testing.T) {
	want := []got{
		{SBE10LittleEndian, string(unhex(t, negotiate)[HeaderLen:])},
		{TagValue, "8=FIX.4.4\x019=5\x0135=0\x0110=000\x01"},
		{SBE10BigEndian, ""},
		{0x0042, strings.Repeat("x", 10000)},
	}
	var stream []byte
	for _, w := range want {
		stream = append(stream, frame(w.enc, w.msg)...)
	}
	for name, r := range readers(stream) {
		t.Run(name, func(t *testing.T) {
			frames, err := readAll(t, r, ReaderOptions{BufferSize: 16})
			if err != io.EOF {
				t.Fatalf("final error %v, want io.EOF", err)
			}
			if len(frames) != len(want) {
				t.Fatalf("%d frames, want %d", len(frames), len(want))
			}
			for i := range want {
				if frames[i] != want[i] {
					t.Errorf("frame %d: %v %q, want %v %q", i, frames[i].enc, frames[i].msg, want[i].enc, want[i].msg)
				}
			}
		})
	}
}

func TestReader_EndOfStream(t *testing.T) {
	full := frame(SBE10LittleEndian, "abcdef")
	for _, tc := range []struct {
		name   string
		stream []byte
		want   error
	}{
		{"empty", nil, io.EOF},
		{"between frames", full, io.EOF},
		{"inside a header", append(bytes.Clone(full), full[:3]...), io.ErrUnexpectedEOF},
		{"inside a message", append(bytes.Clone(full), full[:HeaderLen+2]...), io.ErrUnexpectedEOF},
		{"after a header", append(bytes.Clone(full), full[:HeaderLen]...), io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, r := range readers(tc.stream) {
				if _, err := readAll(t, r, ReaderOptions{}); err != tc.want {
					t.Errorf("%s: %v, want %v", name, err, tc.want)
				}
			}
		})
	}
}

func TestReader_ReaderError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(bytes.NewReader(frame(TagValue, "abc")), iotest.ErrReader(boom))
	frames, err := readAll(t, r, ReaderOptions{})
	if len(frames) != 1 || err != boom {
		t.Fatalf("%d frames, %v; want 1, boom", len(frames), err)
	}

	// Inside a frame, the reader's error is returned as is.
	r = io.MultiReader(bytes.NewReader(frame(TagValue, "abc")[:4]), iotest.ErrReader(boom))
	if frames, err := readAll(t, r, ReaderOptions{}); len(frames) != 0 || err != boom {
		t.Fatalf("%d frames, %v; want 0, boom", len(frames), err)
	}
}

func TestReader_ShortLength(t *testing.T) {
	stream := append(frame(TagValue, "ok"), 0, 0, 0, 5, 0xF0, 0)
	rd := NewReader(bytes.NewReader(append(stream, frame(TagValue, "unreachable")...)), ReaderOptions{})
	if _, msg, err := rd.Next(); err != nil || string(msg) != "ok" {
		t.Fatalf("first frame: %q, %v", msg, err)
	}
	for range 2 {
		if _, _, err := rd.Next(); !errors.Is(err, ErrShortLength) {
			t.Fatalf("Next = %v, want ErrShortLength, every time", err)
		}
	}
}

func TestReader_LongLength(t *testing.T) {
	big := frame(TagValue, strings.Repeat("x", 100))
	rd := NewReader(bytes.NewReader(append(bytes.Clone(big), big...)), ReaderOptions{MaxLength: 50})
	for range 2 {
		if _, _, err := rd.Next(); !errors.Is(err, ErrLongLength) {
			t.Fatalf("Next = %v, want ErrLongLength, every time", err)
		}
	}
	if len(rd.buf) > 50 {
		t.Errorf("buffer of %d bytes, beyond the 50-byte limit", len(rd.buf))
	}

	// Raised before the long frame, the limit lets it through.
	rd = NewReader(bytes.NewReader(append(frame(TagValue, "hi"), big...)), ReaderOptions{MaxLength: 50, BufferSize: 16})
	if _, _, err := rd.Next(); err != nil {
		t.Fatal(err)
	}
	rd.SetMaxLength(len(big))
	if _, msg, err := rd.Next(); err != nil || len(msg) != 100 {
		t.Fatalf("Next = %d bytes, %v", len(msg), err)
	}
	if len(rd.buf) > len(big) {
		t.Errorf("buffer grew to %d, beyond the %d-byte limit", len(rd.buf), len(big))
	}
}

// Silverflash counts Message_Length without the header: its frames do not
// read as SOFH. Its Sequence (docs/fixp-evidence/silverflash-spike) says 22
// bytes and is 28; the 6 left over read as a header of length 0.
func TestReader_SilverflashFramesAreNotSOFH(t *testing.T) {
	seq := unhex(t, "00000016eb5008000800bc0a00000100000000000000000000000000")
	rd := NewReader(bytes.NewReader(seq), ReaderOptions{})
	if _, msg, err := rd.Next(); err != nil || len(msg) != 16 {
		t.Fatalf("first read: %d bytes, %v", len(msg), err)
	}
	if _, _, err := rd.Next(); !errors.Is(err, ErrShortLength) {
		t.Fatalf("second read: %v, want ErrShortLength", err)
	}
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

func TestReader_NoProgress(t *testing.T) {
	if _, _, err := NewReader(emptyReader{}, ReaderOptions{}).Next(); err != io.ErrNoProgress {
		t.Fatalf("Next = %v, want io.ErrNoProgress", err)
	}
}

// Frames split across reads are moved to the front of the buffer, each byte
// a bounded number of times.
func TestReader_FillIsLinear(t *testing.T) {
	var stream []byte
	for range 1000 {
		stream = append(stream, frame(SBE10LittleEndian, strings.Repeat("y", 37))...)
	}
	rd := NewReader(iotest.HalfReader(bytes.NewReader(stream)), ReaderOptions{BufferSize: 100})
	for {
		if _, _, err := rd.Next(); err != nil {
			break
		}
	}
	if rd.moved > len(stream) {
		t.Errorf("moved %d bytes for a %d-byte stream", rd.moved, len(stream))
	}
}

// cycleReader returns data over and over.
type cycleReader struct {
	data []byte
	off  int
}

func (r *cycleReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.data[r.off:])
		n += c
		r.off = (r.off + c) % len(r.data)
	}
	return n, nil
}

func TestReader_DoesNotAllocate(t *testing.T) {
	rd := NewReader(&cycleReader{data: unhex(t, negotiate)}, ReaderOptions{})
	allocs := testing.AllocsPerRun(1000, func() {
		if _, _, err := rd.Next(); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("Next: %v allocs, want 0", allocs)
	}
}

func BenchmarkReader(b *testing.B) {
	raw := unhex(b, negotiate)
	rd := NewReader(&cycleReader{data: raw}, ReaderOptions{BufferSize: 64 << 10})
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := rd.Next(); err != nil {
			b.Fatal(err)
		}
	}
}

// expectFrames splits stream as SOFH frames no longer than maxLength,
// decoding headers on its own, and returns the frames and the error that
// ends the stream.
func expectFrames(stream []byte, maxLength int) ([]got, error) {
	var frames []got
	for off := 0; ; {
		rest := stream[off:]
		switch {
		case len(rest) == 0:
			return frames, io.EOF
		case len(rest) < 6:
			return frames, io.ErrUnexpectedEOF
		}
		n := uint64(rest[0])<<24 | uint64(rest[1])<<16 | uint64(rest[2])<<8 | uint64(rest[3])
		switch {
		case n < 6:
			return frames, ErrShortLength
		case n > uint64(maxLength):
			return frames, ErrLongLength
		case n > uint64(len(rest)):
			return frames, io.ErrUnexpectedEOF
		}
		frames = append(frames, got{Encoding(uint16(rest[4])<<8 | uint16(rest[5])), string(rest[6:n])})
		off += int(n)
	}
}

// FuzzReader checks the Reader splits a stream into the frames its headers
// say, however the stream is split into reads, and ends it with the error
// they call for.
func FuzzReader(f *testing.F) {
	f.Add(unhex(f, negotiate))
	f.Add(append(frame(TagValue, "abc"), frame(SBE10BigEndian, "")...))
	f.Add([]byte{0, 0, 0, 2, 0xEB, 0x50})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xEB, 0x50})
	f.Add(frame(TagValue, strings.Repeat("z", 100))[:40])
	f.Add(unhex(f, "00000016eb5008000800bc0a00000100000000000000000000000000"))
	const maxLength = 64
	f.Fuzz(func(t *testing.T, stream []byte) {
		want, wantErr := expectFrames(stream, maxLength)
		for name, r := range readers(stream) {
			frames, err := readAll(t, r, ReaderOptions{BufferSize: 16, MaxLength: maxLength})
			if !slices.Equal(frames, want) {
				t.Fatalf("%s: frames %v, want %v", name, frames, want)
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("%s: final error %v, want %v", name, err, wantErr)
			}
		}
	})
}
