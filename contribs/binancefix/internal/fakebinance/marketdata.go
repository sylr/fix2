package fakebinance

import (
	"slices"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
)

// The market data's exponents and prices.
const (
	priceExponent = -2
	qtyExponent   = -8
	bidPx         = 6_543_200
	askPx         = 6_543_300
)

// subscription is a connection's market data subscription.
type subscription struct {
	id           string
	streams      []binancefix.Stream
	unsubscribed bool
	linger       int // publications it still gets once unsubscribed
}

// listed reports whether symbol is an instrument of the exchange.
func (x *Exchange) listed(symbol string) bool { return slices.Contains(x.o.MarketData.Symbols, symbol) }

// marketData answers a MarketDataRequest: a subscription is refused whole,
// or taken, its book streams sent their snapshot; an unsubscribe is
// answered with nothing, unless refused.
func (s *conn) marketData(m *fixsbe.MarketDataRequest) {
	x, o := s.x, &s.x.o.MarketData
	reject := func(reason fixsbe.Optional[fixsbe.MdReqRejReason], code fixsbe.Optional[int32], text string) {
		s.send(&fixsbe.MarketDataRequestReject{MDReqRejReason: reason, ErrorCode: code, MDReqID: m.MDReqID, Text: []byte(text)})
	}
	id := string(m.MDReqID)
	i := slices.IndexFunc(s.subs, func(sub *subscription) bool { return sub.id == id && !sub.unsubscribed })
	if m.SubscriptionRequestType == fixsbe.SubscriptionRequestTypeUnsubscribe {
		if i < 0 {
			reject(fixsbe.Optional[fixsbe.MdReqRejReason]{}, fixsbe.Optional[int32]{}, "Unknown MDReqID.")
			return
		}
		s.subs[i].unsubscribed, s.subs[i].linger = true, o.Linger
		if o.RejectUnsubscribe {
			reject(fixsbe.Optional[fixsbe.MdReqRejReason]{}, fixsbe.Optional[int32]{}, "Unsubscribe refused.")
		}
		return
	}
	if i >= 0 {
		reject(fixsbe.Some(fixsbe.MdReqRejReasonDuplicateMdReqID), fixsbe.Optional[int32]{}, "Duplicate MDReqID.")
		return
	}
	var kinds []binancefix.StreamKind
	types := func(t fixsbe.MdEntryType) bool {
		return slices.ContainsFunc(m.MDEntryTypes, func(e fixsbe.MDEntryTypes) bool { return e.MDEntryType == t })
	}
	if types(fixsbe.MdEntryTypeTrade) {
		kinds = append(kinds, binancefix.StreamTrade)
	}
	depth := int(m.MarketDepth.Value)
	if types(fixsbe.MdEntryTypeBid) || types(fixsbe.MdEntryTypeOffer) {
		if depth <= 1 {
			kinds = append(kinds, binancefix.StreamBookTicker)
		} else {
			kinds = append(kinds, binancefix.StreamDepth)
		}
	}
	var streams []binancefix.Stream
	for _, rs := range m.RelatedSym {
		if !x.listed(string(rs.Symbol)) {
			reject(fixsbe.Optional[fixsbe.MdReqRejReason]{}, fixsbe.Some[int32](-1121), "Invalid symbol.")
			return
		}
		for _, k := range kinds {
			streams = append(streams, binancefix.Stream{Symbol: string(rs.Symbol), Kind: k})
		}
	}
	active := 0
	for _, sub := range s.subs {
		if !sub.unsubscribed {
			active += len(sub.streams)
		}
	}
	if active+len(streams) > o.MaxStreams {
		reject(fixsbe.Some(fixsbe.MdReqRejReasonTooManySubscriptions), fixsbe.Optional[int32]{}, "Too many subscriptions.")
		return
	}
	s.subs = append(s.subs, &subscription{id: id, streams: streams})
	for _, st := range streams {
		if st.Kind != binancefix.StreamTrade {
			s.snapshot(st, depth)
		}
	}
}

// snapshot sends the snapshot of book stream st, of depth levels per side
// at most.
func (s *conn) snapshot(st binancefix.Stream, depth int) {
	n := 1
	if st.Kind == binancefix.StreamDepth {
		n = min(depth, s.x.o.MarketData.BookLevels)
	}
	m := &fixsbe.MarketDataSnapshot{LastBookUpdateID: fixsbe.Some(s.x.updateID), PriceExponent: priceExponent,
		QtyExponent: qtyExponent, Symbol: []byte(st.Symbol)}
	for i := range int64(n) {
		m.MDEntriesBids = append(m.MDEntriesBids, fixsbe.MarketDataSnapshotMDEntriesBids{MDEntryPx: bidPx - i, MDEntrySize: 100_000_000 + i})
		m.MDEntriesAsks = append(m.MDEntriesAsks, fixsbe.MarketDataSnapshotMDEntriesAsks{MDEntryPx: askPx + i, MDEntrySize: 100_000_000 + i})
	}
	s.send(m)
}

