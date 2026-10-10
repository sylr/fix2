package fix50sp2

import (
	"bytes"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
)

// TestNoLegsRoundTrip builds an ExecutionReport with two legs, each carrying a
// nested NoNested3PartyIDs group, and parses it back. NoLegs starts with the
// InstrumentLeg component, so its delimiter is LegSymbol (600); LegQty (687)
// comes later in each entry. NoNested3PartyIDs is reached through the
// NestedParties3 component inside NoLegs.
func TestNoLegsRoundTrip(t *testing.T) {
	raw := BuildExecutionReport("FIXT.1.1").
		SetOrderID("O-1").
		SetExecID("E-1").
		SetExecType(ExecTypeNew).
		SetOrdStatus(OrdStatusNew).
		SetSymbol("SPREAD").
		SetSide(SideBuy).
		SetLeavesQty(10).
		SetCumQty(0).
		AddNoLegsEntry(NewNoLegsEntry().
			SetLegSymbol("LEG-A").
			SetLegQty(4).
			AddNoNested3PartyIDsEntry(NewNoNested3PartyIDsEntry().
				SetNested3PartyID("P-A1").
				SetNested3PartyRole(1)).
			AddNoNested3PartyIDsEntry(NewNoNested3PartyIDsEntry().
				SetNested3PartyID("P-A2").
				SetNested3PartyRole(2))).
		AddNoLegsEntry(NewNoLegsEntry().
			SetLegSymbol("LEG-B").
			SetLegQty(6).
			AddNoNested3PartyIDsEntry(NewNoNested3PartyIDsEntry().
				SetNested3PartyID("P-B1").
				SetNested3PartyRole(3))).
		Build()

	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	type party struct {
		id   string
		role int
	}
	want := []struct {
		symbol  string
		qty     float64
		parties []party
	}{
		{"LEG-A", 4, []party{{"P-A1", 1}, {"P-A2", 2}}},
		{"LEG-B", 6, []party{{"P-B1", 3}}},
	}

	legs := WrapExecutionReport(&msg).NoLegsEntries()
	if len(legs) != len(want) {
		t.Fatalf("got %d legs, want %d", len(legs), len(want))
	}
	for i, w := range want {
		leg := legs[i]
		if got, ok := leg.LegSymbol(); !ok || got != w.symbol {
			t.Errorf("leg %d LegSymbol = (%q, %v), want %q", i, got, ok, w.symbol)
		}
		if got, ok, err := leg.LegQty(); err != nil || !ok || got != w.qty {
			t.Errorf("leg %d LegQty = (%v, %v, %v), want %v", i, got, ok, err, w.qty)
		}
		parties := leg.NoNested3PartyIDsEntries()
		if len(parties) != len(w.parties) {
			t.Fatalf("leg %d: got %d parties, want %d", i, len(parties), len(w.parties))
		}
		for j, wp := range w.parties {
			if got, ok := parties[j].Nested3PartyID(); !ok || got != wp.id {
				t.Errorf("leg %d party %d ID = (%q, %v), want %q", i, j, got, ok, wp.id)
			}
			if got, ok, err := parties[j].Nested3PartyRole(); err != nil || !ok || got != wp.role {
				t.Errorf("leg %d party %d Role = (%d, %v, %v), want %d", i, j, got, ok, err, wp.role)
			}
		}
	}
}

// TestLocalMktDate checks that LOCALMKTDATE setters keep the civil date they
// are given, on a message and in a group entry, east and west of UTC.
func TestLocalMktDate(t *testing.T) {
	plus2 := time.FixedZone("UTC+2", 2*3600)
	minus5 := time.FixedZone("UTC-5", -5*3600)
	settle := time.Date(2026, 10, 10, 0, 0, 0, 0, plus2)      // 2026-10-09 22:00 UTC
	legSettle := time.Date(2026, 10, 10, 23, 0, 0, 0, minus5) // 2026-10-11 04:00 UTC

	raw := BuildExecutionReport("FIXT.1.1").
		SetSettlDate(settle).
		AddNoLegsEntry(NewNoLegsEntry().
			SetLegSymbol("L").
			SetLegSettlDateWithFormat(legSettle, fix2.TimeFormatDate)).
		Build()
	for _, want := range []string{"\x0164=20261010\x01", "\x01588=20261010\x01"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("message %q does not contain %q", raw, want)
		}
	}
}

