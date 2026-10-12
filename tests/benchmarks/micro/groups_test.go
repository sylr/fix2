// Package micro measures, below the session, what the fix2 agent spends
// reading the scenarios' messages through the code fixgen generates from
// spec/FIX44.xml (gen/fix2/fix44, make generate): the messages each end
// reads, parsed once, read as the agent reads them, every field its
// handlers read, the group entries' among them.
//
//	make micro
//	go test -run '^$' -bench . -count 10 ./micro/ > new.txt
package micro

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/tests/benchmarks/gen/fix2/fix44"
	"github.com/sylr/fix2/tests/benchmarks/internal/scenario"
)

// parse parses what b builds, as a session reads it.
func parse(b *testing.B, raw []byte) *fix2.Message {
	b.Helper()
	m, err := fix2.Parse(raw)
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	return &m
}

// newOrderSingle returns the scenario's order, with its Parties.
func newOrderSingle(b *testing.B) *fix2.Message {
	nos := fix44.BuildNewOrderSingle(scenario.BeginString).
		SetClOrdID("12345").
		SetHandlInst(fix44.HandlInstAutoexecpriv).
		SetOrderQty(scenario.OrderQty).
		SetOrdType(fix44.OrdTypeLimit).
		SetPrice(scenario.Price).
		SetSide(fix44.SideBuy).
		SetSymbol(scenario.Symbol).
		SetTimeInForce(fix44.TimeInForceDay).
		SetTransactTime(time.Now())
	for _, p := range scenario.Parties {
		nos.AddNoPartyIDsEntry(fix44.NewNoPartyIDsEntry().
			SetPartyID(p.ID).
			SetPartyIDSource(fix44.PartyIDSource(p.Source)).
			SetPartyRole(fix44.PartyRole(p.Role)))
	}
	return parse(b, nos.Build())
}

// executionReport returns the acceptor's acknowledgement of the scenario's
// order, its Parties echoed.
func executionReport(b *testing.B) *fix2.Message {
	er := fix44.BuildExecutionReport(scenario.BeginString).
		SetOrderID("1").
		SetExecID("1").
		SetExecType(fix44.ExecTypeNew).
		SetOrdStatus(fix44.OrdStatusNew).
		SetSide(fix44.SideBuy).
		SetLeavesQty(scenario.OrderQty).
		SetCumQty(0).
		SetAvgPx(0).
		SetClOrdID("12345").
		SetOrderQty(scenario.OrderQty).
		SetPrice(scenario.Price).
		SetSymbol(scenario.Symbol).
		SetTransactTime(time.Now())
	for _, p := range scenario.Parties {
		er.AddNoPartyIDsEntry(fix44.NewNoPartyIDsEntry().
			SetPartyID(p.ID).
			SetPartyIDSource(fix44.PartyIDSource(p.Source)).
			SetPartyRole(fix44.PartyRole(p.Role)))
	}
	return parse(b, er.Build())
}

// marketDataRequest returns the scenario's request.
func marketDataRequest(b *testing.B) *fix2.Message {
	req := fix44.BuildMarketDataRequest(scenario.BeginString).
		SetMDReqID("12345").
		SetSubscriptionRequestType(fix44.SubscriptionRequestTypeSnapshot).
		SetMarketDepth(scenario.MarketDepth)
	for _, t := range scenario.EntryTypes {
		req.AddNoMDEntryTypesEntry(fix44.NewNoMDEntryTypesEntry().SetMDEntryType(fix44.MDEntryType(t)))
	}
	req.AddNoRelatedSymEntry(fix44.NewNoRelatedSymEntry().SetSymbol(scenario.Symbol))
	return parse(b, req.Build())
}

// snapshot returns a snapshot of depth levels of each side of the
// scenario's book, its levels repeated beyond its own depth: depth
// scenario.MarketDepth is the scenario's.
func snapshot(b *testing.B, depth int) *fix2.Message {
	w := fix44.BuildMarketDataSnapshotFullRefresh(scenario.BeginString).
		SetMDReqID("12345").
		SetSymbol(scenario.Symbol)
	for k, t := range scenario.EntryTypes {
		for lvl := range depth {
			l := scenario.Book[k][lvl%scenario.MarketDepth]
			w.AddNoMDEntriesEntry(fix44.NewNoMDEntriesEntry().
				SetMDEntryType(fix44.MDEntryType(t)).
				SetMDEntryPx(l.Px).
				SetMDEntrySize(l.Size))
		}
	}
	return parse(b, w.Build())
}

// sink keeps what the benchmarks read alive.
var sink int

// The reads, as the fix2 agent's handlers make them.

