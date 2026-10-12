package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// fuzzSymbols are the symbols the fuzzed commands ask for.
var fuzzSymbols = []string{"BTCUSDT", "ETHUSDT"}

// fuzzFrame returns the FIX message of body, the fields from MsgType on, '|'
// standing for SOH: it adds the BeginString, BodyLength and CheckSum, so that
// fuzzing reaches the handlers.
func fuzzFrame(body string) []byte {
	body = strings.ReplaceAll(body, "|", "\x01") + "\x01"
	msg := fmt.Sprintf("8=FIX.4.4\x019=%d\x01%s", len(body), body)
	var sum byte
	for i := range len(msg) {
		sum += msg[i]
	}
	return fmt.Appendf(nil, "%s10=%03d\x01", msg, sum)
}

// fuzzSession returns a session of its own to each iteration, never
// connected, for the handlers to send requests and log out on: sends fail,
// as on a connection lost, and logging out returns at once, so that what an
// input exercises does not depend on earlier ones nor on timing.
func fuzzSession(tb testing.TB) *session.Session {
	s, err := session.New(session.Config{
		ID:      session.SessionID{BeginString: beginString, SenderCompID: "FUZZ", TargetCompID: "SPOT"},
		Logger:  slog.New(slog.DiscardHandler),
		Handler: session.BaseHandler{},
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s
}

// fuzzClient returns a client of command kind k as OnLogon leaves it, with
// the requests it would have made, writing to io.Discard. Without symbols,
// instruments asks for all.
func fuzzClient(k kind, jsonLines, terminal, symbols bool, show, levels int) *client {
	ctx, finish := context.WithCancelCause(context.Background())
	c := &client{
		cmd:     command{k},
		symbols: fuzzSymbols,
		levels:  levels,
		show:    show,
		out:     newOutput(io.Discard, jsonLines, terminal),
		log:     slog.New(slog.DiscardHandler),
		ctx:     ctx,
		finish:  finish,
		sent:    map[uint64]string{},
		subs:    map[string]*sub{},
		quotes:  map[string]*tickerEvent{},
		books:   map[string]*book{},
		queries: map[string]bool{},
	}
	c.loggedOn, c.connected = true, true
	var prefix string
	switch k {
	case kindLimits:
		id := c.nextID("LIMITS", "")
		c.queries[id], c.sent[2] = false, id
		return c
	case kindInstruments:
		if !symbols {
			c.symbols = nil
			id := c.nextID("INSTRUMENTS", "")
			c.queries[id], c.sent[2] = false, id
			return c
		}
		for i, sym := range fuzzSymbols {
			id := c.nextID("INSTRUMENT", sym)
			c.queries[id], c.sent[uint64(2+i)] = false, id
		}
		return c
	case kindTrades:
		prefix = "TRADES"
	case kindTicker:
		prefix = "TICKER"
	default:
		prefix = "DEPTH"
	}
	for i, sym := range fuzzSymbols {
		id := c.nextID(prefix, sym)
		c.sent[uint64(2+i)] = id
		c.subs[id] = &sub{id: id, symbol: sym}
	}
	return c
}

// requestIDs returns the IDs of the first and the second request of c.
func requestIDs(c *client) (first, second string) {
	n := func(id string) int { return len(id) - len(strings.TrimRight(id, "0123456789")) }
	for _, id := range append(mapKeys(c.subs), mapKeys(c.queries)...) {
		i, _ := strconv.Atoi(id[len(id)-n(id):])
		if i == 1 {
			first = id
		} else {
			second = id
		}
	}
	return first, second
}

func mapKeys[V any](m map[string]V) (keys []string) {
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// feed hands the messages of input, one per line, to c as the session does:
// "$ID" stands for the ID of the first request, "$ID2" of the second.
func feed(c *client, s *session.Session, input string) {
	first, second := requestIDs(c)
	input = strings.NewReplacer("$ID2", second, "$ID", first).Replace(input)
	for line := range strings.SplitSeq(input, "\n") {
		var m fix2.Message
		if err := fix2.ParseInto(&m, fuzzFrame(line), nil); err != nil {
			continue
		}
		in := session.Inbound{Received: time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)}
		switch string(m.MsgType()) {
		case "0", "1", "2", "3", "4", "5", "A":
			c.FromAdmin(context.Background(), s, &m, in)
		default:
			_ = c.FromApp(context.Background(), s, &m, in)
		}
	}
	_ = c.out.end(context.Background())
	if c.quiet != nil {
		c.quiet.Stop()
	}
}

// FuzzClient feeds every command messages as hostile as the exchange could
// send them: the client must not panic.
func FuzzClient(f *testing.F) {
	const (
		snap  = "35=W|262=$ID|55=BTCUSDT|25044=100|268=4|269=0|270=99.00|271=1.0|269=0|270=98.50|271=2.0|269=1|270=100.00|271=3.0|269=1|270=101.00|271=4.0"
		trade = "35=X|262=$ID|268=2|279=0|269=2|270=10.00000|271=0.01000|55=BTCUSDT|1003=7|60=20241019-05:40:11.464000|2446=1|279=0|269=2|270=10.1|271=1.5|1003=8|2446=2"
		inc   = "35=X|262=$ID|268=2|279=1|270=99.50|271=0.5|269=0|55=BTCUSDT|25043=99|25044=101|279=2|270=100.00|269=1"
		list  = "35=y|320=$ID|146=2|55=BTCUSDT|15=USDT|562=0.00001000|1140=9000.00000000|25039=0.00001000|969=0.01000000|55=ETHUSDT|15=USDT"
		limit = "35=XLR|6136=$ID|25003=2|25004=1|25005=3|25006=100|25007=10|25008=s|25004=2|25005=1|25006=5"
	)
	for k := range 6 {
		for flags := range uint8(8) {
			f.Add(uint8(k), flags, uint8(3), uint8(10), snap+"\n"+inc+"\n"+trade+"\n"+list+"\n"+limit)
		}
	}
	f.Add(uint8(kindBook), uint8(0), uint8(10), uint8(10), snap+"\n35=X|262=$ID|268=1|279=0|269=0|270=98|271=1|55=BTCUSDT|25043=103|25044=104\n35=B|148=maintenance")
	f.Add(uint8(kindBook), uint8(1), uint8(1), uint8(1), "35=W|262=$ID|268=1|269=0|270=-1|271=NaN\n35=X|262=$ID|268=3|279=2|269=0|55=BTCUSDT|270=5|25043=9223372036854775807|25044=-9223372036854775808")
	f.Add(uint8(kindDepth), uint8(0), uint8(0), uint8(0), "35=Y|262=$ID|25016=-1121|58=Invalid symbol.\n35=Y|262=$ID2|281=1\n35=3|45=2|372=X|373=8|58=x\n35=5|58=bye")
	f.Fuzz(func(t *testing.T, k, flags, show, levels uint8, input string) {
		s := fuzzSession(t)
		c := fuzzClient(kind(k%6), flags&1 != 0, flags&2 != 0, flags&4 != 0, int(show%12), int(levels))
		feed(c, s, input)
	})
}

// FuzzBook applies arbitrary updates to a book: its sides stay sorted, and
// hold no level without a quantity.
func FuzzBook(f *testing.F) {
	f.Add("100.5 1\n99 2\n100.5 0\n101 3", "98 1\n102 1", true, int64(5), int64(7))
	f.Add("0 1\n-1 1\n.5 .5\n1e3 x\n00.10 0.1", "", false, int64(0), int64(0))
	f.Fuzz(func(t *testing.T, updates, snapshot string, bid bool, first, last int64) {
		parse := func(s string) (ls []level) {
			for line := range strings.SplitSeq(s, "\n") {
				if p, q, ok := strings.Cut(line, " "); ok {
					ls = append(ls, level{p, q})
				}
			}
			return ls
		}
		b := &book{symbol: "X"}
		b.load(last, parse(snapshot), parse(snapshot))
		for i, l := range parse(updates) {
			// Spread the updates over IDs around the arguments.
			_, _ = b.apply(first+int64(i/2), last+int64(i/2), bid, i%3 == 0, l.Price, l.Qty)
			b.top(i)
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
