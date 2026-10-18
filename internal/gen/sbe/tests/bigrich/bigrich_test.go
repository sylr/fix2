package bigrich

import (
	"bytes"
	"math"
	"testing"
)

// The big-endian schema writes every multi-byte value, header included,
// most significant byte first, and reads back what it wrote.
func TestBigEndian(t *testing.T) {
	m := &Order{
		Id:        0x0102,
		Level:     Some(LevelHigh),
		Price:     Price{Mantissa: Some(int64(-2)), Exponent: -1},
		Triple:    [3]int32{1, -1, math.MaxInt32},
		OptFloat:  Some(float32(1.5)),
		OptDouble: Some(-0.5),
		Legs:      []Legs{{Leg: 0x0304, Fees: []Fees{{Amount: Some(int64(9)), Memo: []byte("m")}}, Note: []byte("n")}},
		Text:      []byte("t"),
	}
	b, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[:HeaderSize], []byte{0, 88, 0, 1, 0, 7, 0, 2, 0, 2, 0, 2}) {
		t.Errorf("header %x", b[:HeaderSize])
	}
	body := b[HeaderSize:]
	for _, c := range []struct {
		name string
		off  int
		want []byte
	}{
		{"Id", 0, []byte{1, 2}},
		{"Level", 7, []byte{0x03, 0xe8}},
		{"Price.Mantissa", 16, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}},
		{"Triple", 49, []byte{0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff, 0x7f, 0xff, 0xff, 0xff}},
		{"OptFloat", 69, []byte{0x3f, 0xc0, 0, 0}},
		{"OptDouble", 73, []byte{0xbf, 0xe0, 0, 0, 0, 0, 0, 0}},
		{"Legs dimension", 88, []byte{0, 11, 0, 1, 0, 1, 0, 1}},
		{"Leg", 96, []byte{3, 4}},
	} {
		if got := body[c.off : c.off+len(c.want)]; !bytes.Equal(got, c.want) {
			t.Errorf("%s: %x, want %x", c.name, got, c.want)
		}
	}
	var got Order
	if _, err := got.Decode(b); err != nil {
		t.Fatal(err)
	}
	again, _ := got.AppendTo(nil)
	if !bytes.Equal(again, b) || got.Id != m.Id || got.Level != m.Level || got.Triple != m.Triple || got.OptDouble != m.OptDouble ||
		got.Legs[0].Leg != 0x0304 || string(got.Legs[0].Fees[0].Memo) != "m" {
		t.Errorf("decoded %+v", got)
	}
}
