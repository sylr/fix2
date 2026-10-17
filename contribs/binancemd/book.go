package main

import (
	"errors"
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

// errGap reports updates missing between a book and the update received.
var errGap = errors.New("book updates missing")

// book is an order book kept up to date from a snapshot and the diff depth
// updates that follow it.
type book struct {
	symbol string
	synced bool  // a snapshot was loaded
	fresh  bool  // no update applied since the snapshot
	lastID int64 // the last update applied, or the snapshot's
	// curFirst and curLast identify the update being applied: its entries
	// share them.
	curFirst, curLast int64
	bids, asks        []level // best first
}

// load replaces the book with a snapshot whose last update is lastID.
func (b *book) load(lastID int64, bids, asks []level) {
	b.bids, b.asks = b.bids[:0], b.asks[:0]
	b.synced, b.fresh, b.lastID = true, true, lastID
	b.curFirst, b.curLast = 0, 0
	for _, l := range bids {
		b.set(true, l.Price, l.Qty)
	}
	for _, l := range asks {
		b.set(false, l.Price, l.Qty)
	}
}

// apply applies an entry of update first to last: a level of the bids, or
// the asks, at price changes to qty, or is deleted. Updates the snapshot
// covers are skipped: it reports whether the entry was applied. errGap
// reports an update that does not follow the book.
func (b *book) apply(first, last int64, bid, del bool, price, qty string) (bool, error) {
	if !b.synced {
		return false, errGap
	}
	if last != 0 && (first != b.curFirst || last != b.curLast) {
		// The first entry of an update.
		switch {
		case last <= b.lastID:
			return false, nil // already in the book
		case b.fresh && first > b.lastID+1, !b.fresh && first != b.lastID+1:
			b.synced = false
			return false, fmt.Errorf("%w: book at update %d, received updates %d to %d", errGap, b.lastID, first, last)
		}
		b.curFirst, b.curLast, b.lastID, b.fresh = first, last, last, false
	}
	if del {
		qty = "0"
	}
	b.set(bid, price, qty)
	return true, nil
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

// has reports whether the bids, or the asks, have a level at price.
func (b *book) has(bid bool, price string) bool {
	price = normDecimal(price)
	side := b.asks
	cmp := func(l level, p string) int { return cmpDecimal(l.Price, p) }
	if bid {
		side = b.bids
		cmp = func(l level, p string) int { return cmpDecimal(p, l.Price) }
	}
	_, found := slices.BinarySearchFunc(side, price, cmp)
	return found
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