// TestGroupBuilderOrder covers group builder call orders that used to
// produce unreadable messages.
func TestGroupBuilderOrder(t *testing.T) {
	parse := func(t *testing.T, raw []byte) *ExecutionReport {
		t.Helper()
		msg, err := fix2.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		return WrapExecutionReport(&msg)
	}

	t.Run("nested group added before the delimiter", func(t *testing.T) {
		raw := BuildExecutionReport("FIXT.1.1").
			AddNoLegsEntry(NewNoLegsEntry().
				AddNoNested3PartyIDsEntry(NewNoNested3PartyIDsEntry().SetNested3PartyID("P")).
				SetLegSymbol("L")).
			Build()
		legs := parse(t, raw).NoLegsEntries()
		if len(legs) != 1 {
			t.Fatalf("%q: %d legs, want 1", raw, len(legs))
		}
		parties := legs[0].NoNested3PartyIDsEntries()
		if len(parties) != 1 {
			t.Fatalf("%q: %d parties, want 1", raw, len(parties))
		}
		if id, _ := parties[0].Nested3PartyID(); id != "P" {
			t.Errorf("party ID = %q", id)
		}
	})

	t.Run("setter between two Add calls", func(t *testing.T) {
		raw := BuildExecutionReport("FIXT.1.1").
			AddNoLegsEntry(NewNoLegsEntry().SetLegSymbol("A")).
			SetText("x").
			AddNoLegsEntry(NewNoLegsEntry().SetLegSymbol("B")).
			Build()
		er := parse(t, raw)
		legs := er.NoLegsEntries()
		if len(legs) != 2 {
			t.Fatalf("%q: %d legs, want 2", raw, len(legs))
		}
		for i, want := range []string{"A", "B"} {
			if got, _ := legs[i].LegSymbol(); got != want {
				t.Errorf("leg %d = %q, want %q", i, got, want)
			}
		}
		if text, _ := er.Text(); text != "x" {
			t.Errorf("Text = %q", text)
		}
	})

	t.Run("setter called twice", func(t *testing.T) {
		raw := BuildExecutionReport("FIXT.1.1").
			AddNoLegsEntry(NewNoLegsEntry().
				SetLegSymbol("A").SetLegSymbol("B").
				SetLegQty(1).SetLegQty(2)).
			Build()
		if n := bytes.Count(raw, []byte("\x01600=")); n != 1 {
			t.Errorf("%q: LegSymbol appears %d times, want 1", raw, n)
		}
		legs := parse(t, raw).NoLegsEntries()
		if len(legs) != 1 {
			t.Fatalf("%q: %d legs, want 1", raw, len(legs))
		}
		if sym, _ := legs[0].LegSymbol(); sym != "B" {
			t.Errorf("LegSymbol = %q, want B", sym)
		}
		if qty, _, _ := legs[0].LegQty(); qty != 2 {
			t.Errorf("LegQty = %v, want 2", qty)
		}
	})

	t.Run("entry changed after Add is built as changed", func(t *testing.T) {
		leg := NewNoLegsEntry().SetLegSymbol("A")
		b := BuildExecutionReport("FIXT.1.1").AddNoLegsEntry(leg)
		leg.SetLegQty(3)
		legs := parse(t, b.Build()).NoLegsEntries()
		if qty, ok, _ := legs[0].LegQty(); !ok || qty != 3 {
			t.Errorf("LegQty = %v (%v), want 3", qty, ok)
		}
	})
}

