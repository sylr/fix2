package wide

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// Where test's message has its fields: a 20-byte header, a 12-byte block,
// the Items dimension, one item with one part, then Name.
const (
	headerBlockLength = 0
	headerVersion     = 12
	seqAt             = HeaderSize
	itemsBlockLength  = HeaderSize + 12
	itemsCount        = itemsBlockLength + 8
	partsBlockLength  = itemsCount + 8 + 4
	partsCount        = partsBlockLength + 4
	tagLength         = partsCount + 4 + 4
	nameLength        = tagLength + 4 + 1
	messageLength     = nameLength + 4 + 1
)

func test() *Batch {
	return &Batch{
		Seq:   Some(uint64(7)),
		Late:  9,
		Items: []Items{{Qty: 1, Parts: []Parts{{Part: 2}}, Tag: []byte("t")}},
		Name:  []byte("n"),
	}
}

func encode(t testing.TB, m *Batch) []byte {
	t.Helper()
	b, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := encode(t, test())
	if len(b) != messageLength || b[tagLength] != 1 || b[nameLength] != 1 {
		t.Fatalf("%d bytes, want %d: %x", len(b), messageLength, b)
	}
	var got Batch
	if err := got.Decode(b); err != nil {
		t.Fatal(err)
	}
	if again := encode(t, &got); !bytes.Equal(again, b) || got.Seq != Some(uint64(7)) || got.Late != 9 {
		t.Fatalf("%+v re-encodes as %x", got, again)
	}
}

// An optional field of a required type: null is its primitive's.
func TestNullOfARequiredType(t *testing.T) {
	m := test()
	m.Seq = Optional[uint64]{}
	b := encode(t, m)
	if v := binary.LittleEndian.Uint64(b[seqAt:]); v != math.MaxUint64 {
		t.Fatalf("null Seq encoded as %#x", v)
	}
	var got Batch
	if err := got.Decode(b); err != nil || got.Seq.Valid {
		t.Fatalf("%v, Seq %+v", err, got.Seq)
	}
}

// Header and dimension fields wider than an int may be decode without
// wrapping it: an error, never a panic.
func TestWideFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    int
		width int
		value uint64
		want  error
	}{
		{"header blockLength, all ones", headerBlockLength, 8, math.MaxUint64, ErrShort},
		{"header blockLength, sign bit", headerBlockLength, 8, 1 << 63, ErrShort},
		{"header blockLength, beyond 32 bits", headerBlockLength, 8, 1<<32 + 12, ErrShort},
		{"header version, all ones", headerVersion, 8, math.MaxUint64, nil},
		{"Items blockLength, all ones", itemsBlockLength, 8, math.MaxUint64, ErrShort},
		{"Items count, all ones", itemsCount, 8, math.MaxUint64, ErrShort},
		{"Items count, sign bit", itemsCount, 8, 1 << 63, ErrShort},
		{"Items count, beyond 32 bits", itemsCount, 8, 1<<32 + 1, ErrShort},
		{"Parts blockLength, all ones", partsBlockLength, 4, math.MaxUint32, ErrShort},
		{"Parts count, all ones", partsCount, 4, math.MaxUint32, ErrShort},
		{"Parts count, sign bit", partsCount, 4, 1 << 31, ErrShort},
		{"Tag length, all ones", tagLength, 4, math.MaxUint32, ErrMalformed},
		{"Tag length, under its minimum", tagLength, 4, 0, ErrMalformed},
		{"Tag length, over its maximum", tagLength, 4, 9, ErrMalformed},
		{"Name length, under its minimum", nameLength, 4, 0, ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := encode(t, test())
			if tc.width == 8 {
				binary.LittleEndian.PutUint64(b[tc.at:], tc.value)
			} else {
				binary.LittleEndian.PutUint32(b[tc.at:], uint32(tc.value))
			}
			var got Batch
			if err := got.Decode(b); !errors.Is(err, tc.want) {
				t.Fatalf("Decode = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLengthBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Batch)
	}{
		{"empty Name", func(m *Batch) { m.Name = nil }},
		{"9-byte Name", func(m *Batch) { m.Name = []byte("123456789") }},
		{"empty Tag", func(m *Batch) { m.Items[0].Tag = []byte{} }},
	} {
		m := test()
		tc.edit(m)
		if b, err := m.AppendTo([]byte("x")); !errors.Is(err, ErrLength) || string(b) != "x" {
			t.Errorf("%s: %q, %v", tc.name, b, err)
		}
	}
	m := test()
	m.Name = []byte("12345678")
	encode(t, m)
}

func FuzzDecode(f *testing.F) {
	f.Add(encode(f, test()))
	f.Fuzz(func(t *testing.T, b []byte) {
		var m Batch
		if m.Decode(b) != nil {
			return
		}
		enc, err := m.AppendTo(nil)
		if err != nil {
			t.Fatalf("decoded but does not encode: %v", err)
		}
		var again Batch
		if err := again.Decode(enc); err != nil {
			t.Fatalf("its encoding does not decode: %v", err)
		}
		if b, _ := again.AppendTo(nil); !bytes.Equal(b, enc) {
			t.Fatal("re-encodes differently")
		}
	})
}
