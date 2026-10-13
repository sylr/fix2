package fix2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

// frameAll reads r with a Framer until a final error, returning the frames
// (copied), the bytes reported skipped and the final error.
func frameAll(t testing.TB, r io.Reader, opts FramerOptions) (frames [][]byte, skipped int, garbled []*GarbledError, err error) {
	t.Helper()
	f := NewFramer(r, opts)
	for range 100000 {
		frame, err := f.Next()
		var g *GarbledError
		switch {
		case errors.As(err, &g):
			skipped += g.Skipped
			garbled = append(garbled, g)
		case err != nil:
			return frames, skipped, garbled, err
		default:
			frames = append(frames, bytes.Clone(frame))
		}
	}
	t.Fatal("Framer did not stop")
	return
}

func TestFramer_FramesBackToBack(t *testing.T) {
	m1 := buildMsg("FIX.4.4", "35=0\x0149=A\x0156=B\x0134=1\x01")
	m2 := buildMsg("FIXT.1.1", "35=1\x01112=TEST\x01")
	m3 := buildMsg("FIX.4.2", "35=D\x0195=5\x0196=8=FIX\x0110=1\x01") // RawData holding a fake frame start and trailer
	stream := bytes.Join([][]byte{m1, m2, m3}, nil)
	for name, r := range map[string]io.Reader{
		"one read":      bytes.NewReader(stream),
		"byte by byte":  iotest.OneByteReader(bytes.NewReader(stream)),
		"data with EOF": iotest.DataErrReader(bytes.NewReader(stream)),
		"half reads":    iotest.HalfReader(bytes.NewReader(stream)),
	} {
		t.Run(name, func(t *testing.T) {
			frames, skipped, _, err := frameAll(t, r, FramerOptions{BufferSize: 16})
			if err != io.EOF {
				t.Fatalf("final error = %v, want io.EOF", err)
			}
			if skipped != 0 {
				t.Errorf("skipped %d bytes, want 0", skipped)
			}
			want := [][]byte{m1, m2, m3}
			if len(frames) != len(want) {
				t.Fatalf("got %d frames, want %d", len(frames), len(want))
			}
			for i := range want {
				if !bytes.Equal(frames[i], want[i]) {
					t.Errorf("frame %d = %q, want %q", i, frames[i], want[i])
				}
			}
		})
	}
}

func TestFramer_Garbled(t *testing.T) {
	good := buildMsg("FIX.4.4", "35=0\x0149=A\x01")
	for _, tc := range []struct {
		name    string
		before  string
		wantErr error
		skipped int
	}{
		{"garbage before a frame", "hello", ErrInvalidBeginTag, 5},
		{"partial frame start", "8=FI", ErrInvalidBeginTag, 4},
		{"BodyLength not second", "8=FIX.4.4\x0135=0\x01", ErrInvalidBodyLen, 15},
		{"BodyLength not a number", "8=FIX.4.4\x019=1x\x01", ErrInvalidBodyLen, 15},
		{"empty BodyLength", "8=FIX.4.4\x019=\x01", ErrInvalidBodyLen, 13},
		{"BodyLength too long", "8=FIX.4.4\x019=1234567890\x01", ErrInvalidBodyLen, 23},
		{"BeginString too long", "8=FIX.4.4.4.4.4.4.4.4\x01", ErrInvalidBeginTag, 22},
		{"BodyLength too large", "8=FIX.4.4\x019=99999\x01", ErrMessageTooLarge, 18},
		{"BeginString not FIX.x", "8=FIXjunk\x019=40\x01", ErrInvalidBeginTag, 15}, // its BodyLength would swallow the good frame
		{"BodyLength too short", "8=FIX.4.4\x019=5\x0135=0\x0149=A\x0110=000\x01", ErrBodyLenMismatch, 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := append([]byte(tc.before), good...)
			frames, skipped, garbled, err := frameAll(t, bytes.NewReader(stream), FramerOptions{MaxBodyLength: 1000})
			if err != io.EOF {
				t.Fatalf("final error = %v, want io.EOF", err)
			}
			if len(frames) != 1 || !bytes.Equal(frames[0], good) {
				t.Fatalf("frames = %q, want the good frame alone", frames)
			}
			if skipped != tc.skipped {
				t.Errorf("skipped %d bytes, want %d", skipped, tc.skipped)
			}
			if len(garbled) == 0 || !errors.Is(garbled[0], tc.wantErr) {
				t.Errorf("garbled = %v, want the first to wrap %v", garbled, tc.wantErr)
			}
		})
	}
}

