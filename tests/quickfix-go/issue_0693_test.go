package quickfixgo

import (
	"bytes"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/tests/dicts/fix40"
)

// https://github.com/quickfixgo/quickfix/issues/693
//
// Only XmlData (213) was read by the length its XmlDataLen gives: the other
// binary DATA fields of the header and trailer, SecureData (91) and
// Signature (89), were read up to the first SOH in them. Every DATA field
// must be read by the LENGTH field before it, wherever it is.
func TestIssue693_DataFieldsInHeaderAndTrailerReadByLength(t *testing.T) {
	data := raw("8=FIX.4.4|35=D|34=2|49=A|52=20240101-00:00:00.000|56=B|90=11|91=se\x01c=10\x01ret|" +
		"212=12|213=<x>\x0110=0</x>|11=1|55=SYM|54=1|38=100|40=1|93=10|89=sig\x0110=000")
	check := func(t *testing.T, m *fix2.Message) {
		t.Helper()
		for tag, want := range map[int]string{91: "se\x01c=10\x01ret", 213: "<x>\x0110=0</x>", 89: "sig\x0110=000", 11: "1"} {
			if got := string(m.Get(tag)); got != want {
				t.Errorf("tag %d is %q, want %q", tag, got, want)
			}
		}
	}

	t.Run("Parse", func(t *testing.T) {
		m, err := fix2.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		check(t, &m)
	})
	t.Run("ParseFromReader", func(t *testing.T) {
		m, err := fix2.ParseFromReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		check(t, &m)
	})
	t.Run("Framer", func(t *testing.T) {
		f := fix2.NewFramer(bytes.NewReader(append(bytes.Clone(data), data...)), fix2.FramerOptions{})
		for range 2 {
			frame, err := f.Next()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frame, data) {
				t.Fatalf("framed %s, want %s", visible(frame), visible(data))
			}
		}
	})
}

// The same from a builder, then a session.
func TestIssue693_DataFieldsInHeaderAndTrailerBuiltAndReceived(t *testing.T) {
	secure, sig := []byte("se\x01c=10\x01ret"), []byte("sig\x0110=000")
	t.Run("Build", func(t *testing.T) {
		frame := fix40.BuildNewOrderSingle(session.FIX40).SetSignature(sig).SetSecureData(secure).
			SetClOrdID("1").SetSymbol("SYM").Build()
		m, err := fix2.Parse(frame)
		if err != nil {
			t.Fatalf("%s: %v", visible(frame), err)
		}
		if !bytes.Equal(m.Get(91), secure) || !bytes.Equal(m.Get(89), sig) {
			t.Errorf("SecureData %q and Signature %q, want %q and %q", m.Get(91), m.Get(89), secure, sig)
		}
	})
	t.Run("session", func(t *testing.T) {
		_, rec, p := acceptedPeer(t, nil)
		p.send("D", 0, "90", "11", "91", string(secure), "212", "12", "213", "<x>\x0110=0</x>",
			"11", "1", "55", "SYM", "54", "1", "38", "100", "40", "1",
			"93", "10", "89", string(sig))
		rec.wait(t, "the order", func() bool { return len(rec.apps) == 1 })
		if got := rec.appValues(91); got[0] != string(secure) {
			t.Errorf("SecureData %q, want %q", got[0], secure)
		}
		if got := rec.appValues(213); got[0] != "<x>\x0110=0</x>" {
			t.Errorf("XmlData %q, want %q", got[0], "<x>\x0110=0</x>")
		}
		if got := rec.appValues(89); got[0] != string(sig) {
			t.Errorf("Signature %q, want %q", got[0], sig)
		}
	})
}
