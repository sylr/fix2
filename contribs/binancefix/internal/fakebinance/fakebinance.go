// Package fakebinance is a fake of Binance's FIX SBE order entry endpoint,
// in memory, for the tests of the probes and of cmd/binancefix: it checks
// the Logon's signature, then answers each request at once, the way a
// Behaviour tells, so that every probe can be played both ways, Binance
// answering as the client assumes or otherwise.
//
// It keeps its orders across connections, in a book of its own: what a
// test placed and did not cancel is still there for the next connection,
// as on Binance. Nothing is matched; a market order fills at once.
//
// Of Kind binancefix.MarketData, it is the market data endpoint instead:
// it takes subscriptions (a snapshot on activation for the book ticker
// and depth, nothing for trades), refuses those Binance documents it
// refuses (a duplicate MDReqID, too many streams, an unknown symbol),
// publishes data when told (Publish), still for a while after an
// unsubscribe, and lists instruments, in several messages if told.
package fakebinance

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// Behaviour tells how the fake answers what the client's assumptions are
// about. The zero value answers as the client assumes.
type Behaviour struct {
	// CancelID is the ClOrdID of the ExecutionReport CANCELED answering an
	// OrderCancelRequest: "" the cancellation's, "order" the order's.
	CancelID string
	// Amend answers an OrderAmendKeepPriorityRequest: "" an
	// ExecutionReport REPLACED, "new" an ExecutionReport NEW, "reject" an
	// OrderAmendReject, "timeout-order-id" an ExecutionReport REJECTED of
	// -1007 carrying the order's ClOrdID, "silent" nothing.
	Amend string
	// CancelReplaceRejectID is the ClOrdID of the OrderCancelReject of a
	// cancel/replace whose cancellation fails: "" its CancelClOrdID,
	// "order" its ClOrdID, "other" another.
	CancelReplaceRejectID string
	// StopOnFailure is what becomes of the new order of a STOP_ON_FAILURE
	// cancel/replace whose cancellation fails, after the OrderCancelReject:
	// "" not attempted, an ExecutionReport REJECTED -2010 "order placement
	// not attempted" telling it (the testnet's answer), "place" placed all
	// the same, "trade" filled all the same, a
	// TRADE its only report, "timeout-trade" an ExecutionReport REJECTED
	// of -1007 (status unknown) then that TRADE, "timeout" the -1007
	// alone.
	StopOnFailure string
	// AllowFailure is what becomes of the new order of an ALLOW_FAILURE
	// cancel/replace whose cancellation fails: "" placed, its
	// ExecutionReport NEW sent before the OrderCancelReject (the testnet's
	// order), "silent" placed, nothing sent but the OrderCancelReject.
	AllowFailure string
	// ListCancel answers the cancellation of a list, a ListStatus
	// ALL_DONE first, carrying the cancellation's ClOrdID as ClListID and
	// the list's as OrigClListID (the testnet's answer), then: "" an
	// ExecutionReport CANCELED per order, carrying the cancellation's
	// ClOrdID; "order-ids" the same, the ExecutionReports carrying the
	// orders' ClOrdIDs; "status-only" nothing more; "status-timeout", when
	// the cancellation names the list, that ListStatus alone, its entries
	// of error -1007 (status unknown), the list left live; "reject" instead an
	// OrderCancelReject when the cancellation names the list (cancelling
	// one of its orders still cancels it).
	ListCancel string
	// Fill answers a market order: "" an ExecutionReport NEW then TRADE,
	// "trade-only" TRADE alone, "expire" NEW then EXPIRED, unfilled.
	Fill string
	// MassCancel answers an OrderMassCancelRequest: "" its report,
	// carrying its ClOrdID, after the orders' ExecutionReports CANCELED;
	// "no-id" the report without ClOrdID.
	MassCancel string
	// Limits answers a LimitQuery: "" a LimitResponse of its ReqID,
	// "other-id" of another, "silent" nothing.
	Limits string
	// RejectOrders refuses every new order and list, with error RejectCode:
	// -2010 (insufficient balance) if 0; -1013 is a filter's refusal.
	RejectOrders bool
	RejectCode   int32
	// TimeoutOrders answers new limit orders with an ExecutionReport
	// REJECTED of error -1007 (TIMEOUT, execution status unknown), and
	// places them all the same; TimeoutLists answers lists with a
	// ListStatus REJECT whose entries are of error -1007, and places them.
	TimeoutOrders bool
	TimeoutLists  bool
	// RejectCancelReplace refuses every cancel/replace whole, with a
	// Reject, before its cancellation is processed.
	RejectCancelReplace bool
	// RejectSells refuses the first RejectSells market sells;
	// PartialSells fills half of the first PartialSells (after those
	// refused), the rest expiring.
	RejectSells  int
	PartialSells int
	// DelaySellTrade withholds the TRADE of a market sell, filled at once
	// all the same, its NEW sent: "after-cancel" sends it 300ms after a
	// cancellation of the sell is refused (-2011: no longer open),
	// "never" never.
	DelaySellTrade string
	// DropAfterAnswer, if positive, closes the connection once the
	// DropAfterAnswer-th request of the exchange's life is answered.
	DropAfterAnswer int
	// DropAfter, if positive, closes the connection on the DropAfter-th
	// request of the exchange's life, unanswered.
	DropAfter int
	// OnRequest, if set, is called with each request before it is
	// answered, the exchange locked.
	OnRequest func(Request)
}

