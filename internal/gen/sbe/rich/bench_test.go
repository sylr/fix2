package rich

import "testing"

// The richer schema's Order, every field, nested groups and data fields
// set (full): encoding into a reused buffer, decoding into a reused
// struct, neither allocating.

func BenchmarkOrderEncode(b *testing.B) {
	m := full()
	buf := encode(b, m)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	var err error
	for b.Loop() {
		if buf, err = m.AppendTo(buf[:0]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderDecode(b *testing.B) {
	buf := encode(b, full())
	var m Order
	if err := m.Decode(buf); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		if err := m.Decode(buf); err != nil {
			b.Fatal(err)
		}
	}
}
