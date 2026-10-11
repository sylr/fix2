package main

import (
	"slices"
	"testing"
)

func TestNormDecimal(t *testing.T) {
	for in, want := range map[string]string{
		"10.00000": "10", "0.01000": "0.01", "007.50": "7.5", "0.00000000": "0", "0": "0", "100": "100", ".5": "0.5", "": "0",
	} {
		if got := normDecimal(in); got != want {
			t.Errorf("normDecimal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpread(t *testing.T) {
	for _, c := range []struct{ bid, ask, want string }{
		{"62000", "62000.01", "0.01"}, {"99.5", "100", "0.5"}, {"0.00001", "0.0001", "0.00009"},
	} {
		if got := spread(c.bid, c.ask); got != c.want {
			t.Errorf("spread(%s, %s) = %s, want %s", c.bid, c.ask, got, c.want)
		}
	}
}

func TestBook_Levels(t *testing.T) {
	var b book
	b.load([]level{{"9", "1"}, {"9.5", "2"}}, []level{{"11", "1"}, {"10.5", "1"}})
	b.set(true, "9.50", "3")   // changed
	b.set(true, "9.25", "1")   // new
	b.set(false, "10.5", "0")  // deleted
	b.set(false, "12.00", "0") // deleting an absent level
	if want := []level{{"9.5", "3"}, {"9.25", "1"}, {"9", "1"}}; !slices.Equal(b.bids, want) {
		t.Errorf("bids %v, want %v", b.bids, want)
	}
	if want := []level{{"11", "1"}}; !slices.Equal(b.asks, want) {
		t.Errorf("asks %v, want %v", b.asks, want)
	}
}