// readNewOrderSingle reads what the acceptor reads of an order.
func readNewOrderSingle(m *fix44.NewOrderSingle) int {
	clOrdID, _ := m.ClOrdID()
	symbol, _ := m.Symbol()
	side, _ := m.Side()
	qty, _, _ := m.OrderQty()
	px, _, _ := m.Price()
	n := len(clOrdID) + len(symbol) + len(side) + int(qty+px)
	for _, p := range m.NoPartyIDsEntries() {
		pid, _ := p.PartyID()
		src, _ := p.PartyIDSource()
		role, _ := p.PartyRole()
		n += len(pid) + len(src) + len(role)
	}
	return n
}

// readExecutionReport reads what the initiator reads of an acknowledgement.
func readExecutionReport(m *fix44.ExecutionReport) int {
	clOrdID, _ := m.ClOrdID()
	execType, _ := m.ExecType()
	ordStatus, _ := m.OrdStatus()
	side, _ := m.Side()
	symbol, _ := m.Symbol()
	qty, _, _ := m.OrderQty()
	px, _, _ := m.Price()
	n := len(clOrdID) + len(execType) + len(ordStatus) + len(side) + len(symbol) + int(qty+px)
	for _, p := range m.NoPartyIDsEntries() {
		pid, _ := p.PartyID()
		src, _ := p.PartyIDSource()
		role, _ := p.PartyRole()
		n += len(pid) + len(src) + len(role)
	}
	return n
}

// readMarketDataRequest reads what the acceptor reads of a request.
func readMarketDataRequest(m *fix44.MarketDataRequest) int {
	reqID, _ := m.MDReqID()
	sub, _ := m.SubscriptionRequestType()
	depth, _, _ := m.MarketDepth()
	n := len(reqID) + len(sub) + depth
	for _, t := range m.NoMDEntryTypesEntries() {
		typ, _ := t.MDEntryType()
		n += len(typ)
	}
	for _, s := range m.NoRelatedSymEntries() {
		symbol, _ := s.Symbol()
		n += len(symbol)
	}
	return n
}

// readSnapshot reads what the initiator reads of a snapshot.
func readSnapshot(m *fix44.MarketDataSnapshotFullRefresh) int {
	reqID, _ := m.MDReqID()
	symbol, _ := m.Symbol()
	n := len(reqID) + len(symbol)
	for _, e := range m.NoMDEntriesEntries() {
		typ, _ := e.MDEntryType()
		px, _, _ := e.MDEntryPx()
		size, _, _ := e.MDEntrySize()
		n += len(typ) + int(px+size)
	}
	return n
}

// BenchmarkRead reads each message of the scenarios as the fix2 agent does:
// the order and its acknowledgement with their three Parties, the market
// data request with its two groups, its snapshot of twenty entries.
func BenchmarkRead(b *testing.B) {
	nos, er, req, w := newOrderSingle(b), executionReport(b), marketDataRequest(b), snapshot(b, scenario.MarketDepth)
	b.Run("NewOrderSingle", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink += readNewOrderSingle(fix44.WrapNewOrderSingle(nos))
		}
	})
	b.Run("ExecutionReport", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink += readExecutionReport(fix44.WrapExecutionReport(er))
		}
	})
	b.Run("MarketDataRequest", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink += readMarketDataRequest(fix44.WrapMarketDataRequest(req))
		}
	})
	b.Run("MarketDataSnapshotFullRefresh", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink += readSnapshot(fix44.WrapMarketDataSnapshotFullRefresh(w))
		}
	})
}

// BenchmarkEntries reads the group entries alone, without their fields:
// the generated XxxEntries readers, for the scenarios' groups and snapshots
// of growing depth.
func BenchmarkEntries(b *testing.B) {
	er := fix44.WrapExecutionReport(executionReport(b))
	b.Run("NoPartyIDs/entries=3", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink += len(er.NoPartyIDsEntries())
		}
	})
	for _, depth := range []int{1, 10, 100} {
		w := fix44.WrapMarketDataSnapshotFullRefresh(snapshot(b, depth))
		b.Run("NoMDEntries/entries="+strconv.Itoa(2*depth), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sink += len(w.NoMDEntriesEntries())
			}
		})
	}
}

// BenchmarkReadSnapshot reads snapshots of growing depth whole, every
// entry's fields included.
func BenchmarkReadSnapshot(b *testing.B) {
	for _, depth := range []int{1, 10, 100} {
		w := snapshot(b, depth)
		b.Run(fmt.Sprintf("entries=%d", 2*depth), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sink += readSnapshot(fix44.WrapMarketDataSnapshotFullRefresh(w))
			}
		})
	}
}
