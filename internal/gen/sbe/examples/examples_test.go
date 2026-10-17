package examples

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sylr/fix2/sofh"
)

// The example messages of SBE 1.0, sections 9.1.2, 9.2.2 and 9.3.2, with
// their framing headers, as the specification's hexadecimal dumps show them.
const (
	newOrderSingleHex = `00 00 00 44 eb 50 36 00 63 00 5b 00 00 00 4f 52
		44 30 30 30 30 31 41 43 43 54 30 31 00 00 47 45
		4d 34 00 00 00 00 31 80 16 b3 3b 13 65 29 15 07
		00 00 00 32 1a 85 01 00 00 00 00 00 00 00 00 00
		00 00 00 80`
	executionReportHex = `00 00 00 54 eb 50 2a 00 62 00 5b 00 00 00 4f 30
		30 30 30 30 30 31 45 58 45 43 30 30 30 30 46 31
		47 45 4d 34 00 00 00 00 de 07 06 ff ff 31 01 00
		00 00 06 00 00 00 75 3e 0c 00 02 00 1a 85 01 00
		00 00 00 00 02 00 00 00 24 85 01 00 00 00 00 00
		04 00 00 00`
	businessMessageRejectHex = `00 00 00 40 eb 50 09 00 61 00 5b 00 00 00 4f 52
		44 30 30 30 30 31 06 27 00 4e 6f 74 20 61 75 74
		68 6f 72 69 7a 65 64 20 74 6f 20 74 72 61 64 65
		20 74 68 61 74 20 69 6e 73 74 72 75 6d 65 6e 74`
)

// frame returns the SBE message of a dump, its framing header checked and
// removed.
func frame(t testing.TB, dump string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.Join(strings.Fields(dump), ""))
	if err != nil {
		t.Fatal(err)
	}
	enc, msg, err := sofh.NewReader(bytes.NewReader(raw), sofh.ReaderOptions{}).Next()
	if err != nil || enc != sofh.SBE10LittleEndian {
		t.Fatalf("framing: %v, %v", enc, err)
	}
	return msg
}

// roundTrip checks m encodes back to msg.
func roundTrip(t *testing.T, m interface{ AppendTo([]byte) ([]byte, error) }, msg []byte) {
	t.Helper()
	got, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("encoded\n%x\nwant\n%x", got, msg)
	}
}

func TestNewOrderSingle(t *testing.T) {
	msg := frame(t, newOrderSingleHex)
	var m NewOrderSingle
	if _, err := m.Decode(msg); err != nil {
		t.Fatal(err)
	}
	if m.ClOrdIDString() != "ORD00001" || m.AccountString() != "ACCT01" || m.SymbolString() != "GEM4" {
		t.Errorf("ids %q %q %q", m.ClOrdIDString(), m.AccountString(), m.SymbolString())
	}
	if m.Side != SideEnumBuy || m.OrdType != OrdTypeEnumLimit || m.OrderQty.Mantissa != 7 || m.OrderQty.Exponent() != 0 {
		t.Errorf("side %v, type %v, qty %d", m.Side, m.OrdType, m.OrderQty.Mantissa)
	}
	if m.Price.Mantissa != Some(int64(99610)) || m.Price.Exponent() != -3 {
		t.Errorf("price %+v", m.Price.Mantissa)
	}
	if m.StopPx.Mantissa.Valid {
		t.Errorf("stop price %+v, want null", m.StopPx.Mantissa)
	}
	// The dump's bytes, not the interpretation table's, which disagrees.
	if m.TransactTime != 0x152965133bb31680 {
		t.Errorf("transact time %#x", m.TransactTime)
	}
	roundTrip(t, &m, msg)
}

