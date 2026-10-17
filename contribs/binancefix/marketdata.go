package binancefix

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
)

var (
	// ErrSessionKind is the error of a request the session's Kind does not
	// take: an order on a market data session, a subscription or an
	// InstrumentListRequest on an order entry one. Nothing is written.
	ErrSessionKind = errors.New("binancefix: request of another kind of session")
	// ErrInvalidSubscription is Subscribe's error for a MarketDataRequest
	// whose streams cannot be told (see StreamKind), or that is not a
	// subscription.
	ErrInvalidSubscription = errors.New("binancefix: invalid subscription")
	// ErrStreamTaken is Subscribe's error for a stream subscribed already
	// on the connection, by this subscription or another, unsubscribed or
	// not: Reconnect gives a clean connection.
	ErrStreamTaken = errors.New("binancefix: stream subscribed on the connection already")
	// ErrUnsubscribed is Unsubscribe's error once called.
	ErrUnsubscribed = errors.New("binancefix: unsubscribed already")
	// ErrRateLimited is the error of a request a market data session's
	// message budget has no room for (MarketDataMessageLimit, less
	// MarketDataMessageReserve): nothing is written.
	ErrRateLimited = errors.New("binancefix: market data message budget spent")
	// ErrInvalidMarketData tells of a market data message the client
	// refuses, beyond a required field null (ErrMissingRequired): an enum
	// value the schema does not have, a depth update's FirstBookUpdateID
	// after its LastBookUpdateID, bytes after the message.
	ErrInvalidMarketData = errors.New("binancefix: invalid market data message")
)

// StreamKind is what a market data stream carries. The streams of a
// MarketDataRequest are told by its MDEntryTypes and MarketDepth, as
// Binance documents them: TRADE alone for trades, BID and OFFER together
// for a book, of MarketDepth 1 for the book ticker, 2 to MaxMarketDepth
// for depth. A request may ask for trades and a book; it may not ask for
// one side of a book.
type StreamKind uint8

const (
	// StreamTrade is a symbol's trades: MarketDataIncrementalTrade, no
	// snapshot.
	StreamTrade StreamKind = iota + 1
	// StreamBookTicker is a symbol's best bid and offer: a
	// MarketDataSnapshot on activation, then
	// MarketDataIncrementalBookTicker, which Binance culls under load,
	// sending only a symbol's newest.
	StreamBookTicker
	// StreamDepth is a symbol's book, MarketDepth levels per side: a
	// MarketDataSnapshot on activation, then MarketDataIncrementalDepth
	// every 20ms.
	StreamDepth
)

func (k StreamKind) String() string {
	switch k {
	case StreamTrade:
		return "trade"
	case StreamBookTicker:
		return "book ticker"
	case StreamDepth:
		return "depth"
	}
	return "invalid"
}

// Stream is a symbol's stream of a kind: data are matched to it by their
// Symbol and their template, Binance's market data carrying no MDReqID.
// A symbol's book ticker and depth streams share MarketDataSnapshot: a
// connection carries one of them at most.
type Stream struct {
	Symbol string
	Kind   StreamKind
}

// Phase is where a subscription's MarketDataRequest, or its unsubscribe,
// stands on the wire.
type Phase uint8

const (
	// PhaseNone: not asked for (an unsubscribe before Unsubscribe).
	PhaseNone Phase = iota
	// PhaseQueued: waiting to be written, or being written.
	PhaseQueued
	// PhaseWritten: wholly written. Binance answers neither a subscription
	// nor an unsubscribe: data, or a reject, may follow, or nothing.
	PhaseWritten
	// PhaseNeverWritten: the connection ended, or our Logout was queued,
	// before a byte of it was written.
	PhaseNeverWritten
	// PhasePartlyWritten: the connection ended as it was being written:
	// Binance may have read it or not.
	PhasePartlyWritten
)

