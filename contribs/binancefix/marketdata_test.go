package binancefix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
)

var (
	tradeTypes = []fixsbe.MdEntryType{fixsbe.MdEntryTypeTrade}
	bookTypes  = []fixsbe.MdEntryType{fixsbe.MdEntryTypeBid, fixsbe.MdEntryTypeOffer}
)

// mdRequest returns a subscription of MDReqID id to symbols, of entry
// types types and MarketDepth depth, null if 0.
func mdRequest(id string, depth uint16, types []fixsbe.MdEntryType, symbols ...string) *MarketDataRequest {
	m := &MarketDataRequest{SubscriptionRequestType: fixsbe.SubscriptionRequestTypeSubscribe, MDReqID: []byte(id)}
	if depth > 0 {
		m.MarketDepth = Some(depth)
	}
	for _, s := range symbols {
		m.RelatedSym = append(m.RelatedSym, MarketDataRequestRelatedSym{Symbol: []byte(s)})
	}
	for _, t := range types {
		m.MDEntryTypes = append(m.MDEntryTypes, MDEntryTypes{MDEntryType: t})
	}
	return m
}

// trades, tickers and depths return subscriptions of symbols' trades,
// book tickers and depth, as binancemd makes them.
func trades(id string, symbols ...string) *MarketDataRequest {
	return mdRequest(id, 1, tradeTypes, symbols...)
}
func tickers(id string, symbols ...string) *MarketDataRequest {
	return mdRequest(id, 1, bookTypes, symbols...)
}
func depths(id string, depth uint16, symbols ...string) *MarketDataRequest {
	return mdRequest(id, depth, bookTypes, symbols...)
}

// tradeOf returns a trade of symbol, qty at px, of exponents pe and qe.
func tradeOf(symbol string, px, qty int64, pe, qe int8) *MarketDataIncrementalTrade {
	return &MarketDataIncrementalTrade{TransactTime: 1_760_000_000_000_000, PriceExponent: pe, QtyExponent: qe,
		MDEntries: []MDEntries{{TradeID: 1, MDEntryPx: px, MDEntrySize: qty, AggressorSide: Some(fixsbe.SideBuy)}}, Symbol: []byte(symbol)}
}

// snapshotOf returns a snapshot of symbol, of levels per side.
func snapshotOf(symbol string, levels int) *MarketDataSnapshot {
	m := &MarketDataSnapshot{LastBookUpdateID: Some[int64](100), PriceExponent: -2, QtyExponent: -8, Symbol: []byte(symbol)}
	for i := range int64(levels) {
		m.MDEntriesBids = append(m.MDEntriesBids, MarketDataSnapshotMDEntriesBids{MDEntryPx: 6_543_200 - i, MDEntrySize: 1 + i})
		m.MDEntriesAsks = append(m.MDEntriesAsks, MarketDataSnapshotMDEntriesAsks{MDEntryPx: 6_543_300 + i, MDEntrySize: 1 + i})
	}
	return m
}

// depthOf returns a depth update of symbol, of update IDs first to last.
func depthOf(symbol string, first, last int64) *MarketDataIncrementalDepth {
	return &MarketDataIncrementalDepth{FirstBookUpdateID: first, LastBookUpdateID: last, PriceExponent: -2, QtyExponent: -8,
		MDEntriesBids: []MarketDataIncrementalDepthMDEntriesBids{{MDEntryPx: 6_543_200, MDEntrySize: Some[int64](5)}, {MDEntryPx: 6_543_100}},
		Symbol:        []byte(symbol)}
}

// tickerOf returns a book ticker update of symbol.
func tickerOf(symbol string) *MarketDataIncrementalBookTicker {
	return &MarketDataIncrementalBookTicker{LastBookUpdateID: 101, PriceExponent: -2, QtyExponent: -8,
		MDEntriesBids: []MarketDataIncrementalBookTickerMDEntriesBids{{MDEntryPx: 6_543_200, MDEntrySize: Some[int64](7)}},
		MDEntriesAsks: []MarketDataIncrementalBookTickerMDEntriesAsks{{MDEntryPx: 6_543_300}},
		Symbol:        []byte(symbol)}
}

// mdSession starts a market data Client, its timers on interval, logged
// on to a peer, cfg and hooks applied.
func mdSession(t *testing.T, interval time.Duration, cfg func(*Config), hooks ...func(*Client)) (*Client, *fake, *peer, *recorder, <-chan error) {
	t.Helper()
	f, h := newFake(t), newRecorder()
	c0 := testConfig(f, h)
	c0.Kind = MarketData
	if cfg != nil {
		cfg(&c0)
	}
	c, done := start(t, c0, interval, hooks...)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	return c, f, p, h, done
}

// expectMDR returns the next MarketDataRequest the peer received, and
// its seqNum.
func (p *peer) expectMDR(t *testing.T) (*fixsbe.MarketDataRequest, uint32) {
	t.Helper()
	m := p.expect(t, fixsbe.MarketDataRequestTemplateID)
	return decode[fixsbe.MarketDataRequest](t, m), m.h.SeqNum
}

// waitSub waits for s's status to satisfy ok, and returns it.
func waitSub(t *testing.T, s *Subscription, ok func(SubscriptionStatus) bool) SubscriptionStatus {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		st := s.Status()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscription %s: status %+v", s.ID(), st)
		}
		time.Sleep(time.Millisecond)
	}
}

func written(st SubscriptionStatus) bool { return st.Request == PhaseWritten }

