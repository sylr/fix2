package newer_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/sylr/fix2/internal/gen/sbe/tests/newer"
	"github.com/sylr/fix2/internal/gen/sbe/tests/older"
	"github.com/sylr/fix2/internal/gen/sbe/tests/oldersame"
	"github.com/sylr/fix2/internal/gen/sbe/tests/oldersameunfilled"
	"github.com/sylr/fix2/internal/gen/sbe/tests/olderunfilled"
)

// The decoders of version 1 under test: older's, strict, oldersame's
// (--assume-same-encodings), olderunfilled's (--tolerate-unfilled-counts) and
// oldersameunfilled's (both), seen through what version 1 knows.
type view struct {
	ID     uint32
	Lines  []line // Order's Legs or Report's Lines
	Text   string
	Remark string // Report's, since version 1
}

type line struct {
	N    uint16 // Leg or Line
	Note string
}

type decoder struct {
	name                                     string
	order, quote, report                     func([]byte) (view, int, error)
	errNewer, errEntries, errShort, errMalfd error
}

var (
	strict = decoder{"strict",
		func(b []byte) (view, int, error) {
			var m older.Order
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text)}
			for _, l := range m.Legs {
				v.Lines = append(v.Lines, line{l.Leg, string(l.Note)})
			}
			return v, n, err
		},
		func(b []byte) (view, int, error) {
			var m older.Quote
			n, err := m.Decode(b)
			return view{ID: m.Id}, n, err
		},
		func(b []byte) (view, int, error) {
			var m older.Report
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text), Remark: string(m.Remark)}
			for _, l := range m.Lines {
				v.Lines = append(v.Lines, line{N: l.Line})
			}
			return v, n, err
		},
		older.ErrNewer, older.ErrEntries, older.ErrShort, older.ErrMalformed}
	same = decoder{"assume-same-encodings",
		func(b []byte) (view, int, error) {
			var m oldersame.Order
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text)}
			for _, l := range m.Legs {
				v.Lines = append(v.Lines, line{l.Leg, string(l.Note)})
			}
			return v, n, err
		},
		func(b []byte) (view, int, error) {
			var m oldersame.Quote
			n, err := m.Decode(b)
			return view{ID: m.Id}, n, err
		},
		func(b []byte) (view, int, error) {
			var m oldersame.Report
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text), Remark: string(m.Remark)}
			for _, l := range m.Lines {
				v.Lines = append(v.Lines, line{N: l.Line})
			}
			return v, n, err
		},
		oldersame.ErrNewer, oldersame.ErrEntries, oldersame.ErrShort, oldersame.ErrMalformed}
	unfilled = decoder{"tolerate-unfilled-counts",
		func(b []byte) (view, int, error) {
			var m olderunfilled.Order
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text)}
			for _, l := range m.Legs {
				v.Lines = append(v.Lines, line{l.Leg, string(l.Note)})
			}
			return v, n, err
		},
		func(b []byte) (view, int, error) {
			var m olderunfilled.Quote
			n, err := m.Decode(b)
			return view{ID: m.Id}, n, err
		},
		func(b []byte) (view, int, error) {
			var m olderunfilled.Report
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text), Remark: string(m.Remark)}
			for _, l := range m.Lines {
				v.Lines = append(v.Lines, line{N: l.Line})
			}
			return v, n, err
		},
		olderunfilled.ErrNewer, olderunfilled.ErrEntries, olderunfilled.ErrShort, olderunfilled.ErrMalformed}
	sameUnfilled = decoder{"assume-same-encodings, tolerate-unfilled-counts",
		func(b []byte) (view, int, error) {
			var m oldersameunfilled.Order
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text)}
			for _, l := range m.Legs {
				v.Lines = append(v.Lines, line{l.Leg, string(l.Note)})
			}
			return v, n, err
		},
		func(b []byte) (view, int, error) {
			var m oldersameunfilled.Quote
			n, err := m.Decode(b)
			return view{ID: m.Id}, n, err
		},
		func(b []byte) (view, int, error) {
			var m oldersameunfilled.Report
			n, err := m.Decode(b)
			v := view{ID: m.Id, Text: string(m.Text), Remark: string(m.Remark)}
			for _, l := range m.Lines {
				v.Lines = append(v.Lines, line{N: l.Line})
			}
			return v, n, err
		},
		oldersameunfilled.ErrNewer, oldersameunfilled.ErrEntries, oldersameunfilled.ErrShort, oldersameunfilled.ErrMalformed}
)