// TestGroupBuilderReview covers the issues found in review of the
// structural group builders.
func TestGroupBuilderReview(t *testing.T) {
	parse := func(t *testing.T, raw []byte) *ExecutionReport {
		t.Helper()
		msg, err := fix2.Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		return WrapExecutionReport(&msg)
	}

	t.Run("embedded setter ends the chain", func(t *testing.T) {
		// SetString is the embedded *fix2.Builder's: its Build must still
		// serialize the groups.
		raw := BuildExecutionReport("FIXT.1.1").
			AddNoLegsEntry(NewNoLegsEntry().SetLegSymbol("A")).
			SetString(TagText, "S").
			Build()
		if n := len(parse(t, raw).NoLegsEntries()); n != 1 {
			t.Errorf("%q: %d legs, want 1", raw, n)
		}
	})

	t.Run("nested group in dictionary position", func(t *testing.T) {
		raw := BuildExecutionReport("FIXT.1.1").
			AddNoLegsEntry(NewNoLegsEntry().
				SetLegSymbol("L").
				SetLegQty(1).
				AddNoLegStipulationsEntry(NewNoLegStipulationsEntry().SetLegStipulationType("T")).
				SetLegPositionEffect("O")).
			Build()
		// The dictionary puts the LegStipulations component before
		// LegPositionEffect.
		stip, effect := bytes.Index(raw, []byte("\x01683=")), bytes.Index(raw, []byte("\x01564="))
		if stip < 0 || effect < 0 || stip > effect {
			t.Errorf("%q: NoLegStipulations at %d, LegPositionEffect at %d", raw, stip, effect)
		}
		legs := parse(t, raw).NoLegsEntries()
		if len(legs) != 1 || len(legs[0].NoLegStipulationsEntries()) != 1 {
			t.Fatalf("%q: legs/stipulations not read back", raw)
		}
		if pe, _ := legs[0].LegPositionEffect(); pe != "O" {
			t.Errorf("LegPositionEffect = %q", pe)
		}
	})

	t.Run("raw counter and typed entries", func(t *testing.T) {
		b := BuildExecutionReport("FIXT.1.1")
		b.SetInt(TagNoLegs, 0)
		b.AddNoLegsEntry(NewNoLegsEntry().SetLegSymbol("A"))
		raw := b.Build()
		if n := bytes.Count(raw, []byte("\x01555=")); n != 1 {
			t.Errorf("%q: %d NoLegs counters, want 1", raw, n)
		}
		if n := len(parse(t, raw).NoLegsEntries()); n != 1 {
			t.Errorf("%q: %d legs, want 1", raw, n)
		}
	})

	t.Run("DATA value copied at Set", func(t *testing.T) {
		// The DATA setter also sets EncodedLegIssuerLen, right before it.
		buf := []byte("a\x01c")
		leg := NewNoLegsEntry().SetLegSymbol("L").SetEncodedLegIssuer(buf)
		b := BuildExecutionReport("FIXT.1.1").AddNoLegsEntry(leg)
		copy(buf, "xyz")
		legs := parse(t, b.Build()).NoLegsEntries()
		if got, _ := legs[0].EncodedLegIssuer(); string(got) != "a\x01c" {
			t.Errorf("EncodedLegIssuer = %q, want a\\x01c", got)
		}
		if n, _, _ := legs[0].EncodedLegIssuerLen(); n != 3 {
			t.Errorf("EncodedLegIssuerLen = %d, want 3", n)
		}
	})

	t.Run("repeated Build", func(t *testing.T) {
		b := BuildExecutionReport("FIXT.1.1").AddNoLegsEntry(NewNoLegsEntry().SetLegSymbol("A"))
		if first, second := b.Build(), b.Build(); !bytes.Equal(first, second) {
			t.Errorf("Build is not repeatable:\n%q\n%q", first, second)
		}
	})
}

// TestDataSetters checks that message-level DATA setters emit the LENGTH
// field right before the DATA field, even after other fields, so the parser
// reads the value by length.
func TestDataSetters(t *testing.T) {
	raw := BuildExecutionReport("FIXT.1.1").
		SetEncodedText([]byte("one")).
		SetText("t").
		SetEncodedText([]byte("t\x01wo")). // replaces, still adjacent to its length
		Build()
	msg, err := fix2.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	er := WrapExecutionReport(&msg)
	if got, _ := er.EncodedText(); string(got) != "t\x01wo" {
		t.Errorf("EncodedText = %q", got)
	}
	if n, _, _ := er.EncodedTextLen(); n != 4 {
		t.Errorf("EncodedTextLen = %d, want 4", n)
	}
}

// TestBuilderCopySharesGroups checks that a builder copied by value shares
// its groups with the original, as it shares the embedded Builder.
func TestBuilderCopySharesGroups(t *testing.T) {
	cp := *BuildExecutionReport("FIXT.1.1")
	cp.AddNoLegsEntry(NewNoLegsEntry().SetLegSymbol("A"))
	msg, err := fix2.Parse(cp.Build())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(WrapExecutionReport(&msg).NoLegsEntries()); n != 1 {
		t.Errorf("copy built %d legs, want 1", n)
	}
}

// TestTimestampPrecision reads UTCTimestamp values written with each
// fraction precision FIX allows, picoseconds being truncated.
func TestTimestampPrecision(t *testing.T) {
	ts := time.Date(2001, 12, 17, 9, 30, 47, 123_456_789, time.UTC)
	for _, c := range []struct {
		layout string
		want   time.Time
	}{
		{fix2.TimeFormatTimestampSec, ts.Truncate(time.Second)},
		{fix2.TimeFormatTimestamp, ts.Truncate(time.Millisecond)},
		{fix2.TimeFormatTimestampMicro, ts.Truncate(time.Microsecond)},
		{fix2.TimeFormatTimestampNano, ts},
		{fix2.TimeFormatTimestampNano + "000", ts},
	} {
		raw := BuildExecutionReport("FIXT.1.1").SetTransactTimeWithFormat(ts, c.layout).Build()
		msg, err := fix2.Parse(raw)
		if err != nil {
			t.Fatalf("%s: Parse: %v", c.layout, err)
		}
		got, ok, err := WrapExecutionReport(&msg).TransactTime()
		if !ok || err != nil || !got.Equal(c.want) {
			t.Errorf("%s: TransactTime = %v, %v, %v; want %v", c.layout, got, ok, err, c.want)
		}
	}
}
