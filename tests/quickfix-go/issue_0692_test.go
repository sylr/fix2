package quickfixgo

import (
	"bytes"
	"context"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/tests/dicts/fix40"
)

// https://github.com/quickfixgo/quickfix/issues/692
//
// The standard trailer was written in ascending tag order, Signature (89)
// before SignatureLength (93), whatever order they were set in: a reader
// cannot know how long the signature is before reading it. SignatureLength
// must come first, right before Signature, then CheckSum.
func TestIssue692_SignatureLengthBeforeSignature(t *testing.T) {
	for _, tc := range []struct {
		name string
		sign func(*fix40.NewOrderSingleBuilder)
	}{
		{"SetSignature", func(b *fix40.NewOrderSingleBuilder) { b.SetSignature(i692Sig) }},
		{"Set in the issue's order", func(b *fix40.NewOrderSingleBuilder) {
			b.SetInt(fix40.TagSignatureLength, len(i692Sig)).Set(fix40.TagSignature, i692Sig)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("Build", func(t *testing.T) { i692Check(t, i692Order(tc.sign).Build()) })
			t.Run("Send", func(t *testing.T) {
				s, _, p := loggedOn(t, func(c *session.Config) { c.LastMsgSeqNumProcessed = true })
				if _, err := s.Send(context.Background(), i692Order(tc.sign)); err != nil {
					t.Fatal(err)
				}
				i692Check(t, p.next())
			})
		})
	}
}

// A generated builder knows the standard trailer's order, but writes its
// fields in the order they were set: Signature set before SignatureLength
// comes first, and the message does not even parse.
func TestIssue692_TrailerOrderWhateverSetOrder(t *testing.T) {
	i692Check(t, i692Order(func(b *fix40.NewOrderSingleBuilder) {
		b.Set(fix40.TagSignature, i692Sig).SetInt(fix40.TagSignatureLength, len(i692Sig))
	}).Build())
}

// i692Sig is a signature: binary, it may contain SOH.
var i692Sig = []byte("VR\x01OEM")

// i692Order returns a NewOrderSingle signed by sign before its body is set.
func i692Order(sign func(*fix40.NewOrderSingleBuilder)) *fix40.NewOrderSingleBuilder {
	b := fix40.BuildNewOrderSingle(session.FIX40)
	sign(b)
	return b.SetClOrdID("1").SetHandlInst(fix40.HandlInstAutomatedExecutionOrderPrivateNoBrokerIntervention).
		SetSymbol("SYM").SetSide(fix40.SideBuy).SetOrderQty(100).SetOrdType(fix40.OrdTypeMarket)
}

// i692Check checks frame ends with SignatureLength, Signature and CheckSum,
// and parses.
func i692Check(t *testing.T, frame []byte) {
	t.Helper()
	trailer := append([]byte("\x0193=6\x0189="), i692Sig...)
	trailer = append(trailer, "\x0110="...)
	if i := bytes.LastIndex(frame, []byte("\x0110=")); i < 0 || !bytes.HasSuffix(frame[:i+4], trailer) {
		t.Fatalf("got %s, want it to end with %s", visible(frame), visible(trailer))
	}
	m, err := fix2.Parse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get(fix40.TagSignature); !bytes.Equal(got, i692Sig) {
		t.Errorf("Signature %q, want %q", got, i692Sig)
	}
}
