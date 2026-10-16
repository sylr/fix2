package fix

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

// buildMsg constructs a raw FIX message with a correct checksum.
// body is everything between the BodyLength SOH and the "10=" tag.
func buildMsg(beginStr, body string) []byte {
	// Header: 8=beginStr\x01 9=len(body)\x01
	// Body: body
	// Trailer: 10=xxx\x01
	header := fmt.Sprintf("8=%s\x019=%d\x01", beginStr, len(body))
	preTrailer := header + body

	var sum uint32
	for i := 0; i < len(preTrailer); i++ {
		sum += uint32(preTrailer[i])
	}
	cs := sum % 256
	return fmt.Appendf(nil, "%s10=%03d\x01", preTrailer, cs)
}

func TestParseValidNewOrderSingle(t *testing.T) {
	body := "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231015-10:00:00\x0111=order1\x0155=AAPL\x0154=1\x0138=100\x0140=2\x0144=150.50\x01"
	raw := buildMsg("FIX.4.4", body)

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if string(msg.BeginString()) != "FIX.4.4" {
		t.Errorf("BeginString = %q, want %q", msg.BeginString(), "FIX.4.4")
	}
	if string(msg.MsgType()) != "D" {
		t.Errorf("MsgType = %q, want %q", msg.MsgType(), "D")
	}
	if v := msg.Get(49); string(v) != "SENDER" {
		t.Errorf("SenderCompID = %q, want %q", v, "SENDER")
	}
	if v := msg.Get(55); string(v) != "AAPL" {
		t.Errorf("Symbol = %q, want %q", v, "AAPL")
	}
}

func TestParseValidFIXT(t *testing.T) {
	body := "35=A\x0149=S\x0156=T\x01"
	raw := buildMsg("FIXT.1.1", body)

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if string(msg.BeginString()) != "FIXT.1.1" {
		t.Errorf("BeginString = %q, want %q", msg.BeginString(), "FIXT.1.1")
	}
	if string(msg.MsgType()) != "A" {
		t.Errorf("MsgType = %q, want %q", msg.MsgType(), "A")
	}
}

func TestParseMinimalMessage(t *testing.T) {
	body := "35=0\x01"
	raw := buildMsg("FIX.4.0", body)

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if msg.Count() != 4 { // 8, 9, 35, 10
		t.Errorf("Count = %d, want 4", msg.Count())
	}
}

func TestParseTooShort(t *testing.T) {
	_, err := Parse([]byte("8=FIX\x01"))
	if err == nil {
		t.Fatal("expected error for short message")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ParseError, got %T", err)
	}
	if !errors.Is(err, ErrMessageTooShort) {
		t.Errorf("expected ErrMessageTooShort, got %v", err)
	}
}

func TestParseInvalidBeginTag(t *testing.T) {
	_, err := Parse([]byte("9=123\x018=FIX.4.4\x0135=A\x0110=000\x01"))
	if !errors.Is(err, ErrInvalidBeginTag) {
		t.Errorf("expected ErrInvalidBeginTag, got %v", err)
	}
}

func TestParseInvalidBeginStringPrefix(t *testing.T) {
	body := "35=A\x01"
	// Build with a non-FIX prefix.
	header := fmt.Sprintf("8=NOTFIX\x019=%d\x01", len(body))
	raw := []byte(header + body + "10=000\x01")
	_, err := Parse(raw)
	if !errors.Is(err, ErrInvalidBeginTag) {
		t.Errorf("expected ErrInvalidBeginTag, got %v", err)
	}
}

func TestParseMissingMsgType(t *testing.T) {
	// Body has tag 49 as third field instead of 35.
	body := "49=S\x0156=T\x01"
	raw := buildMsg("FIX.4.4", body)

	_, err := Parse(raw)
	if !errors.Is(err, ErrMissingMsgType) {
		t.Errorf("expected ErrMissingMsgType, got %v", err)
	}
}

func TestParseChecksumMismatch(t *testing.T) {
	body := "35=D\x0149=S\x0156=T\x01"
	raw := buildMsg("FIX.4.4", body)
	// Corrupt the checksum value.
	raw[len(raw)-4] = '0'
	raw[len(raw)-3] = '0'
	raw[len(raw)-2] = '0'

	_, err := Parse(raw)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got %v", err)
	}
}

func TestParseBodyLenMismatch(t *testing.T) {
	// Manually construct with wrong body length.
	raw := []byte("8=FIX.4.4\x019=999\x0135=A\x0110=000\x01")
	_, err := Parse(raw)
	if !errors.Is(err, ErrBodyLenMismatch) {
		t.Errorf("expected ErrBodyLenMismatch, got %v", err)
	}
}

func TestZeroCopy(t *testing.T) {
	body := "35=D\x0149=SENDER\x01"
	raw := buildMsg("FIX.4.4", body)

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	val := msg.Get(49)
	// Verify the value slice points into the original buffer.
	if &val[0] != &raw[findByte(raw, 0, 'S')] {
		t.Error("value is not a zero-copy slice into original data")
	}
}