// A garbled report tells what is wrong, never the bytes received.
func TestFramer_GarbledWithoutValues(t *testing.T) {
	for _, tc := range []struct {
		name, before, secret string
	}{
		{"BeginString not FIX", "8=FIXS3CRET\x019=40\x01", "S3CRET"},
		{"BodyLength too large", "8=FIX.4.4\x019=424242\x01", "4242"},
		{"no CheckSum after the body", "8=FIX.4.4\x019=97\x0135=0\x01" + strings.Repeat("x", 120), "97"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := append([]byte(tc.before), buildMsg("FIX.4.4", "35=0\x01")...)
			_, _, garbled, _ := frameAll(t, bytes.NewReader(stream), FramerOptions{MaxBodyLength: 5000})
			if len(garbled) == 0 {
				t.Fatal("nothing reported garbled")
			}
			for _, g := range garbled {
				if strings.Contains(g.Error(), tc.secret) {
					t.Errorf("garbled report %q shows the bytes received", g)
				}
			}
		})
	}
}

func TestFramer_EndOfStream(t *testing.T) {
	good := buildMsg("FIX.4.4", "35=0\x01")
	for _, tc := range []struct {
		name    string
		stream  string
		want    error
		skipped int
	}{
		{"empty", "", io.EOF, 0},
		{"after a frame", string(good), io.EOF, 0},
		{"inside the frame start", "8=F", io.ErrUnexpectedEOF, 0},
		{"inside the header", "8=FIX.4.4\x019=1", io.ErrUnexpectedEOF, 0},
		{"inside the body", string(good[:len(good)-3]), io.ErrUnexpectedEOF, 0},
		{"after garbage", "garbage!", io.EOF, 8},
		{"garbage before a partial frame start", "x8=F", io.ErrUnexpectedEOF, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, skipped, _, err := frameAll(t, strings.NewReader(tc.stream), FramerOptions{})
			if err != tc.want {
				t.Errorf("final error = %v, want %v", err, tc.want)
			}
			if skipped != tc.skipped {
				t.Errorf("skipped %d bytes, want %d", skipped, tc.skipped)
			}
		})
	}
}

// A long run of garbage is reported as it is skipped rather than buffered
// until a frame start shows up.
func TestFramer_LongGarbage(t *testing.T) {
	r := io.MultiReader(strings.NewReader(strings.Repeat("x", 3*garbageReport)), strings.NewReader("tail"))
	f := NewFramer(r, FramerOptions{BufferSize: 512})
	_, err := f.Next()
	var g *GarbledError
	if !errors.As(err, &g) || g.Skipped < garbageReport || g.Skipped > garbageReport+512 {
		t.Fatalf("first Next = %v, want a garbled report of about %d bytes", err, garbageReport)
	}
	if len(f.buf) != 512 {
		t.Errorf("buffer grew to %d bytes while skipping garbage", len(f.buf))
	}
}