// A subscription Binance says nothing of stays written, its streams
// unseen: a quiet trade stream is not told from a refused one. Its
// unsubscribe is answered with nothing either. The connection's end lists
// it, not as a request possibly transmitted.
func TestSubscriptionQuiet(t *testing.T) {
	c, _, p, h, done := mdSession(t, time.Hour, func(c *Config) { c.Reconnect.Disabled = true })
	s, err := c.Subscribe(trades("T1", "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := p.expectMDR(t)
	if m.SubscriptionRequestType != fixsbe.SubscriptionRequestTypeSubscribe || string(m.MDReqID) != "T1" || len(m.RelatedSym) != 1 ||
		string(m.RelatedSym[0].Symbol) != "BTCUSDT" || len(m.MDEntryTypes) != 1 || m.MDEntryTypes[0].MDEntryType != fixsbe.MdEntryTypeTrade {
		t.Errorf("MarketDataRequest %+v", m)
	}
	st := waitSub(t, s, written)
	want := []StreamStatus{{Stream: Stream{"BTCUSDT", StreamTrade}}}
	if st.Unsubscribe != PhaseNone || len(st.Rejects) != 0 || !slices.Equal(st.Streams, want) || st.Ended {
		t.Errorf("status %+v", st)
	}
	if err := s.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	if err := s.Unsubscribe(); !errors.Is(err, ErrUnsubscribed) {
		t.Errorf("second Unsubscribe: %v", err)
	}
	if u, _ := p.expectMDR(t); u.SubscriptionRequestType != fixsbe.SubscriptionRequestTypeUnsubscribe || string(u.MDReqID) != "T1" ||
		u.MarketDepth != Some[uint16](1) || len(u.RelatedSym) != 0 || len(u.MDEntryTypes) != 0 {
		t.Errorf("unsubscribe %+v", u)
	}
	waitSub(t, s, func(st SubscriptionStatus) bool { return st.Unsubscribe == PhaseWritten })
	select {
	case <-s.Done():
		t.Fatal("Done before the connection's end")
	default:
	}

	c.Stop()
	p.expect(t, fixsbe.LogoutTemplateID)
	p.send(t, &fixsbe.Logout{Text: []byte("Logout acknowledgment.")})
	e := h.next(t, "logout").ev.(LogoutEvent)
	if !slices.Equal(e.Subscriptions, []*Subscription{s}) || len(e.PossiblyTransmitted) != 0 || len(e.NeverWritten) != 0 {
		t.Errorf("LogoutEvent %+v", e)
	}
	<-s.Done()
	if st := s.Status(); !st.Ended || !errors.Is(st.Err, ErrLoggedOut) || st.Request != PhaseWritten || st.Unsubscribe != PhaseWritten {
		t.Errorf("status %+v", st)
	}
	if err := s.Unsubscribe(); !errors.Is(err, ErrUnsubscribed) {
		t.Errorf("Unsubscribe after the end: %v", err)
	}
	if err := runErr(t, done); err != nil {
		t.Errorf("Run: %v", err)
	}
}

// Each stream is seen on its own: a subscription of two symbols, one
// active, the other quiet. Data of no stream subscribed come without
// Subscription.
func TestSubscriptionPartialActivation(t *testing.T) {
	c, _, p, h, _ := mdSession(t, time.Hour, nil)
	s, err := c.Subscribe(depths("D1", 10, "BTCUSDT", "ETHUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	p.expectMDR(t)
	seq := p.send(t, snapshotOf("BTCUSDT", 3))
	if r := h.next(t, "snapshot"); r.in.Subscription != s || r.id != "BTCUSDT" || r.text != "0.00000001@65432.00 0.00000002@65431.99 0.00000003@65431.98|0.00000001@65433.00 0.00000002@65433.01 0.00000003@65433.02" {
		t.Errorf("snapshot %+v", r)
	}
	st := s.Status()
	if b := st.Streams[0]; b.Stream != (Stream{"BTCUSDT", StreamDepth}) || !b.Seen || b.FirstTemplate != MarketDataSnapshotTemplateID || b.FirstSeqNum != seq || b.FirstReceived.IsZero() {
		t.Errorf("BTCUSDT %+v", b)
	}
	if e := st.Streams[1]; e.Stream != (Stream{"ETHUSDT", StreamDepth}) || e.Seen {
		t.Errorf("ETHUSDT %+v", e)
	}
	// An update before any snapshot is activity all the same.
	seq = p.send(t, depthOf("ETHUSDT", 5, 7))
	if r := h.next(t, "depth"); r.in.Subscription != s || r.text != "0.00000005@65432.00 -@65431.00|" {
		t.Errorf("depth %+v", r)
	}
	if e := s.Status().Streams[1]; !e.Seen || e.FirstTemplate != MarketDataIncrementalDepthTemplateID || e.FirstSeqNum != seq {
		t.Errorf("ETHUSDT %+v", e)
	}
	p.send(t, tradeOf("BTCUSDT", 1, 1, 0, 0))
	if r := h.next(t, "trade"); r.in.Subscription != nil {
		t.Errorf("trade of no subscription: %v", r.in.Subscription.ID())
	}
	p.send(t, snapshotOf("ETHBTC", 1))
	if r := h.next(t, "snapshot"); r.in.Subscription != nil {
		t.Errorf("snapshot of no subscription: %v", r.in.Subscription.ID())
	}
}

// A stream belongs to one subscription of a connection; a symbol's book
// ticker and depth share their snapshot, and so cannot both be
// subscribed; MDReqIDs are unique. Nothing refused is written.
func TestSubscribeOverlap(t *testing.T) {
	c, _, p, _, _ := mdSession(t, time.Hour, nil)
	ok := func(m *MarketDataRequest) {
		t.Helper()
		if _, err := c.Subscribe(m); err != nil {
			t.Fatalf("Subscribe(%s): %v", m.MDReqID, err)
		}
		if got, _ := p.expectMDR(t); string(got.MDReqID) != string(m.MDReqID) {
			t.Fatalf("written %s, want %s", got.MDReqID, m.MDReqID)
		}
	}
	refused := func(m *MarketDataRequest, want error) {
		t.Helper()
		if s, err := c.Subscribe(m); !errors.Is(err, want) || s != nil {
			t.Errorf("Subscribe(%s %v): %v, want %v", m.MDReqID, m.RelatedSym, err, want)
		}
	}
	ok(trades("A", "BTCUSDT"))
	refused(trades("A", "ETHUSDT"), ErrDuplicateID)
	refused(trades("B", "ETHUSDT", "BTCUSDT"), ErrStreamTaken)
	ok(tickers("C", "BTCUSDT"))
	refused(depths("D", 10, "BTCUSDT"), ErrStreamTaken)
	refused(tickers("D", "BTCUSDT"), ErrStreamTaken)
	ok(depths("D", 10, "ETHUSDT"))
	refused(mdRequest("E", 1, append(slices.Clone(tradeTypes), bookTypes...), "ETHUSDT"), ErrStreamTaken)
	ok(mdRequest("E", 1, append(slices.Clone(tradeTypes), bookTypes...), "ETHBTC"))
}

// A stream subscribed on a connection, unsubscribed or not, is not
// subscribed again on it: once reconnected, it is.
func TestResubscribeAfterReconnect(t *testing.T) {
	c, f, p, h, _ := mdSession(t, time.Hour, nil)
	a, err := c.Subscribe(trades("A", "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	p.expectMDR(t)
	if err := a.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	p.expectMDR(t)
	if _, err := c.Subscribe(trades("B", "BTCUSDT")); !errors.Is(err, ErrStreamTaken) {
		t.Errorf("subscribed again: %v", err)
	}
	if _, err := c.Subscribe(trades("A", "ETHUSDT")); !errors.Is(err, ErrDuplicateID) {
		t.Errorf("MDReqID reused: %v", err)
	}
	c.Reconnect(c.Epoch())
	p.expect(t, fixsbe.LogoutTemplateID)
	p.send(t, &fixsbe.Logout{})
	if e := h.next(t, "logout").ev.(LogoutEvent); !slices.Equal(e.Subscriptions, []*Subscription{a}) || !e.Clean {
		t.Errorf("LogoutEvent %+v", e)
	}
	p2 := f.accept()
	p2.logon(t, &c.cfg)
	h.next(t, "logon")
	b, err := c.Subscribe(trades("A", "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	if b == a || b.Epoch() != 2 || a.Epoch() != 1 {
		t.Errorf("epochs %d, %d", a.Epoch(), b.Epoch())
	}
	p2.expectMDR(t)
	p2.send(t, tradeOf("BTCUSDT", 1, 1, 0, 0))
	if r := h.next(t, "trade"); r.in.Subscription != b {
		t.Error("trade not the new subscription's")
	}
	if a.Status().Streams[0].Seen {
		t.Error("the old subscription saw the new connection's trade")
	}
}

// Data after the unsubscribe are still the subscription's; rejects come
// late, after data, after the unsubscribe, by MDReqID or by RefSeqNum,
// and stay visible.
func TestSubscriptionLateData(t *testing.T) {
	c, _, p, h, _ := mdSession(t, time.Hour, nil)
	s, err := c.Subscribe(tickers("T", "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	_, seq := p.expectMDR(t)
	p.send(t, snapshotOf("BTCUSDT", 1))
	h.next(t, "snapshot")
	rejSeq := p.send(t, &MarketDataRequestReject{MDReqRejReason: Some(fixsbe.MdReqRejReasonTooManySubscriptions), ErrorCode: Some[int32](-1),
		MDReqID: []byte("T"), Text: []byte("late")})
	if r := h.next(t, "mdreject"); r.in.Subscription != s {
		t.Error("reject without its subscription")
	}
	if err := s.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	_, unsubSeq := p.expectMDR(t)
	waitSub(t, s, func(st SubscriptionStatus) bool { return st.Unsubscribe == PhaseWritten })
	p.send(t, tickerOf("BTCUSDT"))
	if r := h.next(t, "ticker"); r.in.Subscription != s || r.text != "0.00000007@65432.00|-@65433.00" {
		t.Errorf("ticker after the unsubscribe %+v", r)
	}
	p.send(t, &MarketDataRequestReject{MDReqID: []byte("T"), Text: []byte("unsubscribe")})
	h.next(t, "mdreject")
	p.send(t, &Reject{RefSeqNum: Some(unsubSeq), ErrorCode: Some[int32](-1221), Text: []byte("of the unsubscribe")})
	if r := h.next(t, "reject"); r.in.Subscription != s || r.in.Receipt != nil {
		t.Error("Reject of the unsubscribe without its subscription")
	}
	p.send(t, &Reject{RefSeqNum: Some(seq), Text: []byte("of the request")})
	if r := h.next(t, "reject"); r.in.Subscription != s {
		t.Error("Reject of the request without its subscription")
	}
	p.send(t, &MarketDataRequestReject{MDReqID: []byte("other")})
	if r := h.next(t, "mdreject"); r.in.Subscription != nil {
		t.Error("reject of another MDReqID given a subscription")
	}
	st := s.Status()
	var got []string
	for _, r := range st.Rejects {
		got = append(got, fmt.Sprintf("%d %q %v %v", r.TemplateID, r.Text, r.Unsubscribe, r.Reason.Valid))
	}
	want := []string{`203 "late" false true`, `203 "unsubscribe" true false`, `20003 "of the unsubscribe" true false`, `20003 "of the request" false false`}
	if !slices.Equal(got, want) {
		t.Errorf("rejects %q, want %q", got, want)
	}
	if r := st.Rejects[0]; r.SeqNum != rejSeq || r.ErrorCode != Some[int32](-1) || r.Reason.Value != fixsbe.MdReqRejReasonTooManySubscriptions {
		t.Errorf("first reject %+v", r)
	}
	if b := st.Streams[0]; b.FirstTemplate != MarketDataSnapshotTemplateID {
		t.Errorf("first activity %+v", b)
	}
	if st.Request != PhaseWritten || st.Unsubscribe != PhaseWritten || st.Ended {
		t.Errorf("status %+v", st)
	}
}

// A snapshot of MaxMarketDepth levels per side fits the market data
// session's default frame limit, not order entry's.
func TestMaxDepthSnapshot(t *testing.T) {
	// The documented size, with a 20-byte symbol.
	f, err := encode(snapshotOf("ABCDEFGHIJKLMNOPQRST", MaxMarketDepth))
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 160063 {
		t.Errorf("largest snapshot framed: %d bytes", len(f))
	}
	c, _, p, h, _ := mdSession(t, time.Hour, nil)
	if c.cfg.MaxFrameLength != DefaultMarketDataMaxFrameLength {
		t.Fatalf("MaxFrameLength %d", c.cfg.MaxFrameLength)
	}
	s, err := c.Subscribe(depths("D", MaxMarketDepth, "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := p.expectMDR(t); m.MarketDepth != Some[uint16](MaxMarketDepth) {
		t.Errorf("MarketDepth %v", m.MarketDepth)
	}
	frame := p.frameOf(t, snapshotOf("BTCUSDT", MaxMarketDepth))
	if len(frame) <= DefaultMaxFrameLength || len(frame) > DefaultMarketDataMaxFrameLength {
		t.Fatalf("frame of %d bytes", len(frame))
	}
	p.write(t, frame)
	r := h.next(t, "snapshot")
	if r.in.Subscription != s || strings.Count(r.text, "@") != 2*MaxMarketDepth || !strings.HasSuffix(r.text, "0.00005000@65482.99") {
		t.Errorf("snapshot of %d levels, ending %q", strings.Count(r.text, "@"), r.text[len(r.text)-40:])
	}
}

// Exponents are per message: the same field takes another from one
// message to the next.
func TestExponentsChange(t *testing.T) {
	c, _, p, h, _ := mdSession(t, time.Hour, nil)
	if _, err := c.Subscribe(trades("T", "BTCUSDT")); err != nil {
		t.Fatal(err)
	}
	p.expectMDR(t)
	for _, tc := range []struct {
		px, qty int64
		pe, qe  int8
		want    string
	}{
		{6_543_210, 150_000_000, -2, -8, "1.50000000@65432.10"},
		{654_321, 15, -1, -1, "1.5@65432.1"},
		{65, 2, 3, 0, "2@65000"},
		{0, 0, -4, 2, "0@0.0000"},
		// Any exponent but the null is the schema's.
		{7, 3, 127, -127, "0." + strings.Repeat("0", 126) + "3@7" + strings.Repeat("0", 127)},
	} {
		p.send(t, tradeOf("BTCUSDT", tc.px, tc.qty, tc.pe, tc.qe))
		if r := h.next(t, "trade"); r.text != tc.want {
			t.Errorf("trade %d %d, exponents %d %d: %q, want %q", tc.px, tc.qty, tc.pe, tc.qe, r.text, tc.want)
		}
	}
}

// A snapshot's LastBookUpdateID is optional in the schema: a snapshot
// without reaches the Handler, null.
func TestSnapshotWithoutUpdateID(t *testing.T) {
	c, _, p, h, _ := mdSession(t, time.Hour, nil)
	s, err := c.Subscribe(tickers("T", "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	p.expectMDR(t)
	m := snapshotOf("BTCUSDT", 1)
	m.LastBookUpdateID = Optional[int64]{}
	p.send(t, m)
	if r := h.next(t, "snapshot"); r.in.Subscription != s {
		t.Errorf("snapshot %+v", r)
	}
	if !c.LoggedOn() {
		t.Error("connection ended")
	}
}

// A market data message the client refuses ends the connection, after our
// Logout, before reaching the Handler: a corrupted update would silently
// desynchronise a book.
func TestMalformedMarketData(t *testing.T) {
	type build func(t *testing.T, p *peer) []byte
	of := func(m encoder) build { return func(t *testing.T, p *peer) []byte { return p.frameOf(t, m) } }
	resize := func(b []byte) []byte { binary.LittleEndian.PutUint32(b, uint32(len(b))); return b }
	mod := func(m *MarketDataIncrementalTrade, f func(*MarketDataIncrementalTrade)) *MarketDataIncrementalTrade {
		f(m)
		return m
	}
	trade := func() *MarketDataIncrementalTrade { return tradeOf("BTCUSDT", 1, 1, -2, -8) }
	// header returns the frame of m, its header's field at offset off set
	// to null.
	header := func(m encoder, off int, null uint64) build {
		return func(t *testing.T, p *peer) []byte {
			b := p.frameOf(t, m)
			switch off {
			case sendingTimeOffset - lesofh.HeaderLen:
				binary.LittleEndian.PutUint64(b[lesofh.HeaderLen+off:], null)
			default:
				binary.LittleEndian.PutUint16(b[lesofh.HeaderLen+off:], uint16(null))
			}
			return b
		}
	}
	minInt64 := uint64(1) << 63
	for _, tc := range []struct {
		name  string
		frame build
		want  error
	}{
		{"null TransactTime", of(mod(trade(), func(m *MarketDataIncrementalTrade) { m.TransactTime = math.MinInt64 })), ErrMissingRequired},
		{"null price", of(mod(trade(), func(m *MarketDataIncrementalTrade) { m.MDEntries[0].MDEntryPx = math.MinInt64 })), ErrMissingRequired},
		{"null TradeID", of(mod(trade(), func(m *MarketDataIncrementalTrade) { m.MDEntries[0].TradeID = math.MinInt64 })), ErrMissingRequired},
		{"null exponent", of(mod(trade(), func(m *MarketDataIncrementalTrade) { m.PriceExponent = math.MinInt8 })), ErrMissingRequired},
		{"null QtyExponent", of(mod(trade(), func(m *MarketDataIncrementalTrade) { m.QtyExponent = math.MinInt8 })), ErrMissingRequired},
		{"null sendingTime", header(trade(), sendingTimeOffset-lesofh.HeaderLen, minInt64), ErrMissingRequired},
		{"null sendingTime of a Reject", header(&Reject{Text: []byte("x")}, sendingTimeOffset-lesofh.HeaderLen, minInt64), ErrMissingRequired},
		{"null sendingTime of a LimitResponse", header(&LimitResponse{ReqID: []byte("L")}, sendingTimeOffset-lesofh.HeaderLen, minInt64), ErrMissingRequired},
		{"null blockLength", header(trade(), 0, math.MaxUint16), ErrMissingRequired},
		{"null version", header(trade(), 6, math.MaxUint16), ErrMissingRequired},
		{"null templateId", header(trade(), 2, math.MaxUint16), ErrMissingRequired},
		{"unknown AggressorSide", of(mod(trade(), func(m *MarketDataIncrementalTrade) { m.MDEntries[0].AggressorSide = Some(fixsbe.Side('X')) })), ErrInvalidMarketData},
		{"empty symbol", of(mod(trade(), func(m *MarketDataIncrementalTrade) { m.Symbol = nil })), ErrMissingRequired},
		{"snapshot null size", of(&MarketDataSnapshot{LastBookUpdateID: Some[int64](1), Symbol: []byte("BTCUSDT"),
			MDEntriesAsks: []MarketDataSnapshotMDEntriesAsks{{MDEntryPx: 1, MDEntrySize: math.MinInt64}}}), ErrMissingRequired},
		{"ticker null update ID", of(&MarketDataIncrementalBookTicker{LastBookUpdateID: math.MinInt64, Symbol: []byte("BTCUSDT")}), ErrMissingRequired},
		{"depth First after Last", of(depthOf("BTCUSDT", 8, 7)), ErrInvalidMarketData},
		{"depth null price", of(&MarketDataIncrementalDepth{FirstBookUpdateID: 1, LastBookUpdateID: 1, Symbol: []byte("BTCUSDT"),
			MDEntriesAsks: []MarketDataIncrementalDepthMDEntriesAsks{{MDEntryPx: math.MinInt64}}}), ErrMissingRequired},
		{"unknown MDReqRejReason", of(&MarketDataRequestReject{MDReqRejReason: Some(fixsbe.MdReqRejReason('9')), MDReqID: []byte("X")}), ErrInvalidMarketData},
		{"reject without MDReqID", of(&MarketDataRequestReject{}), ErrMissingRequired},
		{"instrument without currency", of(&InstrumentList{InstrumentReqID: []byte("I"),
			RelatedSym: []InstrumentListRelatedSym{{Symbol: []byte("BTCUSDT")}}}), ErrMissingRequired},
		{"trailing bytes", func(t *testing.T, p *peer) []byte { return resize(append(p.frameOf(t, trade()), 1, 2, 3)) }, ErrInvalidMarketData},
		{"truncated", func(t *testing.T, p *peer) []byte { b := p.frameOf(t, trade()); return resize(b[:len(b)-12]) }, fixsbe.ErrShort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, p, h, done := mdSession(t, time.Hour, func(c *Config) { c.Reconnect.Disabled = true })
			if _, err := c.Subscribe(depths("D", 10, "BTCUSDT")); err != nil {
				t.Fatal(err)
			}
			p.expectMDR(t)
			p.write(t, tc.frame(t, p))
			lo := decode[fixsbe.Logout](t, p.expect(t, fixsbe.LogoutTemplateID))
			var pe *ProtocolError
			err := runErr(t, done)
			if !errors.As(err, &pe) || !errors.Is(err, tc.want) {
				t.Errorf("Run: %v, want a ProtocolError of %v", err, tc.want)
			}
			if !strings.Contains(string(lo.Text), "protocol error") {
				t.Errorf("Logout %q", lo.Text)
			}
			for {
				r := <-h.recs
				if r.kind == "logout" {
					break
				}
				t.Errorf("%s reached the Handler", r.kind)
			}
		})
	}
}

// A market data session's message budget refuses requests beyond its
// limit, less the reserve, and never session messages; the window
// slides.
func TestMarketDataRateBudget(t *testing.T) {
	budget := func(window time.Duration) func(*Client) {
		return func(c *Client) { c.rateLimit, c.rateReserve, c.rateWindow = 10, 4, window }
	}
	subscribeAll := func(t *testing.T, c *Client, p *peer) []*Subscription {
		t.Helper()
		// The Logon counted: 5 requests leave the reserve.
		var subs []*Subscription
		for i := range 5 {
			s, err := c.Subscribe(trades(fmt.Sprint("T", i), fmt.Sprint("S", i)))
			if err != nil {
				t.Fatalf("subscription %d: %v", i, err)
			}
			subs = append(subs, s)
			p.expectMDR(t)
		}
		if _, err := c.Subscribe(trades("T5", "S5")); !errors.Is(err, ErrRateLimited) {
			t.Errorf("sixth subscription: %v", err)
		}
		if _, err := c.Send(&LimitQuery{ReqID: []byte("L")}); !errors.Is(err, ErrRateLimited) {
			t.Errorf("LimitQuery: %v", err)
		}
		if err := subs[0].Unsubscribe(); !errors.Is(err, ErrRateLimited) {
			t.Errorf("Unsubscribe: %v", err)
		}
		if st := subs[0].Status(); st.Unsubscribe != PhaseNone {
			t.Errorf("unsubscribe refused, but %v", st.Unsubscribe)
		}
		return subs
	}
	t.Run("refused", func(t *testing.T) {
		c, _, p, _, _ := mdSession(t, time.Hour, nil, budget(time.Hour))
		subscribeAll(t, c, p)
	})
	t.Run("reserve", func(t *testing.T) {
		const interval = 20 * time.Millisecond
		c, _, p, _, _ := mdSession(t, interval, nil, budget(time.Hour))
		subscribeAll(t, c, p)
		// Heartbeats and TestRequests go on past the limit.
		sent := 0
		for sent < 10 {
			m := p.next(t)
			switch m.h.TemplateId {
			case fixsbe.TestRequestTemplateID:
				p.send(t, &fixsbe.Heartbeat{TestReqID: decode[fixsbe.TestRequest](t, m).TestReqID})
				sent++
			case fixsbe.HeartbeatTemplateID:
				sent++
			default:
				t.Fatalf("template %d", m.h.TemplateId)
			}
		}
		if !c.LoggedOn() {
			t.Error("connection ended")
		}
		if _, err := c.Subscribe(trades("T5", "S5")); !errors.Is(err, ErrRateLimited) {
			t.Errorf("subscription after the session messages: %v", err)
		}
	})
	t.Run("window", func(t *testing.T) {
		const window = 300 * time.Millisecond
		c, _, p, _, _ := mdSession(t, time.Hour, nil, budget(window))
		subs := subscribeAll(t, c, p)
		time.Sleep(window)
		if err := subs[0].Unsubscribe(); err != nil {
			t.Errorf("Unsubscribe once the window slid: %v", err)
		}
		if _, err := c.Subscribe(trades("T5", "S5")); err != nil {
			t.Errorf("subscription once the window slid: %v", err)
		}
	})
}

// Each kind of session takes its own requests, and refuses the other's
// before anything is written; LimitQuery goes on both. Subscribe refuses
// what is no subscription, or whose streams cannot be told.
func TestSessionKindRefusals(t *testing.T) {
	oe, err := New(testConfig(newFake(t), nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(newFake(t), nil)
	cfg.Kind = MarketData
	md, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"order entry Subscribe", second(oe.Subscribe(trades("A", "BTCUSDT"))), ErrSessionKind},
		{"order entry InstrumentListRequest", second(oe.Send(&InstrumentListRequest{InstrumentReqID: []byte("I")})), ErrSessionKind},
		{"order entry LimitQuery", second(oe.Send(&LimitQuery{ReqID: []byte("L")})), ErrNotLoggedOn},
		{"market data order", second(md.Send(order("o"))), ErrSessionKind},
		{"market data list", second(md.Send(&NewOrderList{ClListID: []byte("L")})), ErrSessionKind},
		{"market data MarketDataRequest sent", second(md.Send(trades("A", "BTCUSDT"))), ErrNotRequest},
		{"market data InstrumentListRequest", second(md.Send(&InstrumentListRequest{InstrumentReqID: []byte("I")})), ErrNotLoggedOn},
		{"market data invalid InstrumentReqID", second(md.Send(&InstrumentListRequest{})), ErrInvalidID},
		{"market data LimitQuery", second(md.Send(&LimitQuery{ReqID: []byte("L")})), ErrNotLoggedOn},
		{"market data Subscribe", second(md.Subscribe(trades("A", "BTCUSDT"))), ErrNotLoggedOn},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, tc.err, tc.want)
		}
	}
	unsubscribe := trades("A", "BTCUSDT")
	unsubscribe.SubscriptionRequestType = fixsbe.SubscriptionRequestTypeUnsubscribe
	for _, tc := range []struct {
		name string
		m    *MarketDataRequest
		want error
	}{
		{"unsubscribe", unsubscribe, ErrInvalidSubscription},
		{"no MDReqID", trades("", "BTCUSDT"), ErrInvalidID},
		{"MDReqID too long", trades(strings.Repeat("x", 37), "BTCUSDT"), ErrInvalidID},
		{"no symbol", trades("A"), ErrInvalidSubscription},
		{"empty symbol", trades("A", ""), ErrInvalidSubscription},
		{"symbol twice", trades("A", "BTCUSDT", "BTCUSDT"), ErrInvalidSubscription},
		{"no entry type", mdRequest("A", 1, nil, "BTCUSDT"), ErrInvalidSubscription},
		{"bid alone", mdRequest("A", 1, bookTypes[:1], "BTCUSDT"), ErrInvalidSubscription},
		{"trade twice", mdRequest("A", 1, append(slices.Clone(tradeTypes), tradeTypes...), "BTCUSDT"), ErrInvalidSubscription},
		{"unknown entry type", mdRequest("A", 1, []fixsbe.MdEntryType{'9'}, "BTCUSDT"), ErrInvalidSubscription},
		{"book without depth", depths("A", 0, "BTCUSDT"), ErrInvalidSubscription},
		{"book too deep", depths("A", MaxMarketDepth+1, "BTCUSDT"), ErrInvalidSubscription},
	} {
		if _, err := md.Subscribe(tc.m); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, tc := range []struct {
		m    *MarketDataRequest
		want []Stream
	}{
		{trades("A", "X", "Y"), []Stream{{"X", StreamTrade}, {"Y", StreamTrade}}},
		{mdRequest("A", 0, tradeTypes, "X"), []Stream{{"X", StreamTrade}}},
		{tickers("A", "X"), []Stream{{"X", StreamBookTicker}}},
		{depths("A", 2, "X"), []Stream{{"X", StreamDepth}}},
		{mdRequest("A", 5000, append(slices.Clone(bookTypes), fixsbe.MdEntryTypeTrade), "X", "Y"),
			[]Stream{{"X", StreamTrade}, {"X", StreamDepth}, {"Y", StreamTrade}, {"Y", StreamDepth}}},
	} {
		if got, err := requestStreams(tc.m); err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("streams of %+v: %v, %v", tc.m, got, err)
		}
	}
}

func second[T any](_ T, err error) error { return err }

// The defaults follow the Kind; a market data Config refuses order entry
// options.
func TestConfigKinds(t *testing.T) {
	for _, tc := range []struct {
		kind                   Kind
		endpoint               string
		frame, events, bytes   int
		attempts               int
		window                 time.Duration
		rateLimit, rateReserve int
		rateWindow             time.Duration
	}{
		{OrderEntry, Endpoint, 64 << 10, 4096, 16 << 20, 15, 30 * time.Second, 0, 0, 0},
		{MarketData, MarketDataEndpoint, 1 << 20, 16384, 32 << 20, 300, 300 * time.Second, 2000, 100, time.Minute},
	} {
		c, err := New(Config{Kind: tc.kind, APIKey: "k", PrivateKey: testKey(t), SenderCompID: "S"})
		if err != nil {
			t.Fatal(err)
		}
		cfg := c.cfg
		if cfg.Endpoint != tc.endpoint || cfg.MaxFrameLength != tc.frame || cfg.MaxEvents != tc.events || cfg.MaxEventBytes != tc.bytes ||
			cfg.Reconnect.MaxAttempts != tc.attempts || cfg.Reconnect.Window != tc.window ||
			c.rateLimit != tc.rateLimit || c.rateReserve != tc.rateReserve || c.rateWindow != tc.rateWindow {
			t.Errorf("%v: %+v, rate %d %d %v", tc.kind, cfg, c.rateLimit, c.rateReserve, c.rateWindow)
		}
	}
	for _, mod := range []func(*Config){
		func(c *Config) { c.Kind = 2 },
		func(c *Config) { c.Kind = MarketData; c.ResponseMode = fixsbe.ResponseModeOnlyAcks },
		func(c *Config) { c.Kind = MarketData; c.ExecutionReportType = fixsbe.ExecutionReportTypeMini },
	} {
		cfg := Config{APIKey: "k", PrivateKey: testKey(t), SenderCompID: "S"}
		mod(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
	if MarketData.String() != "market data" || OrderEntry.String() != "order entry" || Kind(9).String() != "invalid" {
		t.Error("Kind.String")
	}
}

// The connection's end lists its subscriptions, each with where its
// request stands, never as requests possibly transmitted: those are its
// receipts'.
func TestLogoutListsSubscriptions(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	cfg := testConfig(f, h)
	cfg.Kind = MarketData
	cfg.Reconnect.Disabled = true
	c, done := start(t, cfg, time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	a, err := c.Subscribe(trades("A", "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	p.expectMDR(t)
	waitSub(t, a, written)
	lq, err := c.Send(&LimitQuery{ReqID: []byte("L")})
	if err != nil {
		t.Fatal(err)
	}
	p.expect(t, fixsbe.LimitQueryTemplateID)
	waitState(t, lq, Written)
	steps := make(chan int)
	g.mu.Lock()
	g.steps = steps
	g.mu.Unlock()
	b, err := c.Subscribe(trades("B", "ETHUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	g.waitWrite(t) // b in the writer's hands
	cc, err := c.Subscribe(trades("C", "ETHBTC"))
	if err != nil {
		t.Fatal(err)
	}
	steps <- 5 // b partly written, then the write fails
	e := h.next(t, "logout").ev.(LogoutEvent)
	if !slices.Equal(e.Subscriptions, []*Subscription{a, b, cc}) {
		t.Errorf("Subscriptions %v", e.Subscriptions)
	}
	if describe(e.PossiblyTransmitted) != describe([]*Receipt{lq}) || len(e.NeverWritten) != 0 {
		t.Errorf("PossiblyTransmitted %s, NeverWritten %s", describe(e.PossiblyTransmitted), describe(e.NeverWritten))
	}
	for _, tc := range []struct {
		s    *Subscription
		want Phase
	}{{a, PhaseWritten}, {b, PhasePartlyWritten}, {cc, PhaseNeverWritten}} {
		if st := tc.s.Status(); st.Request != tc.want || !st.Ended || st.Err == nil {
			t.Errorf("%s: %+v, want %v", tc.s.ID(), st, tc.want)
		}
	}
	if err := runErr(t, done); err == nil {
		t.Error("Run: nil")
	}
}

// The events waiting for a stalled Handler are bounded by their bytes as
// well as by their number.
func TestEventBytes(t *testing.T) {
	release := make(chan struct{})
	got := make(chan struct{}, 8)
	f := newFake(t)
	cfg := testConfig(f, &stallHandler{release: release, got: got})
	cfg.Kind = MarketData
	cfg.MaxEvents, cfg.MaxEventBytes = 100, 1000
	c, _ := start(t, cfg, time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	for range 3 {
		p.send(t, snapshotOf("BTCUSDT", 40)) // 1,304 bytes each
	}
	// The first held by the Handler, the second waits for room: reading
	// waits.
	deadline := time.Now().Add(wait)
	for {
		c.mu.Lock()
		k := c.cur
		c.mu.Unlock()
		if k != nil && k.stalled.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reading did not wait")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 3 {
		select {
		case <-got:
		case <-time.After(wait):
			t.Fatal("snapshot not handled")
		}
	}
}

// stallHandler waits for release on each snapshot.
type stallHandler struct {
	BaseHandler
	release chan struct{}
	got     chan struct{}
}

func (h *stallHandler) OnMarketDataSnapshot(Inbound, *MarketDataSnapshot) {
	<-h.release
	h.got <- struct{}{}
}

// InstrumentListRequest goes through Send: the first InstrumentList of its
// InstrumentReqID answers it, the next ones reach the Handler alone.
func TestInstrumentList(t *testing.T) {
	c, _, p, h, _ := mdSession(t, time.Hour, nil)
	r, err := c.Send(&InstrumentListRequest{InstrumentListRequestType: fixsbe.InstrumentListRequestTypeAllInstruments, InstrumentReqID: []byte("I1")})
	if err != nil {
		t.Fatal(err)
	}
	if m := decode[fixsbe.InstrumentListRequest](t, p.expect(t, fixsbe.InstrumentListRequestTemplateID)); string(m.InstrumentReqID) != "I1" {
		t.Errorf("InstrumentListRequest %+v", m)
	}
	waitState(t, r, Written)
	entry := func(symbol string) InstrumentListRelatedSym {
		return InstrumentListRelatedSym{PriceExponent: -2, QtyExponent: -8, Symbol: []byte(symbol), Currency: []byte(symbol[:3])}
	}
	p.send(t, &InstrumentList{InstrumentReqID: []byte("I1"), RelatedSym: []InstrumentListRelatedSym{entry("BTCUSDT"), entry("ETHUSDT")}})
	if x := h.next(t, "instruments"); x.in.Receipt != r || x.text != "BTCUSDT ETHUSDT" {
		t.Errorf("first part %+v", x)
	}
	p.send(t, &InstrumentList{InstrumentReqID: []byte("I1"), RelatedSym: []InstrumentListRelatedSym{entry("ETHBTC")}})
	if x := h.next(t, "instruments"); x.in.Receipt != nil || x.text != "ETHBTC" {
		t.Errorf("second part %+v", x)
	}
	if r.State() != Answered || legs(r) != "instruments I1 accepted;" {
		t.Errorf("receipt %v %s", r.State(), legs(r))
	}
}

// gatedSession starts a market data Client whose writes, once logged on,
// wait at a gate: each takes a step (see gate.steps).
func gatedSession(t *testing.T, hooks ...func(*Client)) (*Client, *peer, *recorder, chan int, *gate) {
	t.Helper()
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	cfg := testConfig(f, h)
	cfg.Kind = MarketData
	cfg.Reconnect.Disabled = true
	c, _ := start(t, cfg, time.Hour, hooks...)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	steps := make(chan int)
	g.mu.Lock()
	g.steps = steps
	g.mu.Unlock()
	return c, p, h, steps, g
}

// A write failing part way through a batch of subscriptions: those wholly
// before the failure are written, the one it cut partly written, those
// wholly after it never written.
func TestPartialWriteSubscriptions(t *testing.T) {
	c, _, h, steps, g := gatedSession(t)
	blk, err := c.Subscribe(trades("BLK", "BNBUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	g.waitWrite(t) // blk alone in the writer's hands
	var subs []*Subscription
	for i, sym := range []string{"BTCUSDT", "ETHUSDT", "ETHBTC"} {
		s, err := c.Subscribe(trades(fmt.Sprint("S", i), sym))
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, s)
	}
	steps <- -1 // blk written
	g.waitWrite(t)
	frame, err := encode(trades("S0", "BTCUSDT"))
	if err != nil {
		t.Fatal(err)
	}
	steps <- len(frame) + 5 // the three in one write: S0 wholly, S1 partly, S2 not at all
	e := h.next(t, "logout").ev.(LogoutEvent)
	if !slices.Equal(e.Subscriptions, append([]*Subscription{blk}, subs...)) {
		t.Errorf("Subscriptions %v", e.Subscriptions)
	}
	for i, want := range []Phase{PhaseWritten, PhaseWritten, PhasePartlyWritten, PhaseNeverWritten} {
		s := append([]*Subscription{blk}, subs...)[i]
		if st := s.Status(); st.Request != want || !st.Ended {
			t.Errorf("%s: %v, want %v", s.ID(), st.Request, want)
		}
	}
}

// A write held up holds its messages in the budget until it returns, and
// their window runs from then: a burst written late does not leave room
// for another within Binance's window.
func TestRateBudgetSlowWrite(t *testing.T) {
	const window = 300 * time.Millisecond
	c, p, _, steps, g := gatedSession(t, func(c *Client) { c.rateLimit, c.rateReserve, c.rateWindow = 10, 4, window })
	// The Logon counted: 5 requests leave the reserve.
	s0, err := c.Subscribe(trades("T0", "S0"))
	if err != nil {
		t.Fatal(err)
	}
	g.waitWrite(t) // T0 in the writer's hands
	var last *Subscription
	for i := 1; i < 5; i++ {
		if last, err = c.Subscribe(trades(fmt.Sprint("T", i), fmt.Sprint("S", i))); err != nil {
			t.Fatalf("subscription %d: %v", i, err)
		}
	}
	if _, err := c.Subscribe(trades("T5", "S5")); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("sixth subscription: %v", err)
	}
	// Held up beyond the window: only the Logon expires.
	time.Sleep(window + window/3)
	steps <- -1
	g.waitWrite(t)
	steps <- -1
	for range 5 {
		p.expectMDR(t)
	}
	waitSub(t, s0, written)
	waitSub(t, last, written)
	if _, err := c.Subscribe(trades("T5", "S5")); err != nil {
		t.Errorf("subscription in the Logon's room: %v", err)
	}
	if _, err := c.Subscribe(trades("T6", "S6")); !errors.Is(err, ErrRateLimited) {
		t.Errorf("subscription beyond: %v", err)
	}
}

// LogoutEvents count in the event queue's bytes: their errors retain
// Binance's Logout text. Behind a stalled Handler, a connection's end
// waits for room, and Run reconnects no more until there is.
func TestLogoutEventBytes(t *testing.T) {
	f := newFake(t)
	h := &logoutHandler{release: make(chan struct{}), evs: make(chan string, 16)}
	cfg := testConfig(f, h)
	cfg.MaxEvents, cfg.MaxEventBytes = 100, 100_000
	c, _ := start(t, cfg, time.Hour)
	text := bytes.Repeat([]byte("x"), 60_000)
	for i := range 2 {
		p := f.accept()
		p.logon(t, &c.cfg)
		p.send(t, &fixsbe.Logout{Text: text})
		p.expect(t, fixsbe.LogoutTemplateID)
		if i == 0 {
			if e := <-h.evs; e != "logon" {
				t.Fatalf("event %s", e)
			}
		}
	}
	// The first LogoutEvent held by the Handler, the second waits for
	// room: no third connection.
	time.Sleep(300 * time.Millisecond)
	if n := len(f.dials()); n != 2 {
		t.Fatalf("%d connections while the LogoutEvent waits", n)
	}
	close(h.release)
	for _, want := range []string{"logout", "logon", "logout"} {
		select {
		case e := <-h.evs:
			if e != want {
				t.Fatalf("event %s, want %s", e, want)
			}
		case <-time.After(wait):
			t.Fatalf("no %s event", want)
		}
	}
	f.accept()
}

// logoutHandler waits for release on each LogoutEvent.
type logoutHandler struct {
	BaseHandler
	release chan struct{}
	evs     chan string
}

func (h *logoutHandler) OnLogon(LogonEvent) { h.evs <- "logon" }
func (h *logoutHandler) OnLogout(e LogoutEvent) {
	<-h.release
	var pl *PeerLogoutError
	if errors.As(e.Err, &pl) && len(pl.Text) == 60_000 {
		h.evs <- "logout"
		return
	}
	h.evs <- fmt.Sprintf("logout %v", e.Err)
}

// News count in the event queue's bytes: their headlines, retained, are.
func TestNewsEventBytes(t *testing.T) {
	f, h := newFake(t), newRecorder()
	release := make(chan struct{})
	h.onNews = func(NewsEvent) { <-release }
	cfg := testConfig(f, h)
	cfg.MaxEvents, cfg.MaxEventBytes = 100, 100_000
	c, _ := start(t, cfg, time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	for range 3 {
		p.send(t, &fixsbe.News{Headline: bytes.Repeat([]byte("x"), 60_000)})
	}
	// The first held by the Handler, the second waits for room.
	deadline := time.Now().Add(wait)
	for {
		c.mu.Lock()
		k := c.cur
		c.mu.Unlock()
		if k != nil && k.stalled.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reading did not wait")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 3 {
		h.next(t, "news")
	}
}
