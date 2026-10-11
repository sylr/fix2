package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// The events written, in JSON as one object per line.
type (
	tradeEvent struct {
		Type      string    `json:"type"` // "trade"
		Symbol    string    `json:"symbol"`
		Time      time.Time `json:"time"`
		ID        string    `json:"trade_id,omitempty"`
		Price     string    `json:"price"`
		Qty       string    `json:"qty"`
		Aggressor string    `json:"aggressor,omitempty"` // "buy" or "sell"
	}
	tickerEvent struct {
		Type   string    `json:"type"` // "ticker"
		Symbol string    `json:"symbol"`
		Time   time.Time `json:"time"`
		Bid    *level    `json:"bid"`
		Ask    *level    `json:"ask"`
	}
	// bookEvent is a snapshot ("snapshot") or the best levels of a book
	// kept up to date ("book").
	bookEvent struct {
		Type   string    `json:"type"`
		Symbol string    `json:"symbol"`
		Time   time.Time `json:"time"`
		Bids   []level   `json:"bids"`
		Asks   []level   `json:"asks"`
	}
	depthEvent struct {
		Type    string    `json:"type"` // "depth"
		Symbol  string    `json:"symbol"`
		Time    time.Time `json:"time"`
		Changes []change  `json:"changes"`
	}
	change struct {
		Side   string `json:"side"`   // "bid" or "ask"
		Action string `json:"action"` // "new", "change" or "delete"
		Price  string `json:"price"`
		Qty    string `json:"qty,omitempty"`
	}
	instrumentEvent struct {
		Type        string `json:"type"` // "instrument"
		Symbol      string `json:"symbol"`
		Currency    string `json:"currency,omitempty"`     // the quote currency
		MinNotional string `json:"min_notional,omitempty"` // in the quote currency
		TickSize    string `json:"tick_size,omitempty"`
		SizeStep    string `json:"size_step,omitempty"`
		Status      string `json:"status,omitempty"` // such as "full_trading"
	}
)

// output writes events. Its methods are called from one goroutine at a time.
type output interface {
	trade(e *tradeEvent)
	ticker(e *tickerEvent)
	book(e *bookEvent)
	depth(e *depthEvent)
	instrument(e *instrumentEvent)
	// flush writes what is buffered, but tables.
	flush() error
	// end writes everything: the command ends.
	end() error
}

func newOutput(w io.Writer, jsonLines, terminal bool) output {
	bw := bufio.NewWriter(w)
	if jsonLines {
		return &jsonOutput{w: bw, enc: json.NewEncoder(bw)}
	}
	return &textOutput{w: bw, terminal: terminal, books: map[string]string{}}
}

// jsonOutput writes JSON lines.
type jsonOutput struct {
	w   *bufio.Writer
	enc *json.Encoder
}

func (o *jsonOutput) write(v any) {
	// The events always encode.
	_ = o.enc.Encode(v)
}

func (o *jsonOutput) trade(e *tradeEvent)           { o.write(e) }
func (o *jsonOutput) ticker(e *tickerEvent)         { o.write(e) }
func (o *jsonOutput) book(e *bookEvent)             { o.write(e) }
func (o *jsonOutput) depth(e *depthEvent)           { o.write(e) }
func (o *jsonOutput) instrument(e *instrumentEvent) { o.write(e) }
func (o *jsonOutput) flush() error                  { return o.w.Flush() }
func (o *jsonOutput) end() error                    { return o.w.Flush() }

// textOutput writes lines for people. On a terminal, books are redrawn in
// place.
type textOutput struct {
	w        *bufio.Writer
	terminal bool
	books    map[string]string // the last drawing of each book, on a terminal
	table    *tabwriter.Writer // instruments, aligned once complete
}

const timeFormat = "15:04:05.000000"

func (o *textOutput) trade(e *tradeEvent) {
	side := ""
	if e.Aggressor != "" {
		side = " " + strings.ToUpper(e.Aggressor)
	}
	id := ""
	if e.ID != "" {
		id = " " + e.ID
	}
	fmt.Fprintf(o.w, "%s %s trade%s%s %s @ %s\n", e.Time.Format(timeFormat), e.Symbol, id, side, e.Qty, e.Price)
}

func (o *textOutput) ticker(e *tickerEvent) {
	fmt.Fprintf(o.w, "%s %s bid %s  ask %s\n", e.Time.Format(timeFormat), e.Symbol, quote(e.Bid), quote(e.Ask))
}

func quote(l *level) string {
	if l == nil {
		return "-"
	}
	return l.Qty + " @ " + l.Price
}

