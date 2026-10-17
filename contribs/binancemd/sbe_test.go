package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/binancefixtest"
	"github.com/sylr/fix2/transport/mem"
)

// sbeExchange returns a fake FIX SBE market data endpoint checking
// docKey's signatures, its market data md.
func sbeExchange(t *testing.T, md binancefixtest.MarketData, opts ...func(*binancefixtest.Options)) *binancefixtest.Exchange {
	o := binancefixtest.Options{PublicKey: pub(t), APIKey: "APIKEY", Kind: binancefix.MarketData, MarketData: md}
	for _, f := range opts {
		f(&o)
	}
	return binancefixtest.New(o)
}

// startSBE runs cmd with --sbe against x, returning its output and its
// result.
func startSBE(t *testing.T, x *binancefixtest.Exchange, args func(o *options), cmd string, symbols ...string) (*safeBuffer, context.CancelFunc, chan error) {
	t.Helper()
	out := &safeBuffer{}
	cancel, done := runSBE(t, x, args, out, cmd, symbols...)
	return out, cancel, done
}

// runSBE runs cmd with --sbe against x, writing to w.
func runSBE(t *testing.T, x *binancefixtest.Exchange, args func(o *options), w io.Writer, cmd string, symbols ...string) (context.CancelFunc, chan error) {
	t.Helper()
	keyFile, key := writeKey(t)
	o := &options{apiKey: "APIKEY", keyFile: keyFile, sender: "TESTER", endpoint: "fake", heartbeat: 30 * time.Second, levels: 10, show: 3,
		json: true, sbe: true}
	if args != nil {
		args(o)
	}
	log := slog.New(slog.DiscardHandler)
	if os.Getenv("FIX2_TEST_LOG") != "" {
		log = newLogger(os.Stderr, true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- streamSBEWith(ctx, o, commands[cmd], symbols, key, newOutput(w, o.json, false), log, x)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("client did not stop")
		}
	})
	return cancel, done
}

// requests waits for x to have received n requests, and returns them.
func requests(t *testing.T, x *binancefixtest.Exchange, n int) []binancefixtest.Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rs := x.Requests()
		if len(rs) >= n {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests received, want %d", len(rs), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// result waits for the command's result.
func result(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		done <- err // for the cleanup
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not end")
		return nil
	}
}

// microsOf returns the UTC time of FIX timestamp ts in microseconds.
func microsOf(t *testing.T, ts string) int64 {
	t.Helper()
	tm, err := time.Parse("20060102-15:04:05.000000", ts)
	if err != nil {
		t.Fatal(err)
	}
	return tm.UnixMicro()
}

// scenario is a market both paths are given: fix plays it to a FIX client,
// sbe to an SBE client, once it subscribed or sent its query.
type scenario struct {
	cmd      string
	events   int
	fix      func(f *fakeBinance)
	snapshot *binancefix.MarketDataSnapshot // the SBE book stream's on activation
	sbe      []binancefixtest.Message
}

var (
	// snapshot is a book of BTCUSDT at update 100, in FIX and in SBE.
	fixSnapshot = []string{"55", "BTCUSDT", "25044", "100", "268", "4",
		"269", "0", "270", "99.00", "271", "1.0",
		"269", "0", "270", "98.50", "271", "2.0",
		"269", "1", "270", "100.00", "271", "3.0",
		"269", "1", "270", "101.00", "271", "4.0"}
	sbeSnapshot = &binancefix.MarketDataSnapshot{LastBookUpdateID: binancefix.Some[int64](100), PriceExponent: -2, QtyExponent: -1,
		MDEntriesBids: []binancefix.MarketDataSnapshotMDEntriesBids{{MDEntryPx: 9900, MDEntrySize: 10}, {MDEntryPx: 9850, MDEntrySize: 20}},
		MDEntriesAsks: []binancefix.MarketDataSnapshotMDEntriesAsks{{MDEntryPx: 10000, MDEntrySize: 30}, {MDEntryPx: 10100, MDEntrySize: 40}},
		Symbol:        []byte("BTCUSDT")}
	// update 101: a bid new at 99.5, the bid at 99 changed, the ask at 100
	// deleted. FIX says so; SBE leaves it to the book.
	fixUpdate = []string{"268", "3",
		"279", "0", "269", "0", "270", "99.50", "271", "0.5", "55", "BTCUSDT", "25043", "101", "25044", "101",
		"279", "1", "269", "0", "270", "99.00", "271", "1.5",
		"279", "2", "269", "1", "270", "100.00"}
	sbeUpdate = &binancefix.MarketDataIncrementalDepth{FirstBookUpdateID: 101, LastBookUpdateID: 101, PriceExponent: -2, QtyExponent: -1,
		MDEntriesBids: []binancefix.MarketDataIncrementalDepthMDEntriesBids{{MDEntryPx: 9950, MDEntrySize: binancefix.Some[int64](5)},
			{MDEntryPx: 9900, MDEntrySize: binancefix.Some[int64](15)}},
		MDEntriesAsks: []binancefix.MarketDataIncrementalDepthMDEntriesAsks{{MDEntryPx: 10000}},
		Symbol:        []byte("BTCUSDT")}
)

