package fix2

import (
	"bytes"
	"errors"
	"io"
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
	f.Fuzz(func(t *testing.T, stream []byte) {
		frames, skipped, _, err := frameAll(t, bytes.NewReader(stream), FramerOptions{BufferSize: 16, MaxBodyLength: 4096})
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
	})
}