func TestMessageGetMissing(t *testing.T) {
	body := "35=A\x01"
	raw := buildMsg("FIX.4.4", body)

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if v := msg.Get(9999); v != nil {
		t.Errorf("Get(9999) = %q, want nil", v)
	}
}

func TestFindByte(t *testing.T) {
	data := []byte("hello=world\x01extra")
	if got := findByte(data, 0, '='); got != 5 {
		t.Errorf("findByte(data, 0, '=') = %d, want 5", got)
	}
	if got := findByte(data, 6, '\x01'); got != 11 {
		t.Errorf("findByte(data, 6, SOH) = %d, want 11", got)
	}
	if got := findByte(data, 0, 'Z'); got != -1 {
		t.Errorf("findByte(data, 0, 'Z') = %d, want -1", got)
	}
	if got := findByte(data, len(data), '='); got != -1 {
		t.Errorf("findByte(data, len, '=') = %d, want -1", got)
	}
}

func TestFindByteLong(t *testing.T) {
	// Test with data longer than 16 bytes to exercise SIMD main loop + tail.
	data := make([]byte, 50)
	for i := range data {
		data[i] = 'a'
	}
	data[37] = 'X'

	if got := findByte(data, 0, 'X'); got != 37 {
		t.Errorf("findByte long = %d, want 37", got)
	}
	if got := findByte(data, 38, 'X'); got != -1 {
		t.Errorf("findByte long past = %d, want -1", got)
	}
}

func TestChecksumBlock(t *testing.T) {
	data := []byte{1, 2, 3, 4, 5}
	if got := checksumBlock(data); got != 15 {
		t.Errorf("checksumBlock = %d, want 15", got)
	}
	// Test empty.
	if got := checksumBlock(nil); got != 0 {
		t.Errorf("checksumBlock(nil) = %d, want 0", got)
	}
	// Test > 16 bytes.
	data = make([]byte, 35)
	var want uint32
	for i := range data {
		data[i] = byte(i)
		want += uint32(i)
	}
	if got := checksumBlock(data); got != want {
		t.Errorf("checksumBlock 35-byte = %d, want %d", got, want)
	}
}

// TestChecksumBlockMatchesGeneric checks the (possibly SIMD) checksumBlock
// against the scalar reference across chunk and accumulator-flush boundaries.
func TestChecksumBlockMatchesGeneric(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	inputs := map[string][]byte{
		"0xff":   bytes.Repeat([]byte{0xff}, 70000),
		"random": make([]byte, 70000),
	}
	for i := range inputs["random"] {
		inputs["random"][i] = byte(rng.Uint32())
	}
	lengths := []int{65535, 65536, 65537, 70000}
	for n := 0; n <= 4200; n++ {
		lengths = append(lengths, n)
	}
	for name, data := range inputs {
		for _, n := range lengths {
			// Odd offset to exercise unaligned loads.
			off := 1
			if n+off > len(data) {
				off = 0
			}
			b := data[off : off+n]
			if got, want := checksumBlock(b), checksumBlockGeneric(b); got != want {
				t.Fatalf("%s len=%d: checksumBlock = %d, want %d", name, n, got, want)
			}
		}
	}
}

func TestParseFromReader(t *testing.T) {
	body := "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x01"
	raw := buildMsg("FIX.4.4", body)
	raw = append(raw, raw...) // add a null byte to the end
	reader := bytes.NewReader(raw)

	for {
		msg, err := ParseFromReader(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ParseFromReader failed: %v", err)
		}

		// Compare against Parse.
		want, _ := Parse(raw)
		if msg.Count() != want.Count() {
			t.Fatalf("field count %d vs %d", msg.Count(), want.Count())
		}
		for i, f1 := range want.Fields() {
			f2 := msg.Fields()[i]
			if f1.Tag != f2.Tag || string(f1.Value) != string(f2.Value) {
				t.Errorf("field[%d]: %d=%q vs %d=%q", i, f1.Tag, f1.Value, f2.Tag, f2.Value)
			}
		}
	}
}

func TestParseFromReaderStream(t *testing.T) {
	// Two messages concatenated — ParseFromReader must consume exactly the first.
	msg1 := buildMsg("FIX.4.4", "35=A\x0149=S\x0156=T\x01")
	msg2 := buildMsg("FIX.4.4", "35=D\x0149=X\x0156=Y\x01")
	stream := bytes.NewReader(append(msg1, msg2...))

	m, err := ParseFromReader(stream)
	if err != nil {
		t.Fatalf("first ParseFromReader failed: %v", err)
	}
	if string(m.MsgType()) != "A" {
		t.Errorf("first msg MsgType = %q, want %q", m.MsgType(), "A")
	}

	// Second message should still be readable.
	m, err = ParseFromReader(stream)
	if err != nil {
		t.Fatalf("second ParseFromReader failed: %v", err)
	}
	if string(m.MsgType()) != "D" {
		t.Errorf("second msg MsgType = %q, want %q", m.MsgType(), "D")
	}
}

