package session

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sylr/fix2/fix"
)

// plainEncrypter seals nothing: SecureData holds the fields as they are,
// and one starting with '!' does not open.
type plainEncrypter struct{}

func (plainEncrypter) Method() string { return "7" }

func (plainEncrypter) Seal(dst, plain []byte) ([]byte, error) { return append(dst, plain...), nil }

func (plainEncrypter) Open(dst, sealed []byte) ([]byte, error) {
	if len(sealed) > 0 && sealed[0] == '!' {
		return dst, errors.New("does not open")
	}
	return append(dst, sealed...), nil
}

// FuzzEncrypterOpen feeds open a message received on a connection that
// agreed on encryption: header fields, clear after the session's own, then
// SecureData sealed. Whatever it holds, open does not panic; what it cannot
// make out is left as it was; what it opens has no SecureData, and, sealed
// again and opened, is the same frame.
func FuzzEncrypterOpen(f *testing.F) {
	for _, seed := range [][2]string{
		{"", "11=A\x01"},
		{"128=X\x01", "34=2\x01115=Y\x0111=A\x0195=3\x0196=a\x01b\x01"},
		{"", "34=3\x0111=A\x01"},
		{"11=C\x01", "11=A\x01"},
		{"", "!"},
		{"", "11=A"},
		{"", ""},
		{"", "212=3\x01213=<\x01>\x0111=A\x01"},
		{"93=1\x0189=s\x01", "11=A\x01"},
		{"", "90=1\x0191=x\x0111=A\x01"},
		{"", "11=A\x0110=000\x01"},
		{"", "52=20260101-00:00:00.000\x0111=A\x0134=2\x01"},
	} {
		f.Add(seed[0], seed[1])
	}
	s, err := New(Config{ID: SessionID{BeginString: FIX44, SenderCompID: "B", TargetCompID: "A"}, Handler: BaseHandler{}, Encrypter: plainEncrypter{}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, clear, sealed string) {
		body := "35=D\x0134=2\x0149=A\x0152=20260101-00:00:00.000\x0156=B\x01" + clear + "90=" + itoa(uint64(len(sealed))) + "\x0191=" + sealed + "\x01"
		frame := wrapFrame(nil, []byte(FIX44), []byte(body))
		var m fix.Message
		if s.cfg.parseOptions().ParseInto(&m, frame, s.dicts.parse) != nil || string(m.MsgType()) == "A" {
			return
		}
		c := &conn{s: s, encrypted: true}
		om, opened, why := c.open(&m, frame)
		if why != "" {
			if om != &m || !bytes.Equal(opened, frame) {
				t.Fatalf("%q, not made out, changed", frame)
			}
			return
		}
		if om.Get(tagSecureData) != nil || om.Get(tagSecureDataLen) != nil {
			t.Fatalf("%q opened as %q, SecureData left", frame, opened)
		}
		opened = bytes.Clone(opened)
		resealed, err := c.sealFrame(nil, opened)
		if err != nil {
			t.Fatalf("%q opened as %q, which does not seal: %v", frame, opened, err)
		}
		var again fix.Message
		if err := s.cfg.parseOptions().ParseInto(&again, resealed, s.dicts.parse); err != nil {
			t.Fatalf("%q sealed as %q, which does not parse: %v", opened, resealed, err)
		}
		if _, reopened, why := c.open(&again, resealed); why != "" || !bytes.Equal(reopened, opened) {
			t.Fatalf("%q sealed as %q, opened again as %q (%s)", opened, resealed, reopened, why)
		}
	})
}
