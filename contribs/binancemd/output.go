package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// The events written, in JSON as one object per line.
type (
	tradeEvent struct {
		Type      string    `json:"type"` // "trade"
		Symbol    string    `json:"symbol"`
		Time      time.Time `json:"time"`
		ID        int64     `json:"trade_id"`
		Price     string    `json:"price"`
		Qty       string    `json:"qty"`
		Aggressor string    `json:"aggressor,omitempty"` // "buy" or "sell"
	}
	tickerEvent struct {
		Type     string    `json:"type"` // "ticker"
		Symbol   string    `json:"symbol"`
		Time     time.Time `json:"time"`
		UpdateID int64     `json:"update_id,omitempty"`
		Bid      *level    `json:"bid"`
		Ask      *level    `json:"ask"`
	}
	// bookEvent is a snapshot ("snapshot") or the best levels of a book
	// kept up to date ("book").
	bookEvent struct {
		Type     string    `json:"type"`
		Symbol   string    `json:"symbol"`
		Time     time.Time `json:"time"`
		UpdateID int64     `json:"last_update_id"`
		Bids     []level   `json:"bids"`
		Asks     []level   `json:"asks"`
	}
	depthEvent struct {
		Type          string    `json:"type"` // "depth"
		Symbol        string    `json:"symbol"`
		Time          time.Time `json:"time"`
		FirstUpdateID int64     `json:"first_update_id,omitempty"`
		LastUpdateID  int64     `json:"last_update_id,omitempty"`
		Changes       []change  `json:"changes"`
	}
	change struct {
		Side   string `json:"side"`   // "bid" or "ask"
		Action string `json:"action"` // "new", "change" or "delete"
		Price  string `json:"price"`
		Qty    string `json:"qty,omitempty"`
	}
	instrumentEvent struct {
		Type          string `json:"type"` // "instrument"
		Symbol        string `json:"symbol"`
		Currency      string `json:"currency"`
		MinQty        string `json:"min_qty,omitempty"`
		MaxQty        string `json:"max_qty,omitempty"`
		QtyStep       string `json:"qty_step,omitempty"`
		MarketMinQty  string `json:"market_min_qty,omitempty"`
		MarketMaxQty  string `json:"market_max_qty,omitempty"`
		MarketQtyStep string `json:"market_qty_step,omitempty"`
		TickSize      string `json:"tick_size,omitempty"`
		MinPrice      string `json:"min_price,omitempty"`
		MaxPrice      string `json:"max_price,omitempty"`
	}
	limitEvent struct {
		Type     string `json:"type"`  // "limit"
		Limit    string `json:"limit"` // "orders", "messages" or "subscriptions"
		Count    int64  `json:"count"`
		Max      int64  `json:"max"`
		Interval string `json:"interval,omitempty"` // such as "60s"
	}
)

// output renders events, and hands them to a goroutine of its own, the only
// one writing them: a consumer that stalls blocks neither the client's lock
// nor the session's reader. Its methods are called from one goroutine at a
// time: FromApp's, then the command's once the session's callbacks are done.
type output interface {
	trade(e *tradeEvent)
	ticker(e *tickerEvent)
	book(e *bookEvent)
	depth(e *depthEvent)
	instrument(e *instrumentEvent)
	limit(e *limitEvent)
	// flush hands what was rendered, but tables, to the writing goroutine,
	// waiting while its queue is full. It fails once writing did.
	flush() error
	// end renders everything and waits, until ctx is done, for it to be
	// written: the command ends.
	end(ctx context.Context) error
}

// outputQueue bounds the flushes waiting to be written: once it is full,
// FromApp waits, and the session sees a stalled handler (see
// session.Config.MaxDeliveryStall), rather than events being dropped.
const outputQueue = 1024

// errOutputBlocked ends a command whose output stayed blocked once the
// session stopped.
var errOutputBlocked = errors.New("output blocked: what was received is not all written")

func newOutput(w io.Writer, jsonLines, terminal bool) output {
	p := &pipe{w: w, chunks: make(chan []byte, outputQueue), failed: make(chan struct{}), done: make(chan struct{})}
	if jsonLines {
		return &jsonOutput{pipe: p, enc: json.NewEncoder(&p.buf)}
	}
	return &textOutput{pipe: p, w: &p.buf, terminal: terminal, books: map[string]string{}}
}

// pipe holds the events an output rendered, and hands them to its writing
// goroutine, started on the first flush.
type pipe struct {
	buf    bytes.Buffer // rendered, not yet flushed
	w      io.Writer
	start  sync.Once
	chunks chan []byte
	failed chan struct{} // closed once writing failed, with err
	done   chan struct{} // closed once the writing goroutine returned, with err
	err    error
}

