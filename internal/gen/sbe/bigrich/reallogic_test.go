package bigrich

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"
)

// full returns an Order with every field, group and data set: the values
// of rich's full, which testdata/reallogic/RichCheck.java.txt encodes too.
func full() *Order {
	return &Order{
		Id:    0x0102,
		Code:  [4]byte{'A', 'B', 'C'},
		Color: ColorBlue,
		Level: Some(LevelHigh),
		Flags: FlagsUrgent | FlagsLast,
		Price: Price{Mantissa: Some(int64(-12345)), Exponent: -2},
		Stamp: Stamp{Time: 1_700_000_000_000_000_000},
		Tagged: Tagged{
			Kind:  TaggedKindB,
			Price: Price{Mantissa: Some(int64(42)), Exponent: 3},
			Bits:  TaggedBitsX | TaggedBitsY,
			Note:  [2]byte{'h', 'i'},
		},
		Triple:    [3]int32{-1, 0, math.MaxInt32},
		OptU8:     Some(uint8(254)),
		OptI16:    Some(int16(-300)),
		OptU32:    Some(uint32(0)),
		OptChar:   Some(byte('z')),
		OptFloat:  Some(float32(1.5)),
		OptDouble: Some(2.25),
		Legs: []Legs{
			{Leg: 1, Price: Price{Mantissa: Some(int64(100)), Exponent: -1},
				Fees: []Fees{{Amount: Some(int64(-5)), Memo: []byte("fee")}}, Note: []byte("first leg")},
			{Leg: 2, Price: Price{Exponent: -1},
				Fees: []Fees{{Memo: []byte{}}, {Amount: Some(int64(7)), Memo: []byte("0123456789")}}, Note: []byte{}},
		},
		Extras: []Extras{{Key: 9}, {Key: 10}},
		Text:   []byte("free text"),
		Late:   []byte("late"),
	}
}

// The message Real Logic's SBE 1.33.0 encodes from bigrich.xml decodes,
// and full encodes to the same bytes.
func TestRealLogic(t *testing.T) {
	f, err := os.Open("../testdata/reallogic/bigrich.hex")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	msgs := map[string][]byte{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, h, _ := strings.Cut(sc.Text(), " ")
		if msgs[name], err = hex.DecodeString(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	want, err := full().AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, msgs["order"]) {
		t.Fatalf("encoded\n%x\nReal Logic\n%x", want, msgs["order"])
	}
	var got Order
	if _, err := got.Decode(msgs["order"]); err != nil {
		t.Fatal(err)
	}
	if again, _ := got.AppendTo(nil); !bytes.Equal(again, want) || got.Legs[1].Fees[1].Amount != Some(int64(7)) || got.OptDouble != Some(2.25) {
		t.Errorf("decoded %+v", got)
	}
}