func encode(t testing.TB, m interface{ AppendTo([]byte) ([]byte, error) }) []byte {
	t.Helper()
	b, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// orders are messages of version 2 adding a field, groups and data, to the
// message and to its group's entries, the new groups holding groups and
// data of their own; and what version 1 knows of them.
func orders() []struct {
	name string
	m    *newer.Order
	want view
} {
	return []struct {
		name string
		m    *newer.Order
		want view
	}{
		{"a field, empty groups and data", &newer.Order{Id: 7, Qty: 100, Text: []byte("text")}, view{ID: 7, Text: "text"}},
		{"everything", &newer.Order{
			Id: 7, Qty: 100,
			Legs: []newer.Legs{
				{Leg: 1, Px: 11, Allocs: []newer.Allocs{{Account: 3}, {Account: 4}}, Note: []byte("first"), LegMemo: []byte("memo of a leg")},
				{Leg: 2, Note: []byte("second")},
			},
			Fees: []newer.Fees{
				{Amount: 5, Taxes: []newer.Taxes{{Rate: 20, TaxCode: []byte("VAT")}, {Rate: 1}}, FeeMemo: []byte("fee")},
				{Amount: 6},
			},
			Text: []byte("text"),
			Memo: []byte("memo"),
		}, view{ID: 7, Lines: []line{{1, "first"}, {2, "second"}}, Text: "text"}},
	}
}

// Strictly, a message of version 2 whose counts tell of groups or data
// version 1 does not know is ErrNewer: their encodings are not known.
func TestNewerVersionStrict(t *testing.T) {
	for _, c := range orders() {
		if _, n, err := strict.order(encode(t, c.m)); !errors.Is(err, older.ErrNewer) || n != 0 {
			t.Errorf("%s: %d bytes, %v", c.name, n, err)
		}
	}
	b := encode(t, &newer.Quote{Id: 9})
	if _, _, err := strict.quote(b); !errors.Is(err, older.ErrNewer) {
		t.Errorf("Quote: %v", err)
	}
	// Version 2 adding a field alone, its counts version 1's, decodes, its
	// block counted whole.
	v1, err := (&older.Quote{Id: 9}).AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	v2 := append(bytes.Clone(v1), 0xAA, 0xBB, 0xCC, 0xDD)
	binary.LittleEndian.PutUint16(v2[0:], older.QuoteBlockLength+4)
	binary.LittleEndian.PutUint16(v2[6:], 2)
	if v, n, err := strict.quote(append(bytes.Clone(v2), 0xFF)); err != nil || n != len(v2) || v.ID != 9 {
		t.Errorf("Quote, a field added: %+v, %d of %d bytes, %v", v, n, len(v2), err)
	}
}

// With --assume-same-encodings, a decoder of version 1 reads messages of
// version 2 whole: the fields a block adds are skipped by its blockLength,
// the groups and data a message or an entry adds by the counts of its
// header or dimension, recursively (§7.3). What it knows decodes as version
// 2 encoded it, and the length it returns is the message's, whatever
// follows.
func TestNewerVersionSameEncodings(t *testing.T) {
	for _, d := range []decoder{same, sameUnfilled} {
		for _, c := range orders() {
			b := encode(t, c.m)
			got, n, err := d.order(append(bytes.Clone(b), 0xAA, 0xBB))
			if err != nil || n != len(b) || !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: %s: %+v, %d of %d bytes, %v; want %+v", d.name, c.name, got, n, len(b), err, c.want)
			}
			// Every prefix fails: the counts promise what it lacks.
			for i := range len(b) {
				if _, _, err := d.order(b[:i]); err == nil {
					t.Fatalf("%s: %s: decoded %d of %d bytes", d.name, c.name, i, len(b))
				}
			}
		}
		b := encode(t, &newer.Quote{Id: 9, Sizes: []newer.Sizes{{Size: 1}, {Size: 2}}, Venue: []byte("XPAR")})
		if v, n, err := d.quote(b); err != nil || n != len(b) || v.ID != 9 {
			t.Errorf("%s: Quote: %d of %d bytes, %+v, %v", d.name, n, len(b), v, err)
		}
	}
}