func (o *textOutput) book(e *bookEvent) {
	if e.Type == "snapshot" {
		var best []string
		if len(e.Bids) > 0 {
			best = append(best, "best bid "+quote(&e.Bids[0]))
		}
		if len(e.Asks) > 0 {
			best = append(best, "best ask "+quote(&e.Asks[0]))
		}
		fmt.Fprintf(o.w, "%s %s snapshot: %d bids, %d asks", e.Time.Format(timeFormat), e.Symbol, len(e.Bids), len(e.Asks))
		if len(best) > 0 {
			fmt.Fprintf(o.w, "; %s", strings.Join(best, ", "))
		}
		fmt.Fprintln(o.w)
		return
	}
	drawing := drawBook(e)
	if !o.terminal {
		fmt.Fprintln(o.w, drawing)
		return
	}
	// Redraw every book, in symbol order, from the top of the screen.
	o.books[e.Symbol] = drawing
	fmt.Fprint(o.w, "\x1b[H\x1b[2J")
	for _, s := range slices.Sorted(maps.Keys(o.books)) {
		fmt.Fprintln(o.w, o.books[s])
	}
}

// drawBook draws the levels of e side by side, best first.
func drawBook(e *bookEvent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s  %s", e.Symbol, e.Time.Format(timeFormat))
	if len(e.Bids) > 0 && len(e.Asks) > 0 {
		fmt.Fprintf(&sb, "  spread %s", spread(e.Bids[0].Price, e.Asks[0].Price))
	}
	sb.WriteByte('\n')
	width := func(levels []level, f func(level) string, min int) int {
		w := min
		for _, l := range levels {
			w = max(w, len(f(l)))
		}
		return w
	}
	px := func(l level) string { return l.Price }
	qty := func(l level) string { return l.Qty }
	bq, bp := width(e.Bids, qty, 7), width(e.Bids, px, 6)
	ap, aq := width(e.Asks, px, 6), width(e.Asks, qty, 7)
	fmt.Fprintf(&sb, "%*s %*s | %-*s %-*s\n", bq, "BID QTY", bp, "BID", ap, "ASK", aq, "ASK QTY")
	for i := range max(len(e.Bids), len(e.Asks)) {
		var b, a level
		if i < len(e.Bids) {
			b = e.Bids[i]
		}
		if i < len(e.Asks) {
			a = e.Asks[i]
		}
		fmt.Fprintf(&sb, "%*s %*s | %-*s %-*s\n", bq, b.Qty, bp, b.Price, ap, a.Price, aq, a.Qty)
	}
	return sb.String()
}

// spread returns ask - bid, both decimals, exactly.
func spread(bid, ask string) string {
	bi, bf, _ := strings.Cut(bid, ".")
	ai, af, _ := strings.Cut(ask, ".")
	n := max(len(bf), len(af))
	pad := func(i, f string) string { return i + f + strings.Repeat("0", n-len(f)) }
	a, ok1 := new(big.Int).SetString(pad(ai, af), 10)
	b, ok2 := new(big.Int).SetString(pad(bi, bf), 10)
	if !ok1 || !ok2 {
		return "?"
	}
	d := a.Sub(a, b).String()
	neg := strings.HasPrefix(d, "-")
	d = strings.TrimPrefix(d, "-")
	if n > 0 {
		if len(d) <= n {
			d = strings.Repeat("0", n-len(d)+1) + d
		}
		d = d[:len(d)-n] + "." + d[len(d)-n:]
	}
	if neg {
		d = "-" + d
	}
	return d
}

func (o *textOutput) depth(e *depthEvent) {
	for _, c := range e.Changes {
		qty := ""
		if c.Qty != "" {
			qty = " " + c.Qty
		}
		fmt.Fprintf(o.w, "%s %s %s %s %s%s\n", e.Time.Format(timeFormat), e.Symbol, c.Side, c.Action, c.Price, qty)
	}
}

func (o *textOutput) instrument(e *instrumentEvent) {
	if o.table == nil {
		o.table = tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(o.table, "SYMBOL\tCCY\tMIN NOTIONAL\tTICK\tSIZE STEP\tSTATUS")
	}
	fmt.Fprintf(o.table, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Symbol, dash(e.Currency), dash(e.MinNotional), dash(e.TickSize), dash(e.SizeStep), dash(e.Status))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (o *textOutput) flush() error { return o.w.Flush() }

func (o *textOutput) end() error {
	if o.table != nil {
		o.table.Flush()
	}
	return o.w.Flush()
}