func TestParseFromReaderTruncated(t *testing.T) {
	raw := buildMsg("FIX.4.4", "35=A\x01")
	// Truncate the message mid-body.
	truncated := raw[:len(raw)-5]
	_, err := ParseFromReader(bytes.NewReader(truncated))
	if err == nil {
		t.Fatal("expected error for truncated message")
	}
}

func TestParseFromReaderEmpty(t *testing.T) {
	_, err := ParseFromReader(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected error for empty reader")
	}
}

func TestParseFromReaderOneByteReader(t *testing.T) {
	// Wrap in a one-byte-at-a-time reader to stress the byte-by-byte header logic.
	raw := buildMsg("FIX.4.4", "35=D\x0149=S\x01")
	r := iotest.OneByteReader(bytes.NewReader(raw))
	msg, err := ParseFromReader(r)
	if err != nil {
		t.Fatalf("ParseFromReader with OneByteReader failed: %v", err)
	}
	want, _ := Parse(raw)
	if msg.Count() != want.Count() {
		t.Fatalf("field count %d vs %d", msg.Count(), want.Count())
	}
}

func TestParseErrorConsistency(t *testing.T) {
	tests := []struct {
		name     string
		raw      []byte
		sentinel error
	}{
		{
			name:     "wrong tag at begin",
			raw:      []byte("9=123\x018=FIX.4.4\x0135=A\x0110=000\x01"),
			sentinel: ErrInvalidBeginTag,
		},
		{
			name: "invalid begin string prefix",
			raw: func() []byte {
				body := "35=A\x01"
				header := fmt.Sprintf("8=NOTFIX\x019=%d\x01", len(body))
				preTrailer := header + body
				var sum uint32
				for i := range len(preTrailer) {
					sum += uint32(preTrailer[i])
				}
				return fmt.Appendf(nil, "%s10=%03d\x01", preTrailer, sum%256)
			}(),
			sentinel: ErrInvalidBeginTag,
		},
		{
			name:     "invalid body length separator",
			raw:      []byte("8=FIX.4.4\x019X5\x0135=A\x0110=000\x01"),
			sentinel: ErrInvalidBodyLen,
		},
		{
			name:     "missing MsgType",
			raw:      buildMsg("FIX.4.4", "49=S\x0156=T\x01"),
			sentinel: ErrMissingMsgType,
		},
		{
			name: "wrong tag at checksum",
			raw: func() []byte {
				body := "35=A\x01"
				header := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", len(body))
				return []byte(header + body + "11=000\x01")
			}(),
			sentinel: ErrInvalidCheckSum,
		},
		{
			name: "checksum non-numeric",
			raw: func() []byte {
				body := "35=A\x01"
				header := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", len(body))
				return []byte(header + body + "10=ABC\x01")
			}(),
			sentinel: ErrInvalidCheckSum,
		},
		{
			name: "checksum out of range",
			raw: func() []byte {
				body := "35=A\x01"
				header := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", len(body))
				return []byte(header + body + "10=999\x01")
			}(),
			sentinel: ErrInvalidCheckSum,
		},
		{
			name: "checksum mismatch",
			raw: func() []byte {
				raw := buildMsg("FIX.4.4", "35=D\x0149=S\x0156=T\x01")
				raw[len(raw)-4] = '0'
				raw[len(raw)-3] = '0'
				raw[len(raw)-2] = '0'
				return raw
			}(),
			sentinel: ErrChecksumMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errParse := Parse(tt.raw)
			_, errReader := ParseFromReader(bytes.NewReader(tt.raw))

			if errParse == nil {
				t.Fatal("expected Parse to return error")
			}
			if errReader == nil {
				t.Fatal("expected ParseFromReader to return error")
			}

			if !errors.Is(errParse, tt.sentinel) {
				t.Errorf("Parse: expected %v, got %v", tt.sentinel, errParse)
			}
			if !errors.Is(errReader, tt.sentinel) {
				t.Errorf("ParseFromReader: expected %v, got %v", tt.sentinel, errReader)
			}

			if errParse.Error() != errReader.Error() {
				t.Errorf("error messages differ:\n  Parse:           %q\n  ParseFromReader: %q", errParse.Error(), errReader.Error())
			}
		})
	}
}