// Options configures an Exchange.
type Options struct {
	// APIKey is the key a Logon's Username must be, any if empty.
	APIKey string
	// PublicKey verifies the Logons' signatures.
	PublicKey ed25519.PublicKey
	// SignatureTimes are the forms of the SendingTime a signature is
	// accepted of, binancefix.SignatureTimeMicros (Binance's) if none.
	SignatureTimes []binancefix.SignatureTime
	// LogonAckSeq is the LogonAck's sequence number, 1 if 0.
	LogonAckSeq uint32
	// Kind is the endpoint's: order entry, or market data, which refuses
	// orders with a Reject, as order entry refuses market data requests.
	Kind binancefix.Kind
	Behaviour
	MarketData MarketData
}

// MarketData tells how the market data endpoint answers.
type MarketData struct {
	// Symbols are the instruments listed, BTCUSDT, ETHUSDT and ETHBTC if
	// none: a subscription to another is refused whole, with a
	// MarketDataRequestReject of error -1121.
	Symbols []string
	// MaxStreams bounds a connection's streams, binancefix.MaxStreams if
	// 0: a subscription beyond is refused whole, with a
	// MarketDataRequestReject TOO_MANY_SUBSCRIPTIONS.
	MaxStreams int
	// BookLevels is the levels per side of each symbol's book,
	// binancefix.MaxMarketDepth if 0: a depth subscription's snapshot
	// takes MarketDepth of them at most.
	BookLevels int
	// InstrumentsPerMessage splits an InstrumentList into messages of as
	// many instruments, one message if 0.
	InstrumentsPerMessage int
	// Linger is how many publications still reach a subscription after
	// its unsubscribe, Binance's data possibly flowing for a while.
	Linger int
	// RejectUnsubscribe answers an unsubscribe with a
	// MarketDataRequestReject of its MDReqID, unsubscribing all the same.
	RejectUnsubscribe bool
}

// Request is a request the exchange received.
type Request struct {
	Name string // its message's, as "NewOrderSingle"
	// IDs are its ClOrdID, CancelClOrdID, ClListID, its orders' ClOrdIDs or
	// its ReqID.
	IDs          []string
	SenderCompID string
	At           time.Time
}

// Logon is a Logon the exchange received.
type Logon struct {
	SenderCompID string
	// SignatureTime is the form its signature verified with, "" if none.
	SignatureTime string
	Accepted      bool
}

type order struct {
	id       int64
	clOrdID  string
	symbol   string
	side     fixsbe.Side
	ordType  fixsbe.OrdType
	price    fixsbe.Optional[int64]
	qty      int64
	priceExp int8
	qtyExp   int8
	open     bool
	pending  bool
	list     *list
}

type list struct {
	id       int64
	clListID string
	orders   []*order
}

// Exchange is the fake: Dial connects to it.
type Exchange struct {
	o Options

	mu       sync.Mutex
	orders   []*order
	lists    []*list
	nextID   int64
	execID   int64
	requests []Request
	logons   []Logon
	dials    int
	sells    int     // market sells received
	net      big.Rat // base asset bought minus sold
	// withheld are the TRADE reports DelaySellTrade withholds, by
	// OrderID.
	withheld map[int64]*fixsbe.ExecutionReport
	// The market data endpoint's: its logged-on connections, its trades'
	// and book updates' numbers.
	mdConns  []*conn
	tradeID  int64
	updateID int64
}

// Net returns the base asset the market orders bought minus what they
// sold, as a fraction: "0" when flat.
func (x *Exchange) Net() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.net.RatString()
}

func (x *Exchange) fill(side fixsbe.Side, q int64, e int8) {
	r := new(big.Rat).SetFrac(big.NewInt(q), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-e)), nil))
	if side == fixsbe.SideSell {
		r.Neg(r)
	}
	x.net.Add(&x.net, r)
}

var _ transport.Dialer = (*Exchange)(nil)

// New returns an exchange of options o.
func New(o Options) *Exchange {
	if len(o.SignatureTimes) == 0 {
		o.SignatureTimes = []binancefix.SignatureTime{binancefix.SignatureTimeMicros}
	}
	if o.LogonAckSeq == 0 {
		o.LogonAckSeq = 1
	}
	md := &o.MarketData
	if len(md.Symbols) == 0 {
		md.Symbols = []string{"BTCUSDT", "ETHUSDT", "ETHBTC"}
	}
	if md.MaxStreams == 0 {
		md.MaxStreams = binancefix.MaxStreams
	}
	if md.BookLevels == 0 {
		md.BookLevels = binancefix.MaxMarketDepth
	}
	return &Exchange{o: o, nextID: 1000, updateID: 1_000_000}
}

// Dial opens a connection to the exchange.
func (x *Exchange) Dial(ctx context.Context) (transport.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	x.mu.Lock()
	x.dials++
	x.mu.Unlock()
	a, b := mem.Pipe()
	go x.serve(b)
	return a, nil
}

// Dials returns how many connections were opened.
func (x *Exchange) Dials() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.dials
}