func (p Phase) String() string {
	switch p {
	case PhaseNone:
		return "none"
	case PhaseQueued:
		return "queued"
	case PhaseWritten:
		return "written"
	case PhaseNeverWritten:
		return "never written"
	case PhasePartlyWritten:
		return "partly written"
	}
	return "invalid"
}

// SubscriptionReject is Binance's refusal of a subscription: a
// MarketDataRequestReject naming its MDReqID, or a Reject naming its
// request's seqNum, or its unsubscribe's.
type SubscriptionReject struct {
	// TemplateID is MarketDataRequestRejectTemplateID or
	// RejectTemplateID.
	TemplateID uint16
	// SeqNum is the reject's sequence number, Received when it was read.
	SeqNum   uint32
	Received time.Time
	// Reason is a MarketDataRequestReject's MDReqRejReason.
	Reason    Optional[MdReqRejReason]
	ErrorCode Optional[int32]
	Text      string
	// Unsubscribe is set when it may refuse our unsubscribe: a Reject
	// naming its seqNum, or a MarketDataRequestReject read once the
	// unsubscribe's writing began (its MDReqID names both requests, which
	// one it refuses is not told).
	Unsubscribe bool
}

// StreamStatus tells of a stream's data on a connection: activity seen,
// never acceptance. A stream with no data yet may be quiet (trades), or
// refused.
type StreamStatus struct {
	Stream
	// Seen is set once data of the stream were read; the others tell of
	// the first: its template, sequence number and time of reading.
	Seen          bool
	FirstTemplate uint16
	FirstSeqNum   uint32
	FirstReceived time.Time
}

// SubscriptionStatus is a subscription's state at a time.
type SubscriptionStatus struct {
	// Request is where the MarketDataRequest stands, Unsubscribe where
	// its unsubscribe does.
	Request, Unsubscribe Phase
	// Rejects are Binance's rejects of it, in the order read, late ones
	// included: after data, after the unsubscribe.
	Rejects []SubscriptionReject
	// Streams are its streams, each with its first data.
	Streams []StreamStatus
	// Ended is set once its connection ended, with Err why (ErrLoggedOut
	// for a Logout of ours): its streams ended with it, and are never
	// carried over to another connection.
	Ended bool
	Err   error
}

// Subscription follows a MarketDataRequest from Subscribe on, on its
// connection: it never outlives it. Binance acknowledges no subscription:
// a Subscription tells what was written, what Binance refused, and which
// streams have shown data, never that it was accepted. Its methods are
// safe for concurrent use; its status may run ahead of the Handler.
type Subscription struct {
	k       *conn
	epoch   uint64
	id      string
	depth   Optional[uint16]
	streams []Stream
	done    chan struct{}

	mu        sync.Mutex
	request   Phase
	unsub     Phase
	reqSent   bool // the request's writing began
	unsubSent bool // the unsubscribe's writing began
	rejects   []SubscriptionReject
	status    []StreamStatus
	ended     bool
	err       error
}

// ID returns the subscription's MDReqID.
func (s *Subscription) ID() string { return s.id }

// Epoch returns the connection the subscription belongs to.
func (s *Subscription) Epoch() uint64 { return s.epoch }

// Streams returns the subscription's streams, in the request's order:
// its symbols, then its kinds. The slice is the caller's.
func (s *Subscription) Streams() []Stream { return slices.Clone(s.streams) }

// Status returns the subscription's state. Its slices are the caller's.
func (s *Subscription) Status() SubscriptionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SubscriptionStatus{Request: s.request, Unsubscribe: s.unsub, Rejects: slices.Clone(s.rejects),
		Streams: slices.Clone(s.status), Ended: s.ended, Err: s.err}
}

