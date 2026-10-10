package fix2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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
			name: "wrong tag at begin",
			raw:  []byte("9=123\x018=FIX.4.4\x0135=A\x0110=000\x01"),
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
			raw:      "8=FIX.4.4\x019=21\x0135=0\x0149=XXXXXXXXXXXXX\x01",
			sentinel: ErrInvalidCheckSum,
		},
		{
			name:     "truncated trailer tag",
			raw:      "8=FIX.4.4\x019=21\x0135=0\x0149=XXXXXXXXXXXXX\x0110",
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

	f.Fuzz(func(t *testing.T, data []byte) {
		// Parser must not panic.
		msg, err := Parse(data)
		if err != nil {
			return
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
