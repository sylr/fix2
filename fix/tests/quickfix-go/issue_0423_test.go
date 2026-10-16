package quickfixgo

import (
	"bytes"
	"testing"

	"github.com/sylr/fix2/fix"
)

// https://github.com/quickfixgo/quickfix/issues/423
//
// ParseMessage and ParseMessageWithDataDictionary panicked on an empty or
// malformed message, indexing the first of no fields. They must return an
// error.
func TestIssue423_EmptyOrMalformedMessageIsAnError(t *testing.T) {
	d := dict(t, "FIX44")
	for _, data := range []string{
		"",
		"\x01",
		"=",
		"8=",
		"8=FIX.4.4",
		"8=FIX.4.4\x01",
		"8=FIX.4.4\x019=",
		"8=FIX.4.4\x019=0\x01",
		"8=FIX.4.4\x019=0\x0110=000\x01",
		"8=FIX.4.4\x019=5\x0135=0\x01",
		"9=5\x0135=0\x0110=000\x01",
		"8=FIX.4.4\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01",
		// CheckSums right.
		string(fix.AppendTrailer([]byte("8=FIX.4.4\x019=0\x01"), 0)),
		string(fix.AppendTrailer([]byte("8=FIX.4.4\x019=12\x01=\x01=\x01=\x01=\x01=\x01=\x01"), 0)),
		string(fix.AppendTrailer([]byte("8=FIX.4.4\x019=15\x0149=A\x0156=B\x0134=1\x01"), 0)),
	} {
		if _, err := fix.Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%q) succeeded", data)
		}
		if _, err := fix.ParseAndValidate([]byte(data), d); err == nil {
			t.Errorf("ParseAndValidate(%q) succeeded", data)
		}
		var m fix.Message
		if err := fix.ParseInto(&m, []byte(data), nil); err == nil {
			t.Errorf("ParseInto(%q) succeeded", data)
		}
		if _, err := fix.ParseFromReader(bytes.NewReader([]byte(data))); err == nil {
			t.Errorf("ParseFromReader(%q) succeeded", data)
		}
		if _, err := fix.ParseFromReaderAndValidate(bytes.NewReader([]byte(data)), d); err == nil {
			t.Errorf("ParseFromReaderAndValidate(%q) succeeded", data)
		}
		f := fix.NewFramer(bytes.NewReader([]byte(data)), fix.FramerOptions{})
		if frame, err := f.Next(); err == nil {
			if _, err := fix.Parse(frame); err == nil {
				t.Errorf("Framer.Next(%q) = %q, which parses", data, frame)
			}
		}
	}
}