// TestParseErrorMessages pins the error text: a single "fix2:" prefix, the
// sentinel chain, then the detail.
func TestParseErrorMessages(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "wrong tag at begin",
			raw:  []byte("9=123\x018=FIX.4.4\x0135=A\x0110=000\x01"),
			want: `fix2: invalid BeginString (tag 8): invalid tag number: expected "8="`,
		},
		{
			name: "invalid begin string prefix",
			raw:  []byte("8=NOTFIX\x019=5\x0135=A\x0110=000\x01XXXXXX"),
			want: `fix2: invalid BeginString (tag 8): invalid tag number: expected "FIX." or "FIXT."`,
		},
		{
			name: "invalid body length separator",
			raw:  []byte("8=FIX.4.4\x019X5\x0135=A\x0110=000\x01"),
			want: "fix2: invalid BodyLength (tag 9): field separator not found where expected",
		},
		{
			name: "missing MsgType",
			raw:  buildMsg("FIX.4.4", "49=S\x0156=T\x01"),
			want: "fix2: MsgType (tag 35) is not the third field",
		},
		{
			name: "wrong tag at checksum",
			raw:  []byte("8=FIX.4.4\x019=5\x0135=A\x0111=000\x01XXXXXXXX"),
			want: `fix2: invalid CheckSum (tag 10): invalid tag number: expected "10=" got "11="`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.raw)
			if err == nil {
				t.Fatal("expected error")
			}
			if got := err.Error(); got != tt.want {
				t.Errorf("Parse error:\n  got:  %s\n  want: %s", got, tt.want)
			}
			if !errors.Is(err, ErrFix2) {
				t.Errorf("errors.Is(err, ErrFix2) = false")
			}
		})
	}
}

// TestParseTruncated checks that input ending mid-field returns an error
// instead of reading past the end of the buffer.
func TestParseTruncated(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		sentinel error
	}{
		{
			name:     "truncated body tag",
			raw:      "8=FIX.\x019=25\x0100=00000000000000000000\x010",
			sentinel: ErrInvalidTag,
		},
		{
			name:     "truncated body length tag",
			raw:      "8=FIX.4.4XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\x019",
			sentinel: ErrInvalidBodyLen,
		},
		{
			name:     "missing body length",
			raw:      "8=FIX.4.4XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\x01",
			sentinel: ErrInvalidBodyLen,
		},
		{
			name:     "missing trailer",
			raw:      "8=FIX.4.4\x019=22\x0135=0\x0149=XXXXXXXXXXXXX\x01",
			sentinel: ErrInvalidCheckSum,
		},
		{
			name:     "truncated trailer tag",
			raw:      "8=FIX.4.4\x019=22\x0135=0\x0149=XXXXXXXXXXXXX\x0110",
			sentinel: ErrInvalidCheckSum,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.raw)); !errors.Is(err, tt.sentinel) {
				t.Errorf("Parse: expected %v, got %v", tt.sentinel, err)
			}
			if _, err := ParseFromReader(strings.NewReader(tt.raw)); err == nil {
				t.Error("ParseFromReader: expected error")
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	// Seed with a valid message.
	body := "35=D\x0149=SENDER\x0156=TARGET\x01"
	f.Add(buildMsg("FIX.4.4", body))
	// Seed with minimal.
	f.Add(buildMsg("FIX.4.0", "35=0\x01"))
	// Seed with garbage.
	f.Add([]byte("not a fix message"))
	// Seed with a header longer than ParseFromReader reads.
	f.Add(buildMsg("FIX."+strings.Repeat("x", 64), "35=0\x01"))
	// Seed with a CheckSum too short, its trailer still 7 bytes long.
	f.Add([]byte("8=FIX.4.4\x019=5\x0135=0\x0110=1\x01ab"))
	// Seed with DATA fields (embedded SOH, read by length).
	f.Add(buildMsg("FIX.4.4", "35=A\x0195=6\x0196=A\x0155=X\x01212=3\x01213=\x01\x01\x01"))
	// Seed with headers, bodies and trailers malformed one field at a time.
	for _, raw := range []string{
		"8=FIX.4.4 9=5 35=0 10=000 ",                                // BeginString without SOH
		"8=FIX.4.4\x019=5 35=0 10=000 ",                             // BodyLength without SOH
		"8=FIX.4.4\x018=5\x0135=0\x0110=000\x01",                    // BeginString where BodyLength belongs
		"8=FIX.4.4\x01x=5\x0135=0\x0110=000\x01",                    // a tag that is not a number
		"8=FIX.4.4\x019=\x0135=0\x0110=000\x01",                     // BodyLength empty
		"8=FIX.4.4\x019=0000000000000000000x\x0135=0\x0110=000\x01", // BodyLength longer than the fast path reads, not a number
		"8=FIX.4.4\x01\x0135=0\x0149=A\x0110=000\x01",               // BodyLength missing
		"8=FIX.4.4\x019=12\x0135=0\x0158=abcd",                      // a body field without SOH
		"8=FIX.4.4\x019=5\x0135=0\x01100=000\x01",                   // a three-digit tag where CheckSum belongs
		"8=FIX.4.4\x019=5\x0135=0\x0110=000",                        // CheckSum without SOH
	} {
		f.Add([]byte(raw))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// The reader parsers and the Framer read the same bytes, malformed
		// headers and trailers included, from readers of any pace.
		for _, r := range []io.Reader{bytes.NewReader(data), iotest.OneByteReader(bytes.NewReader(data)), iotest.DataErrReader(bytes.NewReader(data))} {
			_, _ = ParseFromReader(r)
		}
		for _, r := range []io.Reader{iotest.HalfReader(bytes.NewReader(data)), iotest.TimeoutReader(bytes.NewReader(data))} {
			fr := NewFramer(r, FramerOptions{BufferSize: 16, MaxBodyLength: 256})
			for range 8 {
				var g *GarbledError
				if _, err := fr.Next(); errors.As(err, &g) {
					_ = g.Error()
				} else if err != nil {
					break
				}
			}
		}

		// Parser must not panic, and must judge the CheckSum as FIX defines
		// it, whichever way it sums.
		msg, err := Parse(data)
		checkChecksum(t, "Parse", data, err)
		_, rerr := ParseFromReader(bytes.NewReader(data))
		checkChecksum(t, "ParseFromReader", data, rerr)
		if err != nil {
			return
		}
		// A message Parse accepts, the reader parser reads alike, unless its
		// header is longer than the reader parser reads to find BodyLength.
		mr, err := ParseFromReader(iotest.OneByteReader(bytes.NewReader(data)))
		if errors.Is(err, ErrMessageTooLarge) {
			return
		}
		if err != nil || !slices.EqualFunc(msg.Fields(), mr.Fields(), fieldEqual) {
			t.Fatalf("ParseFromReader: %v, %v; Parse: %v", mr.Fields(), err, msg.Fields())
		}
		// If parse succeeded, basic invariants must hold.
		if msg.Count() < 4 {
			t.Errorf("parsed message has only %d fields, expected at least 4", msg.Count())
		}
		if msg.BeginString() == nil {
			t.Error("BeginString is nil on successful parse")
		}
		if msg.MsgType() == nil {
			t.Error("MsgType is nil on successful parse")
		}
	})
}