// The assumption is what it says: version 2's Report has a data field of a
// uint32 length in its Lines' entries, where version 1's lengths are
// uint16, and a group of another dimension, which a decoder assuming
// version 1's encodings misreads. Strictly, they are ErrNewer.
func TestNewerVersionOtherEncodings(t *testing.T) {
	m := &newer.Report{Id: 3,
		Lines: []newer.Lines{{Line: 1, LineBlob: []byte("XY")}, {Line: 2}},
		Wide:  []newer.Wide{{W: 5}},
		Text:  []byte("report")}
	b := encode(t, m)
	want := view{ID: 3, Lines: []line{{N: 1}, {N: 2}}, Text: "report"}
	if _, _, err := strict.report(b); !errors.Is(err, older.ErrNewer) {
		t.Errorf("strict: %v", err)
	}
	for _, d := range []decoder{same, sameUnfilled} {
		if got, n, err := d.report(b); err == nil && n == len(b) && reflect.DeepEqual(got, want) {
			t.Errorf("%s: decoded right, %+v: the test no longer shows the assumption", d.name, got)
		}
	}
	// The data field alone, Wide's group removed: the misreading is silent,
	// `02 00 00 00 58 59` skipped as a uint16 length and 2 bytes.
	m.Wide = nil
	b = encode(t, m)
	binary.LittleEndian.PutUint16(b[8:], 1) // numGroups
	at := len(b) - (2 + len("report")) - newer.WideGroupSizeSize
	b = append(b[:at:at], b[at+newer.WideGroupSizeSize:]...)
	if _, _, err := strict.report(b); !errors.Is(err, older.ErrNewer) {
		t.Errorf("strict, the data field alone: %v", err)
	}
	for _, d := range []decoder{same, sameUnfilled} {
		if got, n, err := d.report(b); err != nil || reflect.DeepEqual(got, want) {
			t.Errorf("%s: the data field alone: %+v, %d bytes, %v; want a silent misreading", d.name, got, n, err)
		}
	}
}

// zero clears the counts of a header, or of the dimension at.
func zero(b []byte, at int) []byte {
	b = bytes.Clone(b)
	binary.LittleEndian.PutUint16(b[at+4:], 0)
	binary.LittleEndian.PutUint16(b[at+6:], 0)
	return b
}

// The header's and dimensions' numGroups and numVarDataFields are those of
// the received version, whatever it is, or ErrMalformed (§7.3.2); with
// --tolerate-unfilled-counts, a count of 0 is taken for one left unfilled,
// as Real Logic's encoders leave them, and the decoder reads what it knows
// the version holds.
func TestCounts(t *testing.T) {
	enc := func(m interface{ AppendTo([]byte) ([]byte, error) }) []byte { return encode(t, m) }
	order := enc(&older.Order{Id: 7, Legs: []older.Legs{{Leg: 1, Note: []byte("n")}}, Text: []byte("text")})
	orderWant := view{ID: 7, Lines: []line{{1, "n"}}, Text: "text"}
	legsAt := older.HeaderSize + older.OrderBlockLength
	version := func(b []byte, v uint16) []byte {
		b = bytes.Clone(b)
		binary.LittleEndian.PutUint16(b[6:], v)
		return b
	}
	set := func(b []byte, at int, v uint16) []byte {
		b = bytes.Clone(b)
		binary.LittleEndian.PutUint16(b[at:], v)
		return b
	}
	// Version 0 of Report, without Remark: its length, last, removed.
	report1 := enc(&older.Report{Id: 3, Lines: []older.Lines{{Line: 1}}, Text: []byte("r")})
	report0 := set(version(report1[:len(report1)-2], 0), 10, 1)
	reportWant := view{ID: 3, Lines: []line{{N: 1}}, Text: "r"}
	for _, c := range []struct {
		name      string
		decode    func(decoder) func([]byte) (view, int, error)
		b         []byte
		want      view
		malformed bool // strictly
		tolerated bool // with --tolerate-unfilled-counts
	}{
		{"version 1, filled", decoder.orderOf, order, orderWant, false, true},
		{"version 1, header unfilled", decoder.orderOf, zero(order, 4), orderWant, true, true},
		{"version 1, dimension unfilled", decoder.orderOf, zero(order, legsAt), orderWant, true, true},
		{"version 1, a group too many", decoder.orderOf, set(order, 8, 2), orderWant, true, false},
		{"version 1, data too few", decoder.orderOf, set(order, 10, 0), orderWant, true, true},
		{"version 1, data of the dimension too many", decoder.orderOf, set(order, legsAt+6, 2), orderWant, true, false},
		{"version 0, filled", decoder.reportOf, report0, reportWant, false, true},
		{"version 0, header unfilled", decoder.reportOf, zero(report0, 4), reportWant, true, true},
		{"version 0, counting data of version 1", decoder.reportOf, set(report0, 10, 2), reportWant, true, false},
		{"version 0, dimension unfilled", decoder.orderOf, zero(version(order, 0), legsAt), orderWant, true, true},
	} {
		for _, d := range []decoder{strict, same, unfilled, sameUnfilled} {
			tolerating := d.name == unfilled.name || d.name == sameUnfilled.name
			v, n, err := c.decode(d)(c.b)
			if c.malformed && !(tolerating && c.tolerated) {
				if !errors.Is(err, d.errMalfd) {
					t.Errorf("%s: %s: %v, want ErrMalformed", d.name, c.name, err)
				}
			} else if err != nil || n != len(c.b) || !reflect.DeepEqual(v, c.want) {
				t.Errorf("%s: %s: %+v, %d of %d bytes, %v; want %+v", d.name, c.name, v, n, len(c.b), err, c.want)
			}
		}
	}
}

