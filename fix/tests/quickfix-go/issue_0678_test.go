package quickfixgo

import (
	"bytes"
	"testing"

	"github.com/sylr/fix2/fix"
)

// https://github.com/quickfixgo/quickfix/issues/678
//
// Malformed data made the parser panic: a BodyLength overflowing an int
// sliced out of range, and a message cut short after BeginString indexed
// past its end. Both must be errors.
func TestIssue678_MalformedDataDoesNotPanic(t *testing.T) {
	for _, data := range []string{
		"8=\x019=119999999999999999999999999999999999999999999999999999999999970\x01",
		"8=0000000\x01",
	} {
		if _, err := fix.Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%q) succeeded", data)
		}
		if _, err := fix.ParseFromReader(bytes.NewReader([]byte(data))); err == nil {
			t.Errorf("ParseFromReader(%q) succeeded", data)
		}
		f := fix.NewFramer(bytes.NewReader([]byte(data)), fix.FramerOptions{})
		if frame, err := f.Next(); err == nil {
			t.Errorf("Framer.Next(%q) = %q", data, frame)
		}
	}
}