func TestExecutionReport(t *testing.T) {
	msg := frame(t, executionReportHex)
	var m ExecutionReport
	if _, err := m.Decode(msg); err != nil {
		t.Fatal(err)
	}
	if m.OrderIDString() != "O0000001" || m.ExecIDString() != "EXEC0000" || m.SymbolString() != "GEM4" {
		t.Errorf("ids %q %q %q", m.OrderIDString(), m.ExecIDString(), m.SymbolString())
	}
	if m.ExecType != ExecTypeEnumTrade || m.OrdStatus != OrdStatusEnumPartialFilled || m.Side != SideEnumBuy {
		t.Errorf("exec type %v, status %v, side %v", m.ExecType, m.OrdStatus, m.Side)
	}
	if my := m.MaturityMonthYear; my.Year != 2014 || my.Month != 6 || my.Day != 255 || my.Week != 255 {
		t.Errorf("maturity %+v", my)
	}
	if m.LeavesQty.Mantissa != 1 || m.CumQty.Mantissa != 6 || m.TradeDate != 15989 {
		t.Errorf("leaves %d, cum %d, date %d", m.LeavesQty.Mantissa, m.CumQty.Mantissa, m.TradeDate)
	}
	want := []FillsGrp{
		{FillPx: OptionalDecimalEncoding{Mantissa: Some(int64(99610))}, FillQty: QtyEncoding{Mantissa: 2}},
		{FillPx: OptionalDecimalEncoding{Mantissa: Some(int64(99620))}, FillQty: QtyEncoding{Mantissa: 4}},
	}
	if len(m.FillsGrp) != 2 || m.FillsGrp[0] != want[0] || m.FillsGrp[1] != want[1] {
		t.Errorf("fills %+v", m.FillsGrp)
	}
	roundTrip(t, &m, msg)
}

func TestBusinessMessageReject(t *testing.T) {
	msg := frame(t, businessMessageRejectHex)
	var m BusinessMessageReject
	if _, err := m.Decode(msg); err != nil {
		t.Fatal(err)
	}
	if m.BusinessRejectRefIDString() != "ORD00001" || m.BusinessRejectReason != BusinessRejectReasonEnumNotAuthorized ||
		string(m.Text) != "Not authorized to trade that instrument" {
		t.Errorf("%q %v %q", m.BusinessRejectRefIDString(), m.BusinessRejectReason, m.Text)
	}
	roundTrip(t, &m, msg)
}

