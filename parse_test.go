package fix2

import (
	"errors"
	"fmt"
	"testing"
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
	return []byte(fmt.Sprintf("%s10=%03d\x01", preTrailer, cs))
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

func TestParseWithChecksumMatchesParse(t *testing.T) {
	msgs := [][]byte{
		buildMsg("FIX.4.4", "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231015-10:00:00\x0111=order1\x0155=AAPL\x0154=1\x0138=100\x0140=2\x0144=150.50\x01"),
		buildMsg("FIXT.1.1", "35=A\x0149=S\x0156=T\x01"),
		buildMsg("FIX.4.0", "35=0\x01"),
		buildMsg("FIX.4.4", "35=D\x0149=SENDER\x01"),
	}
	for i, raw := range msgs {
		m1, err1 := Parse(raw)
		m2, err2 := ParseWithChecksum(raw)

		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("msg[%d]: Parse err=%v, ParseWithChecksum err=%v", i, err1, err2)
		}
		if err1 != nil {
			continue
		}
		if m1.Count() != m2.Count() {
			t.Fatalf("msg[%d]: field count %d vs %d", i, m1.Count(), m2.Count())
		}
		for j, f1 := range m1.Fields() {
			f2 := m2.Fields()[j]
			if f1.Tag != f2.Tag {
				t.Errorf("msg[%d] field[%d]: tag %d vs %d", i, j, f1.Tag, f2.Tag)
			}
			if string(f1.Value) != string(f2.Value) {
				t.Errorf("msg[%d] field[%d]: value %q vs %q", i, j, f1.Value, f2.Value)
			}
		}
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
		// Both parsers must not panic.
		msg, err := Parse(data)
		msg2, err2 := ParseWithChecksum(data)

		// Both must agree on success/failure.
		if (err == nil) != (err2 == nil) {
			t.Fatalf("Parse err=%v, ParseWithChecksum err=%v", err, err2)
		}
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
		// Both must produce identical fields.
		if msg.Count() != msg2.Count() {
			t.Fatalf("field count %d vs %d", msg.Count(), msg2.Count())
		}
		for i, f1 := range msg.Fields() {
			f2 := msg2.Fields()[i]
			if f1.Tag != f2.Tag || string(f1.Value) != string(f2.Value) {
				t.Errorf("field[%d] mismatch: %d=%q vs %d=%q", i, f1.Tag, f1.Value, f2.Tag, f2.Value)
			}
		}
	})
}