// Done is closed once the subscription's connection ended.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Err returns why the subscription's connection ended, nil before.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Unsubscribe queues the subscription's unsubscribe: a MarketDataRequest
// of SubscriptionRequestType UNSUBSCRIBE and its MDReqID. Binance answers
// none: it proves neither that Binance took it nor that data stop, and
// data read after it are still the subscription's. Its streams are not
// freed: on this connection, they cannot be subscribed again. It never
// waits, failing with ErrUnsubscribed after a first call, ErrNotLoggedOn
// once the connection is not logged on, ErrQueueFull or ErrRateLimited.
func (s *Subscription) Unsubscribe() error {
	frame, err := encode(&fixsbe.MarketDataRequest{SubscriptionRequestType: fixsbe.SubscriptionRequestTypeUnsubscribe,
		MarketDepth: s.depth, MDReqID: []byte(s.id)})
	if err != nil {
		return err
	}
	k := s.k
	k.mu.Lock()
	defer k.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsub != PhaseNone {
		return ErrUnsubscribed
	}
	if err := k.admitLocked(); err != nil {
		return err
	}
	s.unsub = PhaseQueued
	k.app = append(k.app, pending{frame: frame, s: s, unsub: true})
	k.wake()
	return nil
}

// sending marks the request, or the unsubscribe, as being written.
func (s *Subscription) sending(unsub bool) {
	s.mu.Lock()
	if unsub {
		s.unsubSent = true
	} else {
		s.reqSent = true
	}
	s.mu.Unlock()
}

// phase returns the request's phase, or the unsubscribe's. s's lock is
// held.
func (s *Subscription) phase(unsub bool) *Phase {
	if unsub {
		return &s.unsub
	}
	return &s.request
}

// written marks the request, or the unsubscribe, wholly written.
func (s *Subscription) written(unsub bool) {
	s.mu.Lock()
	if p := s.phase(unsub); *p == PhaseQueued {
		*p = PhaseWritten
	}
	s.mu.Unlock()
}

// unsent marks the request, or the unsubscribe, never written.
func (s *Subscription) unsent(unsub bool) {
	s.mu.Lock()
	if p := s.phase(unsub); *p == PhaseQueued {
		*p = PhaseNeverWritten
	}
	s.mu.Unlock()
}

// end ends the subscription with its connection, for err.
func (s *Subscription) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for _, unsub := range []bool{false, true} {
		if p := s.phase(unsub); *p == PhaseQueued {
			*p = PhaseNeverWritten
			if (unsub && s.unsubSent) || (!unsub && s.reqSent) {
				*p = PhasePartlyWritten
			}
		}
	}
	s.ended, s.err = true, err
	close(s.done)
}

// reject records rejection r.
func (s *Subscription) reject(r SubscriptionReject, byID bool) {
	s.mu.Lock()
	if byID {
		r.Unsubscribe = s.unsubSent
	}
	s.rejects = append(s.rejects, r)
	s.mu.Unlock()
}

// saw records data of stream st, in.
func (s *Subscription) saw(st Stream, tid uint16, in Inbound) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.status, func(x StreamStatus) bool { return x.Stream == st }); i >= 0 && !s.status[i].Seen {
		s.status[i].Seen, s.status[i].FirstTemplate = true, tid
		s.status[i].FirstSeqNum, s.status[i].FirstReceived = in.Header.SeqNum, in.Received
	}
}

