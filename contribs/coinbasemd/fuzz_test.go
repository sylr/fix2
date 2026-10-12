package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// fuzzSymbols are the symbols the fuzzed commands ask for.
var fuzzSymbols = []string{"BTC-USD", "ETH-USD"}

// fuzzFrame returns the FIX message of body, the fields after MsgType's
// predecessors, '|' standing for SOH: it adds the BeginString, BodyLength and
// CheckSum, so that fuzzing reaches the handlers.
func fuzzFrame(body string) []byte {
	body = strings.ReplaceAll(body, "|", "\x01") + "\x01"
	msg := fmt.Sprintf("8=FIXT.1.1\x019=%d\x01%s", len(body), body)
	var sum byte
	for i := range len(msg) {
		sum += msg[i]
	}
	return fmt.Appendf(nil, "%s10=%03d\x01", msg, sum)
}

// fuzzClient returns a client of command kind k as OnLogon leaves it, with
// the subscriptions it would have made, writing to io.Discard.
func fuzzClient(k kind, jsonLines, terminal bool, show int) *client {
	ctx, finish := context.WithCancelCause(context.Background())
	c := &client{
		cmd:     command{k},
		symbols: fuzzSymbols,
		show:    show,
		out:     newOutput(io.Discard, jsonLines, terminal),
		log:     slog.New(slog.DiscardHandler),
		ctx:     ctx,
		finish:  finish,
		sent:    map[uint64]string{},
		subs:    map[string]*sub{},
		quotes:  map[string]*tickerEvent{},
		books:   map[string]*book{},
		listed:  map[string]bool{},
	}
	c.loggedOn, c.connected = true, true
	if k == kindInstruments {
		c.query = c.nextID("INSTRUMENTS", "")
		c.sent[2] = c.query
		return c
	}
	prefix := map[kind]string{kindTrades: "TRADES", kindTicker: "TICKER"}[k]
	if prefix == "" {
		prefix = "DEPTH"
	}
	for i, sym := range fuzzSymbols {
		id := c.nextID(prefix, sym)
		c.sent[uint64(2+i)] = id
		c.subs[id] = &sub{id: id, symbol: sym}
	}
	return c
}

// feed hands the messages of input, one per line, to c as the session does:
// "$ID" stands for the ID of the first request, "$ID2" of the second.
func feed(c *client, input string) {
	ids := map[string]string{"$ID2": "", "$ID": c.query}
	for id, sb := range c.subs {
		if sb.symbol == fuzzSymbols[0] {
			ids["$ID"] = id
		} else {
			ids["$ID2"] = id
		}
	}
	input = strings.NewReplacer("$ID2", ids["$ID2"], "$ID", ids["$ID"]).Replace(input)
	for line := range strings.SplitSeq(input, "\n") {
		var m fix2.Message
		if err := fix2.ParseInto(&m, fuzzFrame(line), nil); err != nil {
			continue
		}
		in := session.Inbound{Received: time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)}
		switch string(m.MsgType()) {
		case "0", "1", "2", "3", "4", "5", "A":
			c.FromAdmin(context.Background(), nil, &m, in)
		default:
			_ = c.FromApp(context.Background(), nil, &m, in)
		}
	}
	_ = c.out.end(context.Background())
}

// FuzzClient feeds every command messages as hostile as the exchange could
// send them: the client must not panic.
func FuzzClient(f *testing.F) {
	const (
		snap  = "35=W|262=$ID|893=Y|55=BTC-USD|268=2|269=0|270=99.00|271=1|269=1|270=101.00|271=4"
		trade = "35=X|262=$ID|268=1|279=0|269=2|55=BTC-USD|270=62000.01|271=0.01|60=20260612-10:00:00.123456|37=ord-1|5797=1"
		inc   = "35=X|262=$ID|268=2|279=1|269=0|55=BTC-USD|270=99.5|271=1|279=2|269=1|270=101.00"
		list  = "35=y|320=$ID|322=R1|560=0|393=2|893=Y|146=2|55=BTC-USD|15=USD|562=1.00|969=0.01|29003=0.00000001|1682=full_trading|55=ETH-USD"
	)
	for k := range 5 {
		for flags := range uint8(4) {
			f.Add(uint8(k), flags, uint8(3), snap+"\n"+inc+"\n"+trade+"\n"+list)
		}
	}
	f.Add(uint8(kindBook), uint8(0), uint8(10), "35=W|262=$ID|893=N|55=BTC-USD|268=1|269=0|270=-1|271=NaN\n35=W|262=$ID|893=Y|268=1|269=1|270=1e999|271=")
	f.Add(uint8(kindBook), uint8(1), uint8(1), snap+"\n35=X|262=$ID|268=3|279=2|269=0|55=BTC-USD|270=5\n35=X|262=$ID|268=99|279=9|269=Z")
	f.Add(uint8(kindInstruments), uint8(0), uint8(0), "35=y|320=$ID|560=1|146=1|55=\n35=3|45=1|372=A|58=x\n35=5|58=bye\n35=j|45=1|380=0|58=x")
	f.Add(uint8(kindDepth), uint8(0), uint8(0), "35=Y|262=$ID|281=0|58=Unknown symbol\n35=Y|262=$ID2\n35=f|55=BTC-USD|1682=halted\n35=A|98=0")
	f.Fuzz(func(t *testing.T, k, flags, show uint8, input string) {
		c := fuzzClient(kind(k%5), flags&1 != 0, flags&2 != 0, int(show%12))
		feed(c, input)
	})
}

// FuzzBook applies arbitrary updates to a book: its sides stay sorted, and
// hold no level without a quantity.
func FuzzBook(f *testing.F) {
	f.Add("100.5 1\n99 2\n100.5 0\n101 3", "98 1\n102 1", true)
	f.Add("0 1\n-1 1\n.5 .5\n1e3 x\n00.10 0.1", "", false)
	f.Fuzz(func(t *testing.T, updates, snapshot string, bid bool) {
		parse := func(s string) (ls []level) {
			for line := range strings.SplitSeq(s, "\n") {
				if p, q, ok := strings.Cut(line, " "); ok {
					ls = append(ls, level{p, q})
				}
			}
			return ls
		}
		b := &book{symbol: "X"}
		b.load(parse(snapshot), parse(snapshot))
		for _, l := range parse(updates) {
			b.set(bid, l.Price, l.Qty)
			b.set(!bid, l.Price, l.Qty)
		}
		for _, side := range [][]level{b.bids, b.asks} {
			for _, l := range side {
				if l.Qty == "0" {
					t.Fatalf("level %v has no quantity", l)
				}
			}
		}
		for i := 1; i < len(b.bids); i++ {
			if cmpDecimal(b.bids[i-1].Price, b.bids[i].Price) <= 0 {
				t.Fatalf("bids not sorted: %v", b.bids)
			}
		}
		for i := 1; i < len(b.asks); i++ {
			if cmpDecimal(b.asks[i-1].Price, b.asks[i].Price) >= 0 {
				t.Fatalf("asks not sorted: %v", b.asks)
			}
		}
	})
}