// Publish sends, on every connection, an update of each stream of symbol
// subscribed: a trade, a book ticker update, a depth update. A
// subscription unsubscribed still gets MarketData.Linger of them. It
// returns how many it sent.
func (x *Exchange) Publish(symbol string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, s := range x.mdConns {
		for _, sub := range s.subs {
			if sub.unsubscribed {
				if sub.linger == 0 {
					continue
				}
				sub.linger--
			}
			for _, st := range sub.streams {
				if st.Symbol == symbol {
					s.publish(st)
					n++
				}
			}
		}
	}
	return n
}

// publish sends an update of stream st.
func (s *conn) publish(st binancefix.Stream) {
	x := s.x
	switch st.Kind {
	case binancefix.StreamTrade:
		x.tradeID++
		s.send(&fixsbe.MarketDataIncrementalTrade{TransactTime: time.Now().UnixMicro(), PriceExponent: priceExponent, QtyExponent: qtyExponent,
			MDEntries: []fixsbe.MDEntries{{TradeID: x.tradeID, MDEntryPx: bidPx + 10, MDEntrySize: 1000, AggressorSide: fixsbe.Some(fixsbe.SideBuy)}},
			Symbol:    []byte(st.Symbol)})
	case binancefix.StreamBookTicker:
		x.updateID++
		s.send(&fixsbe.MarketDataIncrementalBookTicker{LastBookUpdateID: x.updateID, PriceExponent: priceExponent, QtyExponent: qtyExponent,
			MDEntriesBids: []fixsbe.MarketDataIncrementalBookTickerMDEntriesBids{{MDEntryPx: bidPx, MDEntrySize: fixsbe.Some[int64](150_000_000)}},
			MDEntriesAsks: []fixsbe.MarketDataIncrementalBookTickerMDEntriesAsks{{MDEntryPx: askPx, MDEntrySize: fixsbe.Some[int64](2)}},
			Symbol:        []byte(st.Symbol)})
	case binancefix.StreamDepth:
		first := x.updateID + 1
		x.updateID += 3
		s.send(&fixsbe.MarketDataIncrementalDepth{FirstBookUpdateID: first, LastBookUpdateID: x.updateID, PriceExponent: priceExponent,
			QtyExponent: qtyExponent,
			MDEntriesBids: []fixsbe.MarketDataIncrementalDepthMDEntriesBids{
				{MDEntryPx: bidPx, MDEntrySize: fixsbe.Some[int64](1)},
				{MDEntryPx: bidPx - 1}, // deleted
			},
			MDEntriesAsks: []fixsbe.MarketDataIncrementalDepthMDEntriesAsks{{MDEntryPx: askPx, MDEntrySize: fixsbe.Some[int64](4)}},
			Symbol:        []byte(st.Symbol)})
	}
}

// instruments answers InstrumentListRequest m, of sequence number seq:
// its InstrumentList, in several messages if told; a Reject for an
// unknown symbol.
func (s *conn) instruments(seq uint32, m *fixsbe.InstrumentListRequest) {
	x := s.x
	symbols := x.o.MarketData.Symbols
	if m.InstrumentListRequestType == fixsbe.InstrumentListRequestTypeSingleInstrument {
		if !x.listed(string(m.Symbol)) {
			s.send(&fixsbe.Reject{RefSeqNum: fixsbe.Some(seq), ErrorCode: fixsbe.Some[int32](-1121), Text: []byte("Invalid symbol.")})
			return
		}
		symbols = []string{string(m.Symbol)}
	}
	per := x.o.MarketData.InstrumentsPerMessage
	if per <= 0 {
		per = len(symbols)
	}
	for part := range slices.Chunk(symbols, per) {
		l := &fixsbe.InstrumentList{InstrumentReqID: m.InstrumentReqID}
		for _, sym := range part {
			l.RelatedSym = append(l.RelatedSym, fixsbe.InstrumentListRelatedSym{PriceExponent: priceExponent, QtyExponent: qtyExponent,
				MinTradeVol: fixsbe.Some[int64](1000), MaxTradeVol: fixsbe.Some[int64](900_000_000_000), MinQtyIncrement: fixsbe.Some[int64](1000),
				MinPriceIncrement: fixsbe.Some[int64](1), Symbol: []byte(sym), Currency: []byte(sym[:3])})
		}
		s.send(l)
	}
}