// withChecksum appends the CheckSum trailer to s, header and body.
func withChecksum(s string) []byte {
	var sum int
	for i := range len(s) {
		sum += int(s[i])
	}
	return fmt.Appendf(nil, "%s10=%03d\x01", s, sum%256)
}

// TestParseHugeBodyLength checks that BodyLength values that overflow, or are
// too large to allocate, are rejected without panicking.
func TestParseHugeBodyLength(t *testing.T) {
	maxInt := strconv.Itoa(math.MaxInt)

	tests := []struct {
		name       string
		raw        []byte
		parse      error // expected from Parse
		fromReader error // expected from ParseFromReader
	}{
		{
			name:       "wraps past 2^64",
			raw:        withChecksum("8=FIX.4.4\x019=18446744073709551621\x0135=0\x01"),
			parse:      ErrInvalidBodyLen,
			fromReader: ErrInvalidBodyLen,
		},
		{
			name:       "MaxInt",
			raw:        withChecksum("8=FIX.4.4\x019=" + maxInt + "\x0135=0\x01"),
			parse:      ErrBodyLenMismatch,
			fromReader: ErrMessageTooLarge,
		},
		{
			name:       "MaxBodyLength+1",
			raw:        withChecksum("8=FIX.4.4\x019=" + strconv.Itoa(MaxBodyLength+1) + "\x0135=0\x01"),
			parse:      ErrBodyLenMismatch,
			fromReader: ErrMessageTooLarge,
		},
		{
			name:       "MaxBodyLength, stream too short",
			raw:        withChecksum("8=FIX.4.4\x019=" + strconv.Itoa(MaxBodyLength) + "\x0135=0\x01"),
			parse:      ErrBodyLenMismatch,
			fromReader: ErrBodyLenMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.raw); !errors.Is(err, tt.parse) {
				t.Errorf("Parse: got %v, want %v", err, tt.parse)
			}
			if _, err := ParseFromReader(bytes.NewReader(tt.raw)); !errors.Is(err, tt.fromReader) {
				t.Errorf("ParseFromReader: got %v, want %v", err, tt.fromReader)
			}
		})
	}
}

// TestParseFromReaderHeaderLimit checks that a stream that never completes
// BeginString and BodyLength stops being read after maxHeaderLen bytes.
func TestParseFromReaderHeaderLimit(t *testing.T) {
	r := bytes.NewReader(bytes.Repeat([]byte("8"), 1<<20))
	_, err := ParseFromReader(r)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v, want ErrMessageTooLarge", err)
	}
	if read := int(r.Size()) - r.Len(); read > maxHeaderLen {
		t.Errorf("read %d bytes, want at most %d", read, maxHeaderLen)
	}
}