// requestStreams returns the streams of subscription m, refusing what
// cannot be told apart or is not a subscription.
func requestStreams(m *MarketDataRequest) ([]Stream, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidSubscription}, args...)...)
	}
	if m.SubscriptionRequestType != fixsbe.SubscriptionRequestTypeSubscribe {
		return nil, invalid("SubscriptionRequestType %v, SUBSCRIBE required (Subscription.Unsubscribe unsubscribes)", m.SubscriptionRequestType)
	}
	if !idPattern.MatchString(string(m.MDReqID)) {
		return nil, fmt.Errorf("%w: MDReqID %q", ErrInvalidID, m.MDReqID)
	}
	if len(m.RelatedSym) == 0 {
		return nil, invalid("no symbol")
	}
	var trade, bid, offer bool
	for _, e := range m.MDEntryTypes {
		var seen *bool
		switch e.MDEntryType {
		case fixsbe.MdEntryTypeTrade:
			seen = &trade
		case fixsbe.MdEntryTypeBid:
			seen = &bid
		case fixsbe.MdEntryTypeOffer:
			seen = &offer
		default:
			return nil, invalid("MDEntryType %v", e.MDEntryType)
		}
		if *seen {
			return nil, invalid("MDEntryType %v twice", e.MDEntryType)
		}
		*seen = true
	}
	var kinds []StreamKind
	if trade {
		kinds = append(kinds, StreamTrade)
	}
	switch {
	case bid != offer:
		return nil, invalid("BID and OFFER go together: a book stream carries both sides")
	case bid && !m.MarketDepth.Valid:
		return nil, invalid("MarketDepth required for a book: 1 for the book ticker, 2 to %d for depth", MaxMarketDepth)
	case bid && m.MarketDepth.Value == 1:
		kinds = append(kinds, StreamBookTicker)
	case bid && m.MarketDepth.Value >= 2 && m.MarketDepth.Value <= MaxMarketDepth:
		kinds = append(kinds, StreamDepth)
	case bid:
		return nil, invalid("MarketDepth %d: 1 for the book ticker, 2 to %d for depth", m.MarketDepth.Value, MaxMarketDepth)
	}
	if len(kinds) == 0 {
		return nil, invalid("no MDEntryType")
	}
	var streams []Stream
	for i, rs := range m.RelatedSym {
		if len(rs.Symbol) == 0 {
			return nil, invalid("empty symbol")
		}
		if slices.ContainsFunc(m.RelatedSym[:i], func(o MarketDataRequestRelatedSym) bool { return string(o.Symbol) == string(rs.Symbol) }) {
			return nil, invalid("symbol %q twice", rs.Symbol)
		}
		for _, k := range kinds {
			streams = append(streams, Stream{Symbol: string(rs.Symbol), Kind: k})
		}
	}
	return streams, nil
}

// subscribe queues subscription frame, of request m and streams streams.
func (k *conn) subscribe(frame []byte, m *MarketDataRequest, streams []Stream) (*Subscription, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.state != stateActive {
		return nil, ErrNotLoggedOn
	}
	id := string(m.MDReqID)
	if k.subs[id] != nil {
		return nil, fmt.Errorf("%w: MDReqID %q", ErrDuplicateID, id)
	}
	for _, st := range streams {
		if s := k.streamOwner(st); s != nil {
			return nil, fmt.Errorf("%w: %s %s, by %q", ErrStreamTaken, st.Symbol, st.Kind, s.id)
		}
	}
	if err := k.admitLocked(); err != nil {
		return nil, err
	}
	s := &Subscription{k: k, epoch: k.epoch, id: id, depth: m.MarketDepth, streams: streams, done: make(chan struct{}),
		request: PhaseQueued}
	for _, st := range streams {
		s.status = append(s.status, StreamStatus{Stream: st})
		k.streams[st] = s
	}
	k.subs[id] = s
	k.subList = append(k.subList, s)
	k.app = append(k.app, pending{frame: frame, s: s})
	k.wake()
	return s, nil
}

// streamOwner returns the subscription of stream st on the connection, or
// of the stream st cannot be told from, nil if none. k's lock is held.
func (k *conn) streamOwner(st Stream) *Subscription {
	if s := k.streams[st]; s != nil {
		return s
	}
	switch st.Kind {
	case StreamBookTicker:
		return k.streams[Stream{st.Symbol, StreamDepth}]
	case StreamDepth:
		return k.streams[Stream{st.Symbol, StreamBookTicker}]
	}
	return nil
}

