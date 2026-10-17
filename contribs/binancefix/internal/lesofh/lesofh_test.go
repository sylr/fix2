package lesofh

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

// The header of Binance's sample Logon: 209 bytes, header included, and
// the encoding 0xEB50 as the bytes 50 EB.
func TestHeader(t *testing.T) {
	sample := []byte{0xd1, 0x00, 0x00, 0x00, 0x50, 0xeb}
	n, err := ParseHeader(sample)
	if err != nil || n != 209 {
		t.Fatalf("ParseHeader = %d, %v", n, err)
	}
	var b [HeaderLen]byte
	if err := PutHeader(b[:], 203); err != nil || !bytes.Equal(b[:], sample) {
		t.Errorf("PutHeader = %x, %v", b, err)
	}
	for _, tc := range []struct {
		h    []byte
		want error
	}{
		{[]byte{5, 0, 0, 0, 0x50, 0xeb}, ErrShortLength},
		{[]byte{0, 0, 0, 0, 0x50, 0xeb}, ErrShortLength},
		{[]byte{26, 0, 0, 0, 0xeb, 0x50}, ErrEncoding}, // big-endian SBE
		{[]byte{26, 0, 0, 0, 0xe0, 0x5b}, ErrEncoding},
		{[]byte{0, 0, 0, 26, 0x50, 0xeb}, nil}, // read little-endian: 436207616
	} {
		if _, err := ParseHeader(tc.h); !errors.Is(err, tc.want) {
			t.Errorf("ParseHeader(%x) = %v, want %v", tc.h, err, tc.want)
		}
	}
	if err := PutHeader(b[:], -1); !errors.Is(err, ErrLongLength) {
		t.Errorf("PutHeader(-1) = %v", err)
	}
}

func frames(t *testing.T, msgs ...string) []byte {
	t.Helper()
	var b []byte
	for _, m := range msgs {
		var err error
		if b, err = AppendFrame(b, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func readAll(t *testing.T, r *Reader) ([]string, error) {
	t.Helper()
	var got []string
	for {
		m, err := r.Next()
		if err != nil {
			return got, err
		}
		got = append(got, string(m))
	}
}

// Frames are read whole however the stream splits or merges them.
func TestReader(t *testing.T) {
	msgs := []string{"", "a", "hello", string(bytes.Repeat([]byte("x"), 10000)), "end"}
	stream := frames(t, msgs...)
	for _, tc := range []struct {
		name string
		r    io.Reader
	}{
		{"coalesced", bytes.NewReader(stream)},
		{"byte by byte", iotest.OneByteReader(bytes.NewReader(stream))},
		{"half reads", iotest.HalfReader(bytes.NewReader(stream))},
		{"data with EOF", iotest.DataErrReader(bytes.NewReader(stream))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readAll(t, NewReader(tc.r, 0))
			if err != io.EOF {
				t.Errorf("end %v", err)
			}
			if len(got) != len(msgs) {
				t.Fatalf("%d messages, want %d", len(got), len(msgs))
			}
			for i := range msgs {
				if got[i] != msgs[i] {
					t.Errorf("message %d: %d bytes, want %d", i, len(got[i]), len(msgs[i]))
				}
			}
		})
	}
}

// A frame beyond the maximum, or of a malformed header, fails the Reader
// for good; a stream ending inside a frame is io.ErrUnexpectedEOF.
func TestReaderFailures(t *testing.T) {
	r := NewReader(bytes.NewReader(frames(t, "ok", "toolong")), HeaderLen+5)
	if m, err := r.Next(); err != nil || string(m) != "ok" {
		t.Fatalf("Next = %q, %v", m, err)
	}
	for range 2 {
		if _, err := r.Next(); !errors.Is(err, ErrLongLength) {
			t.Errorf("Next = %v, want ErrLongLength", err)
		}
	}
	bad := append(frames(t, "ok"), 26, 0, 0, 0, 0xeb, 0x50)
	r = NewReader(bytes.NewReader(bad), 0)
	r.Next()
	if _, err := r.Next(); !errors.Is(err, ErrEncoding) {
		t.Errorf("Next = %v, want ErrEncoding", err)
	}
	if _, err := r.Next(); !errors.Is(err, ErrEncoding) {
		t.Errorf("Next again = %v, want ErrEncoding", err)
	}
	cut := frames(t, "hello")
	for _, n := range []int{3, HeaderLen + 2} {
		r = NewReader(bytes.NewReader(cut[:n]), 0)
		if _, err := r.Next(); err != io.ErrUnexpectedEOF {
			t.Errorf("cut at %d: %v", n, err)
		}
	}
	r = NewReader(iotest.ErrReader(errors.New("boom")), 0)
	if _, err := r.Next(); err == nil || err.Error() != "boom" {
		t.Errorf("read error %v", err)
	}
	// A stream returning nothing, endlessly.
	r = NewReader(zeroReader{}, 0)
	if _, err := r.Next(); err != io.ErrNoProgress {
		t.Errorf("no progress: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

func FuzzReader(f *testing.F) {
	seed, _ := AppendFrame(nil, []byte("a"))
	seed, _ = AppendFrame(seed, []byte("bc"))
	f.Add(seed, 64)
	f.Add([]byte{0xd1, 0, 0, 0, 0x50, 0xeb}, 0)
	f.Fuzz(func(t *testing.T, b []byte, maxLength int) {
		r := NewReader(iotest.HalfReader(bytes.NewReader(b)), maxLength%(1<<20))
		for range 1000 {
			m, err := r.Next()
			if err != nil {
				return
			}
			if len(m)+HeaderLen > r.max {
				t.Fatalf("a frame of %d bytes, %d at most", len(m)+HeaderLen, r.max)
			}
		}
	})
}