func (d decoder) orderOf() func([]byte) (view, int, error)  { return d.order }
func (d decoder) reportOf() func([]byte) (view, int, error) { return d.report }

// In a message of a newer version, a count below what version 1 knows is
// malformed as in any; a count of 0, unfilled, with
// --tolerate-unfilled-counts, tells nothing of the newer version's groups and
// data: ErrNewer, as where nothing counts them, where they could come before
// what the decoder reads, unless --assume-same-encodings, with which they are
// taken to be none, and a new group's dimension before known data is read
// as that data. Counts of zero where version 1 knows nothing agree with it:
// what follows is not read, nor counted.
func TestNewerVersionUnfilledCounts(t *testing.T) {
	quote := zero(encode(t, &newer.Quote{Id: 9, Sizes: []newer.Sizes{{Size: 1}}, Venue: []byte("XPAR")}), 4)
	order := zero(encode(t, &newer.Order{Id: 7, Fees: []newer.Fees{{Amount: 1}}, Text: []byte("text")}), 4)
	for _, d := range []decoder{strict, same, unfilled, sameUnfilled} {
		if v, n, err := d.quote(quote); err != nil || n != older.HeaderSize+older.QuoteBlockLength || v.ID != 9 {
			t.Errorf("%s: Quote: %+v, %d bytes, %v", d.name, v, n, err)
		}
	}
	// Version 2 adding a field alone, encoded as Real Logic's encoders do,
	// its counts left zero.
	v1 := encode(t, &older.Order{Id: 7, Legs: []older.Legs{{Leg: 1, Note: []byte("n")}}, Text: []byte("text")})
	v2 := zero(slices.Concat(v1[:older.HeaderSize+older.OrderBlockLength], []byte{0xAA, 0xBB}, v1[older.HeaderSize+older.OrderBlockLength:]), 4)
	binary.LittleEndian.PutUint16(v2[0:], older.OrderBlockLength+2)
	binary.LittleEndian.PutUint16(v2[6:], 2)
	for _, d := range []decoder{strict, same} {
		for _, b := range [][]byte{order, v2} {
			if _, _, err := d.order(b); !errors.Is(err, d.errMalfd) {
				t.Errorf("%s: Order: %v", d.name, err)
			}
		}
	}
	for _, b := range [][]byte{order, v2} {
		if _, _, err := unfilled.order(b); !errors.Is(err, unfilled.errNewer) {
			t.Errorf("%s: Order: %v", unfilled.name, err)
		}
	}
	want := view{ID: 7, Lines: []line{{1, "n"}}, Text: "text"}
	if v, n, err := sameUnfilled.order(v2); err != nil || n != len(v2) || !reflect.DeepEqual(v, want) {
		t.Errorf("%s: Order, a field added: %+v, %d of %d bytes, %v", sameUnfilled.name, v, n, len(v2), err)
	}
	if v, _, err := sameUnfilled.order(order); err == nil && v.Text == "text" {
		t.Errorf("%s: Order decoded right, %+v: the test no longer shows the risk", sameUnfilled.name, v)
	}
}