func scenarios(t *testing.T) []scenario {
	subscribed := func(f *fakeBinance, fields ...string) {
		id := string(f.expect("V").Get(262))
		for i := 0; i < len(fields); {
			j := slices.Index(fields[i:], "|")
			msg := fields[i:]
			if j >= 0 {
				msg = fields[i : i+j]
			}
			f.send(msg[0], append([]string{"262", id}, msg[1:]...)...)
			if j < 0 {
				break
			}
			i += j + 1
		}
	}
	return []scenario{
		{cmd: "trades", events: 2,
			fix: func(f *fakeBinance) {
				subscribed(f, "X", "268", "2",
					"279", "0", "269", "2", "270", "10.00000", "271", "0.01000", "55", "BTCUSDT", "1003", "7", "60", "20241019-05:40:11.464000", "2446", "1",
					"279", "0", "269", "2", "270", "10.10000", "271", "1.50000", "1003", "8", "60", "20241019-05:40:11.465000", "2446", "2")
			},
			// Exponents changing from one message to the next.
			sbe: []binancefixtest.Message{
				&binancefix.MarketDataIncrementalTrade{TransactTime: microsOf(t, "20241019-05:40:11.464000"), PriceExponent: -5, QtyExponent: -5,
					MDEntries: []binancefix.MDEntries{{TradeID: 7, MDEntryPx: 1_000_000, MDEntrySize: 1000, AggressorSide: binancefix.Some(binancefix.SideBuy)}},
					Symbol:    []byte("BTCUSDT")},
				&binancefix.MarketDataIncrementalTrade{TransactTime: microsOf(t, "20241019-05:40:11.465000"), PriceExponent: -1, QtyExponent: -1,
					MDEntries: []binancefix.MDEntries{{TradeID: 8, MDEntryPx: 101, MDEntrySize: 15, AggressorSide: binancefix.Some(binancefix.SideSell)}},
					Symbol:    []byte("BTCUSDT")},
			}},
		{cmd: "ticker", events: 3,
			fix: func(f *fakeBinance) {
				subscribed(f, "W", "55", "BTCUSDT", "25044", "5", "268", "2",
					"269", "0", "270", "99.00", "271", "1.0", "269", "1", "270", "100.00", "271", "3.0", "|",
					"X", "268", "1", "279", "1", "270", "99.10", "271", "2", "269", "0", "55", "BTCUSDT", "25044", "6", "|",
					"X", "268", "1", "279", "2", "270", "100.00", "269", "1", "55", "BTCUSDT", "25044", "7")
			},
			snapshot: &binancefix.MarketDataSnapshot{LastBookUpdateID: binancefix.Some[int64](5), PriceExponent: -2, QtyExponent: -1,
				MDEntriesBids: []binancefix.MarketDataSnapshotMDEntriesBids{{MDEntryPx: 9900, MDEntrySize: 10}},
				MDEntriesAsks: []binancefix.MarketDataSnapshotMDEntriesAsks{{MDEntryPx: 10000, MDEntrySize: 30}},
				Symbol:        []byte("BTCUSDT")},
			sbe: []binancefixtest.Message{
				&binancefix.MarketDataIncrementalBookTicker{LastBookUpdateID: 6, PriceExponent: -3,
					MDEntriesBids: []binancefix.MarketDataIncrementalBookTickerMDEntriesBids{{MDEntryPx: 99100, MDEntrySize: binancefix.Some[int64](2)}},
					Symbol:        []byte("BTCUSDT")},
				// The ask's size missing: the side emptied.
				&binancefix.MarketDataIncrementalBookTicker{LastBookUpdateID: 7, PriceExponent: -2,
					MDEntriesAsks: []binancefix.MarketDataIncrementalBookTickerMDEntriesAsks{{MDEntryPx: 10000}},
					Symbol:        []byte("BTCUSDT")},
			}},
		{cmd: "depth", events: 2,
			fix: func(f *fakeBinance) {
				subscribed(f, append(append(append([]string{"W"}, fixSnapshot...), "|", "X"), fixUpdate...)...)
			},
			snapshot: sbeSnapshot, sbe: []binancefixtest.Message{sbeUpdate}},
		{cmd: "book", events: 2,
			fix: func(f *fakeBinance) {
				subscribed(f, append(append(append([]string{"W"}, fixSnapshot...), "|", "X"), fixUpdate...)...)
			},
			snapshot: sbeSnapshot, sbe: []binancefixtest.Message{sbeUpdate}},
		{cmd: "instruments", events: 1,
			// The fake's own instrument, as FIX tells it.
			fix: func(f *fakeBinance) {
				x := f.expect("x")
				f.send("y", "320", string(x.Get(320)), "146", "1", "55", "BTCUSDT", "15", "BTC", "562", "0.00001000",
					"1140", "9000.00000000", "25039", "0.00001000", "969", "0.01000000")
				f.expect("5")
				f.send("5")
			}},
		{cmd: "limits", events: 2,
			// The fake's own limits, as FIX tells them.
			fix: func(f *fakeBinance) {
				q := f.expect("XLQ")
				f.send("XLR", "6136", string(q.Get(6136)), "25003", "2",
					"25004", "2", "25005", "1", "25006", "2000", "25007", "60", "25008", "s",
					"25004", "3", "25005", "0", "25006", "1000")
				f.expect("5")
				f.send("5")
			}},
	}
}