// Requests returns the requests received, in order.
func (x *Exchange) Requests() []Request {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.requests)
}

// Logons returns the Logons received, in order.
func (x *Exchange) Logons() []Logon {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.logons)
}

// Open returns the ClOrdIDs of the orders open, pending ones included.
func (x *Exchange) Open() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var ids []string
	for _, o := range x.orders {
		if o.open {
			ids = append(ids, o.clOrdID)
		}
	}
	return ids
}

// conn is a connection's server side.
type conn struct {
	x      *Exchange
	c      transport.Conn
	seq    uint32
	sender string
	subs   []*subscription // market data's, unsubscribed ones lingering
}

func (s *conn) send(m interface {
	AppendWithHeader([]byte, fixsbe.MessageHeader) ([]byte, error)
}) {
	b, err := m.AppendWithHeader(nil, fixsbe.MessageHeader{SeqNum: s.seq, SendingTime: time.Now().UnixMicro()})
	if err != nil {
		panic(err)
	}
	f, err := lesofh.AppendFrame(nil, b)
	if err != nil {
		panic(err)
	}
	s.seq++
	s.c.Write(f)
}

// verify returns the form Logon l's signature verifies with, of header h.
func (x *Exchange) verify(h fixsbe.MessageHeader, l *fixsbe.Logon) (binancefix.SignatureTime, bool) {
	sig, err := base64.StdEncoding.DecodeString(string(l.RawData))
	if err != nil || x.o.PublicKey == nil {
		return 0, false
	}
	at := time.UnixMicro(int64(h.SendingTime))
	for _, f := range x.o.SignatureTimes {
		payload := strings.Join([]string{"A", string(l.SenderCompId), string(l.TargetCompId), strconv.FormatUint(uint64(h.SeqNum), 10), f.Format(at)}, "\x01")
		if ed25519.Verify(x.o.PublicKey, []byte(payload), sig) {
			return f, true
		}
	}
	return 0, false
}

func (x *Exchange) serve(c transport.Conn) {
	defer c.Close()
	fr := lesofh.NewReader(c, 0)
	b, err := fr.Next()
	if err != nil {
		return
	}
	h, body, err := fixsbe.ReadHeader(b)
	if err != nil || h.TemplateId != fixsbe.LogonTemplateID {
		return
	}
	var l fixsbe.Logon
	if _, err := l.DecodeBody(h, body); err != nil {
		return
	}
	s := &conn{x: x, c: c, seq: 1, sender: string(l.SenderCompId)}
	f, ok := x.verify(h, &l)
	rec := Logon{SenderCompID: s.sender, Accepted: ok}
	if ok {
		rec.SignatureTime = f.String()
	}
	x.mu.Lock()
	x.logons = append(x.logons, rec)
	x.mu.Unlock()
	switch {
	case x.o.APIKey != "" && string(l.Username) != x.o.APIKey:
		s.send(&fixsbe.Reject{RefSeqNum: fixsbe.Some[uint32](1), ErrorCode: fixsbe.Some[int32](-2015),
			Text: []byte("Invalid API-key, IP, or permissions for action.")})
		return
	case !ok:
		s.send(&fixsbe.Reject{RefSeqNum: fixsbe.Some[uint32](1), ErrorCode: fixsbe.Some[int32](-1022),
			SessionRejectReason: fixsbe.Some(fixsbe.SessionRejectReasonSignatureProblem),
			Text:                []byte("Signature for this request is not valid.")})
		return
	}
	s.seq = x.o.LogonAckSeq
	x.mu.Lock()
	s.send(&fixsbe.LogonAck{EncryptMethod: fixsbe.Some[uint8](0), HeartBtInt: l.HeartBtInt,
		SbeSchemaIdVersionDeprecated: fixsbe.BoolEnumFalse, UUID: []byte("fake-" + s.sender)})
	if x.o.Kind == binancefix.MarketData {
		x.mdConns = append(x.mdConns, s)
		defer func() {
			x.mu.Lock()
			x.mdConns = slices.DeleteFunc(x.mdConns, func(o *conn) bool { return o == s })
			x.mu.Unlock()
		}()
	}
	x.mu.Unlock()
	for {
		b, err := fr.Next()
		if err != nil {
			return
		}
		h, body, err := fixsbe.ReadHeader(b)
		if err != nil {
			return
		}
		if !s.handle(h, body) {
			return
		}
	}
}

