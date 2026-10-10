package dictionary_test

import (
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
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