// TestParseDataFields checks that a DATA field announced by its LENGTH field
// is read as exactly that many bytes, even when they contain SOH or "tag=".
func TestParseDataFields(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  map[int]string // expected field values; "" key absent means not checked
		count int            // expected number of fields, 0 = not checked
		err   error
	}{
		{
			name:  "RawData with embedded SOH and tag=value",
			body:  "35=A\x0195=6\x0196=A\x0155=X\x01",
			want:  map[int]string{96: "A\x0155=X"},
			count: 6, // 8, 9, 35, 95, 96, 10: no separate 55
		},
		{
			name: "XmlData",
			body: "35=A\x01212=7\x01213=<a>\x01</>\x01",
			want: map[int]string{213: "<a>\x01</>"},
		},
		{
			name: "EncodedText",
			body: "35=A\x01354=3\x01355=\x01\x01\x01\x0158=t\x01",
			want: map[int]string{355: "\x01\x01\x01", 58: "t"},
		},
		{
			name: "EncodedSymbol",
			body: "35=A\x011359=2\x011360=\x01\x01\x01",
			want: map[int]string{1360: "\x01\x01"},
		},
		{
			name: "zero length",
			body: "35=A\x0195=0\x0196=\x0158=t\x01",
			want: map[int]string{96: "", 58: "t"},
		},
		{
			name: "SecureData",
			body: "35=A\x0190=3\x0191=\x01=\x01\x0158=t\x01",
			want: map[int]string{91: "\x01=\x01", 58: "t"},
		},
		{
			name: "Signature in the trailer, inside BodyLength",
			body: "35=A\x0158=t\x0193=2\x0189=\x01\x01\x01",
			want: map[int]string{89: "\x01\x01"},
		},
		{
			name: "DATA not right after its LENGTH",
			body: "35=A\x0195=6\x0158=t\x0196=A\x0155=X\x01",
			err:  ErrMalformedField,
		},
		{
			name: "DATA without its LENGTH",
			body: "35=A\x0196=AB\x01",
			err:  ErrMalformedField,
		},
		{
			name: "LENGTH at the end of the body",
			body: "35=A\x0195=2\x01",
			err:  ErrMalformedField,
		},
		{
			name: "MaxInt length",
			body: "35=A\x0195=9223372036854775807\x0196=A\x01",
			err:  ErrMalformedField,
		},
		{
			name: "declared length longer than the body",
			body: "35=A\x0195=99\x0196=A\x01",
			err:  ErrMalformedField,
		},
		{
			name: "declared length does not end at SOH",
			body: "35=A\x0195=3\x0196=ABCD\x01",
			err:  ErrMalformedField,
		},
		{
			name: "invalid length",
			body: "35=A\x0195=x\x0196=A\x01",
			err:  ErrMalformedField,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := buildMsg("FIX.4.4", tt.body)
			for _, p := range []struct {
				name  string
				parse func([]byte) (Message, error)
			}{
				{"Parse", Parse},
				{"ParseFromReader", func(b []byte) (Message, error) { return ParseFromReader(bytes.NewReader(b)) }},
			} {
				msg, err := p.parse(raw)
				if tt.err != nil {
					if !errors.Is(err, tt.err) {
						t.Errorf("%s: got %v, want %v", p.name, err, tt.err)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s: %v", p.name, err)
					continue
				}
				for tag, want := range tt.want {
					if got := msg.Get(tag); string(got) != want || got == nil {
						t.Errorf("%s: tag %d = %q, want %q", p.name, tag, got, want)
					}
				}
				if tt.count != 0 && msg.Count() != tt.count {
					t.Errorf("%s: %d fields, want %d", p.name, msg.Count(), tt.count)
				}
			}
		})
	}
}

// TestParseErrorsWithoutValues checks that parse errors tell what is wrong
// and where, never the values received: a malformed field may hold a
// credential, and the errors are logged.
func TestParseErrorsWithoutValues(t *testing.T) {
	const secret = "S3CRET"
	// A CheckSum other than the right one.
	badSum := buildMsg("FIX.4.4", "35=A\x0149=S\x01")
	sum := "199"
	if string(badSum[len(badSum)-4:len(badSum)-1]) == sum {
		sum = "198"
	}
	badSum = append(badSum[:len(badSum)-4:len(badSum)-4], sum+"\x01"...)
	for _, tc := range []struct {
		name   string
		raw    []byte
		err    error
		secret string // what the error must not show
	}{
		{"invalid length", buildMsg("FIX.4.4", "35=A\x0195="+secret+"\x0196="+secret+"\x01"), ErrMalformedField, "S3"},
		{"BeginString not FIX", buildMsg(secret, "35=A\x01"), ErrInvalidBeginTag, "S3"},
		{"no BeginString", []byte(secret + "\x019=15\x0135=A\x0149=SENDER\x0110=000\x01"), ErrInvalidBeginTag, "S3"},
		{"declared length", buildMsg("FIX.4.4", "35=A\x01554=42424242\x0195=42424242\x0196=x\x01"), ErrMalformedField, "4242"},
		{"BodyLength too large", []byte("8=FIX.4.4\x019=42424242\x0135=A\x0149=SENDER\x0110=000\x01"), ErrFix2, "4242"},
		{"CheckSum mismatch", badSum, ErrChecksumMismatch, sum},
		{"CheckSum too long", append(buildMsg("FIX.4.4", "35=A\x01")[:25:25], "4242\x01"...), ErrInvalidCheckSum, "4242"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range []struct {
				name  string
				parse func([]byte) (Message, error)
			}{
				{"Parse", Parse},
				{"ParseFromReader", func(b []byte) (Message, error) { return ParseFromReader(bytes.NewReader(b)) }},
			} {
				_, err := p.parse(tc.raw)
				if !errors.Is(err, tc.err) {
					t.Errorf("%s: got %v, want %v", p.name, err, tc.err)
				}
				if err != nil && strings.Contains(err.Error(), tc.secret) {
					t.Errorf("%s: error %q shows the value", p.name, err)
				}
			}
		})
	}
}

