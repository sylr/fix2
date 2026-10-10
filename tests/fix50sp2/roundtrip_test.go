package fix50sp2

import (
	"testing"

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