// admitLocked checks that a request may be queued: the connection logged
// on, room in the queue and, for market data, in the message budget,
// which it then takes. k's lock is held.
func (k *conn) admitLocked() error {
	if k.state != stateActive {
		return ErrNotLoggedOn
	}
	if len(k.app) >= k.c.cfg.MaxQueued {
		return ErrQueueFull
	}
	if k.rate != nil && !k.rate.admit(time.Now()) {
		return ErrRateLimited
	}
	return nil
}

// route returns the subscription market data of template tid and symbol
// belong to, nil if none, and records its activity.
func (k *conn) route(tid uint16, symbol string, in Inbound) *Subscription {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := Stream{Symbol: symbol}
	switch tid {
	case fixsbe.MarketDataIncrementalTradeTemplateID:
		st.Kind = StreamTrade
	case fixsbe.MarketDataIncrementalBookTickerTemplateID:
		st.Kind = StreamBookTicker
	case fixsbe.MarketDataIncrementalDepthTemplateID:
		st.Kind = StreamDepth
	case fixsbe.MarketDataSnapshotTemplateID:
		// The symbol's book, ticker or depth: one at most.
		st.Kind = StreamBookTicker
		if k.streams[st] == nil {
			st.Kind = StreamDepth
		}
	}
	s := k.streams[st]
	if s != nil {
		s.saw(st, tid, in)
	}
	return s
}

// rejectedByID returns the subscription MarketDataRequestReject m names,
// nil if none, and records the rejection.
func (k *conn) rejectedByID(m *fixsbe.MarketDataRequestReject, in Inbound) *Subscription {
	k.mu.Lock()
	s := k.subs[string(m.MDReqID)]
	k.mu.Unlock()
	if s != nil {
		s.reject(SubscriptionReject{TemplateID: in.Header.TemplateId, SeqNum: in.Header.SeqNum, Received: in.Received,
			Reason: m.MDReqRejReason, ErrorCode: m.ErrorCode, Text: string(m.Text)}, true)
	}
	return s
}

// rejectedBySeq returns the subscription whose request or unsubscribe
// Reject m names by its RefSeqNum, nil if none, and records the
// rejection.
func (k *conn) rejectedBySeq(m *fixsbe.Reject, in Inbound) *Subscription {
	if !m.RefSeqNum.Valid {
		return nil
	}
	k.mu.Lock()
	ref, ok := k.subSeqs[m.RefSeqNum.Value]
	k.mu.Unlock()
	if !ok {
		return nil
	}
	ref.s.reject(SubscriptionReject{TemplateID: in.Header.TemplateId, SeqNum: in.Header.SeqNum, Received: in.Received,
		ErrorCode: m.ErrorCode, Text: string(m.Text), Unsubscribe: ref.unsub}, false)
	return ref.s
}

// subRef is a subscription's request, or its unsubscribe.
type subRef struct {
	s     *Subscription
	unsub bool
}

// decodeMarketData decodes and checks market data message h, body, and
// returns it and its Symbol, if it has one. It reports false for a
// template that is not market data. A message the codec cannot decode,
// followed by bytes it did not read, or that checkMarketData refuses, is
// an error.
func decodeMarketData(h MessageHeader, body []byte) (m any, symbol []byte, ok bool, err error) {
	var n int
	var name string
	switch h.TemplateId {
	case fixsbe.InstrumentListTemplateID:
		x := new(fixsbe.InstrumentList)
		n, err = x.DecodeBody(h, body)
		m, name = x, "InstrumentList"
	case fixsbe.MarketDataRequestRejectTemplateID:
		x := new(fixsbe.MarketDataRequestReject)
		n, err = x.DecodeBody(h, body)
		m, name = x, "MarketDataRequestReject"
	case fixsbe.MarketDataSnapshotTemplateID:
		x := new(fixsbe.MarketDataSnapshot)
		n, err = x.DecodeBody(h, body)
		m, symbol, name = x, x.Symbol, "MarketDataSnapshot"
	case fixsbe.MarketDataIncrementalTradeTemplateID:
		x := new(fixsbe.MarketDataIncrementalTrade)
		n, err = x.DecodeBody(h, body)
		m, symbol, name = x, x.Symbol, "MarketDataIncrementalTrade"
	case fixsbe.MarketDataIncrementalBookTickerTemplateID:
		x := new(fixsbe.MarketDataIncrementalBookTicker)
		n, err = x.DecodeBody(h, body)
		m, symbol, name = x, x.Symbol, "MarketDataIncrementalBookTicker"
	case fixsbe.MarketDataIncrementalDepthTemplateID:
		x := new(fixsbe.MarketDataIncrementalDepth)
		n, err = x.DecodeBody(h, body)
		m, symbol, name = x, x.Symbol, "MarketDataIncrementalDepth"
	default:
		return nil, nil, false, nil
	}
	if err == nil && n != len(body) {
		err = fmt.Errorf("%w: %d bytes after the message", ErrInvalidMarketData, len(body)-n)
	}
	if err == nil {
		err = checkMarketData(m)
	}
	if err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", name, err)
	}
	return m, symbol, true, nil
}

