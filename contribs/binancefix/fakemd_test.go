package binancefix_test

import (
	"context"
	"crypto/ed25519"
	"slices"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/fakebinance"
)

// mdEvent is what mdHandler records.
type mdEvent struct {
	kind   string
	in     binancefix.Inbound
	id     string // Symbol, MDReqID, InstrumentReqID or ReqID
	levels int    // bids and asks, trades, instruments, limits
	reject *binancefix.MarketDataRequestReject
	logout binancefix.LogoutEvent
}

// mdHandler records a market data session's events.
type mdHandler struct {
	binancefix.BaseHandler
	evs chan mdEvent
}

func (h *mdHandler) OnLogon(binancefix.LogonEvent) { h.evs <- mdEvent{kind: "logon"} }
func (h *mdHandler) OnLogout(e binancefix.LogoutEvent) {
	h.evs <- mdEvent{kind: "logout", logout: e}
}
func (h *mdHandler) OnMarketDataSnapshot(in binancefix.Inbound, m *binancefix.MarketDataSnapshot) {
	h.evs <- mdEvent{kind: "snapshot", in: in, id: string(m.Symbol), levels: len(m.MDEntriesBids) + len(m.MDEntriesAsks)}
}
func (h *mdHandler) OnMarketDataIncrementalTrade(in binancefix.Inbound, m *binancefix.MarketDataIncrementalTrade) {
	h.evs <- mdEvent{kind: "trade", in: in, id: string(m.Symbol), levels: len(m.MDEntries)}
}
func (h *mdHandler) OnMarketDataIncrementalBookTicker(in binancefix.Inbound, m *binancefix.MarketDataIncrementalBookTicker) {
	h.evs <- mdEvent{kind: "ticker", in: in, id: string(m.Symbol), levels: len(m.MDEntriesBids) + len(m.MDEntriesAsks)}
}
func (h *mdHandler) OnMarketDataIncrementalDepth(in binancefix.Inbound, m *binancefix.MarketDataIncrementalDepth) {
	h.evs <- mdEvent{kind: "depth", in: in, id: string(m.Symbol), levels: len(m.MDEntriesBids) + len(m.MDEntriesAsks)}
}
func (h *mdHandler) OnMarketDataRequestReject(in binancefix.Inbound, m *binancefix.MarketDataRequestReject) {
	r := *m
	h.evs <- mdEvent{kind: "mdreject", in: in, id: string(m.MDReqID), reject: &r}
}
func (h *mdHandler) OnInstrumentList(in binancefix.Inbound, m *binancefix.InstrumentList) {
	h.evs <- mdEvent{kind: "instruments", in: in, id: string(m.InstrumentReqID), levels: len(m.RelatedSym)}
}
func (h *mdHandler) OnLimitResponse(in binancefix.Inbound, m *binancefix.LimitResponse) {
	h.evs <- mdEvent{kind: "limits", in: in, id: string(m.ReqID), levels: len(m.LimitIndicators)}
}
func (h *mdHandler) OnUnknown(in binancefix.Inbound, _ []byte, err error) {
	h.evs <- mdEvent{kind: "unknown", in: in}
}