// TestParseDataFieldsRepeated checks repeated LENGTH/DATA pairs with different
// lengths (as in a repeating group), checksum coverage of DATA bytes, and
// error offsets from both parsers.
func TestParseDataFieldsRepeated(t *testing.T) {
	raw := buildMsg("FIX.4.4", "35=A\x0178=2\x0195=1\x0196=\x01\x0195=3\x0196=a\x01b\x01")
	for name, parse := range map[string]func([]byte) (Message, error){
		"Parse":           Parse,
		"ParseFromReader": func(b []byte) (Message, error) { return ParseFromReader(bytes.NewReader(b)) },
	} {
		msg, err := parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, f := range msg.Fields() {
			if f.Tag == 96 {
				got = append(got, string(f.Value))
			}
		}
		if want := []string{"\x01", "a\x01b"}; !slices.Equal(got, want) {
			t.Errorf("%s: RawData values = %q, want %q", name, got, want)
		}

		// Corrupting a byte inside a DATA value must break the checksum.
		bad := bytes.Clone(raw)
		i := bytes.Index(bad, []byte("a\x01b"))
		bad[i] = 'c'
		if _, err := parse(bad); !errors.Is(err, ErrChecksumMismatch) {
			t.Errorf("%s: corrupted DATA: got %v, want ErrChecksumMismatch", name, err)
		}
	}

	// The error offset is the start of the offending field in the message.
	bad := buildMsg("FIX.4.4", "35=A\x0158=t\x0196=AB\x01")
	want := bytes.Index(bad, []byte("96=AB"))
	for name, parse := range map[string]func([]byte) (Message, error){
		"Parse":           Parse,
		"ParseFromReader": func(b []byte) (Message, error) { return ParseFromReader(bytes.NewReader(b)) },
	} {
		_, err := parse(bad)
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Offset != want {
			t.Errorf("%s: got %v (offset %v), want offset %d", name, err, pe, want)
		}
	}
}

// TestParseDataFieldsTable runs every standard LENGTH/DATA pair, and the
// adjacency rules, through both parsers.
func TestParseDataFieldsTable(t *testing.T) {
	parsers := map[string]func([]byte) (Message, error){
		"Parse":           Parse,
		"ParseFromReader": func(b []byte) (Message, error) { return ParseFromReader(bytes.NewReader(b)) },
	}
	for _, p := range dataFields {
		body := fmt.Sprintf("35=A\x01%d=4\x01%d=a\x01=b\x0158=t\x01", p.length, p.data)
		for name, parse := range parsers {
			msg, err := parse(buildMsg("FIX.4.4", body))
			if err != nil {
				t.Errorf("%s: pair %d/%d: %v", name, p.length, p.data, err)
				continue
			}
			if got := string(msg.Get(p.data)); got != "a\x01=b" {
				t.Errorf("%s: pair %d/%d: value %q", name, p.length, p.data, got)
			}
		}
	}

	for _, tt := range []struct {
		name string
		body string
		err  error
		at   string // field the error offset must point at
	}{
		// EP105 EncodedRejectText: the payload must not leak 55=X.
		{name: "EncodedRejectText smuggling", body: "35=3\x011664=6\x011665=A\x0155=X\x01"},
		{name: "wrong partner", body: "35=A\x0195=2\x01213=ab\x01", err: ErrMalformedField, at: "213=ab"},
		{name: "consecutive LENGTH fields", body: "35=A\x0195=2\x0195=2\x0196=ab\x01", err: ErrMalformedField, at: "95=2\x0196"},
		{name: "missing partner at end of body", body: "35=A\x0158=t\x0195=2\x01", err: ErrMalformedField},
		// Tags the FIX repository and QuickFIX FIX 5.0 SP2 disagree on are
		// not treated as LENGTH/DATA (here: the official throttle group).
		{name: "excluded tags parse as ordinary fields", body: "35=A\x011618=2\x011619=D\x011619=8\x01"},
		{name: "excluded 1525/1527 parse as ordinary fields", body: "35=A\x011525=24\x011527=sub\x01"},
		{name: "excluded 1578/1579 parse as ordinary fields", body: "35=A\x011578=3\x011579=1\x01"},
		// Tags above the table and up to MaxInt are ordinary fields.
		{name: "tag just above the table", body: fmt.Sprintf("35=A\x01%d=1\x01%d=x\x01", maxDataTag+1, maxDataTag+1)},
		{name: "MaxInt tag", body: fmt.Sprintf("35=A\x01%d=x\x01", math.MaxInt)},
	} {
		raw := buildMsg("FIX.4.4", tt.body)
		for name, parse := range parsers {
			msg, err := parse(raw)
			if tt.err == nil {
				if err != nil {
					t.Errorf("%s: %s: %v", name, tt.name, err)
				} else if msg.Get(55) != nil {
					t.Errorf("%s: %s: tag 55 leaked out of a DATA payload", name, tt.name)
				}
				continue
			}
			var pe *ParseError
			if !errors.Is(err, tt.err) || !errors.As(err, &pe) {
				t.Errorf("%s: %s: got %v, want %v", name, tt.name, err, tt.err)
				continue
			}
			if tt.at != "" {
				if want := bytes.Index(raw, []byte(tt.at)); pe.Offset != want {
					t.Errorf("%s: %s: offset %d, want %d", name, tt.name, pe.Offset, want)
				}
			}
		}
	}
}