// Every prefix of a message, and every truncation of its body, fails to
// decode, without panicking.
func TestTruncation(t *testing.T) {
	for _, dump := range []string{newOrderSingleHex, executionReportHex, businessMessageRejectHex} {
		msg := frame(t, dump)
		for n := range len(msg) {
			for _, m := range []interface{ Decode([]byte) (int, error) }{&NewOrderSingle{}, &ExecutionReport{}, &BusinessMessageReject{}} {
				if _, err := m.Decode(msg[:n]); err == nil {
					t.Fatalf("%T decoded %d of %d bytes of %x", m, n, len(msg), msg)
				}
			}
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	msg := frame(t, newOrderSingleHex)
	var m NewOrderSingle
	if _, err := m.Decode(msg); err != nil {
		t.Fatal(err)
	}

	other := bytes.Clone(msg)
	other[4] = 92 // schema id
	if _, err := m.Decode(other); !errors.Is(err, ErrSchema) || m.Side != 0 {
		t.Errorf("another schema: %v, m %+v", err, m)
	}
	var er ExecutionReport
	if _, err := er.Decode(msg); !errors.Is(err, ErrTemplate) {
		t.Errorf("another template: %v", err)
	}
	short := bytes.Clone(msg)
	short[0] = 53 // block length, one less than the fields
	if _, err := m.Decode(short); !errors.Is(err, ErrMalformed) {
		t.Errorf("short block: %v", err)
	}
}

// The length decoding returns counts the root block and the group entries
// by the header's and the dimension's blockLength, padding included.
func TestConsumedLength(t *testing.T) {
	for _, c := range []struct {
		dump string
		m    interface{ Decode([]byte) (int, error) }
	}{
		{newOrderSingleHex, &NewOrderSingle{}},
		{executionReportHex, &ExecutionReport{}},
		{businessMessageRejectHex, &BusinessMessageReject{}},
	} {
		msg := frame(t, c.dump)
		if n, err := c.m.Decode(append(bytes.Clone(msg), 0xFF)); err != nil || n != len(msg) {
			t.Errorf("%T: %d of %d bytes, %v", c.m, n, len(msg), err)
		}
	}

	// ExecutionReport's FillsGrp entries, 12 bytes, padded to 14.
	msg := frame(t, executionReportHex)
	at := HeaderSize + ExecutionReportBlockLength
	count := int(binary.LittleEndian.Uint16(msg[at+2:]))
	padded := bytes.Clone(msg[:at+GroupSizeEncodingSize])
	binary.LittleEndian.PutUint16(padded[at:], 14)
	for i := range count {
		entry := msg[at+GroupSizeEncodingSize+12*i:][:12]
		padded = append(append(padded, entry...), 0xEE, 0xEE)
	}
	var want, got ExecutionReport
	if _, err := want.Decode(msg); err != nil {
		t.Fatal(err)
	}
	if n, err := got.Decode(append(padded, 0xFF)); err != nil || n != len(padded) || !reflect.DeepEqual(got, want) {
		t.Errorf("padded entries: %d of %d bytes, %+v, %v", n, len(padded), got, err)
	}
}

// This schema's header and dimension have no numGroups nor
// numVarDataFields: nothing tells of a newer version's groups and data.
// Its fields, at the end of the root block, are skipped by the header's
// blockLength (§7.3.1). After all the schema knows of a message, its
// groups and data cannot be told from bytes after the message: they are
// not read, nor counted. Before what it knows, they may be anywhere a
// newer version may add them, unseen: a message holding data, where a group
// may come before them, or entries, which may hold groups and data after
// theirs, is ErrNewer.
func TestNewerVersionUncounted(t *testing.T) {
	newer := func(dump string, block int, extend, tail []byte) []byte {
		msg := frame(t, dump)
		b := slices.Concat(msg[:HeaderSize+block], extend, msg[HeaderSize+block:], tail)
		binary.LittleEndian.PutUint16(b[0:], uint16(block+len(extend)))
		binary.LittleEndian.PutUint16(b[6:], 1)
		return b
	}
	// A field and a group added: the field skipped, the group not read.
	var nos NewOrderSingle
	b := newer(newOrderSingleHex, NewOrderSingleBlockLength, []byte{0xAA, 0xBB}, []byte{4, 0, 1, 0, 0xEE, 0xEE, 0xEE, 0xEE})
	if n, err := nos.Decode(b); err != nil || n != len(frame(t, newOrderSingleHex))+2 || nos.ClOrdIDString() != "ORD00001" {
		t.Errorf("NewOrderSingle: %d bytes, %q, %v", n, nos.ClOrdIDString(), err)
	}
	// Data: a group may come before it.
	var bmr BusinessMessageReject
	if _, err := bmr.Decode(newer(businessMessageRejectHex, BusinessMessageRejectBlockLength, []byte{0xAA}, nil)); !errors.Is(err, ErrNewer) {
		t.Errorf("BusinessMessageReject: %v", err)
	}
	// Entries: they may hold groups and data.
	var er ExecutionReport
	if _, err := er.Decode(newer(executionReportHex, ExecutionReportBlockLength, nil, nil)); !errors.Is(err, ErrNewer) {
		t.Errorf("ExecutionReport: %v", err)
	}
	// None.
	empty, err := (&ExecutionReport{}).AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(empty[6:], 1)
	if n, err := er.Decode(append(empty, 0xEE)); err != nil || n != len(empty) {
		t.Errorf("ExecutionReport without entries: %d of %d bytes, %v", n, len(empty), err)
	}
}

func TestGroupBounds(t *testing.T) {
	msg := frame(t, executionReportHex)
	// A count the bytes left cannot hold.
	many := bytes.Clone(msg)
	many[8+42+2] = 0xFF
	var m ExecutionReport
	if _, err := m.Decode(many); !errors.Is(err, ErrShort) {
		t.Errorf("count beyond the bytes: %v", err)
	}
	// Entries of no bytes.
	empty := bytes.Clone(msg[:8+42+4])
	empty[8+42] = 0
	empty[8+42+1] = 0
	if _, err := m.Decode(empty); !errors.Is(err, ErrMalformed) {
		t.Errorf("empty entries: %v", err)
	}
	// Encoding as many entries as a uint16 holds: its largest value is null.
	m.FillsGrp = make([]FillsGrp, 65535)
	if _, err := m.AppendTo(nil); !errors.Is(err, ErrEntries) {
		t.Errorf("65535 entries: %v", err)
	}
	// But decoding them, as §5.4.10.1 allows.
	m.FillsGrp = m.FillsGrp[:65534]
	b, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	b[8+42+2], b[8+42+3] = 0xff, 0xff
	b = append(b, make([]byte, 12)...)
	if _, err := m.Decode(b); err != nil || len(m.FillsGrp) != 65535 {
		t.Errorf("decoding 65535 entries: %v, %d", err, len(m.FillsGrp))
	}
}

func TestDataTooLong(t *testing.T) {
	m := BusinessMessageReject{Text: make([]byte, 65535)}
	if b, err := m.AppendTo([]byte("x")); !errors.Is(err, ErrLength) || string(b) != "x" {
		t.Errorf("65535 bytes: %q, %v", b, err)
	}
	m.Text = m.Text[:65534]
	if _, err := m.AppendTo(nil); err != nil {
		t.Errorf("65534 bytes: %v", err)
	}
}

func TestDecodeDoesNotAllocate(t *testing.T) {
	msgs := [][]byte{frame(t, newOrderSingleHex), frame(t, executionReportHex), frame(t, businessMessageRejectHex)}
	var nos NewOrderSingle
	var er ExecutionReport
	var bmr BusinessMessageReject
	decode := func() {
		_, err1 := nos.Decode(msgs[0])
		_, err2 := er.Decode(msgs[1])
		_, err3 := bmr.Decode(msgs[2])
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatal("decode failed")
		}
	}
	decode()
	if allocs := testing.AllocsPerRun(100, decode); allocs != 0 {
		t.Errorf("%v allocs, want 0", allocs)
	}
}

func FuzzDecode(f *testing.F) {
	for _, dump := range []string{newOrderSingleHex, executionReportHex, businessMessageRejectHex} {
		f.Add(frame(f, dump))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, m := range []interface {
			Decode([]byte) (int, error)
			AppendTo([]byte) ([]byte, error)
		}{&NewOrderSingle{}, &ExecutionReport{}, &BusinessMessageReject{}} {
			if _, err := m.Decode(b); err != nil {
				continue
			}
			// What decodes encodes, its encoding decodes, and encodes
			// again to the same bytes.
			enc, err := m.AppendTo(nil)
			if er, ok := m.(*ExecutionReport); ok && len(er.FillsGrp) == 65535 && errors.Is(err, ErrEntries) {
				continue // a count of 65535 decodes but does not encode (docs/sbe.md)
			}
			if err != nil {
				t.Fatalf("%T: decoded but does not encode: %v", m, err)
			}
			fresh := reflect.New(reflect.TypeOf(m).Elem()).Interface().(interface {
				Decode([]byte) (int, error)
				AppendTo([]byte) ([]byte, error)
			})
			if _, err := fresh.Decode(enc); err != nil {
				t.Fatalf("%T: its encoding %x does not decode: %v", m, enc, err)
			}
			again, err := fresh.AppendTo(nil)
			if err != nil || !bytes.Equal(enc, again) {
				t.Fatalf("%T: %x re-encodes as %x, %v", m, enc, again, err)
			}
		}
	})
}