// next returns the next event, which must be of kind.
func (h *mdHandler) next(t *testing.T, kind string) mdEvent {
	t.Helper()
	select {
	case e := <-h.evs:
		if e.kind != kind {
			t.Fatalf("%s event %+v, want %s", e.kind, e, kind)
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s event", kind)
		return mdEvent{}
	}
}

func mdRequest(id string, depth uint16, types []binancefix.MdEntryType, symbols ...string) *binancefix.MarketDataRequest {
	m := &binancefix.MarketDataRequest{SubscriptionRequestType: binancefix.SubscriptionRequestTypeSubscribe, MDReqID: []byte(id),
		MarketDepth: binancefix.Some(depth)}
	for _, s := range symbols {
		m.RelatedSym = append(m.RelatedSym, binancefix.MarketDataRequestRelatedSym{Symbol: []byte(s)})
	}
	for _, t := range types {
		m.MDEntryTypes = append(m.MDEntryTypes, binancefix.MDEntryTypes{MDEntryType: t})
	}
	return m
}

var (
	tradeTypes = []binancefix.MdEntryType{binancefix.MdEntryTypeTrade}
	bookTypes  = []binancefix.MdEntryType{binancefix.MdEntryTypeBid, binancefix.MdEntryTypeOffer}
)

// The client against the fake market data endpoint: book streams get
// their snapshot on activation, trades nothing; data reach each stream's
// subscription, still for a while after its unsubscribe; Binance's
// refusals (an unknown symbol, too many streams) reach the subscription
// they name; an instrument list split in two answers its request with its
// first part; the end of the connection lists every subscription.
func TestFakeMarketData(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	x := fakebinance.New(fakebinance.Options{PublicKey: pub, Kind: binancefix.MarketData,
		MarketData: fakebinance.MarketData{MaxStreams: 4, BookLevels: 100, InstrumentsPerMessage: 2, Linger: 1}})
	h := &mdHandler{evs: make(chan mdEvent, 256)}
	c, err := binancefix.New(binancefix.Config{Kind: binancefix.MarketData, Dialer: x, APIKey: "k", PrivateKey: priv,
		SenderCompID: "MD1", Handler: h, LogoutTimeout: time.Second, Reconnect: binancefix.Reconnect{Disabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	t.Cleanup(func() {
		c.Stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	})
	h.next(t, "logon")
	// requests waits for the exchange to have received n requests.
	requests := func(n int) []fakebinance.Request {
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
	subscribe := func(m *binancefix.MarketDataRequest) *binancefix.Subscription {
		t.Helper()
		s, err := c.Subscribe(m)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	tr := subscribe(mdRequest("TR", 1, tradeTypes, "BTCUSDT"))
	dp := subscribe(mdRequest("DP", 500, bookTypes, "ETHUSDT"))
	tk := subscribe(mdRequest("TK", 1, bookTypes, "ETHBTC"))
	if e := h.next(t, "snapshot"); e.in.Subscription != dp || e.id != "ETHUSDT" || e.levels != 200 {
		t.Errorf("depth snapshot %+v", e)
	}
	if e := h.next(t, "snapshot"); e.in.Subscription != tk || e.id != "ETHBTC" || e.levels != 2 {
		t.Errorf("ticker snapshot %+v", e)
	}
	requests(3)
	if st := tr.Status(); st.Streams[0].Seen {
		t.Errorf("trades seen before any: %+v", st)
	}
	for _, tc := range []struct {
		symbol, kind string
		s            *binancefix.Subscription
	}{{"BTCUSDT", "trade", tr}, {"ETHUSDT", "depth", dp}, {"ETHBTC", "ticker", tk}} {
		if n := x.Publish(tc.symbol); n != 1 {
			t.Errorf("%s: %d published", tc.symbol, n)
		}
		if e := h.next(t, tc.kind); e.in.Subscription != tc.s || e.id != tc.symbol {
			t.Errorf("%s %+v", tc.kind, e)
		}
	}
	if st := tr.Status(); !st.Streams[0].Seen || st.Streams[0].FirstTemplate != binancefix.MarketDataIncrementalTradeTemplateID {
		t.Errorf("trades %+v", st)
	}

	bad := subscribe(mdRequest("BAD", 1, tradeTypes, "NOPE"))
	if e := h.next(t, "mdreject"); e.in.Subscription != bad || e.reject.ErrorCode != binancefix.Some[int32](-1121) {
		t.Errorf("unknown symbol %+v", e)
	}
	many := subscribe(mdRequest("MANY", 1, tradeTypes, "ETHUSDT", "ETHBTC"))
	if e := h.next(t, "mdreject"); e.in.Subscription != many || e.reject.MDReqRejReason != binancefix.Some(binancefix.MdReqRejReasonTooManySubscriptions) {
		t.Errorf("too many streams %+v", e)
	}
	if st := many.Status(); len(st.Rejects) != 1 || st.Rejects[0].Unsubscribe {
		t.Errorf("rejects %+v", st.Rejects)
	}

	if err := tr.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	requests(6)
	if n := x.Publish("BTCUSDT"); n != 1 {
		t.Errorf("%d published after the unsubscribe", n)
	}
	if e := h.next(t, "trade"); e.in.Subscription != tr {
		t.Errorf("trade after the unsubscribe %+v", e)
	}
	if n := x.Publish("BTCUSDT"); n != 0 {
		t.Errorf("%d published once the stream stopped", n)
	}

	r, err := c.Send(&binancefix.InstrumentListRequest{InstrumentListRequestType: binancefix.InstrumentListRequestTypeAllInstruments,
		InstrumentReqID: []byte("I1")})
	if err != nil {
		t.Fatal(err)
	}
	if e := h.next(t, "instruments"); e.in.Receipt != r || e.levels != 2 {
		t.Errorf("first part %+v", e)
	}
	if e := h.next(t, "instruments"); e.in.Receipt != nil || e.levels != 1 {
		t.Errorf("second part %+v", e)
	}
	if r.State() != binancefix.Answered {
		t.Errorf("InstrumentListRequest %v", r.State())
	}
	lr, err := c.Send(&binancefix.LimitQuery{ReqID: []byte("L1")})
	if err != nil {
		t.Fatal(err)
	}
	if e := h.next(t, "limits"); e.in.Receipt != lr || e.levels != 2 {
		t.Errorf("limits %+v", e)
	}
	if _, err := c.Send(&binancefix.NewOrderSingle{ClOrdID: []byte("o")}); err == nil {
		t.Error("an order sent on a market data session")
	}
	var names []string
	for _, q := range x.Requests() {
		names = append(names, q.Name+" "+q.IDs[0])
	}
	want := []string{"MarketDataRequest TR", "MarketDataRequest DP", "MarketDataRequest TK", "MarketDataRequest BAD",
		"MarketDataRequest MANY", "MarketDataRequest TR", "InstrumentListRequest I1", "LimitQuery L1"}
	if !slices.Equal(names, want) {
		t.Errorf("requests %q, want %q", names, want)
	}

	c.Stop()
	e := h.next(t, "logout")
	if !slices.Equal(e.logout.Subscriptions, []*binancefix.Subscription{tr, dp, tk, bad, many}) || !e.logout.Clean ||
		len(e.logout.PossiblyTransmitted) != 0 {
		t.Errorf("LogoutEvent %+v", e.logout)
	}
	if st := tr.Status(); !st.Ended || st.Request != binancefix.PhaseWritten || st.Unsubscribe != binancefix.PhaseWritten {
		t.Errorf("status %+v", st)
	}
}
