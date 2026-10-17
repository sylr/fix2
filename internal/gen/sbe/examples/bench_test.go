package examples

import "testing"

// The codecs fix2gen sbe generates, on SBE's own examples: a message of
// fixed fields only, and one with a group. Encoding appends to a reused
// buffer, decoding fills a reused struct: neither allocates.

func benchOrder() *NewOrderSingle {
	o := &NewOrderSingle{
		Side:         SideEnumBuy,
		TransactTime: 1_700_000_000_000_000_000,
		OrderQty:     QtyEncoding{Mantissa: 100},
		OrdType:      OrdTypeEnumLimit,
		Price:        OptionalDecimalEncoding{Mantissa: Some(int64(150_000))},
	}
	copy(o.ClOrdID[:], "ORD00001")
	copy(o.Account[:], "ACCT")
	copy(o.Symbol[:], "GEM4")
	return o
}

func benchReport() *ExecutionReport {
	r := &ExecutionReport{
		ExecType:  ExecTypeEnumTrade,
		OrdStatus: OrdStatusEnumFilled,
		Side:      SideEnumBuy,
		LeavesQty: QtyEncoding{Mantissa: 0},
		CumQty:    QtyEncoding{Mantissa: 100},
		TradeDate: 20_000,
		FillsGrp: []FillsGrp{
			{FillPx: OptionalDecimalEncoding{Mantissa: Some(int64(150_000))}, FillQty: QtyEncoding{Mantissa: 60}},
			{FillPx: OptionalDecimalEncoding{Mantissa: Some(int64(150_100))}, FillQty: QtyEncoding{Mantissa: 40}},
		},
	}
	copy(r.OrderID[:], "ORD00001")
	copy(r.ExecID[:], "EXEC0001")
	copy(r.Symbol[:], "GEM4")
	return r
}

type message interface {
	AppendTo([]byte) ([]byte, error)
	Decode([]byte) (int, error)
}

func benchEncode(b *testing.B, m message) {
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
}

func benchDecode(b *testing.B, m, into message) {
	buf, err := m.AppendTo(nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := into.Decode(buf); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := into.Decode(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewOrderSingleEncode(b *testing.B)  { benchEncode(b, benchOrder()) }
func BenchmarkNewOrderSingleDecode(b *testing.B)  { benchDecode(b, benchOrder(), &NewOrderSingle{}) }
func BenchmarkExecutionReportEncode(b *testing.B) { benchEncode(b, benchReport()) }
func BenchmarkExecutionReportDecode(b *testing.B) {
	benchDecode(b, benchReport(), &ExecutionReport{})
}
