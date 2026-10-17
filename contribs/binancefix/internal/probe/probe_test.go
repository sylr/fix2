package probe

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/fakebinance"
)

// docKey is the example private key of Binance's FIX API documentation.
const docKey = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIIJEYWtGBrhACmb9Dvy+qa8WEf0lQOl1s4CLIAB9m89u
-----END PRIVATE KEY-----
`

func testKey(t testing.TB) ed25519.PrivateKey {
	t.Helper()
	k, err := binancefix.ParsePrivateKey([]byte(docKey))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// exchange returns a fake Binance of behaviour b, accepting the forms of
// signed SendingTime forms (the default's if none).
func exchange(t *testing.T, b fakebinance.Behaviour, forms ...binancefix.SignatureTime) *fakebinance.Exchange {
	return fakebinance.New(fakebinance.Options{APIKey: "api-key", PublicKey: testKey(t).Public().(ed25519.PublicKey), SignatureTimes: forms, Behaviour: b})
}

// options returns Options of x, its waits short.
func options(t *testing.T, x *fakebinance.Exchange) Options {
	return Options{Endpoint: binancefix.TestnetEndpoint, Dialer: x, APIKey: "api-key", PrivateKey: testKey(t), SenderCompID: "PTEST",
		PriceFar: "70000", HeartBtInt: 5 * time.Second, Wait: time.Second, Settle: 30 * time.Millisecond, Interval: time.Millisecond,
		CleanupTimeout: 10 * time.Second, Logf: t.Logf}
}

// PriceFar has no default: the probes resting limit orders refuse to run
// without it, before dialing; the others run.
func TestRunPriceRequired(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{})
	o := options(t, x)
	o.PriceFar = ""
	for _, names := range [][]string{nil, {"order"}, {"amend"}, {"cancel-replace"}, {"list-cancel"}, {"mass-cancel"}, {"limits", "g"}} {
		if !PriceRequired(names...) {
			t.Errorf("PriceRequired(%v) false", names)
		}
		rep, err := Run(context.Background(), o, names...)
		if !errors.Is(err, ErrPriceRequired) || !strings.Contains(rep.Error, "bidMultiplierDown") {
			t.Errorf("%v: %v", names, err)
		}
	}
	if x.Dials() != 0 {
		t.Fatalf("%d dials", x.Dials())
	}
	if PriceRequired("logon-seqnum", "fill", "limits") {
		t.Error("PriceRequired(a, f, h)")
	}
	rep, err := Run(context.Background(), o, "logon-seqnum", "fill", "limits")
	if err != nil || len(rep.Results) != 3 || rep.PriceFar != "" {
		t.Fatalf("Run: %v %+v", err, rep)
	}
	for _, r := range rep.Results {
		if r.Verdict != Confirmed {
			t.Errorf("%s: %s %+v", r.Probe, r.Verdict, r.Checks)
		}
	}
}

func verdicts(cs []Check) []Verdict {
	var vs []Verdict
	for _, c := range cs {
		vs = append(vs, c.Verdict)
	}
	return vs
}

const (
	ok  = Confirmed
	bad = Contradicted
	unk = Inconclusive
)

// Each probe, Binance answering as the client assumes, then otherwise: the
// verdicts follow, and the probe leaves no order open.
func TestProbes(t *testing.T) {
	type fb = fakebinance.Behaviour
	for _, tc := range []struct {
		name   string
		probe  string
		b      fb
		want   []Verdict
		detail string // in the first check's detail, if set
	}{
		// Answered with -1007, the orders are of unknown status, live in
		// the fake: the cleanup cancels them all the same.
		{"order/timeout", "order", fb{TimeoutOrders: true}, []Verdict{unk, unk}, "status unknown"},
		{"list-cancel/timeout", "list-cancel", fb{TimeoutLists: true}, []Verdict{unk, unk}, "status unknown"},
		// A filter's refusal: inconclusive, telling what to adjust.
		{"order/filter-refusal", "order", fb{RejectOrders: true, RejectCode: -1013}, []Verdict{unk, unk}, "adjust --price-far and --qty"},
		{"list-cancel/filter-refusal", "list-cancel", fb{RejectOrders: true, RejectCode: -1013}, []Verdict{unk, unk}, "adjust --price-far and --qty"},
		{"fill/filter-refusal", "fill", fb{RejectOrders: true, RejectCode: -1013}, []Verdict{unk}, "adjust --price-far and --qty"},
		// A Reject of the whole cancel/replace shows neither leg's answer.
		{"cancel-replace/rejected-whole", "cancel-replace", fb{RejectCancelReplace: true}, []Verdict{unk, unk, unk}, "the whole request refused by Reject"},
		// The sell-back refused or partly filled: traded back by the
		// cleanup all the same.
		{"fill/sell-back-refused", "fill", fb{RejectSells: 1}, []Verdict{ok}, "Sell 0.001: ExecutionReport REJECTED"},
		{"fill/sell-back-partial", "fill", fb{PartialSells: 1}, []Verdict{ok}, "now 0.0005 bought, not sold back"},
		{"logon-seqnum", "logon-seqnum", fb{}, []Verdict{ok}, ""},
		{"order", "order", fb{}, []Verdict{ok, ok}, ""},
		{"order/cancel-carries-order-id", "order", fb{CancelID: "order"}, []Verdict{ok, bad}, ""},
		{"order/rejected", "order", fb{RejectOrders: true}, []Verdict{unk, unk}, ""},
		{"amend", "amend", fb{}, []Verdict{ok}, ""},
		{"amend/answered-new", "amend", fb{Amend: "new"}, []Verdict{bad}, ""},
		{"amend/rejected", "amend", fb{Amend: "reject"}, []Verdict{unk}, ""},
		{"amend/unanswered", "amend", fb{Amend: "silent"}, []Verdict{unk}, ""},
		{"amend/no-order", "amend", fb{RejectOrders: true}, []Verdict{unk}, ""},
		{"cancel-replace", "cancel-replace", fb{}, []Verdict{ok, ok, ok}, ""},
		{"cancel-replace/reject-carries-clordid", "cancel-replace", fb{CancelReplaceRejectID: "order"}, []Verdict{ok, ok, ok}, ""},
		{"cancel-replace/reject-carries-another-id", "cancel-replace", fb{CancelReplaceRejectID: "other"}, []Verdict{ok, bad, bad}, ""},
		{"cancel-replace/stop-on-failure-places", "cancel-replace", fb{StopOnFailure: "place"}, []Verdict{ok, bad, ok}, ""},
		// A TRADE alone, no NEW, proves the new order executed.
		{"cancel-replace/stop-on-failure-trade-only", "cancel-replace", fb{StopOnFailure: "trade"}, []Verdict{ok, bad, ok}, ""},
		// Execution proven outweighs an earlier status unknown (-1007);
		// the -1007 alone is uncertainty.
		{"cancel-replace/stop-on-failure-timeout-then-trade", "cancel-replace", fb{StopOnFailure: "timeout-trade"}, []Verdict{ok, bad, ok}, ""},
		{"cancel-replace/stop-on-failure-timeout", "cancel-replace", fb{StopOnFailure: "timeout"}, []Verdict{ok, unk, ok}, ""},
		// A -1007 report is never evidence of another behaviour.
		{"amend/timeout-under-order-id", "amend", fb{Amend: "timeout-order-id"}, []Verdict{unk}, ""},
		// The sell-back filled unseen, its TRADE late, its cancellation
		// refused -2011: resolved by the late report, never sold twice.
		{"fill/sell-back-trade-late", "fill", fb{DelaySellTrade: "after-cancel"}, []Verdict{ok}, "now flat"},
		// Nothing of the new order within Wait: a timeout, no evidence.
		{"cancel-replace/allow-failure-silent", "cancel-replace", fb{AllowFailure: "silent"}, []Verdict{ok, ok, unk}, ""},
		{"cancel-replace/cancel-carries-order-id", "cancel-replace", fb{CancelID: "order"}, []Verdict{ok, ok, ok}, ""},
		{"list-cancel", "list-cancel", fb{}, []Verdict{ok, ok}, ""},
		{"list-cancel/orders-ids", "list-cancel", fb{ListCancel: "order-ids"}, []Verdict{ok, bad}, ""},
		{"list-cancel/list-status-only", "list-cancel", fb{ListCancel: "status-only"}, []Verdict{ok, bad}, ""},
		{"list-cancel/rejected", "list-cancel", fb{ListCancel: "reject"}, []Verdict{ok, unk}, ""},
		// A ListStatus alone whose entries carry -1007: status unknown,
		// not another behaviour; the list, live, is cancelled by the
		// cleanup.
		{"list-cancel/list-status-timeout", "list-cancel", fb{ListCancel: "status-timeout"}, []Verdict{ok, unk}, ""},
		{"list-cancel/list-rejected", "list-cancel", fb{RejectOrders: true}, []Verdict{unk, unk}, ""},
		{"fill", "fill", fb{}, []Verdict{ok}, ""},
		{"fill/trade-only", "fill", fb{Fill: "trade-only"}, []Verdict{bad}, ""},
		{"fill/not-filled", "fill", fb{Fill: "expire"}, []Verdict{unk}, ""},
		{"fill/rejected", "fill", fb{RejectOrders: true}, []Verdict{unk}, ""},
		{"mass-cancel", "mass-cancel", fb{}, []Verdict{ok, ok}, ""},
		{"mass-cancel/report-without-id", "mass-cancel", fb{MassCancel: "no-id"}, []Verdict{bad, ok}, ""},
		{"limits", "limits", fb{}, []Verdict{ok}, ""},
		{"limits/other-reqid", "limits", fb{Limits: "other-id"}, []Verdict{bad}, ""},
		{"limits/unanswered", "limits", fb{Limits: "silent"}, []Verdict{unk}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := exchange(t, tc.b)
			rep, err := Run(context.Background(), options(t, x), tc.probe)
			if err != nil {
				t.Fatalf("Run: %v (%+v)", err, rep)
			}
			if len(rep.Results) != 1 || rep.Results[0].Probe != tc.probe {
				t.Fatalf("results %+v", rep.Results)
			}
			res := rep.Results[0]
			for _, c := range res.Checks {
				t.Logf("%s: %s: %s", c.Verdict, c.Name, c.Detail)
			}
			if got := verdicts(res.Checks); !slices.Equal(got, tc.want) {
				t.Errorf("verdicts %v, want %v", got, tc.want)
			}
			if tc.detail != "" && !strings.Contains(res.Checks[0].Detail, tc.detail) {
				t.Errorf("detail %q, want %q in it", res.Checks[0].Detail, tc.detail)
			}
			if net := x.Net(); net != "0" || rep.Position != "" {
				t.Errorf("position %s left, report %q", net, rep.Position)
			}
			if res.Verdict != combine(res.Checks) || len(res.Messages) == 0 {
				t.Errorf("verdict %s, %d messages", res.Verdict, len(res.Messages))
			}
			if open := x.Open(); len(open) > 0 || len(rep.Leftover) > 0 {
				t.Errorf("left open: %v, leftover %v, cleanup %+v", open, rep.Leftover, rep.Cleanup)
			}
		})
	}
}

// All the probes in one run, Binance as assumed: all confirmed, the
// messages each saw its own, nothing left open.
func TestRunAll(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{})
	rep, err := Run(context.Background(), options(t, x))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != len(catalog) {
		t.Fatalf("%d results", len(rep.Results))
	}
	for i, res := range rep.Results {
		if res.Probe != catalog[i].Name || res.Verdict != Confirmed {
			t.Errorf("%s: %s %+v", res.Probe, res.Verdict, res.Checks)
		}
	}
	if !rep.Logon.Accepted || rep.Logon.LogonAck.SeqNum != 1 || rep.Logon.SignedSendingTime == "" {
		t.Errorf("logon %+v", rep.Logon)
	}
	// The messages of (b) are its order's and cancellation's.
	for _, m := range rep.Results[1].Messages {
		if m.ClOrdID != "" && !strings.Contains(m.ClOrdID, "-b") && !strings.Contains(m.OrigClOrdID, "-b") {
			t.Errorf("(b) shows %s", m)
		}
	}
	if open := x.Open(); len(open) > 0 {
		t.Errorf("left open: %v", open)
	}
	// Answers are matched: an ExecutionReport answering (b)'s order.
	found := false
	for _, m := range rep.Results[1].Messages {
		if m.Type == "ExecutionReport" && m.ExecType == "New" && m.Answers == m.ClOrdID {
			found = true
		}
	}
	if !found {
		t.Errorf("no ExecutionReport NEW matched to (b)'s order: %v", rep.Results[1].Messages)
	}
}

// Run refuses any endpoint but the testnet's, before dialing.
func TestRunTestnetOnly(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{})
	for _, ep := range []string{binancefix.Endpoint, "127.0.0.1:9002", "fix-oe.testnet.binance.vision.evil.com:9002", "localhost"} {
		o := options(t, x)
		o.Endpoint = ep
		rep, err := Run(context.Background(), o)
		if !errors.Is(err, ErrNotTestnet) || rep.Error == "" {
			t.Errorf("%s: %v", ep, err)
		}
	}
	if x.Dials() != 0 {
		t.Errorf("%d dials", x.Dials())
	}
	for ep, want := range map[string]bool{
		binancefix.TestnetEndpoint:                true,
		"FIX-OE.TESTNET.BINANCE.VISION:9002":      true,
		"fix-oe.testnet.binance.vision.:9002":     true,
		"fix-oe.testnet.binance.vision":           true,
		binancefix.Endpoint:                       false,
		"fix-md.testnet.binance.vision:9000":      false,
		"x.fix-oe.testnet.binance.vision:9002":    false,
		"fix-oe.testnet.binance.vision@evil:9002": false,
	} {
		if IsTestnet(ep) != want {
			t.Errorf("IsTestnet(%q) = %v", ep, !want)
		}
	}
}

// A run interrupted while waiting for an answer cancels what it placed.
func TestRunInterrupted(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{Amend: "silent"})
	o := options(t, x)
	o.Wait = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !slices.ContainsFunc(x.Requests(), func(r fakebinance.Request) bool { return r.Name == "OrderAmendKeepPriorityRequest" }) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	rep, err := Run(ctx, o, "amend", "limits")
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Run took %v", d)
	}
	if len(rep.Results) != 1 || rep.Results[0].Verdict != Inconclusive || !strings.Contains(rep.Results[0].Checks[0].Detail, "interrupted") {
		t.Errorf("results %+v", rep.Results)
	}
	if open := x.Open(); len(open) > 0 || len(rep.Leftover) > 0 {
		t.Errorf("left open: %v, leftover %v", open, rep.Leftover)
	}
	if !slices.ContainsFunc(rep.Cleanup, func(c Cleanup) bool { return c.Outcome == "canceled" }) {
		t.Errorf("cleanup %+v", rep.Cleanup)
	}
}

// The connection lost, what was placed is cancelled from a session of
// another SenderCompID: Binance refuses the first for twice HeartBtInt.
func TestRunConnectionLost(t *testing.T) {
	// The order is answered, its cancellation's request closes the
	// connection.
	x := exchange(t, fakebinance.Behaviour{DropAfter: 2})
	rep, err := Run(context.Background(), options(t, x), "order", "limits")
	if err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 1 || verdicts(rep.Results[0].Checks)[1] != Inconclusive {
		t.Errorf("results %+v", rep.Results)
	}
	logons := x.Logons()
	if len(logons) != 2 || logons[1].SenderCompID != otherSender("PTEST") || logons[1].SenderCompID == "PTEST" {
		t.Errorf("logons %+v", logons)
	}
	if open := x.Open(); len(open) > 0 || len(rep.Leftover) > 0 {
		t.Errorf("left open: %v, leftover %v, cleanup %+v", open, rep.Leftover, rep.Cleanup)
	}
}

// The connection lost during the last probe, Run fails, though the cleanup
// on another connection succeeds.
func TestRunConnectionLostLastProbe(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{DropAfter: 2})
	rep, err := Run(context.Background(), options(t, x), "order")
	if err == nil || !strings.Contains(err.Error(), "connection lost") || !strings.Contains(rep.Error, "connection lost") {
		t.Fatalf("Run: %v", err)
	}
	if open := x.Open(); len(open) > 0 || len(rep.Leftover) > 0 || len(x.Logons()) != 2 {
		t.Errorf("left open: %v, leftover %v, logons %+v", open, rep.Leftover, x.Logons())
	}
}

// Interrupted once the market order is bought, before the probe sells it
// back: the cleanup sells it.
func TestRunFillInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := exchange(t, fakebinance.Behaviour{OnRequest: func(r fakebinance.Request) {
		if r.Name == "NewOrderSingle" && strings.Contains(r.IDs[0], "-f") {
			cancel()
		}
	}})
	rep, err := Run(ctx, options(t, x), "fill")
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Run: %v", err)
	}
	if net := x.Net(); net != "0" || rep.Position != "" {
		t.Errorf("position %s left, report %q, cleanup %+v", net, rep.Position, rep.Cleanup)
	}
	if !slices.ContainsFunc(rep.Cleanup, func(c Cleanup) bool { return c.Outcome == "trade back" && strings.Contains(c.Detail, "now flat") }) {
		t.Errorf("cleanup %+v", rep.Cleanup)
	}
}

// Every sell-back refused, the position left is reported exactly, and Run
// fails.
func TestRunPositionLeft(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{RejectSells: 99})
	rep, err := Run(context.Background(), options(t, x), "fill")
	if err == nil || !strings.Contains(err.Error(), "position not traded back: BTCUSDT 0.001 bought, not sold back") {
		t.Fatalf("Run: %v", err)
	}
	if rep.Position != "BTCUSDT 0.001 bought, not sold back" || x.Net() != "1/1000" {
		t.Errorf("report %q, fake %s", rep.Position, x.Net())
	}
	if d := rep.Results[0].Checks[0].Detail; strings.Contains(d, "sold back:") || !strings.Contains(d, "REJECTED") {
		t.Errorf("detail %q", d)
	}
	sells := 0
	for _, r := range x.Requests() {
		if r.Name == "NewOrderSingle" && strings.Contains(r.IDs[0], "-fs") {
			sells++
		}
	}
	if sells != 1+maxFlatten {
		t.Errorf("%d sells", sells)
	}
}

// The sell-back's fills never known (its TRADE never comes, its
// cancellation refused -2011): nothing more is sold, the exposure is
// reported unknown, and Run fails.
func TestRunExposureUnknown(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{DelaySellTrade: "never"})
	rep, err := Run(context.Background(), options(t, x), "fill")
	if err == nil || !strings.Contains(err.Error(), "exposure unknown") || !strings.Contains(rep.Position, "exposure unknown: the fills of") {
		t.Fatalf("Run: %v, position %q", err, rep.Position)
	}
	sells := 0
	for _, r := range x.Requests() {
		if r.Name == "NewOrderSingle" && strings.Contains(r.IDs[0], "-fs") {
			sells++
		}
	}
	if sells != 1 || x.Net() != "0" {
		t.Errorf("%d sells, the fake's position %s", sells, x.Net())
	}
}

// STOP_ON_FAILURE's observation window lost with the connection: no
// verdict but inconclusive.
func TestRunStopOnFailureConnectionLost(t *testing.T) {
	// 1: d1's order, 2: d1's cancel/replace, 3: d2's, answered, then the
	// connection closed.
	x := exchange(t, fakebinance.Behaviour{DropAfterAnswer: 3})
	rep, err := Run(context.Background(), options(t, x), "cancel-replace")
	if err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("Run: %v", err)
	}
	cs := rep.Results[0].Checks
	if got := verdicts(cs); !slices.Equal(got, []Verdict{ok, unk, unk}) || !strings.Contains(cs[1].Detail, "connection lost while watching") {
		t.Errorf("verdicts %v: %+v", got, cs)
	}
	if open := x.Open(); len(open) > 0 {
		t.Errorf("left open: %v", open)
	}
}

// The position counts each market order by its own reports: the largest
// CumQty, final once a final status (not -1007) is seen.
func TestPosition(t *testing.T) {
	none := binancefix.Optional[int32]{}
	p := newPosition()
	p.watch("b", binancefix.SideBuy)
	p.watch("s", binancefix.SideSell)
	p.report("b", "", decimal{1, -3}, binancefix.OrdStatusFilled, none)
	p.report("x", "", decimal{7, 0}, binancefix.OrdStatusFilled, none) // not watched
	p.report("s", "", decimal{25, -5}, binancefix.OrdStatusPartiallyFilled, none)
	if q, side, ok := p.flat(); !ok || side != binancefix.SideSell || q != (decimal{75, -5}) || !slices.Equal(p.pending(), []string{"s"}) {
		t.Errorf("%v %v %v, pending %v", q, side, ok, p.pending())
	}
	// A cancellation's report names the order as OrigClOrdID; CumQty never
	// goes down.
	p.report("x2", "s", decimal{1000000, -9}, binancefix.OrdStatusCanceled, none)
	p.report("s", "", decimal{0, 0}, binancefix.OrdStatusNew, none)
	if _, _, ok := p.flat(); ok || !p.settled() || len(p.pending()) > 0 || p.String() != "flat" {
		t.Errorf("not flat: %s, pending %v", p, p.pending())
	}
	// -1007: not final; given up, the exposure is unknown.
	p.watch("u", binancefix.SideSell)
	p.report("u", "", decimal{}, binancefix.OrdStatusRejected, binancefix.Some[int32](-1007))
	if !slices.Equal(p.pending(), []string{"u"}) {
		t.Errorf("pending %v", p.pending())
	}
	p.giveUp("u")
	if p.settled() || len(p.pending()) > 0 || !strings.Contains(p.String(), "exposure unknown: the fills of u (Sell) unknown, as seen flat") {
		t.Errorf("%s", p)
	}
	p.watch("v", binancefix.SideSell)
	p.report("v", "", decimal{25, -5}, binancefix.OrdStatusFilled, none)
	if q, side, ok := p.flat(); !ok || side != binancefix.SideBuy || q != (decimal{25, -5}) || !strings.Contains(p.String(), "0.00025 sold, not bought back") {
		t.Errorf("%v %v %v %s", q, side, ok, p)
	}
}

// Requests are spaced by Interval at least, and bounded in number.
func TestRunPaced(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{})
	o := options(t, x)
	o.Interval = 40 * time.Millisecond
	if _, err := Run(context.Background(), o, "order", "cancel-replace"); err != nil {
		t.Fatal(err)
	}
	reqs := x.Requests()
	if len(reqs) < 6 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		// Received times: the transport's jitter aside.
		if d := reqs[i].At.Sub(reqs[i-1].At); d < 30*time.Millisecond {
			t.Errorf("request %d %v after the last", i, d)
		}
	}

	p := &pacer{}
	for range maxRequests {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.wait(context.Background()); !errors.Is(err, errTooMany) {
		t.Errorf("request %d: %v", maxRequests+1, err)
	}
}

func TestLogon(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{})
	lr := Logon(context.Background(), options(t, x))
	if !lr.Accepted || lr.Error != "" || lr.LogonAck == nil || lr.LogonAck.SeqNum != 1 || lr.LogonAck.HeartBtInt != 5 ||
		lr.LogonAck.UUID != "fake-PTEST" || lr.LogonAck.SchemaDeprecated != "False" || lr.SignatureTime != "us" {
		t.Fatalf("%+v", lr)
	}
	var types []string
	for _, m := range lr.Messages {
		dir := "<"
		if m.Sent {
			dir = ">"
		}
		types = append(types, dir+m.Type)
	}
	if !slices.Equal(types, []string{">Logon", "<LogonAck", ">Logout", "<Logout"}) {
		t.Errorf("messages %v", types)
	}
	if _, err := time.Parse("20060102-15:04:05.000000", lr.SignedSendingTime); err != nil {
		t.Errorf("signed %q: %v", lr.SignedSendingTime, err)
	}
	// The API key is not shown.
	for _, m := range lr.Messages {
		if strings.Contains(m.String(), "api-key") {
			t.Errorf("%s", m)
		}
	}
}

// Refused, the Logon tells why; a LogonAck numbered otherwise than 1 is
// Binance's acceptance, which the client refuses.
func TestLogonRefused(t *testing.T) {
	x := exchange(t, fakebinance.Behaviour{}, binancefix.SignatureTimeMillis)
	lr := Logon(context.Background(), options(t, x))
	if lr.Accepted || lr.Refusal == nil || lr.Refusal.Message != "Reject" || *lr.Refusal.ErrorCode != -1022 || lr.Error == "" {
		t.Fatalf("%+v", lr)
	}

	x = fakebinance.New(fakebinance.Options{PublicKey: testKey(t).Public().(ed25519.PublicKey), LogonAckSeq: 2})
	lr = Logon(context.Background(), options(t, x))
	if !lr.Accepted || lr.LogonAck.SeqNum != 2 || !strings.Contains(lr.Error, "sequence number 2, expected 1") {
		t.Fatalf("%+v", lr)
	}
	rep, err := Run(context.Background(), options(t, x))
	if err == nil || len(rep.Results) != 1 || rep.Results[0].Verdict != Contradicted {
		t.Fatalf("Run: %v %+v", err, rep.Results)
	}
}

// LogonAuto tries the forms in turn, twice HeartBtInt apart, until one is
// accepted.
func TestLogonAuto(t *testing.T) {
	var mu sync.Mutex
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return nil
	}
	forms := binancefix.SignatureTimes()

	x := exchange(t, fakebinance.Behaviour{}, binancefix.SignatureTimeNanos)
	o := options(t, x)
	o.Sleep = sleep
	res := LogonAuto(context.Background(), o, forms)
	if res.Accepted != "ns" || res.Error != "" || len(res.Attempts) != 4 {
		t.Fatalf("%+v", res)
	}
	for i, lr := range res.Attempts {
		if lr.SignatureTime != forms[i].String() || lr.Accepted != (i == 3) {
			t.Errorf("attempt %d: %+v", i, lr)
		}
	}
	if !slices.Equal(waits, []time.Duration{10 * time.Second, 10 * time.Second, 10 * time.Second}) {
		t.Errorf("waits %v", waits)
	}
	if logons := x.Logons(); len(logons) != 4 || logons[3].SignatureTime != "ns" {
		t.Errorf("logons %+v", logons)
	}

	// None accepted: another key.
	waits = nil
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	x = fakebinance.New(fakebinance.Options{PublicKey: pub, SignatureTimes: forms})
	o = options(t, x)
	o.Sleep = sleep
	res = LogonAuto(context.Background(), o, forms)
	if res.Accepted != "" || res.Error == "" || len(res.Attempts) != len(forms) || len(waits) != len(forms)-1 {
		t.Fatalf("%+v, waits %v", res, waits)
	}

	// Refused for another cause: no other attempt.
	x = exchange(t, fakebinance.Behaviour{})
	o = options(t, x)
	o.Sleep, o.APIKey = sleep, "another"
	res = LogonAuto(context.Background(), o, forms)
	if len(res.Attempts) != 1 || !strings.Contains(res.Error, "-2015") {
		t.Fatalf("%+v", res)
	}

	if Cooldown(5*time.Second) != 10*time.Second || Cooldown(0) != 2*time.Second {
		t.Error("Cooldown")
	}
}

func TestDecimal(t *testing.T) {
	for s, want := range map[string]decimal{"30000.00": {3000000, -2}, "0.001": {1, -3}, "12": {12, 0}, ".5": {5, -1}, "007.10": {710, -2}} {
		d, err := parseDecimal(s)
		if err != nil || d != want {
			t.Errorf("parseDecimal(%q) = %v, %v", s, d, err)
		}
	}
	for _, s := range []string{"", ".", "1.", "-1", "0", "0.000", "1e5", "1.2.3", "abc", "1234567890123456789"} {
		if d, err := parseDecimal(s); err == nil {
			t.Errorf("parseDecimal(%q) = %v", s, d)
		}
	}
	for d, want := range map[decimal]string{{3000000, -2}: "30000.00", {1, -3}: "0.001", {5, 0}: "5", {5, 2}: "500", {-15, -1}: "-1.5"} {
		if d.String() != want {
			t.Errorf("%v: %q", d, d.String())
		}
	}
	if h := (decimal{1, -3}).half(); h != (decimal{5, -4}) {
		t.Errorf("half %v", h)
	}
	if h := (decimal{4, -3}).half(); h != (decimal{2, -3}) {
		t.Errorf("half %v", h)
	}
}

func TestSelectProbes(t *testing.T) {
	ps, err := selectProbes([]string{"h", "ORDER"})
	if err != nil || len(ps) != 2 || ps[0].Name != "order" || ps[1].Name != "limits" {
		t.Errorf("%v %v", ps, err)
	}
	if _, err := selectProbes([]string{"nope"}); err == nil {
		t.Error("unknown probe accepted")
	}
	if otherSender("ABCDEFGH") != "ABCDEFGC" || otherSender("ABCDEFGC") != "ABCDEFGD" || otherSender("P1") != "P1C" {
		t.Error("otherSender")
	}
}