// normalized returns line, a JSON event, without the time it was received
// at, which differs from one run to the next.
func normalized(t *testing.T, line string) string {
	t.Helper()
	var e map[string]any
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	if e["type"] != "trade" {
		delete(e, "time")
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The same market, through the FIX path and through the SBE path, gives
// the same events: SBE's exponents, its missing sizes and its depth
// updates without action told as FIX tells them.
func TestSBESameEventsAsFIX(t *testing.T) {
	for _, sc := range scenarios(t) {
		t.Run(sc.cmd, func(t *testing.T) {
			symbols := []string{"BTCUSDT"}
			if sc.cmd == "limits" {
				symbols = nil
			}
			l := mem.Listen("binance")
			fixOut, _, fixDone := startClient(t, l, nil, sc.cmd, symbols...)
			f := acceptBinance(t, l, pub(t))
			f.logon()
			sc.fix(f)
			fixLines := waitLines(t, fixOut, sc.events)

			md := binancefixtest.MarketData{}
			if sc.snapshot != nil {
				md.Snapshot = func(binancefix.Stream, int) *binancefix.MarketDataSnapshot { return sc.snapshot }
			}
			x := sbeExchange(t, md)
			sbeOut, _, sbeDone := startSBE(t, x, nil, sc.cmd, symbols...)
			requests(t, x, 1)
			for _, m := range sc.sbe {
				x.Broadcast(m)
			}
			sbeLines := waitLines(t, sbeOut, sc.events)
			if sc.cmd == "instruments" || sc.cmd == "limits" {
				// Completed: nothing more written.
				if err := result(t, fixDone); err != nil {
					t.Errorf("FIX: %v", err)
				}
				if err := result(t, sbeDone); err != nil {
					t.Errorf("SBE: %v", err)
				}
				fixLines, sbeLines = strings.Split(strings.TrimSpace(fixOut.String()), "\n"), strings.Split(strings.TrimSpace(sbeOut.String()), "\n")
			}
			if len(fixLines) != sc.events || len(sbeLines) != sc.events {
				t.Fatalf("FIX:\n%s\nSBE:\n%s", fixOut.String(), sbeOut.String())
			}
			for i := range fixLines {
				if got, want := normalized(t, sbeLines[i]), normalized(t, fixLines[i]); got != want {
					t.Errorf("event %d:\n SBE %s\n FIX %s", i, got, want)
				}
			}
		})
	}
}

// depthUpdate returns an update of BTCUSDT of IDs first to last, its bids
// and asks given as price, size pairs in hundredths, -1 for no size.
func depthUpdate(first, last int64, bids, asks []int64) *binancefix.MarketDataIncrementalDepth {
	m := &binancefix.MarketDataIncrementalDepth{FirstBookUpdateID: first, LastBookUpdateID: last, PriceExponent: -2, QtyExponent: -2,
		Symbol: []byte("BTCUSDT")}
	size := func(v int64) binancefix.Optional[int64] {
		if v < 0 {
			return binancefix.Optional[int64]{}
		}
		return binancefix.Some(v)
	}
	for i := 0; i+1 < len(bids); i += 2 {
		m.MDEntriesBids = append(m.MDEntriesBids, binancefix.MarketDataIncrementalDepthMDEntriesBids{MDEntryPx: bids[i], MDEntrySize: size(bids[i+1])})
	}
	for i := 0; i+1 < len(asks); i += 2 {
		m.MDEntriesAsks = append(m.MDEntriesAsks, binancefix.MarketDataIncrementalDepthMDEntriesAsks{MDEntryPx: asks[i], MDEntrySize: size(asks[i+1])})
	}
	return m
}

// testSBEClient returns an sbeClient of command k, logged on to no
// connection: its Sends fail, its Reconnects are recorded.
func testSBEClient(t testing.TB, k kind, out output) *sbeClient {
	c := newSBEClient(testBinancefix(t), k, out)
	t.Cleanup(func() { c.finish(nil) })
	return c
}

// testBinancefix returns a binancefix market data Client never run: its
// Sends fail, its Reconnects are queued, to nothing.
func testBinancefix(t testing.TB) *binancefix.Client {
	_, key := writeKeyTB(t)
	bc, err := binancefix.New(binancefix.Config{Kind: binancefix.MarketData, Dialer: binancefixtest.New(binancefixtest.Options{}), APIKey: "k",
		PrivateKey: key, SenderCompID: "FUZZ"})
	if err != nil {
		t.Fatal(err)
	}
	return bc
}

// newSBEClient returns an sbeClient of command k, of bc, as logged on to
// connection 1, writing to out.
func newSBEClient(bc *binancefix.Client, k kind, out output) *sbeClient {
	ctx, finish := context.WithCancelCause(context.Background())
	return &sbeClient{bc: bc, cmd: command{k}, symbols: []string{"BTCUSDT"}, levels: 10, show: 10, out: out,
		log: slog.New(slog.DiscardHandler), ctx: ctx, finish: finish, flushCtx: ctx, epoch: 1,
		subs: map[string]*sub{}, receipts: map[*binancefix.Receipt]string{}, quotes: map[string]*tickerEvent{},
		books: map[string]*book{}, queries: map[string]bool{}}
}

// writeKeyTB is writeKey for fuzzing too.
func writeKeyTB(t testing.TB) (string, ed25519.PrivateKey) {
	t.Helper()
	path := t.TempDir() + "/key.pem"
	if err := os.WriteFile(path, []byte(docKey), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := loadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, key
}

// The depth command infers each change's action from the book kept since
// the snapshot, as Binance's rule says: no size, delete; a level the book
// has, change; one it has not, new. An update the snapshot covers is not
// shown.
func TestSBEDepthActions(t *testing.T) {
	var out safeBuffer
	c := testSBEClient(t, kindDepth, newOutput(&out, true, false))
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	c.onSnapshot(at, sbeSnapshot) // bids 99, 98.5; asks 100, 101
	for _, m := range []*binancefix.MarketDataIncrementalDepth{
		depthUpdate(99, 100, []int64{9900, 100}, nil),                                     // covered by the snapshot
		depthUpdate(100, 102, []int64{9900, 300, 9800, 100, 9850, -1}, []int64{10050, 7}), // change, new, delete; new
		depthUpdate(103, 103, []int64{9800, 200, 9700, -1}, []int64{10000, -1, 10050, 8}), // change, delete of none; delete, change
	} {
		c.onDepth(at, m)
	}
	if err := c.out.end(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var e depthEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Type != "depth" {
			continue
		}
		s := strconv.FormatInt(e.FirstUpdateID, 10) + "-" + strconv.FormatInt(e.LastUpdateID, 10) + ":"
		for _, ch := range e.Changes {
			s += " " + ch.Side + " " + ch.Action + " " + ch.Price + " " + ch.Qty
		}
		got = append(got, strings.TrimSpace(s))
	}
	want := []string{
		"100-102: bid change 99 3 bid new 98 1 bid delete 98.5  ask new 100.5 0.07",
		"103-103: bid change 98 2 bid delete 97  ask delete 100  ask change 100.5 0.08",
	}
	if !slices.Equal(got, want) {
		t.Errorf("updates:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	b := c.books["BTCUSDT"]
	if bids, asks := b.top(10); !slices.Equal(bids, []level{{"99", "3"}, {"98", "2"}}) || !slices.Equal(asks, []level{{"100.5", "0.08"}, {"101", "4"}}) {
		t.Errorf("book %v %v", bids, asks)
	}
}

// A book whose updates have a gap cannot be subscribed again on its
// connection: the command reconnects, subscribes again, and starts over
// from the new snapshot. So does a snapshot without LastBookUpdateID.
func TestSBEGapReconnects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first *binancefix.MarketDataSnapshot
		gap   bool // the first snapshot taken, then a gap
	}{
		{"gap", sbeSnapshot, true},
		{"snapshot without update ID", &binancefix.MarketDataSnapshot{QtyExponent: -1, Symbol: []byte("BTCUSDT"),
			MDEntriesBids: []binancefix.MarketDataSnapshotMDEntriesBids{{MDEntryPx: 1, MDEntrySize: 1}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshots atomic.Int32
			second := &binancefix.MarketDataSnapshot{LastBookUpdateID: binancefix.Some[int64](500), PriceExponent: -2, QtyExponent: -1,
				MDEntriesBids: []binancefix.MarketDataSnapshotMDEntriesBids{{MDEntryPx: 9000, MDEntrySize: 10}},
				Symbol:        []byte("BTCUSDT")}
			x := sbeExchange(t, binancefixtest.MarketData{Snapshot: func(binancefix.Stream, int) *binancefix.MarketDataSnapshot {
				if snapshots.Add(1) == 1 {
					return tc.first
				}
				return second
			}})
			out, _, done := startSBE(t, x, nil, "book", "BTCUSDT")
			requests(t, x, 1)
			if tc.gap {
				waitLines(t, out, 1)
				x.Broadcast(depthUpdate(101, 101, []int64{9950, 50}, nil))
				waitLines(t, out, 2)
				x.Broadcast(depthUpdate(105, 106, []int64{9960, 50}, nil)) // 102 to 104 missing
			}
			// Logged on again, subscribed again: the new snapshot.
			rs := requests(t, x, 2)
			if rs[1].Name != "MarketDataRequest" || rs[1].IDs[0] == rs[0].IDs[0] {
				t.Errorf("requests %+v", rs)
			}
			if n := len(x.Logons()); n != 2 {
				t.Errorf("%d Logons, want 2", n)
			}
			n := 1
			if tc.gap {
				n = 3
			}
			lines := waitLines(t, out, n)
			if !strings.Contains(lines[n-1], `"type":"book"`) || !strings.Contains(lines[n-1], `"last_update_id":500,"bids":[["90","1"]],`) {
				t.Errorf("output:\n%s", out.String())
			}
			select {
			case err := <-done:
				t.Fatalf("the command ended: %v", err)
			default:
			}
		})
	}
}

// Instruments in two messages: written as they come, the command ending
// once they stop.
func TestSBEInstrumentsSeveralMessages(t *testing.T) {
	x := sbeExchange(t, binancefixtest.MarketData{InstrumentsPerMessage: 2})
	out, _, done := startSBE(t, x, func(o *options) { o.json = false }, "instruments")
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{"BTCUSDT", "ETHUSDT", "ETHBTC"} {
		if !strings.Contains(out.String(), sym+"  ") {
			t.Errorf("%s not listed:\n%s", sym, out.String())
		}
	}
	if rs := requests(t, x, 1); len(rs) != 1 || rs[0].Name != "InstrumentListRequest" {
		t.Errorf("requests %+v", rs)
	}
}

// A subscription Binance refuses ends the command as on the FIX path.
func TestSBESubscriptionRefused(t *testing.T) {
	x := sbeExchange(t, binancefixtest.MarketData{})
	_, _, done := startSBE(t, x, nil, "trades", "NOPE")
	err := result(t, done)
	if err == nil || !strings.Contains(err.Error(), "Invalid symbol.") || !strings.Contains(err.Error(), "-1121") ||
		!strings.Contains(err.Error(), "no subscription left: NOPE") {
		t.Errorf("stream = %v", err)
	}
}

// A refused Logon, before any succeeded, ends the command at once, as on
// the FIX path.
func TestSBELogonRefused(t *testing.T) {
	x := sbeExchange(t, binancefixtest.MarketData{}, func(o *binancefixtest.Options) { o.RefuseLogon = func(int) bool { return true } })
	_, _, done := startSBE(t, x, nil, "trades", "BTCUSDT")
	err := result(t, done)
	if err == nil || !strings.Contains(err.Error(), "logon rejected by Binance: Signature for this request is not valid. (error code -1022)") {
		t.Errorf("stream = %v", err)
	}
	if n := len(x.Logons()); n != 1 {
		t.Errorf("%d Logons, want 1", n)
	}
	// So does a bad API key.
	x = sbeExchange(t, binancefixtest.MarketData{})
	_, _, done = startSBE(t, x, func(o *options) { o.apiKey = "OTHER" }, "trades", "BTCUSDT")
	if err := result(t, done); err == nil || !strings.Contains(err.Error(), "(error code -2015)") {
		t.Errorf("stream = %v", err)
	}
}

// Once logged on, a refused Logon is retried (maxRefusals in a row at
// most), as on the FIX path: the command does not give up at the first.
func TestSBELogonRefusedAfterReconnect(t *testing.T) {
	// The first Logon accepted, the next refused.
	x := sbeExchange(t, binancefixtest.MarketData{}, func(o *binancefixtest.Options) { o.RefuseLogon = func(n int) bool { return n > 1 } })
	_, _, done := startSBE(t, x, nil, "trades", "BTCUSDT")
	requests(t, x, 1)
	// Maintenance: a clean Logout, and a Logon at once. Each refusal then
	// waits twice HeartBtInt (Binance's cooldown) before the next: the
	// first one tells the command did not give up.
	x.News("Your connection will be disconnected in 10 seconds. Please reconnect.")
	deadline := time.Now().Add(5 * time.Second)
	for len(x.Logons()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no Logon again")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("gave up after a refusal: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// Maintenance news: the command reconnects, and subscribes again.
func TestSBENewsReconnects(t *testing.T) {
	x := sbeExchange(t, binancefixtest.MarketData{})
	out, _, done := startSBE(t, x, nil, "trades", "BTCUSDT")
	requests(t, x, 1)
	x.News("Your connection will be disconnected in 30 seconds. Please reconnect.")
	rs := requests(t, x, 2)
	if len(x.Logons()) != 2 || rs[1].Name != "MarketDataRequest" {
		t.Errorf("Logons %d, requests %+v", len(x.Logons()), rs)
	}
	deadline := time.Now().Add(5 * time.Second)
	for x.Connections() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections", x.Connections())
		}
		time.Sleep(time.Millisecond)
	}
	if n := x.Publish("BTCUSDT"); n != 1 {
		t.Errorf("%d published", n)
	}
	if lines := waitLines(t, out, 1); !strings.Contains(lines[0], `"type":"trade"`) {
		t.Errorf("output:\n%s", out.String())
	}
	select {
	case err := <-done:
		t.Fatalf("the command ended: %v", err)
	default:
	}
}

// --trace logs every SBE message, both ways, field by field, the Logon's
// API key and signature redacted.
func TestSBETrace(t *testing.T) {
	x := sbeExchange(t, binancefixtest.MarketData{}, func(o *binancefixtest.Options) { o.APIKey = "SECRET-API-KEY" })
	keyFile, key := writeKey(t)
	o := &options{apiKey: "SECRET-API-KEY", keyFile: keyFile, sender: "TESTER", endpoint: "fake", heartbeat: 30 * time.Second,
		levels: 10, show: 3, json: true, sbe: true, trace: true}
	var logs safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- streamSBEWith(ctx, o, commands["trades"], []string{"BTCUSDT"}, key, newOutput(io.Discard, true, false), newLogger(&logs, true), x)
	}()
	requests(t, x, 1)
	x.Publish("BTCUSDT")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "MarketDataIncrementalTrade") {
		if time.Now().After(deadline) {
			t.Fatalf("no trade traced:\n%s", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
	s := logs.String()
	for _, want := range []string{"SBE out", "SBE in", "template=Logon", "Username=[redacted]", "RawData=[redacted]", "SenderCompId=TESTER",
		"template=LogonAck", "template=MarketDataRequest", "MDReqID=TRADES_BTCUSDT_1", "RelatedSym[0].Symbol=BTCUSDT",
		"template=MarketDataIncrementalTrade", "MDEntries[0].TradeID=", "template=Logout"} {
		if !strings.Contains(s, want) {
			t.Errorf("trace lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "SECRET-API-KEY") {
		t.Errorf("trace shows the API key:\n%s", s)
	}
}

// --validate is the FIX dictionary's check: refused with --sbe.
func TestSBEValidateRefused(t *testing.T) {
	var stderr bytes.Buffer
	t.Setenv("BINANCE_API_KEY", "k")
	t.Setenv("BINANCE_PRIVATE_KEY", "k.pem")
	if code := run([]string{"--sbe", "--validate", "trades", "BTCUSDT"}, io.Discard, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "--validate") || !strings.Contains(stderr.String(), "--sbe") {
		t.Errorf("exit %d; stderr: %s", code, stderr.String())
	}
}

// --sbe's endpoints: binancefix's, --testnet's, --endpoint over both.
func TestSBEEndpoints(t *testing.T) {
	for _, tc := range []struct {
		o    options
		want string
	}{
		{options{sbe: true}, binancefix.MarketDataEndpoint},
		{options{sbe: true, testnet: true}, "fix-md.testnet.binance.vision:9002"},
		{options{sbe: true, testnet: true, endpoint: "h:1"}, "h:1"},
		{options{}, prodEndpoint},
		{options{testnet: true}, testnetEndpoint},
	} {
		o := tc.o
		o.apiKey, o.keyFile, o.heartbeat, o.levels, o.show = "k", "k.pem", 30*time.Second, 10, 1
		if err := o.check(command{kindTrades}, []string{"BTCUSDT"}); err != nil || o.endpoint != tc.want {
			t.Errorf("%+v: %q, %v; want %q", tc.o, o.endpoint, err, tc.want)
		}
	}
}

// startSBEWriting runs the ticker command for BTCUSDT with --sbe against x,
// writing JSON lines to w, and returns once it subscribed.
func startSBEWriting(t *testing.T, w io.Writer, opts ...func(*binancefixtest.Options)) (*binancefixtest.Exchange, output, context.CancelFunc, chan error) {
	t.Helper()
	x := sbeExchange(t, binancefixtest.MarketData{Snapshot: func(binancefix.Stream, int) *binancefix.MarketDataSnapshot { return nil }}, opts...)
	keyFile, key := writeKey(t)
	o := &options{apiKey: "APIKEY", keyFile: keyFile, sender: "TESTER", endpoint: "fake", heartbeat: 30 * time.Second, levels: 10, show: 1,
		json: true, sbe: true}
	out := newOutput(w, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- streamSBEWith(ctx, o, commands["ticker"], []string{"BTCUSDT"}, key, out, slog.New(slog.DiscardHandler), x)
	}()
	t.Cleanup(cancel)
	requests(t, x, 1)
	return x, out, cancel, done
}

// tickerOf returns a book ticker update of BTCUSDT whose best bid is px.
func tickerOf(px int) *binancefix.MarketDataIncrementalBookTicker {
	return &binancefix.MarketDataIncrementalBookTicker{LastBookUpdateID: int64(px),
		MDEntriesBids: []binancefix.MarketDataIncrementalBookTickerMDEntriesBids{{MDEntryPx: int64(px), MDEntrySize: binancefix.Some[int64](1)}},
		Symbol:        []byte("BTCUSDT")}
}

// The SBE path stops within its bound though the output stays blocked: its
// Handler never waits on the output once the command ends, and binancefix's
// Run, which waits for the Handler, returns. The command fails with
// errOutputBlocked; once unblocked, the output holds the first events, in
// order, each once, never written by two goroutines at once.
func TestSBEShutdownWhileOutputBlocked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events int
		peer   func(x *binancefixtest.Exchange)
	}{
		{"logout answered", 1, func(*binancefixtest.Exchange) {}},
		{"exchange hangs up", 1, func(x *binancefixtest.Exchange) { x.Disconnect() }},
		// The Handler waits on the full queue when the command ends.
		{"queue full, logout answered", outputQueue + 10, func(*binancefixtest.Exchange) {}},
		{"queue full, exchange hangs up", outputQueue + 10, func(x *binancefixtest.Exchange) { x.Disconnect() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setOutputTimeout(t, 500*time.Millisecond)
			w := newBlockedWriter(t)
			x, out, cancel, done := startSBEWriting(t, w)
			x.Broadcast(tickerOf(1000))
			select {
			case <-w.first:
			case <-time.After(5 * time.Second):
				t.Fatal("nothing written")
			}
			for i := 1; i < tc.events; i++ {
				x.Broadcast(tickerOf(1000 + i))
			}
			if tc.events > outputQueue {
				waitQueueFull(t, out)
			}
			start := time.Now()
			cancel()
			tc.peer(x)
			// binancefix's Logout, bounded by LogoutTimeout, then the
			// output's.
			select {
			case err := <-done:
				if !errors.Is(err, errOutputBlocked) {
					t.Errorf("command failed with %v, want errOutputBlocked", err)
				}
				t.Logf("stopped in %v", time.Since(start))
			case <-time.After(2*time.Second + outputTimeout + 2*time.Second):
				t.Fatal("the command did not stop")
			}
			w.unblock()
			want := min(tc.events, outputQueue+1)
			waitGrowing(t, "lines written", func() int { return strings.Count(w.String(), "\n") }, want)
			// What was handed over before the end: the first events, in
			// order, each once.
			checkTickers(t, w.String(), strings.Count(w.String(), "\n"))
			if w.overlap.Load() {
				t.Error("two Writes ran at once")
			}
		})
	}
}

// A command ending on its own, its context never cancelled, stops within
// its bound though its output stays blocked: binancefix connecting no
// more (Logons refused after the connection was lost) ends the Handler's
// waits on the output, as does the command failing (a subscription
// refused). Once unblocked, the output holds the first events, in order,
// each once.
func TestSBEFatalEndWhileOutputBlocked(t *testing.T) {
	saved := sbeReconnect
	sbeReconnect.MaxRefusals, sbeReconnect.MinBackoff, sbeReconnect.MaxBackoff = 1, 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sbeReconnect = saved })
	refuseAgain := func(o *binancefixtest.Options) { o.RefuseLogon = func(n int) bool { return n > 1 } }
	malformed := &binancefix.MarketDataIncrementalTrade{TransactTime: -1 << 63, Symbol: []byte("BTCUSDT")}
	for _, tc := range []struct {
		name   string
		events int
		end    func(x *binancefixtest.Exchange)
		want   string
	}{
		{"Binance logs out, Logon refused, queue full", outputQueue + 10,
			func(x *binancefixtest.Exchange) { x.Broadcast(&binancefix.Logout{Text: []byte("bye")}) }, "logon rejected by Binance"},
		{"protocol error, Logon refused, queue full", outputQueue + 10,
			func(x *binancefixtest.Exchange) { x.Broadcast(malformed) }, "logon rejected by Binance"},
		{"Binance logs out, Logon refused", 1,
			func(x *binancefixtest.Exchange) { x.Broadcast(&binancefix.Logout{Text: []byte("bye")}) }, "logon rejected by Binance"},
		// The Handler free (the queue has room): it fails the command.
		{"subscription refused", 1, func(x *binancefixtest.Exchange) {
			x.Broadcast(&binancefix.MarketDataRequestReject{MDReqID: []byte("TICKER_BTCUSDT_1"), ErrorCode: binancefix.Some[int32](-1121),
				Text: []byte("Invalid symbol.")})
		}, "no subscription left: BTCUSDT refused by Binance: Invalid symbol. (error code -1121)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setOutputTimeout(t, 500*time.Millisecond)
			w := newBlockedWriter(t)
			x, out, _, done := startSBEWriting(t, w, refuseAgain)
			x.Broadcast(tickerOf(1000))
			select {
			case <-w.first:
			case <-time.After(5 * time.Second):
				t.Fatal("nothing written")
			}
			for i := 1; i < tc.events; i++ {
				x.Broadcast(tickerOf(1000 + i))
			}
			if tc.events > outputQueue {
				waitQueueFull(t, out)
			}
			start := time.Now()
			tc.end(x)
			select {
			case err := <-done:
				if !errors.Is(err, errOutputBlocked) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("command failed with %v, want %q and errOutputBlocked", err, tc.want)
				}
				t.Logf("stopped in %v", time.Since(start))
			case <-time.After(2*time.Second + outputTimeout + 3*time.Second):
				t.Fatal("the command did not stop")
			}
			w.unblock()
			want := min(tc.events, outputQueue+1)
			waitGrowing(t, "lines written", func() int { return strings.Count(w.String(), "\n") }, want)
			checkTickers(t, w.String(), strings.Count(w.String(), "\n"))
			if w.overlap.Load() {
				t.Error("two Writes ran at once")
			}
		})
	}
}