// Skipping is bounded as decoding is.
func TestNewerVersionBounds(t *testing.T) {
	// MaxEntries counts the unknown entries with the others.
	m := &newer.Order{Fees: make([]newer.Fees, oldersame.MaxEntries-1), Legs: []newer.Legs{{}}}
	b := encode(t, m)
	if _, n, err := same.order(b); err != nil || n != len(b) {
		t.Errorf("%d entries: %d of %d bytes, %v", oldersame.MaxEntries, n, len(b), err)
	}
	m.Legs = append(m.Legs, newer.Legs{})
	if _, _, err := same.order(encode(t, m)); !errors.Is(err, oldersame.ErrEntries) {
		t.Errorf("%d entries: %v", oldersame.MaxEntries+1, err)
	}

	// A count the bytes left cannot hold.
	b = encode(t, &newer.Order{Fees: []newer.Fees{{Amount: 1}}})
	feesAt := newer.HeaderSize + newer.OrderBlockLength + newer.GroupSizeEncodingSize // after the empty Legs
	binary.LittleEndian.PutUint16(b[feesAt+2:], 0xFFFF)
	if _, _, err := same.order(b); !errors.Is(err, oldersame.ErrShort) {
		t.Errorf("a count beyond the bytes: %v", err)
	}
	// Entries of no bytes.
	b = encode(t, &newer.Order{Fees: []newer.Fees{{Amount: 1}}})
	for _, off := range []int{0, 4, 6} { // blockLength, numGroups, numVarDataFields
		binary.LittleEndian.PutUint16(b[feesAt+off:], 0)
	}
	if _, _, err := same.order(b); !errors.Is(err, oldersame.ErrMalformed) {
		t.Errorf("empty entries: %v", err)
	}
}

func FuzzNewerVersion(f *testing.F) {
	f.Add(encode(f, &newer.Order{Id: 1, Legs: []newer.Legs{{Allocs: []newer.Allocs{{}}, LegMemo: []byte("x")}},
		Fees: []newer.Fees{{Taxes: []newer.Taxes{{TaxCode: []byte("y")}}}}, Memo: []byte("z")}))
	f.Add(encode(f, &newer.Report{Lines: []newer.Lines{{LineBlob: []byte("x")}}, Wide: []newer.Wide{{}}}))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, d := range []decoder{strict, same, unfilled, sameUnfilled} {
			for _, decode := range []func([]byte) (view, int, error){d.order, d.quote, d.report} {
				v, n, err := decode(b)
				if err != nil {
					continue
				}
				if n < older.HeaderSize || n > len(b) {
					t.Fatalf("%s: read %d of %d bytes", d.name, n, len(b))
				}
				if again, m, err := decode(b[:n]); err != nil || m != n || !reflect.DeepEqual(again, v) {
					t.Fatalf("%s: the %d bytes read decode as %d, %v", d.name, n, m, err)
				}
			}
		}
	})
}

// A group a version does not have is released when decoded into a struct
// that held it: its entries, and theirs, hold nothing of the frame before.
func TestAbsentGroupReleased(t *testing.T) {
	src := newer.Order{Fees: []newer.Fees{{FeeMemo: []byte("fee"), Taxes: []newer.Taxes{{TaxCode: []byte("tax")}}}}}
	frame, err := src.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	var old older.Order
	oldFrame, err := old.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got newer.Order
	for _, b := range [][]byte{frame, oldFrame} {
		if _, err := got.Decode(b); err != nil {
			t.Fatal(err)
		}
	}
	if len(got.Fees) != 0 || cap(got.Fees) != 1 {
		t.Fatalf("Fees: %d of %d, want 0 of 1", len(got.Fees), cap(got.Fees))
	}
	fee := got.Fees[:1][0]
	if fee.FeeMemo != nil || len(fee.Taxes) != 0 || cap(fee.Taxes) != 1 || fee.Taxes[:1][0].TaxCode != nil {
		t.Errorf("dropped fee holds %+v", fee)
	}
}