// TestParseBodyLengthBoundaries checks that BodyLength must match the body
// exactly, from the BodyLength SOH to the SOH before "10=", in both parsers.
func TestParseBodyLengthBoundaries(t *testing.T) {
	body := "35=0\x0149=SENDER\x0158=text\x01"
	header := func(n int) string { return fmt.Sprintf("8=FIX.4.4\x019=%d\x01", n) }
	for delta := -3; delta <= 3; delta++ {
		t.Run(fmt.Sprintf("delta=%d", delta), func(t *testing.T) {
			raw := withChecksum(header(len(body)+delta) + body)
			_, err := Parse(raw)
			_, rerr := ParseFromReader(bytes.NewReader(raw))
			if delta == 0 {
				if err != nil || rerr != nil {
					t.Fatalf("Parse = %v, ParseFromReader = %v", err, rerr)
				}
				return
			}
			if err == nil || rerr == nil {
				t.Errorf("Parse = %v, ParseFromReader = %v, want errors", err, rerr)
			}
		})
	}
	// The SOH of the last body field sits right at bodyEnd.
	raw := withChecksum("8=FIX.4.4\x019=4\x0135=0\x01")
	if _, err := Parse(raw); !errors.Is(err, ErrBodyLenMismatch) {
		t.Errorf("Parse = %v, want ErrBodyLenMismatch", err)
	}
	if _, err := ParseFromReader(bytes.NewReader(raw)); err == nil {
		t.Error("ParseFromReader: expected error")
	}
}

// TestParseBodyEndSentinels checks the exact error and offset both parsers
// return when the last body field, DATA or not, ends right at bodyEnd.
func TestParseBodyEndSentinels(t *testing.T) {
	tests := []struct {
		name   string
		raw    []byte
		err    error // nil: valid
		offset int
	}{
		{"field SOH at bodyEnd", withChecksum("8=FIX.4.4\x019=4\x0135=0\x01"), ErrBodyLenMismatch, 14},
		{"DATA SOH at bodyEnd", withChecksum("8=FIX.4.4\x019=15\x0135=0\x0195=2\x0196=ab\x01"), ErrMalformedField, 25},
		{"empty DATA ending the body", withChecksum("8=FIX.4.4\x019=14\x0135=0\x0195=0\x0196=\x01"), nil, 0},
		{"DATA ending the body", withChecksum("8=FIX.4.4\x019=16\x0135=0\x0195=2\x0196=ab\x01"), nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.raw)
			_, rerr := ParseFromReader(bytes.NewReader(tt.raw))
			for name, err := range map[string]error{"Parse": err, "ParseFromReader": rerr} {
				if tt.err == nil {
					if err != nil {
						t.Errorf("%s: %v", name, err)
					}
					continue
				}
				var pe *ParseError
				if !errors.Is(err, tt.err) || !errors.As(err, &pe) || pe.Offset != tt.offset {
					t.Errorf("%s = %v (offset %d), want %v at offset %d", name, err, offsetOf(err), tt.err, tt.offset)
				}
			}
		})
	}
}

func offsetOf(err error) int {
	var pe *ParseError
	if errors.As(err, &pe) {
		return pe.Offset
	}
	return -1
}
