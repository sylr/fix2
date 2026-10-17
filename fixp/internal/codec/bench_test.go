package codec

import (
	"testing"

	"github.com/sylr/fix2/sofh"
)

// FIXP's session messages: the heartbeat a sequenced flow sends most, a
// RetransmitRequest, an Establish with credentials; and the classification
// every inbound frame goes through.

var benchMessages = []struct {
	name string
	m    func() message
}{
	{"Sequence", func() message { return &Sequence{NextSeqNo: 1_000_000} }},
	{"RetransmitRequest", func() message {
		return &RetransmitRequest{SessionId: [16]uint8{6: 0x40, 8: 0x80}, Timestamp: 1_700_000_000_000_000_000, FromSeqNo: 100, Count: 50}
	}},
	{"Establish", func() message {
		return &Establish{Timestamp: 1_700_000_000_000_000_000, KeepaliveInterval: 1000, NextSeqNo: Some(uint64(42)), Credentials: []byte("user:alice,token:0123456789abcdef")}
	}},
}

func BenchmarkEncode(b *testing.B) {
	for _, bm := range benchMessages {
		b.Run(bm.name, func(b *testing.B) {
			m := bm.m()
			buf, err := m.AppendTo(nil)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(buf)))
			b.ReportAllocs()
			for b.Loop() {
				if buf, err = m.AppendTo(buf[:0]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	for _, bm := range benchMessages {
		b.Run(bm.name, func(b *testing.B) {
			buf, err := bm.m().AppendTo(nil)
			if err != nil {
				b.Fatal(err)
			}
			into := bm.m()
			b.SetBytes(int64(len(buf)))
			b.ReportAllocs()
			for b.Loop() {
				if err := into.Decode(buf); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkClassify(b *testing.B) {
	seq, _ := (&Sequence{NextSeqNo: 7}).AppendTo(nil)
	app := []byte{4, 0, 1, 0, 100, 0, 0, 0, 1, 2, 3, 4}
	for _, bm := range []struct {
		name string
		msg  []byte
	}{{"session", seq}, {"application", app}} {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, _, err := Classify(sofh.SBE10LittleEndian, bm.msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
