package main

import (
	"fmt"
	"slices"
	"strings"
)

// level is a price level: its price and quantity, as decimal strings.
type level struct {
	Price string
	Qty   string
}

// MarshalJSON writes l as ["price","qty"], as Binance's other APIs do.
func (l level) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "[%q,%q]", l.Price, l.Qty), nil
}

// book is an order book of price levels, kept up to date from a snapshot
// and the changes that follow it, in the order received: Coinbase's L2
// updates carry no sequence number, the FIX session's guarantee none is
// lost.
type book struct {
	symbol     string
	synced     bool    // a snapshot was loaded
	bids, asks []level // best first
}

// load replaces the book with a snapshot.
func (b *book) load(bids, asks []level) {
	b.bids, b.asks = b.bids[:0], b.asks[:0]
	b.synced = true
	for _, l := range bids {
		b.set(true, l.Price, l.Qty)
	}
	for _, l := range asks {
		b.set(false, l.Price, l.Qty)
	}
}

// set sets the level at price of the bids, or the asks, to qty, removing it
// if qty is zero.
func (b *book) set(bid bool, price, qty string) {
	price, qty = normDecimal(price), normDecimal(qty)
	side := &b.asks
	cmp := func(l level, p string) int { return cmpDecimal(l.Price, p) }
	if bid {
		side = &b.bids
		cmp = func(l level, p string) int { return cmpDecimal(p, l.Price) }
	}
	i, found := slices.BinarySearchFunc(*side, price, cmp)
	switch {
	case qty == "0" && found:
		*side = slices.Delete(*side, i, i+1)
	case qty == "0":
	case found:
		(*side)[i].Qty = qty
	default:
		*side = slices.Insert(*side, i, level{price, qty})
	}
}

// top returns the n best levels of each side.
func (b *book) top(n int) (bids, asks []level) {
	return b.bids[:min(n, len(b.bids))], b.asks[:min(n, len(b.asks))]
}

// normDecimal returns the non-negative decimal s without leading zeros in
// its integer part, nor trailing zeros in its fraction.
func normDecimal(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	s = strings.TrimLeft(s, "0")
	if s == "" || s[0] == '.' {
		s = "0" + s
	}
	return s
}

// cmpDecimal compares the normalized non-negative decimals a and b.
func cmpDecimal(a, b string) int {
	ai, af, _ := strings.Cut(a, ".")
	bi, bf, _ := strings.Cut(b, ".")
	if c := len(ai) - len(bi); c != 0 {
		return c
	}
	if c := strings.Compare(ai, bi); c != 0 {
		return c
	}
	// Without trailing zeros, fractions compare as strings.
	return strings.Compare(af, bf)
}
