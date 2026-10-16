package rich

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
)

// full returns an Order with every field, group and data set.
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

func encode(t testing.TB, m *Order) []byte {
	t.Helper()
	b, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// same reports whether two Orders hold the same values, an empty slice
// being a nil one.
func same(a, b *Order) bool {
	ea, eb := encodeNoErr(a), encodeNoErr(b)
	return bytes.Equal(ea, eb) && reflect.DeepEqual(normalize(a), normalize(b))
}

func encodeNoErr(m *Order) []byte {
	b, _ := m.AppendTo(nil)
	return b
}

func normalize(m *Order) Order {
	c := *m
	for _, s := range []*[]byte{&c.Text, &c.Late} {
		if len(*s) == 0 {
			*s = nil
		}
	}
	if len(c.Legs) == 0 {
		c.Legs = nil
	}
	if len(c.Extras) == 0 {
		c.Extras = nil
	}
	legs := make([]Legs, len(c.Legs))
	for i, l := range c.Legs {
		if len(l.Note) == 0 {
			l.Note = nil
		}
		fees := make([]Fees, len(l.Fees))
		for j, f := range l.Fees {
			if len(f.Memo) == 0 {
				f.Memo = nil
			}
			fees[j] = f
		}
		l.Fees = fees
		legs[i] = l
	}
	if c.Legs != nil {
		c.Legs = legs
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	want := full()
	b := encode(t, want)
	var got Order
	if err := got.Decode(b); err != nil {
		t.Fatal(err)
	}
	if !same(&got, want) {
		t.Fatalf("decoded\n%+v\nwant\n%+v", got, *want)
	}
	h, _, _ := ReadHeader(b)
	if h.BlockLength != 88 || h.TemplateId != 1 || h.SchemaId != 7 || h.Version != 2 || h.NumGroups != 2 || h.NumVarDataFields != 2 {
		t.Errorf("header %+v", h)
	}
	if got.Venue() != "XPAR" || got.Seven() != 7 || got.Unit() != TimeUnitSecond || got.Stamp.Unit() != TimeUnitNanosecond || got.CodeString() != "ABC" {
		t.Errorf("constants %q %d %v %v %q", got.Venue(), got.Seven(), got.Unit(), got.Stamp.Unit(), got.CodeString())
	}
	// Padding and the block's end are zero.
	body := b[HeaderSize:]
	if !bytes.Equal(body[10:16], make([]byte, 6)) || !bytes.Equal(body[81:88], make([]byte, 7)) {
		t.Errorf("padding %x %x", body[10:16], body[81:88])
	}
}

// Every optional null encodes as its null value, and decodes as null.
func TestNulls(t *testing.T) {
	m := &Order{Legs: []Legs{{Fees: []Fees{{}}}}}
	b := encode(t, m)
	body := b[HeaderSize:]
	for _, c := range []struct {
		name string
		off  int
		want []byte
	}{
		{"Level", 7, []byte{0xff, 0xff}},
		{"Price.Mantissa", 16, []byte{0, 0, 0, 0, 0, 0, 0, 0x80}},
		{"Tagged.Price.Mantissa", 33 + 2, []byte{0, 0, 0, 0, 0, 0, 0, 0x80}},
		{"OptU8", 61, []byte{0xff}},
		{"OptI16", 62, []byte{0, 0}},
		{"OptU32", 64, []byte{0xff, 0xff, 0xff, 0xff}},
		{"OptChar", 68, []byte{0}},
		{"OptDouble", 73, binary.LittleEndian.AppendUint64(nil, math.Float64bits(-1))},
	} {
		if got := body[c.off : c.off+len(c.want)]; !bytes.Equal(got, c.want) {
			t.Errorf("%s: %x, want %x", c.name, got, c.want)
		}
	}
	if f := math.Float32frombits(binary.LittleEndian.Uint32(body[69:])); f == f {
		t.Errorf("OptFloat: %v, want NaN", f)
	}
	var got Order
	if err := got.Decode(b); err != nil {
		t.Fatal(err)
	}
	if got.Level.Valid || got.Price.Mantissa.Valid || got.Tagged.Price.Mantissa.Valid || got.OptU8.Valid || got.OptI16.Valid ||
		got.OptU32.Valid || got.OptChar.Valid || got.OptFloat.Valid || got.OptDouble.Valid || got.Legs[0].Fees[0].Amount.Valid {
		t.Errorf("not null: %+v", got)
	}
}

// pieces encodes m, which must have no Extras nor Late, and returns its
// header, block, Legs group and Text data, apart.
func pieces(t *testing.T, m *Order) (header, block, legs, text []byte) {
	t.Helper()
	b := encode(t, m)
	header, block = b[:HeaderSize], b[HeaderSize:HeaderSize+88]
	rest := b[HeaderSize+88:]
	textLen := 2 + len(m.Text)
	legsLen := len(rest) - GroupSizeEncodingSize - textLen - 1
	legs, text = rest[:legsLen], rest[legsLen+GroupSizeEncodingSize:legsLen+GroupSizeEncodingSize+textLen]
	return
}

// Messages of the schema's earlier versions decode: what a version lacks is
// absent.
func TestEarlierVersions(t *testing.T) {
	m := full()
	m.Extras, m.Late = nil, nil
	header, block, legs, text := pieces(t, m)
	extras := encode(t, &Order{Extras: []Extras{{Key: 9}}})
	extrasGroup := extras[HeaderSize+88+GroupSizeEncodingSize : HeaderSize+88+2*GroupSizeEncodingSize+2]

	for _, c := range []struct {
		version, blockLen int
		groups            [][]byte
	}{
		{0, 69, [][]byte{legs}},
		{1, 73, [][]byte{legs, extrasGroup}},
	} {
		h := bytes.Clone(header)
		binary.LittleEndian.PutUint16(h[0:], uint16(c.blockLen))
		binary.LittleEndian.PutUint16(h[6:], uint16(c.version))
		msg := append(h, block[:c.blockLen]...)
		for _, gr := range c.groups {
			msg = append(msg, gr...)
		}
		msg = append(msg, text...)
		var got Order
		if err := got.Decode(msg); err != nil {
			t.Fatalf("version %d: %v", c.version, err)
		}
		if got.Id != m.Id || got.Code != m.Code || !reflect.DeepEqual(normalize(&Order{Legs: got.Legs}).Legs, normalize(&Order{Legs: m.Legs}).Legs) ||
			string(got.Text) != string(m.Text) || got.Late != nil || got.OptDouble.Valid {
			t.Errorf("version %d: %+v", c.version, got)
		}
		if c.version == 0 && (got.OptFloat.Valid || got.Tagged.Note != [2]byte{} || got.Extras != nil) {
			t.Errorf("version 0: Float %v, Note %q, Extras %v", got.OptFloat, got.Tagged.Note, got.Extras)
		}
		if c.version == 1 && (got.OptFloat != m.OptFloat || got.Tagged.Note != m.Tagged.Note || len(got.Extras) != 1 || got.Extras[0].Key != 9) {
			t.Errorf("version 1: Float %v, Note %q, Extras %v", got.OptFloat, got.Tagged.Note, got.Extras)
		}
		// Decoding again into the same struct clears what the version lacks.
		full := full()
		if err := full.Decode(msg); err != nil || full.Late != nil || full.OptDouble.Valid {
			t.Errorf("version %d into a full Order: %v, Late %q, OptDouble %v", c.version, err, full.Late, full.OptDouble)
		}
	}
	// A version promising a field its block lacks is malformed.
	h := bytes.Clone(header)
	binary.LittleEndian.PutUint16(h[0:], 69)
	msg := append(append(append(h, block[:69]...), legs...), text...)
	var got Order
	if err := got.Decode(msg); !errors.Is(err, ErrMalformed) {
		t.Errorf("version 2 in a version 0 block: %v", err)
	}
}

func TestBounds(t *testing.T) {
	m := full()
	m.Legs[0].Fees = nil
	if _, err := m.AppendTo(nil); !errors.Is(err, ErrEntries) {
		t.Errorf("no fee, at least one required: %v", err)
	}
	m = full()
	m.Legs[0].Fees = make([]Fees, 4)
	if _, err := m.AppendTo(nil); !errors.Is(err, ErrEntries) {
		t.Errorf("4 fees, at most 3: %v", err)
	}
	m = full()
	m.Legs[1].Fees[1].Memo = []byte("0123456789x")
	if _, err := m.AppendTo(nil); !errors.Is(err, ErrLength) {
		t.Errorf("an 11-byte memo, at most 10: %v", err)
	}

	// The same on decoding: a Fees count of 0, a memo length of 11.
	m = &Order{Legs: []Legs{{Fees: []Fees{{Memo: []byte("x")}}}}}
	b := encode(t, m)
	countAt := HeaderSize + 88 + GroupSizeEncodingSize + 2 + PriceSize + 1
	if b[countAt] != 1 {
		t.Fatalf("fees count at %d is %d", countAt, b[countAt])
	}
	zero := bytes.Clone(b)
	zero[countAt] = 0
	var got Order
	if err := got.Decode(zero); !errors.Is(err, ErrEntries) {
		t.Errorf("no fee on the wire: %v", err)
	}
	long := bytes.Clone(b)
	long[countAt+1+8] = 11
	if err := got.Decode(long); !errors.Is(err, ErrMalformed) {
		t.Errorf("an 11-byte memo on the wire: %v", err)
	}
}

// MaxEntries bounds the entries of a message across its groups.
func TestMaxEntries(t *testing.T) {
	m := &Order{Legs: make([]Legs, MaxEntries/2+1)}
	for i := range m.Legs {
		m.Legs[i].Fees = []Fees{{}}
	}
	b := encode(t, m)
	var got Order
	if err := got.Decode(b); !errors.Is(err, ErrEntries) {
		t.Errorf("%d entries: %v", 2*len(m.Legs), err)
	}
	m.Legs = m.Legs[:MaxEntries/2]
	if err := got.Decode(encode(t, m)); err != nil {
		t.Errorf("%d entries: %v", 2*len(m.Legs), err)
	}
}

func TestTruncation(t *testing.T) {
	b := encode(t, full())
	var got Order
	for n := range len(b) {
		if err := got.Decode(b[:n]); err == nil {
			t.Fatalf("decoded %d of %d bytes", n, len(b))
		}
	}
}

func TestEmptyMessage(t *testing.T) {
	b, err := (&Ping{}).AppendTo(nil)
	if err != nil || len(b) != HeaderSize {
		t.Fatalf("%x, %v", b, err)
	}
	var p Ping
	if err := p.Decode(b); err != nil {
		t.Fatal(err)
	}
}

func TestEnums(t *testing.T) {
	if !ColorBlue.Known() || Color('X').Known() || ColorBlue.String() != "Blue" || Color('X').String() != "Color(88)" {
		t.Errorf("Color: %v %v %q %q", ColorBlue.Known(), Color('X').Known(), ColorBlue, Color('X'))
	}
	if LevelHigh != 1000 || FlagsLast != 1<<7 || TaggedBitsY != 1<<15 {
		t.Errorf("values %d %d %d", LevelHigh, FlagsLast, TaggedBitsY)
	}
}

func TestDecodeDoesNotAllocate(t *testing.T) {
	b := encode(t, full())
	var got Order
	if err := got.Decode(b); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if err := got.Decode(b); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Errorf("%v allocs, want 0", allocs)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(encode(f, full()))
	f.Add(encode(f, &Order{}))
	f.Fuzz(func(t *testing.T, b []byte) {
		var m Order
		if m.Decode(b) != nil {
			return
		}
		enc, err := m.AppendTo(nil)
		if (len(m.Legs) == 65535 || len(m.Extras) == 65535) && errors.Is(err, ErrEntries) {
			return // a count of 65535 decodes but does not encode (docs/sbe.md)
		}
		if err != nil {
			t.Fatalf("decoded but does not encode: %v", err)
		}
		var again Order
		if err := again.Decode(enc); err != nil {
			t.Fatalf("its encoding does not decode: %v", err)
		}
		if !bytes.Equal(encodeNoErr(&again), enc) {
			t.Fatal("re-encodes differently")
		}
	})
}