// run writes the chunks flushed, in order, until end: through a buffer,
// itself flushed whenever no chunk waits.
func (p *pipe) run() {
	defer close(p.done)
	bw := bufio.NewWriter(p.w)
	for b := range p.chunks {
		if p.err != nil {
			continue // dropped: end waits for the queue to be drained
		}
		_, err := bw.Write(b)
		if err == nil && len(p.chunks) == 0 {
			err = bw.Flush()
		}
		if err != nil {
			p.err = err
			close(p.failed)
		}
	}
}

// send hands what was rendered to the writing goroutine, waiting until ctx
// is done while its queue is full. Once writing failed, it drops it and
// fails.
func (p *pipe) send(ctx context.Context) error {
	p.start.Do(func() { go p.run() })
	select {
	case <-p.failed:
		p.buf.Reset()
		return p.err
	default:
	}
	if p.buf.Len() == 0 {
		return nil
	}
	b := bytes.Clone(p.buf.Bytes())
	p.buf.Reset()
	select {
	case p.chunks <- b:
		return nil
	case <-p.failed:
		return p.err
	case <-ctx.Done():
		return errOutputBlocked
	}
}

func (p *pipe) flush() error { return p.send(context.Background()) }

func (p *pipe) end(ctx context.Context) error {
	err := p.send(ctx)
	close(p.chunks)
	if err != nil {
		return err
	}
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return errOutputBlocked
	}
}

// jsonOutput writes JSON lines.
type jsonOutput struct {
	*pipe
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
func (o *jsonOutput) limit(e *limitEvent)           { o.write(e) }

// textOutput writes lines for people. On a terminal, books are redrawn in
// place.
type textOutput struct {
	*pipe
	w        *bytes.Buffer // the pipe's
	terminal bool
	books    map[string]string // the last drawing of each book, on a terminal
	table    *tabwriter.Writer // instruments and limits, aligned once complete
}

const timeFormat = "15:04:05.000000"

func (o *textOutput) trade(e *tradeEvent) {
	side := ""
	if e.Aggressor != "" {
		side = " " + strings.ToUpper(e.Aggressor)
	}
	fmt.Fprintf(o.w, "%s %s trade %d%s %s @ %s\n", e.Time.Format(timeFormat), e.Symbol, e.ID, side, e.Qty, e.Price)
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
		fmt.Fprintf(o.w, "%s %s snapshot at update %d: %d bids, %d asks", e.Time.Format(timeFormat), e.Symbol, e.UpdateID, len(e.Bids), len(e.Asks))
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
	fmt.Fprintf(&sb, "%s  update %d  %s", e.Symbol, e.UpdateID, e.Time.Format(timeFormat))
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
	ids := ""
	if e.LastUpdateID != 0 {
		ids = fmt.Sprintf(" [%d-%d]", e.FirstUpdateID, e.LastUpdateID)
	}
	for _, c := range e.Changes {
		qty := ""
		if c.Qty != "" {
			qty = " " + c.Qty
		}
		fmt.Fprintf(o.w, "%s %s %s %s %s%s%s\n", e.Time.Format(timeFormat), e.Symbol, c.Side, c.Action, c.Price, qty, ids)
	}
}

func (o *textOutput) instrument(e *instrumentEvent) {
	if o.table == nil {
		o.table = tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(o.table, "SYMBOL\tCCY\tMIN QTY\tMAX QTY\tQTY STEP\tMKT MIN QTY\tMKT MAX QTY\tMKT QTY STEP\tTICK\tMIN PRICE\tMAX PRICE")
	}
	fmt.Fprintf(o.table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Symbol, e.Currency, dash(e.MinQty), dash(e.MaxQty), dash(e.QtyStep),
		dash(e.MarketMinQty), dash(e.MarketMaxQty), dash(e.MarketQtyStep), dash(e.TickSize), dash(e.MinPrice), dash(e.MaxPrice))
}

func (o *textOutput) limit(e *limitEvent) {
	if o.table == nil {
		o.table = tabwriter.NewWriter(o.w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(o.table, "LIMIT\tCOUNT\tMAX\tINTERVAL")
	}
	fmt.Fprintf(o.table, "%s\t%d\t%d\t%s\n", e.Limit, e.Count, e.Max, dash(e.Interval))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (o *textOutput) end(ctx context.Context) error {
	if o.table != nil {
		o.table.Flush()
	}
	return o.pipe.end(ctx)
}
