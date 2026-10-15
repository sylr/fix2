package dictionary_test

import (
	"fmt"
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
)

func BenchmarkValidate(b *testing.B) {
	d, err := xmldict.Load(strings.NewReader(testDictXML))
	if err != nil {
		b.Fatal(err)
	}
	const header = "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01"
	const order = "11=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	for _, bc := range []struct{ name, body string }{
		{"NewOrderSingle", header + order},
		{"NewOrderSingle/NoAllocs", header + order + "78=2\x0179=ACC1\x0179=ACC2\x01"},
	} {
		msg, err := fix2.Parse(buildFIXMsg(bc.body))
		if err != nil {
			b.Fatal(err)
		}
		if err := d.Validate(&msg); err != nil {
			b.Fatalf("%s: %v", bc.name, err)
		}
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = d.Validate(&msg)
			}
		})
	}
}

// A QuickFIX FIX 4.4 NewOrderSingle with nested groups, validated as a
// session does by default.
func BenchmarkValidate_FIX44Groups(b *testing.B) {
	d, err := xmldict.LoadFile("../tests/benchmarks/spec/FIX44.xml")
	if err != nil {
		b.Fatal(err)
	}
	const body = "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01" +
		"11=ORD1\x01453=2\x01448=P1\x01447=D\x01452=1\x01802=2\x01523=S1\x01803=1\x01523=S2\x01803=2\x01448=P2\x01447=D\x01452=3\x01" +
		"78=2\x0179=ACC1\x0180=10\x0179=ACC2\x0180=20\x01" +
		"55=AAPL\x0154=1\x0160=20231001-12:00:00\x0138=30\x0140=1\x01"
	msg, err := fix2.Parse(buildFIXMsg(body))
	if err != nil {
		b.Fatal(err)
	}
	for _, bc := range []struct {
		name   string
		checks dictionary.Checks
	}{
		{"Validate", 0},
		{"CheckAll", dictionary.CheckAll},
	} {
		validate := func() error { return d.ValidateWith(&msg, bc.checks) }
		if bc.checks == 0 {
			validate = func() error { return d.Validate(&msg) }
		}
		if err := validate(); err != nil {
			b.Fatalf("%s: %v", bc.name, err)
		}
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = validate()
			}
		})
	}
}

// Small entries of groups of growing width, with an optional component they
// do not hold (see TestValidateWith_WideGroupCost).
func BenchmarkValidateWith_WideGroup(b *testing.B) {
	for _, width := range []int{64, 4096, 65536} {
		d := wideGroupDict(b, width)
		m := wideGroupMsg(b, d, width, 1000)
		b.Run(fmt.Sprintf("width=%d", width), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = d.ValidateWith(m, dictionary.CheckAll)
			}
		})
	}
}