func TestFramer_LargeFrameGrowsBuffer(t *testing.T) {
	big := buildMsg("FIX.4.4", "35=B\x0158="+strings.Repeat("y", 10000)+"\x01")
	frames, _, _, err := frameAll(t, bytes.NewReader(append(big, big...)), FramerOptions{BufferSize: 64})
	if err != io.EOF || len(frames) != 2 || !bytes.Equal(frames[1], big) {
		t.Fatalf("got %d frames, err %v", len(frames), err)
	}
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

func TestFramer_NoProgress(t *testing.T) {
	if _, err := NewFramer(emptyReader{}, FramerOptions{}).Next(); err != io.ErrNoProgress {
		t.Fatalf("Next = %v, want io.ErrNoProgress", err)
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

func TestFramer_DoesNotAllocate(t *testing.T) {
	f := NewFramer(&cycleReader{data: benchMsg(10)}, FramerOptions{})
	var m Message
	allocs := testing.AllocsPerRun(1000, func() {
		frame, err := f.Next()
		if err != nil {
			t.Fatal(err)
		}
		if err := ParseInto(&m, frame, nil); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("Next + ParseInto: %v allocs, want 0", allocs)
	}
}

func BenchmarkFramer(b *testing.B) {
	raw := benchMsg(10)
	f := NewFramer(&cycleReader{data: raw}, FramerOptions{BufferSize: 64 << 10})
	var m Message
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		frame, err := f.Next()
		if err != nil {
			b.Fatal(err)
		}
		if err := ParseInto(&m, frame, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// FuzzFramer checks every byte of the stream ends up in a frame or reported
// skipped, and every frame is delimited as FIX requires.
func FuzzFramer(f *testing.F) {
	good := buildMsg("FIX.4.4", "35=0\x0149=A\x01")
	f.Add(good)
	f.Add(append([]byte("junk8=FIX"), good...))
	f.Add([]byte("8=FIX.4.4\x019=5\x0135=0\x0110=000\x01"))
	f.Add(append(good[:20:20], good...))
	// A stream ending right after the BeginString.
	f.Add([]byte("8=FIX.4.4\x01"))
	// A BodyLength too short, in a frame whose CheckSum search runs into the
	// bytes searched for that of a frame whose BodyLength is too long: neither
	// is found.
	f.Add([]byte("8=FIX.4.4\x019=60\x0135=0\x01" + "8=FIX.4.4\x019=5\x0135=0\x0149=A\x01" + "58=" + strings.Repeat("x", 40) + "\x01"))
	f.Fuzz(func(t *testing.T, stream []byte) {
		// In one read, and a byte at a time.
		for _, r := range []io.Reader{bytes.NewReader(stream), iotest.OneByteReader(bytes.NewReader(stream))} {
			frames, skipped, _, err := frameAll(t, r, FramerOptions{BufferSize: 16, MaxBodyLength: 4096})
			total := skipped
			for _, fr := range frames {
				total += len(fr)
				if !bytes.HasPrefix(fr, frameStart) || len(fr) < trailerLen || !bytes.HasPrefix(fr[len(fr)-trailerLen:], []byte("10=")) || fr[len(fr)-1] != soh {
					t.Fatalf("malformed frame %q", fr)
				}
			}
			switch err {
			case io.EOF:
				if total != len(stream) {
					t.Fatalf("frames and skipped bytes cover %d of %d bytes", total, len(stream))
				}
			case io.ErrUnexpectedEOF:
				if total > len(stream) {
					t.Fatalf("frames and skipped bytes cover %d of %d bytes", total, len(stream))
				}
			default:
				t.Fatalf("final error = %v", err)
			}
		}
	})
}

// A BodyLength too long swallows the next frame, up to its CheckSum, as
// QuickFIX engines read it; the frame after is read again.
func TestFramer_BodyLengthTooLong(t *testing.T) {
	m1 := []byte("8=FIX.4.4\x019=40\x0135=D\x0134=4\x0149=TW\x0110=000\x01")
	m2 := buildMsg("FIX.4.4", "35=D\x0134=5\x0149=TW\x01")
	m3 := buildMsg("FIX.4.4", "35=0\x0134=6\x0149=TW\x01")
	stream := bytes.Join([][]byte{m1, m2, m3}, nil)
	frames, skipped, garbled, err := frameAll(t, bytes.NewReader(stream), FramerOptions{BufferSize: 32})
	if err != io.EOF {
		t.Fatalf("final error = %v", err)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0], m3) {
		t.Fatalf("frames = %q, want the third alone", frames)
	}
	if skipped != len(m1)+len(m2) || len(garbled) != 1 || !errors.Is(garbled[0], ErrBodyLenMismatch) {
		t.Errorf("skipped %d bytes in %v, want %d in one report", skipped, garbled, len(m1)+len(m2))
	}
}

// chunkReader returns at most n bytes per Read, as a peer trickling bytes.
type chunkReader struct {
	r io.Reader
	n int
}

func (c chunkReader) Read(p []byte) (int, error) { return c.r.Read(p[:min(len(p), c.n)]) }

// readSizes are the read sizes the CheckSum tests split streams into: a
// CheckSum field split at every point, a TCP segment, and the whole stream.
var readSizes = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 1460, 1 << 20}

// frameOutcomes reads stream with a Framer, n bytes at a time, and describes
// what each call to Next returned, up to the final error.
func frameOutcomes(t *testing.T, stream []byte, n int, opts FramerOptions) []string {
	t.Helper()
	f := NewFramer(chunkReader{bytes.NewReader(stream), n}, opts)
	var out []string
	for range 100000 {
		frame, err := f.Next()
		var g *GarbledError
		switch {
		case errors.As(err, &g):
			out = append(out, fmt.Sprintf("skipped %d: %v", g.Skipped, g.Err))
		case err != nil:
			return append(out, err.Error())
		default:
			out = append(out, fmt.Sprintf("frame %q", frame))
		}
	}
	t.Fatal("Framer did not stop")
	return nil
}

// badFrame is a frame whose BodyLength, 5, ends inside its body.
const badFrame = "8=FIX.4.4\x019=5\x0135=0\x01xx"

// TestFramer_SkipToCheckSum covers frames whose CheckSum is not where their
// BodyLength says: the Framer skips up to the first CheckSum field after the
// declared body, whatever the size of the reads.
func TestFramer_SkipToCheckSum(t *testing.T) {
	good := buildMsg("FIX.4.4", "35=0\x0149=A\x01")
	mismatch := func(skipped int) string {
		return fmt.Sprintf("skipped %d: %v: no CheckSum field where BodyLength says the body ends", skipped, ErrBodyLenMismatch)
	}
	for _, tc := range []struct {
		name    string
		garbled string // up to where the Framer resumes
		after   []byte
		want    []string // a report of garbled, the frame after and io.EOF if nil
	}{
		{name: "CheckSum after the body", garbled: badFrame + "\x0110=000\x01", after: good},
		{name: "CheckSum after filler", garbled: badFrame + strings.Repeat("a", 100) + "\x0110=000\x01", after: good},
		{name: "CheckSum after SOH-dense filler", garbled: badFrame + strings.Repeat("\x011", 50) + "\x0110=000\x01", after: good},
		{name: "16 bytes without SOH are no CheckSum value", garbled: badFrame + "\x0110=" + strings.Repeat("7", 16) + "\x0110=1\x01", after: good},
		{name: "15 bytes then SOH are", garbled: badFrame + "\x0110=" + strings.Repeat("7", 15) + "\x01", after: good},
		{name: "frame start before the CheckSum", garbled: badFrame + "8=FIX.4.4\x019=5\x0135=0\x0110=000\x01", after: good},
		{
			name:    "CheckSum value cut by the end of the stream",
			garbled: badFrame + "\x0110=12",
			// All but a tail that may begin a frame start, then the tail.
			want: []string{
				mismatch(len(badFrame+"\x0110=12") - (len(frameStart) - 1)),
				fmt.Sprintf("skipped %d: %v: bytes before the end of the stream", len(frameStart)-1, ErrInvalidBeginTag),
				io.EOF.Error(),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := append([]byte(tc.garbled), tc.after...)
			want := tc.want
			if want == nil {
				want = []string{mismatch(len(tc.garbled)), fmt.Sprintf("frame %q", tc.after), io.EOF.Error()}
			}
			for _, n := range readSizes {
				if got := frameOutcomes(t, stream, n, FramerOptions{BufferSize: 16}); !slices.Equal(got, want) {
					t.Errorf("%d-byte reads:\n got %q\nwant %q", n, got, want)
				}
			}
		})
	}
}

// TestFramer_SkipToCheckSumLimit covers a malformed frame without a CheckSum
// field within MaxBodyLength: the Framer resyncs on the next frame start,
// reporting the bytes up to it, even when it read past the limit, as far as
// the next frame's CheckSum field.
func TestFramer_SkipToCheckSumLimit(t *testing.T) {
	good := buildMsg("FIX.4.4", "35=0\x0149=A\x01")
	for _, filler := range []string{"a", "\x011", "\x0110=" + strings.Repeat("7", 16)} {
		garbled := badFrame + strings.Repeat(filler, 100/len(filler)+1)
		for _, size := range []int{16, DefaultFramerBufferSize} {
			for _, n := range readSizes {
				r := chunkReader{strings.NewReader(garbled + string(good)), n}
				frames, skipped, reports, err := frameAll(t, r, FramerOptions{BufferSize: size, MaxBodyLength: 64})
				if err != io.EOF || len(frames) != 1 || !bytes.Equal(frames[0], good) {
					t.Fatalf("filler %q, %d-byte buffer, %d-byte reads: frames %q, err %v; want the good frame, io.EOF", filler, size, n, frames, err)
				}
				if skipped != len(garbled) || !errors.Is(reports[0], ErrBodyLenMismatch) {
					t.Errorf("filler %q, %d-byte buffer, %d-byte reads: skipped %d bytes in %v, want %d from a BodyLength mismatch", filler, size, n, skipped, reports, len(garbled))
				}
			}
		}
	}
}

// garbledStream returns badFrame followed by n bytes of filler and, if
// checkSum, a CheckSum field.
func garbledStream(n int, filler string, checkSum bool) []byte {
	stream := []byte(badFrame + strings.Repeat(filler, n/len(filler)))
	if checkSum {
		stream = append(stream, "\x0110=000\x01"...)
	}
	return stream
}

// embeddedStream returns n bytes of malformed frames back to back, without
// CheckSum fields: each is skipped up to the next.
func embeddedStream(n int, _ string, _ bool) []byte {
	return bytes.Repeat([]byte(badFrame), n/len(badFrame))
}

// searched returns how many bytes a Framer searched for CheckSum fields
// while reading stream, n bytes at a time, to the end.
func searched(stream []byte, n int, opts FramerOptions) int {
	f := NewFramer(chunkReader{bytes.NewReader(stream), n}, opts)
	for {
		_, err := f.Next()
		var g *GarbledError
		if err != nil && !errors.As(err, &g) {
			return f.searched
		}
	}
}

// TestFramer_SkipToCheckSumIsLinear checks that skipping malformed frames
// searches each byte a bounded number of times, however the stream is split
// into reads: four times the input, about four times the work.
func TestFramer_SkipToCheckSumIsLinear(t *testing.T) {
	const n = 16 << 10
	for _, tc := range []struct {
		name     string
		stream   func(n int, filler string, checkSum bool) []byte
		filler   string
		checkSum bool
		read     int
	}{
		{"filler, byte by byte", garbledStream, "a", true, 1},
		{"SOH-dense filler, byte by byte", garbledStream, "\x011", true, 1},
		{"filler, by segment", garbledStream, "a", true, 1460},
		{"no CheckSum, byte by byte", garbledStream, "a", false, 1},
		{"SOH-dense, no CheckSum, byte by byte", garbledStream, "\x011", false, 1},
		{"malformed frames back to back, by segment", embeddedStream, "", false, 1460},
		{"malformed frames back to back, one read", embeddedStream, "", false, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small := searched(tc.stream(n, tc.filler, tc.checkSum), tc.read, FramerOptions{})
			large := searched(tc.stream(4*n, tc.filler, tc.checkSum), tc.read, FramerOptions{})
			if large > 5*small || large > 8*4*n {
				t.Errorf("searched %d bytes for %d bytes of input, %d for %d: want linear growth", small, n, large, 4*n)
			}
		})
	}
}

// TestFramer_FillIsLinear checks that the bytes a Framer moves in its
// buffer grow with the input, not with the input times MaxBodyLength:
// malformed frames back to back, each declaring the largest BodyLength,
// each resyncing a few bytes further and needing as many bytes buffered.
// The buffer stays within half as much again as that window.
func TestFramer_FillIsLinear(t *testing.T) {
	moved := func(t *testing.T, maxBody, read int) (moved, input int) {
		frame := fmt.Sprintf("8=FIX.4.4\x019=%d\x01xxxxxxxx", maxBody)
		stream := bytes.Repeat([]byte(frame), 8*maxBody/len(frame))
		f := NewFramer(chunkReader{bytes.NewReader(stream), read}, FramerOptions{MaxBodyLength: maxBody})
		for {
			_, err := f.Next()
			var g *GarbledError
			if err != nil && !errors.As(err, &g) {
				break
			}
		}
		if window := maxHeaderLen + 2*maxBody + trailerLen; len(f.buf) > window+window/2 {
			t.Errorf("MaxBodyLength %d: buffer of %d bytes, want at most %d", maxBody, len(f.buf), window+window/2)
		}
		return f.moved, len(stream)
	}
	const n = 16 << 10
	for _, read := range []int{1, 1460} {
		t.Run(fmt.Sprintf("%dB reads", read), func(t *testing.T) {
			small, smallInput := moved(t, n, read)
			large, largeInput := moved(t, 4*n, read)
			if large > 5*small || large > 4*largeInput {
				t.Errorf("moved %d bytes for %d bytes of input, %d for %d: want linear growth", small, smallInput, large, largeInput)
			}
		})
	}
}

// BenchmarkFramer_Garbled skips a malformed frame with 64 KiB before its
// CheckSum field, read a byte or a TCP segment at a time.
func BenchmarkFramer_Garbled(b *testing.B) {
	for _, filler := range []struct{ name, s string }{{"filler", "a"}, {"SOH-dense", "\x011"}} {
		stream := append(garbledStream(64<<10, filler.s, true), buildMsg("FIX.4.4", "35=0\x01")...)
		for _, n := range []int{1, 1460} {
			b.Run(fmt.Sprintf("%s/%dB", filler.name, n), func(b *testing.B) {
				b.SetBytes(int64(len(stream)))
				for b.Loop() {
					f := NewFramer(chunkReader{bytes.NewReader(stream), n}, FramerOptions{})
					if _, err := f.Next(); err == nil {
						b.Fatal("malformed frame read")
					}
					if _, err := f.Next(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
