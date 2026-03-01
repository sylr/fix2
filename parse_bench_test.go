package fix2

import (
	"fmt"
	"strings"
	"testing"
)

// benchMsg builds a FIX message with the given number of extra fields beyond
// the required 8, 9, 35, and 10.
func benchMsg(extraFields int) []byte {
	var b strings.Builder
	b.WriteString("35=D\x01")
	b.WriteString("49=SENDER\x01")
	b.WriteString("56=TARGET\x01")
	b.WriteString("34=12345\x01")
	b.WriteString("52=20231015-10:00:00.000\x01")
	b.WriteString("11=order-12345\x01")
	b.WriteString("55=AAPL\x01")
	b.WriteString("54=1\x01")
	b.WriteString("38=100\x01")
	b.WriteString("40=2\x01")
	b.WriteString("44=150.50\x01")
	for i := range extraFields {
		fmt.Fprintf(&b, "5%03d=value%d\x01", i, i)
	}
	return buildMsg("FIX.4.4", b.String())
}

func BenchmarkParse(b *testing.B) {
	for _, extra := range []int{0, 10, 50} {
		raw := benchMsg(extra)
		name := fmt.Sprintf("fields=%d", extra+4) // +4 for 8,9,35,10
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				_, err := Parse(raw)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseWithChecksum(b *testing.B) {
	for _, extra := range []int{0, 10, 50} {
		raw := benchMsg(extra)
		name := fmt.Sprintf("fields=%d", extra+4)
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				_, err := ParseWithChecksum(raw)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFindByte(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		data := make([]byte, size)
		for i := range data {
			data[i] = 'a'
		}
		data[size-1] = 'X'
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.SetBytes(int64(size))
			for b.Loop() {
				findByte(data, 0, 'X')
			}
		})
	}
}

func BenchmarkChecksumBlock(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i)
		}
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.SetBytes(int64(size))
			for b.Loop() {
				checksumBlock(data)
			}
		})
	}
}
