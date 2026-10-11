package main

import (
	"errors"
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

func TestCmpDecimal(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"10", "9.99", 1}, {"0.5", "0.51", -1}, {"1.1", "1.1", 0}, {"100", "99", 1}, {"0.001", "0.01", -1},
	} {
		got := cmpDecimal(c.a, c.b)
		if got > 0 {
			got = 1
		} else if got < 0 {
			got = -1
		}
		if got != c.want {
			t.Errorf("cmpDecimal(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSpread(t *testing.T) {
	for _, c := range []struct{ bid, ask, want string }{
		{"62000", "62000.01", "0.01"}, {"99.5", "100", "0.5"}, {"0.00001", "0.0001", "0.00009"}, {"10", "12", "2"},
	} {
		if got := spread(c.bid, c.ask); got != c.want {
			t.Errorf("spread(%s, %s) = %s, want %s", c.bid, c.ask, got, c.want)
		}
	}
}

func TestBook_Sequencing(t *testing.T) {
	var b book
	if _, err := b.apply(1, 2, true, false, "1", "1"); !errors.Is(err, errGap) {
		t.Fatalf("update before a snapshot: %v", err)
	}
	b.load(10, []level{{"9", "1"}, {"9.5", "2"}}, []level{{"11", "1"}, {"10.5", "1"}})
	if want := []level{{"9.5", "2"}, {"9", "1"}}; !slices.Equal(b.bids, want) {
		t.Errorf("bids %v, want %v", b.bids, want)
	}
	if want := []level{{"10.5", "1"}, {"11", "1"}}; !slices.Equal(b.asks, want) {
		t.Errorf("asks %v, want %v", b.asks, want)
	}
	steps := []struct {
		first, last int64
		bid, del    bool
		price, qty  string
		gap         bool
	}{
		{5, 10, true, false, "9", "100", false},  // covered by the snapshot: skipped
		{9, 12, true, false, "9", "3", false},    // straddles the snapshot
		{9, 12, false, true, "10.5", "", false},  // same update, another entry
		{13, 13, true, false, "9.5", "0", false}, // zero quantity: deleted
		{15, 16, true, false, "8", "1", true},    // 14 missing
	}
	for i, s := range steps {
		_, err := b.apply(s.first, s.last, s.bid, s.del, s.price, s.qty)
		if gap := errors.Is(err, errGap); gap != s.gap {
			t.Fatalf("step %d: error %v, want gap %v", i, err, s.gap)
		}
	}
	if want := []level{{"9", "3"}}; !slices.Equal(b.bids, want) {
		t.Errorf("bids %v, want %v", b.bids, want)
	}
	if want := []level{{"11", "1"}}; !slices.Equal(b.asks, want) {
		t.Errorf("asks %v, want %v", b.asks, want)
	}
	if b.lastID != 13 || b.synced {
		t.Errorf("lastID %d synced %v after a gap", b.lastID, b.synced)
	}
}

func TestBook_FirstUpdateAfterSnapshotMustReachIt(t *testing.T) {
	var b book
	b.load(10, nil, nil)
	if _, err := b.apply(12, 13, true, false, "1", "1"); !errors.Is(err, errGap) {
		t.Errorf("update 12-13 after snapshot 10: %v", err)
	}
}