// nullInt64 is the null value of int64 fields, which SBE forbids in a
// required one (§4.4.2).
const nullInt64 = math.MinInt64

// checkHeader returns why the client refuses the header h of a message
// for the Handler of a market data session, nil if it does not: a
// required field null, its type's null value (blockLength, templateId,
// version: 65,535; sendingTime: math.MinInt64). The schemaId is checked
// already, and the seqNum's 4,294,967,295 is a number of the series,
// followed by 0.
func checkHeader(h MessageHeader) error {
	switch {
	case h.SendingTime == nullInt64:
		return fmt.Errorf("%w: header sendingTime", ErrMissingRequired)
	case h.BlockLength == math.MaxUint16:
		return fmt.Errorf("%w: header blockLength", ErrMissingRequired)
	case h.TemplateId == math.MaxUint16:
		return fmt.Errorf("%w: header templateId", ErrMissingRequired)
	case h.Version == math.MaxUint16:
		return fmt.Errorf("%w: header version", ErrMissingRequired)
	}
	return nil
}

// firstErr returns the first of errs not nil.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// checkMarketData returns why the client refuses market data message m,
// nil if it does not. The codecs check structure only: here, required
// fields not null (int64's math.MinInt64, an exponent's -128) nor empty (a
// varString8), enums of the schema's values, a depth update's
// FirstBookUpdateID up to its LastBookUpdateID. Any exponent but the
// null is the schema's; a snapshot's LastBookUpdateID is optional.
func checkMarketData(m any) error {
	missing := func(field string) error { return fmt.Errorf("%w: %s", ErrMissingRequired, field) }
	required := func(field string, v int64) error {
		if v == nullInt64 {
			return missing(field)
		}
		return nil
	}
	exponents := func(pe, qe int8) error {
		switch {
		case pe == math.MinInt8:
			return missing("PriceExponent")
		case qe == math.MinInt8:
			return missing("QtyExponent")
		}
		return nil
	}
	text := func(field string, v []byte) error {
		if len(v) == 0 {
			return missing(field)
		}
		return nil
	}
	levels := func(side string, n int, px func(int) int64, size func(int) (int64, bool)) error {
		for i := range n {
			if err := required(fmt.Sprintf("%s[%d].MDEntryPx", side, i), px(i)); err != nil {
				return err
			}
			if v, required := size(i); required && v == nullInt64 {
				return missing(fmt.Sprintf("%s[%d].MDEntrySize", side, i))
			}
		}
		return nil
	}
	optional := func(i int) (int64, bool) { return 0, false }
	switch m := m.(type) {
	case *fixsbe.InstrumentList:
		for i, e := range m.RelatedSym {
			if err := exponents(e.PriceExponent, e.QtyExponent); err != nil {
				return fmt.Errorf("RelatedSym[%d]: %w", i, err)
			}
			if err := firstErr(text(fmt.Sprintf("RelatedSym[%d].Symbol", i), e.Symbol),
				text(fmt.Sprintf("RelatedSym[%d].Currency", i), e.Currency)); err != nil {
				return err
			}
		}
		return text("InstrumentReqID", m.InstrumentReqID)
	case *fixsbe.MarketDataRequestReject:
		if m.MDReqRejReason.Valid && !m.MDReqRejReason.Value.Known() {
			return fmt.Errorf("%w: MDReqRejReason %v", ErrInvalidMarketData, m.MDReqRejReason.Value)
		}
		return text("MDReqID", m.MDReqID)
	case *fixsbe.MarketDataSnapshot:
		// LastBookUpdateID is optional: a book kept from depth updates
		// needs it, which is its keeper's to check.
		return firstErr(exponents(m.PriceExponent, m.QtyExponent), text("Symbol", m.Symbol),
			levels("MDEntriesBids", len(m.MDEntriesBids), func(i int) int64 { return m.MDEntriesBids[i].MDEntryPx },
				func(i int) (int64, bool) { return m.MDEntriesBids[i].MDEntrySize, true }),
			levels("MDEntriesAsks", len(m.MDEntriesAsks), func(i int) int64 { return m.MDEntriesAsks[i].MDEntryPx },
				func(i int) (int64, bool) { return m.MDEntriesAsks[i].MDEntrySize, true }))
	case *fixsbe.MarketDataIncrementalTrade:
		if err := firstErr(required("TransactTime", m.TransactTime), exponents(m.PriceExponent, m.QtyExponent),
			text("Symbol", m.Symbol)); err != nil {
			return err
		}
		for i, e := range m.MDEntries {
			if err := firstErr(required(fmt.Sprintf("MDEntries[%d].TradeID", i), e.TradeID),
				required(fmt.Sprintf("MDEntries[%d].MDEntryPx", i), e.MDEntryPx),
				required(fmt.Sprintf("MDEntries[%d].MDEntrySize", i), e.MDEntrySize)); err != nil {
				return err
			}
			if e.AggressorSide.Valid && !e.AggressorSide.Value.Known() {
				return fmt.Errorf("%w: MDEntries[%d].AggressorSide %v", ErrInvalidMarketData, i, e.AggressorSide.Value)
			}
		}
		return nil
	case *fixsbe.MarketDataIncrementalBookTicker:
		return firstErr(required("LastBookUpdateID", m.LastBookUpdateID), exponents(m.PriceExponent, m.QtyExponent),
			text("Symbol", m.Symbol),
			levels("MDEntriesBids", len(m.MDEntriesBids), func(i int) int64 { return m.MDEntriesBids[i].MDEntryPx }, optional),
			levels("MDEntriesAsks", len(m.MDEntriesAsks), func(i int) int64 { return m.MDEntriesAsks[i].MDEntryPx }, optional))
	case *fixsbe.MarketDataIncrementalDepth:
		if err := firstErr(required("FirstBookUpdateID", m.FirstBookUpdateID), required("LastBookUpdateID", m.LastBookUpdateID),
			exponents(m.PriceExponent, m.QtyExponent), text("Symbol", m.Symbol),
			levels("MDEntriesBids", len(m.MDEntriesBids), func(i int) int64 { return m.MDEntriesBids[i].MDEntryPx }, optional),
			levels("MDEntriesAsks", len(m.MDEntriesAsks), func(i int) int64 { return m.MDEntriesAsks[i].MDEntryPx }, optional)); err != nil {
			return err
		}
		if m.FirstBookUpdateID > m.LastBookUpdateID {
			return fmt.Errorf("%w: FirstBookUpdateID %d after LastBookUpdateID %d", ErrInvalidMarketData, m.FirstBookUpdateID, m.LastBookUpdateID)
		}
	}
	return nil
}