// handle answers a message, and reports whether the connection goes on.
func (s *conn) handle(h fixsbe.MessageHeader, body []byte) bool {
	x := s.x
	switch h.TemplateId {
	case fixsbe.HeartbeatTemplateID:
		return true
	case fixsbe.TestRequestTemplateID:
		var m fixsbe.TestRequest
		if _, err := m.DecodeBody(h, body); err == nil {
			x.mu.Lock()
			s.send(&fixsbe.Heartbeat{TestReqID: m.TestReqID})
			x.mu.Unlock()
		}
		return true
	case fixsbe.LogoutTemplateID:
		// Sent locked, as every send but the Logon's: a late report may
		// be sent from a timer.
		x.mu.Lock()
		s.send(&fixsbe.Logout{Text: []byte("Logout acknowledgment.")})
		x.mu.Unlock()
		return false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	req := Request{SenderCompID: s.sender, At: time.Now()}
	var answer func()
	md := h.TemplateId == fixsbe.MarketDataRequestTemplateID || h.TemplateId == fixsbe.InstrumentListRequestTemplateID
	if md != (x.o.Kind == binancefix.MarketData) && h.TemplateId != fixsbe.LimitQueryTemplateID {
		// A request of the other endpoint.
		s.send(&fixsbe.Reject{RefSeqNum: fixsbe.Some(h.SeqNum), ErrorCode: fixsbe.Some[int32](-1221), Text: []byte("Invalid/missing field(s) in SBE message.")})
		return true
	}
	switch h.TemplateId {
	case fixsbe.MarketDataRequestTemplateID:
		var m fixsbe.MarketDataRequest
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "MarketDataRequest", []string{string(m.MDReqID)}
		answer = func() { s.marketData(&m) }
	case fixsbe.InstrumentListRequestTemplateID:
		var m fixsbe.InstrumentListRequest
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "InstrumentListRequest", []string{string(m.InstrumentReqID)}
		answer = func() { s.instruments(h.SeqNum, &m) }
	case fixsbe.NewOrderSingleTemplateID:
		var m fixsbe.NewOrderSingle
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "NewOrderSingle", []string{string(m.ClOrdID)}
		answer = func() { s.newOrder(&m) }
	case fixsbe.OrderCancelRequestTemplateID:
		var m fixsbe.OrderCancelRequest
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "OrderCancelRequest", []string{string(m.ClOrdID)}
		answer = func() { s.cancel(&m) }
	case fixsbe.OrderCancelRequestAndNewOrderSingleTemplateID:
		var m fixsbe.OrderCancelRequestAndNewOrderSingle
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "OrderCancelRequestAndNewOrderSingle", []string{string(m.CancelClOrdID), string(m.ClOrdID)}
		answer = func() { s.cancelReplace(&m) }
	case fixsbe.OrderAmendKeepPriorityRequestTemplateID:
		var m fixsbe.OrderAmendKeepPriorityRequest
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "OrderAmendKeepPriorityRequest", []string{string(m.ClOrdID)}
		answer = func() { s.amend(&m) }
	case fixsbe.NewOrderListTemplateID:
		var m fixsbe.NewOrderList
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "NewOrderList", []string{string(m.ClListID)}
		for _, o := range m.Orders {
			req.IDs = append(req.IDs, string(o.ClOrdID))
		}
		answer = func() { s.newList(&m) }
	case fixsbe.OrderMassCancelRequestTemplateID:
		var m fixsbe.OrderMassCancelRequest
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "OrderMassCancelRequest", []string{string(m.ClOrdID)}
		answer = func() { s.massCancel(&m) }
	case fixsbe.LimitQueryTemplateID:
		var m fixsbe.LimitQuery
		if _, err := m.DecodeBody(h, body); err != nil {
			return false
		}
		req.Name, req.IDs = "LimitQuery", []string{string(m.ReqID)}
		answer = func() { s.limits(&m) }
	default:
		s.send(&fixsbe.Reject{RefSeqNum: fixsbe.Some(h.SeqNum), ErrorCode: fixsbe.Some[int32](-1221), Text: []byte("Invalid/missing field(s) in SBE message.")})
		return true
	}
	x.requests = append(x.requests, req)
	if x.o.OnRequest != nil {
		x.o.OnRequest(req)
	}
	if x.o.DropAfter > 0 && len(x.requests) == x.o.DropAfter {
		return false
	}
	if x.o.RejectCancelReplace && h.TemplateId == fixsbe.OrderCancelRequestAndNewOrderSingleTemplateID {
		s.send(&fixsbe.Reject{RefSeqNum: fixsbe.Some(h.SeqNum), ErrorCode: fixsbe.Some[int32](-1221), RefMsgType: []byte("XCN"),
			Text: []byte("Invalid/missing field(s) in SBE message.")})
		return true
	}
	answer()
	return x.o.DropAfterAnswer <= 0 || len(x.requests) != x.o.DropAfterAnswer
}

func (s *conn) report(o *order, et fixsbe.ExecType, st fixsbe.OrdStatus) *fixsbe.ExecutionReport {
	s.x.execID++
	m := &fixsbe.ExecutionReport{PriceExponent: o.priceExp, QtyExponent: o.qtyExp, ExecID: fixsbe.Some(s.x.execID),
		OrderID: fixsbe.Some(o.id), OrderQty: fixsbe.Some(o.qty), OrdType: o.ordType, Side: o.side, Price: o.price,
		ExecType: et, OrdStatus: st, LeavesQty: fixsbe.Some(o.qty), ClOrdID: []byte(o.clOrdID), Symbol: []byte(o.symbol)}
	if o.list != nil {
		m.ListID = fixsbe.Some(o.list.id)
	}
	return m
}

func (s *conn) place(clOrdID, symbol []byte, side fixsbe.Side, ot fixsbe.OrdType, price fixsbe.Optional[int64], qty fixsbe.Optional[int64], pe, qe int8) *order {
	s.x.nextID++
	o := &order{id: s.x.nextID, clOrdID: string(clOrdID), symbol: string(symbol), side: side, ordType: ot, price: price,
		qty: qty.Value, priceExp: pe, qtyExp: qe, open: true}
	s.x.orders = append(s.x.orders, o)
	return o
}

// refusal returns the error code and text of a refused order.
func (x *Exchange) refusal() (int32, string) {
	if x.o.RejectCode == -1013 {
		return -1013, "Filter failure: PERCENT_PRICE_BY_SIDE"
	}
	if x.o.RejectCode != 0 {
		return x.o.RejectCode, "Order refused."
	}
	return -2010, "Account has insufficient balance for requested action."
}

func (s *conn) rejectOrder(clOrdID, symbol []byte, side fixsbe.Side, ot fixsbe.OrdType) {
	code, text := s.x.refusal()
	s.send(&fixsbe.ExecutionReport{OrdType: ot, Side: side, ExecType: fixsbe.ExecTypeRejected, OrdStatus: fixsbe.OrdStatusRejected,
		ErrorCode: fixsbe.Some(code), ErrorText: []byte(text), ClOrdID: clOrdID, Symbol: symbol})
}

// accept answers a new order: NEW, then, for a market order, its fill.
func (s *conn) accept(o *order) {
	x := s.x
	if o.ordType != fixsbe.OrdTypeMarket {
		if x.o.TimeoutOrders {
			// Placed, though the answer says otherwise.
			s.send(&fixsbe.ExecutionReport{OrdType: o.ordType, Side: o.side, ExecType: fixsbe.ExecTypeRejected, OrdStatus: fixsbe.OrdStatusRejected,
				ErrorCode: fixsbe.Some[int32](-1007), ErrorText: []byte("Timeout waiting for response from backend server. Send status unknown; execution status unknown."),
				ClOrdID: []byte(o.clOrdID), Symbol: []byte(o.symbol)})
			return
		}
		s.send(s.report(o, fixsbe.ExecTypeNew, fixsbe.OrdStatusNew))
		return
	}
	o.open = false
	if o.side == fixsbe.SideSell {
		x.sells++
		if x.o.DelaySellTrade != "" {
			// Filled at once; only its NEW told.
			s.send(s.report(o, fixsbe.ExecTypeNew, fixsbe.OrdStatusNew))
			m := s.report(o, fixsbe.ExecTypeTrade, fixsbe.OrdStatusFilled)
			m.CumQty, m.LastQty, m.LeavesQty = o.qty, o.qty, fixsbe.Some[int64](0)
			m.LastPx = fixsbe.Some[int64](100000)
			x.fill(o.side, o.qty, o.qtyExp)
			if x.withheld == nil {
				x.withheld = map[int64]*fixsbe.ExecutionReport{}
			}
			x.withheld[o.id] = m
			return
		}
		switch {
		case x.sells <= x.o.RejectSells:
			s.rejectOrder([]byte(o.clOrdID), []byte(o.symbol), o.side, o.ordType)
			return
		case x.sells <= x.o.RejectSells+x.o.PartialSells:
			// Half filled, in a finer exponent, the rest expired.
			s.send(s.report(o, fixsbe.ExecTypeNew, fixsbe.OrdStatusNew))
			m := s.report(o, fixsbe.ExecTypeTrade, fixsbe.OrdStatusPartiallyFilled)
			m.QtyExponent, m.OrderQty = o.qtyExp-1, fixsbe.Some(o.qty*10)
			m.CumQty, m.LastQty, m.LeavesQty = o.qty*5, o.qty*5, fixsbe.Some(o.qty*5)
			m.LastPx = fixsbe.Some[int64](100000)
			s.send(m)
			x.fill(o.side, o.qty*5, o.qtyExp-1)
			e := s.report(o, fixsbe.ExecTypeExpired, fixsbe.OrdStatusExpired)
			e.QtyExponent, e.OrderQty, e.CumQty, e.LeavesQty = o.qtyExp-1, fixsbe.Some(o.qty*10), o.qty*5, fixsbe.Some[int64](0)
			s.send(e)
			return
		}
	}
	trade := func() {
		m := s.report(o, fixsbe.ExecTypeTrade, fixsbe.OrdStatusFilled)
		m.CumQty, m.LastQty, m.LeavesQty = o.qty, o.qty, fixsbe.Some[int64](0)
		m.LastPx = fixsbe.Some[int64](100000)
		s.send(m)
		x.fill(o.side, o.qty, o.qtyExp)
	}
	switch x.o.Fill {
	case "trade-only":
		trade()
	case "expire":
		s.send(s.report(o, fixsbe.ExecTypeNew, fixsbe.OrdStatusNew))
		s.send(s.report(o, fixsbe.ExecTypeExpired, fixsbe.OrdStatusExpired))
	default:
		s.send(s.report(o, fixsbe.ExecTypeNew, fixsbe.OrdStatusNew))
		trade()
	}
}

func (s *conn) newOrder(m *fixsbe.NewOrderSingle) {
	if s.x.o.RejectOrders {
		s.rejectOrder(m.ClOrdID, m.Symbol, m.Side, m.OrdType)
		return
	}
	s.accept(s.place(m.ClOrdID, m.Symbol, m.Side, m.OrdType, m.Price, m.OrderQty, m.PriceExponent, m.QtyExponent))
}

// find returns the open order of OrderID id, or else of ClOrdID clOrdID.
func (x *Exchange) find(id fixsbe.Optional[int64], clOrdID []byte) *order {
	for _, o := range x.orders {
		if o.open && ((id.Valid && o.id == id.Value) || (!id.Valid && len(clOrdID) > 0 && o.clOrdID == string(clOrdID))) {
			return o
		}
	}
	return nil
}

func (s *conn) cancelReject(clOrdID, symbol []byte, code int32, text string) {
	s.send(&fixsbe.OrderCancelReject{CxlRejResponseTo: fixsbe.CxlRejResponseToOrderCancelRequest, ErrorCode: code,
		ClOrdID: clOrdID, Symbol: symbol, ErrorText: []byte(text)})
}

func (s *conn) cancel(m *fixsbe.OrderCancelRequest) {
	x := s.x
	var l *list
	byList := len(m.OrigClListID) > 0 || m.ListID.Valid
	if byList {
		for _, c := range x.lists {
			if (m.ListID.Valid && c.id == m.ListID.Value) || (!m.ListID.Valid && c.clListID == string(m.OrigClListID)) {
				l = c
			}
		}
	} else if o := x.find(m.OrderID, m.OrigClOrdID); o != nil {
		if o.list != nil {
			l = o.list
		} else {
			o.open = false
			r := s.report(o, fixsbe.ExecTypeCanceled, fixsbe.OrdStatusCanceled)
			if x.o.CancelID == "" {
				r.ClOrdID, r.OrigClOrdID = m.ClOrdID, []byte(o.clOrdID)
			}
			s.send(r)
			return
		}
	}
	if l == nil || !slices.ContainsFunc(l.orders, func(o *order) bool { return o.open }) {
		s.cancelReject(m.ClOrdID, m.Symbol, -2011, "Unknown order sent.")
		if byList || x.o.DelaySellTrade != "after-cancel" {
			return
		}
		// A sell filled unseen: its TRADE comes late.
		for _, o := range x.orders {
			if r := x.withheld[o.id]; r != nil && ((m.OrderID.Valid && o.id == m.OrderID.Value) || (!m.OrderID.Valid && o.clOrdID == string(m.OrigClOrdID))) {
				delete(x.withheld, o.id)
				time.AfterFunc(300*time.Millisecond, func() {
					x.mu.Lock()
					defer x.mu.Unlock()
					s.send(r)
				})
			}
		}
		return
	}
	if x.o.ListCancel == "reject" && byList {
		s.cancelReject(m.ClOrdID, m.Symbol, -1013, "Order list cancellation refused.")
		return
	}
	if x.o.ListCancel == "status-timeout" && byList {
		// A ListStatus alone, its entries of status unknown; the list
		// stays.
		s.listStatusOf(l, fixsbe.ListStatusTypeAllDone, fixsbe.ListOrderStatusAllDone, m.ClOrdID, []byte(l.clListID), -1007)
		return
	}
	// As the testnet answered on 2026-10-17: the ListStatus ALL_DONE
	// first, carrying the cancellation's ClOrdID as ClListID and the list's
	// as OrigClListID, then an ExecutionReport CANCELED per order.
	s.listStatusOf(l, fixsbe.ListStatusTypeAllDone, fixsbe.ListOrderStatusAllDone, m.ClOrdID, []byte(l.clListID))
	for _, o := range l.orders {
		if !o.open {
			continue
		}
		o.open = false
		if x.o.ListCancel == "status-only" {
			continue
		}
		r := s.report(o, fixsbe.ExecTypeCanceled, fixsbe.OrdStatusCanceled)
		if x.o.ListCancel == "" || x.o.ListCancel == "status-timeout" {
			r.ClOrdID, r.OrigClOrdID = m.ClOrdID, []byte(o.clOrdID)
		}
		s.send(r)
	}
}

func (s *conn) listStatus(l *list, t fixsbe.ListStatusType, st fixsbe.ListOrderStatus) {
	s.listStatusOf(l, t, st, []byte(l.clListID), nil)
}

// listStatusOf sends the ListStatus of l, of ClListID clListID and
// OrigClListID orig.
func (s *conn) listStatusOf(l *list, t fixsbe.ListStatusType, st fixsbe.ListOrderStatus, clListID, orig []byte, codes ...int32) {
	m := &fixsbe.ListStatus{ListID: fixsbe.Some(l.id), ContingencyType: fixsbe.Some(fixsbe.ContingencyTypeOneTriggersTheOther),
		ListStatusType: t, ListOrderStatus: st, ClListID: clListID, OrigClListID: orig}
	for _, o := range l.orders {
		m.Orders = append(m.Orders, fixsbe.ListStatusOrders{OrderID: fixsbe.Some(o.id), ClOrdID: []byte(o.clOrdID), Symbol: []byte(o.symbol), ErrorCode: entryCode(codes)})
	}
	s.send(m)
}

// entryCode returns the error code of a ListStatus's entries: codes[0], if
// given.
func entryCode(codes []int32) fixsbe.Optional[int32] {
	if len(codes) == 0 {
		return fixsbe.Optional[int32]{}
	}
	return fixsbe.Some(codes[0])
}

func (s *conn) cancelReplace(m *fixsbe.OrderCancelRequestAndNewOrderSingle) {
	x := s.x
	place := func() *order {
		return s.place(m.ClOrdID, m.Symbol, m.Side, m.OrdType, m.Price, m.OrderQty, m.PriceExponent, m.QtyExponent)
	}
	if o := x.find(m.OrderID, m.OrigClOrdID); o != nil && o.list == nil {
		o.open = false
		r := s.report(o, fixsbe.ExecTypeCanceled, fixsbe.OrdStatusCanceled)
		r.ClOrdID, r.OrigClOrdID = m.CancelClOrdID, []byte(o.clOrdID)
		s.send(r)
		s.accept(place())
		return
	}
	id := m.CancelClOrdID
	switch x.o.CancelReplaceRejectID {
	case "order":
		id = m.ClOrdID
	case "other":
		id = []byte("other")
	}
	if m.OrderCancelRequestAndNewOrderSingleMode != fixsbe.OrderCancelRequestAndNewOrderSingleModeStopOnFailure {
		// ALLOW_FAILURE, as the testnet answered on 2026-10-17: the new
		// order's NEW before the OrderCancelReject.
		o := place()
		if x.o.AllowFailure != "silent" {
			s.accept(o)
		}
		s.cancelReject(id, m.Symbol, -2011, "Unknown order sent.")
		return
	}
	s.cancelReject(id, m.Symbol, -2011, "Unknown order sent.")
	switch x.o.StopOnFailure {
	case "":
		// As the testnet answered: the new order not attempted, told
		// by an ExecutionReport REJECTED -2010.
		s.send(&fixsbe.ExecutionReport{OrdType: m.OrdType, Side: m.Side, ExecType: fixsbe.ExecTypeRejected, OrdStatus: fixsbe.OrdStatusRejected,
			ErrorCode: fixsbe.Some[int32](-2010), ErrorText: []byte("Unable to cancel order, order placement not attempted."),
			ClOrdID: m.ClOrdID, Symbol: m.Symbol})
	case "place":
		s.accept(place())
	case "trade", "timeout-trade", "timeout":
		if x.o.StopOnFailure != "trade" {
			// Its status unknown first.
			s.send(&fixsbe.ExecutionReport{OrdType: m.OrdType, Side: m.Side, ExecType: fixsbe.ExecTypeRejected, OrdStatus: fixsbe.OrdStatusRejected,
				ErrorCode: fixsbe.Some[int32](-1007), ErrorText: []byte("Timeout waiting for response from backend server. Send status unknown; execution status unknown."),
				ClOrdID: m.ClOrdID, Symbol: m.Symbol})
		}
		if x.o.StopOnFailure != "timeout" {
			o := place()
			o.open = false
			r := s.report(o, fixsbe.ExecTypeTrade, fixsbe.OrdStatusFilled)
			r.CumQty, r.LastQty, r.LeavesQty = o.qty, o.qty, fixsbe.Some[int64](0)
			s.send(r)
		}
	}
}

func (s *conn) amend(m *fixsbe.OrderAmendKeepPriorityRequest) {
	x := s.x
	o := x.find(m.OrderID, m.OrigClOrdID)
	reject := func(code int32, text string) {
		s.send(&fixsbe.OrderAmendReject{OrderID: m.OrderID, QtyExponent: m.QtyExponent, OrderQty: m.OrderQty, ErrorCode: code,
			ClOrdID: m.ClOrdID, OrigClOrdID: m.OrigClOrdID, Symbol: m.Symbol, ErrorText: []byte(text)})
	}
	if o == nil {
		reject(-2011, "Unknown order sent.")
		return
	}
	switch x.o.Amend {
	case "reject":
		reject(-2038, "The requested action would change no state; rejecting.")
	case "timeout-order-id":
		// Status unknown, told under the order's own ClOrdID.
		r := s.report(o, fixsbe.ExecTypeRejected, fixsbe.OrdStatusRejected)
		r.ErrorCode, r.ErrorText = fixsbe.Some[int32](-1007), []byte("Timeout waiting for response from backend server. Send status unknown; execution status unknown.")
		s.send(r)
	case "silent":
	case "new":
		o.clOrdID = string(m.ClOrdID)
		s.send(s.report(o, fixsbe.ExecTypeNew, fixsbe.OrdStatusNew))
	default:
		orig := o.clOrdID
		o.clOrdID, o.qty, o.qtyExp = string(m.ClOrdID), m.OrderQty, m.QtyExponent
		r := s.report(o, fixsbe.ExecTypeReplaced, fixsbe.OrdStatusNew)
		r.OrigClOrdID = []byte(orig)
		s.send(r)
	}
}

func (s *conn) newList(m *fixsbe.NewOrderList) {
	x := s.x
	refuse := func(code int32, text string) {
		r := &fixsbe.ListStatus{ListStatusType: fixsbe.ListStatusTypeAllDone, ListOrderStatus: fixsbe.ListOrderStatusReject,
			ListRejectReason: fixsbe.Some(fixsbe.ListRejectReasonOther), ClListID: m.ClListID}
		for _, o := range m.Orders {
			r.Orders = append(r.Orders, fixsbe.ListStatusOrders{ErrorCode: fixsbe.Some(code), ClOrdID: o.ClOrdID, Symbol: o.Symbol,
				ErrorText: []byte(text)})
		}
		s.send(r)
	}
	if x.o.RejectOrders {
		// As the testnet answered a list out of a filter: ListStatus
		// ALL_DONE/REJECT, its orders' errors in it, then an
		// ExecutionReport REJECTED per order.
		code, text := x.refusal()
		refuse(code, text)
		for _, o := range m.Orders {
			s.send(&fixsbe.ExecutionReport{OrdType: o.OrdType, Side: o.Side, ExecType: fixsbe.ExecTypeRejected, OrdStatus: fixsbe.OrdStatusRejected,
				ErrorCode: fixsbe.Some(code), ErrorText: []byte(text), ClOrdID: o.ClOrdID, Symbol: o.Symbol})
		}
		return
	}
	x.nextID++
	l := &list{id: x.nextID, clListID: string(m.ClListID)}
	x.lists = append(x.lists, l)
	for i, lo := range m.Orders {
		o := s.place(lo.ClOrdID, lo.Symbol, lo.Side, lo.OrdType, lo.Price, lo.OrderQty, lo.PriceExponent, lo.QtyExponent)
		o.list, o.pending = l, i > 0
		l.orders = append(l.orders, o)
	}
	if x.o.TimeoutLists {
		// Placed, though the answer says otherwise.
		refuse(-1007, "Timeout waiting for response from backend server. Send status unknown; execution status unknown.")
		return
	}
	s.listStatus(l, fixsbe.ListStatusTypeExecStarted, fixsbe.ListOrderStatusExecuting)
	for _, o := range l.orders {
		st := fixsbe.OrdStatusNew
		if o.pending {
			st = fixsbe.OrdStatusPendingNew
		}
		s.send(s.report(o, fixsbe.ExecTypeNew, st))
	}
}

func (s *conn) massCancel(m *fixsbe.OrderMassCancelRequest) {
	x := s.x
	n := int64(0)
	var lists []*list
	for _, o := range x.orders {
		if !o.open || o.symbol != string(m.Symbol) {
			continue
		}
		o.open = false
		n++
		r := s.report(o, fixsbe.ExecTypeCanceled, fixsbe.OrdStatusCanceled)
		r.ClOrdID, r.OrigClOrdID = m.ClOrdID, []byte(o.clOrdID)
		s.send(r)
		if o.list != nil && !slices.Contains(lists, o.list) {
			lists = append(lists, o.list)
		}
	}
	for _, l := range lists {
		s.listStatus(l, fixsbe.ListStatusTypeAllDone, fixsbe.ListOrderStatusAllDone)
	}
	r := &fixsbe.OrderMassCancelReport{MassCancelRequestType: m.MassCancelRequestType,
		MassCancelResponse: fixsbe.MassCancelResponseCancelSymbolOrders, TotalAffectedOrders: fixsbe.Some(n),
		Symbol: m.Symbol, ClOrdID: m.ClOrdID}
	if x.o.MassCancel == "no-id" {
		r.ClOrdID = nil
	}
	s.send(r)
}

func (s *conn) limits(m *fixsbe.LimitQuery) {
	id := m.ReqID
	switch s.x.o.Limits {
	case "silent":
		return
	case "other-id":
		id = []byte("other")
	}
	sec, day := fixsbe.Some(fixsbe.LimitResetIntervalResolutionSecond), fixsbe.Some(fixsbe.LimitResetIntervalResolutionDay)
	if s.x.o.Kind == binancefix.MarketData {
		streams := 0
		for _, sub := range s.subs {
			if !sub.unsubscribed {
				streams += len(sub.streams)
			}
		}
		s.send(&fixsbe.LimitResponse{ReqID: id, LimitIndicators: []fixsbe.LimitIndicators{
			{LimitType: fixsbe.LimitTypeMessageLimit, LimitCount: uint64(len(s.x.requests)), LimitMax: binancefix.MarketDataMessageLimit,
				LimitResetInterval: fixsbe.Some[uint32](60), LimitResetIntervalResolution: sec},
			{LimitType: fixsbe.LimitTypeSubscriptionLimit, LimitCount: uint64(streams), LimitMax: uint64(s.x.o.MarketData.MaxStreams)},
		}})
		return
	}
	s.send(&fixsbe.LimitResponse{ReqID: id, LimitIndicators: []fixsbe.LimitIndicators{
		{LimitType: fixsbe.LimitTypeMessageLimit, LimitCount: uint64(len(s.x.requests)), LimitMax: 10000, LimitResetInterval: fixsbe.Some[uint32](10), LimitResetIntervalResolution: sec},
		{LimitType: fixsbe.LimitTypeOrderLimit, LimitCount: uint64(len(s.x.orders)), LimitMax: 50, LimitResetInterval: fixsbe.Some[uint32](10), LimitResetIntervalResolution: sec},
		{LimitType: fixsbe.LimitTypeOrderLimit, LimitCount: uint64(len(s.x.orders)), LimitMax: 160000, LimitResetInterval: fixsbe.Some[uint32](1), LimitResetIntervalResolution: day},
	}})
}
