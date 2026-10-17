package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/tcp"
)

// sbeReconnect paces --sbe's connections: binancefix's defaults, Binance's
// market data limits, but for refused Logons, which end the command at the
// first before any succeeded, after maxRefusals in a row once one did, as
// on the FIX path.
var sbeReconnect = binancefix.Reconnect{MaxRefusals: maxRefusals, MaxInitialRefusals: 1}

// streamSBE runs cmd over Binance's FIX SBE market data API.
func streamSBE(ctx context.Context, o *options, cmd command, symbols []string, key ed25519.PrivateKey, out output, log *slog.Logger) error {
	return streamSBEWith(ctx, o, cmd, symbols, key, out, log, &tcp.Dialer{
		Addresses: []string{o.endpoint},
		TLS:       &tls.Config{MinVersion: tls.VersionTLS12},
	})
}

// streamSBEWith runs cmd over FIX SBE connections from dialer.
func streamSBEWith(ctx context.Context, o *options, cmd command, symbols []string, key ed25519.PrivateKey, out output, log *slog.Logger, dialer transport.Dialer) error {
	sender := o.sender
	if sender == "" {
		sender = randomID(8)
	}
	ctx, finish := context.WithCancelCause(ctx)
	defer finish(nil)
	c := &sbeClient{
		ctx:     ctx,
		cmd:     cmd,
		symbols: symbols,
		levels:  o.levels,
		show:    o.show,
		out:     out,
		log:     log,
		finish:  finish,
	}
	cfg := binancefix.Config{
		Kind:         binancefix.MarketData,
		Endpoint:     o.endpoint,
		Dialer:       dialer,
		APIKey:       o.apiKey,
		PrivateKey:   key,
		SenderCompID: sender,
		HeartBtInt:   o.heartbeat,
		// As on the FIX path: 2s for Binance's answer to our Logout.
		LogoutTimeout: 2 * time.Second,
		Reconnect:     sbeReconnect,
		Logger:        log,
		Handler:       c,
	}
	if o.trace {
		cfg.Trace = traceSBE(log)
	}
	bc, err := binancefix.New(cfg)
	if err != nil {
		return err
	}
	c.bc = bc
	// The Handler's flushes wait for the output until the command ends,
	// or binancefix connects no more: Run waits for the Handler, which
	// must then hand what is left to end, not wait for a blocked output.
	flushCtx, stopFlushing := context.WithCancel(ctx)
	defer stopFlushing()
	c.flushCtx = flushCtx
	go func() {
		select {
		case <-bc.Ending():
			stopFlushing()
		case <-flushCtx.Done():
		}
	}()
	// A failed write stops the command, though no event follows to be
	// handed to the output.
	go func() {
		if err := out.failure(ctx); err != nil {
			finish(fmt.Errorf("writing: %w", err))
		}
	}()
	log.LogAttrs(ctx, slog.LevelInfo, "connecting", slog.String("endpoint", o.endpoint), slog.String("sender_comp_id", sender), slog.Bool("sbe", true))
	runErr := refusal(bc.Run(ctx))
	// Run returned once the Handler was done, its calls never waiting on
	// the output past ctx: what they rendered and could not hand over is
	// written now, within outputTimeout.
	octx, cancel := context.WithTimeout(context.Background(), outputTimeout)
	defer cancel()
	endErr := out.end(octx)
	if runErr != nil {
		return errors.Join(runErr, endErr)
	}
	if err := context.Cause(ctx); !errors.Is(err, errDone) && !errors.Is(err, context.Canceled) {
		if endErr != nil && errors.Is(err, endErr) {
			// The output's failure stopped the command: told once.
			endErr = nil
		}
		return errors.Join(err, endErr)
	}
	return endErr
}

// refusal returns err, a Logon refused by Binance told as the FIX path
// tells it: "rejected" with its error code for a Reject, "refused" for a
// Logout.
func refusal(err error) error {
	var r *binancefix.LogonRefusedError
	switch {
	case !errors.As(err, &r) || r.Err != nil:
		return err
	case r.ErrorCode.Valid:
		return fmt.Errorf("logon rejected by Binance: %s (error code %d)", r.Text, r.ErrorCode.Value)
	}
	return fmt.Errorf("logon refused by Binance: %s", r.Text)
}

// sbeClient is the FIX SBE session's Handler: it subscribes on every
// Logon, and writes what it receives as the FIX path's client does, to
// the same output. Its methods run on binancefix's Handler goroutine, one
// at a time.
type sbeClient struct {
	binancefix.BaseHandler
	bc      *binancefix.Client
	cmd     command
	symbols []string
	levels  int
	show    int
	out     output
	log     *slog.Logger
	ctx     context.Context // done once the command ends
	finish  context.CancelCauseFunc
	// flushCtx bounds the waits for the output: done once ctx is, or
	// binancefix connects no more (Client.Ending).
	flushCtx context.Context

	epoch       uint64 // the connection logged on
	maintenance bool   // the server announced maintenance: reconnecting
	resync      bool   // a book lost its updates: reconnecting
	n           int    // requests sent, for their IDs
	subs        map[string]*sub
	receipts    map[*binancefix.Receipt]string // the queries' IDs, for Rejects
	quotes      map[string]*tickerEvent
	books       map[string]*book
	queries     map[string]bool // the InstrumentReqIDs or ReqID sent: answered or not
	quiet       *time.Timer     // completes the queries once answers stop
}

// OnLogon sends the command's requests: each connection starts afresh,
// its subscriptions gone with the last one.
func (c *sbeClient) OnLogon(e binancefix.LogonEvent) {
	c.log.LogAttrs(c.ctx, slog.LevelInfo, "logged on", slog.Uint64("epoch", e.Epoch), slog.String("uuid", e.UUID))
	c.epoch, c.maintenance, c.resync = e.Epoch, false, false
	c.subs = map[string]*sub{}
	c.receipts = map[*binancefix.Receipt]string{}
	c.quotes = map[string]*tickerEvent{}
	c.books = map[string]*book{}
	c.queries = map[string]bool{}
	switch c.cmd.kind {
	case kindInstruments:
		if len(c.symbols) == 0 {
			id := c.nextID("INSTRUMENTS", "")
			c.query(id, &binancefix.InstrumentListRequest{InstrumentReqID: []byte(id),
				InstrumentListRequestType: binancefix.InstrumentListRequestTypeAllInstruments})
		}
		for _, sym := range c.symbols {
			id := c.nextID("INSTRUMENT", sym)
			c.query(id, &binancefix.InstrumentListRequest{InstrumentReqID: []byte(id),
				InstrumentListRequestType: binancefix.InstrumentListRequestTypeSingleInstrument, Symbol: []byte(sym)})
		}
	case kindLimits:
		id := c.nextID("LIMITS", "")
		c.query(id, &binancefix.LimitQuery{ReqID: []byte(id)})
	default:
		for _, sym := range c.symbols {
			c.subscribe(sym)
		}
	}
}

// OnLogout tells of a disconnection: binancefix reconnects, unless the
// command ends.
func (c *sbeClient) OnLogout(e binancefix.LogoutEvent) {
	if c.ctx.Err() != nil {
		c.log.LogAttrs(c.ctx, slog.LevelInfo, "logged out")
		return
	}
	c.log.LogAttrs(c.ctx, slog.LevelWarn, "disconnected, reconnecting", slog.Any("reason", e.Err))
}

// nextID returns a new request ID for prefix and symbol, as the FIX path
// makes them.
func (c *sbeClient) nextID(prefix, symbol string) string {
	c.n++
	if symbol == "" {
		return prefix + "_" + strconv.Itoa(c.n)
	}
	return prefix + "_" + symbol + "_" + strconv.Itoa(c.n)
}

// query sends query m, of ID id.
func (c *sbeClient) query(id string, m binancefix.Request) {
	r, err := c.bc.Send(m)
	if err != nil {
		c.finish(fmt.Errorf("sending %s: %w", id, err))
		return
	}
	c.receipts[r] = id
	c.queries[id] = false
}

// subscribe subscribes to symbol's stream: one subscription per symbol,
// as on the FIX path.
func (c *sbeClient) subscribe(symbol string) {
	var id string
	var depth int
	var types []binancefix.MdEntryType
	switch c.cmd.kind {
	case kindTrades:
		id, depth, types = c.nextID("TRADES", symbol), 1, []binancefix.MdEntryType{binancefix.MdEntryTypeTrade}
	case kindTicker:
		id, depth, types = c.nextID("TICKER", symbol), 1, []binancefix.MdEntryType{binancefix.MdEntryTypeBid, binancefix.MdEntryTypeOffer}
	default:
		id, depth, types = c.nextID("DEPTH", symbol), c.levels, []binancefix.MdEntryType{binancefix.MdEntryTypeBid, binancefix.MdEntryTypeOffer}
	}
	m := &binancefix.MarketDataRequest{
		SubscriptionRequestType: binancefix.SubscriptionRequestTypeSubscribe,
		MarketDepth:             binancefix.Some(uint16(depth)),
		AggregatedBook:          binancefix.Some(binancefix.BoolEnumTrue),
		RelatedSym:              []binancefix.MarketDataRequestRelatedSym{{Symbol: []byte(symbol)}},
		MDReqID:                 []byte(id),
	}
	for _, t := range types {
		m.MDEntryTypes = append(m.MDEntryTypes, binancefix.MDEntryTypes{MDEntryType: t})
	}
	if _, err := c.bc.Subscribe(m); err != nil {
		c.log.LogAttrs(c.ctx, slog.LevelError, "subscribing failed", slog.String("symbol", symbol), slog.Any("error", err))
		return
	}
	c.subs[id] = &sub{id: id, symbol: symbol}
	// Binance sends book streams a snapshot on activation, trades nothing:
	// say what to wait for.
	msg := "subscribed, waiting for the snapshot"
	if c.cmd.kind == kindTrades {
		msg = "subscribed, waiting for trades"
	}
	c.log.LogAttrs(c.ctx, slog.LevelInfo, msg, slog.String("symbol", symbol), slog.String("md_req_id", id))
}

// current returns the subscription of in's market data, if it is one of
// the connection's subscriptions of the command: nil for data of none,
// or of a connection gone.
func (c *sbeClient) current(in binancefix.Inbound) *sub {
	if in.Subscription == nil || in.Subscription.Epoch() != c.epoch {
		return nil
	}
	return c.subs[in.Subscription.ID()]
}

// flush hands what was rendered to the output, waiting while its queue is
// full, but never once the command ends nor once binancefix connects no
// more (flushCtx): binancefix's Run waits for the Handler. What was not
// handed over stays, after what was, for the output's end: the events are
// written in order, each once, or not at all.
func (c *sbeClient) flush() {
	if err := c.out.flushContext(c.flushCtx); err != nil && !errors.Is(err, errOutputBlocked) {
		c.finish(fmt.Errorf("writing: %w", err))
	}
}

// reconnect asks for another connection: a book lost updates, and a
// stream is never subscribed again on its connection (binancefix's rule:
// its old and new data could not be told apart). OnLogon subscribes
// everything again; the next snapshot starts the books afresh.
func (c *sbeClient) reconnect(symbol string, err error) {
	if b := c.books[symbol]; b != nil {
		b.synced = false
	}
	if c.resync {
		return
	}
	c.resync = true
	c.log.LogAttrs(c.ctx, slog.LevelWarn, "order book out of sync, reconnecting to subscribe again", slog.String("symbol", symbol), slog.Any("error", err))
	c.bc.Reconnect(c.epoch)
}

// sbeDecimal returns mantissa m of exponent e as a normalized decimal.
func sbeDecimal(m int64, e int8) string { return normDecimal(binancefix.FormatDecimal(m, e)) }

// sbeOptional returns the normalized decimal of v, of exponent e, "" if
// null.
func sbeOptional(v binancefix.Optional[int64], e int8) string {
	if !v.Valid {
		return ""
	}
	return sbeDecimal(v.Value, e)
}

// The market data of a subscription of the command, on the connection, are
// handled by onTrade, onSnapshot, onBookTicker and onDepth.

func (c *sbeClient) OnMarketDataIncrementalTrade(in binancefix.Inbound, m *binancefix.MarketDataIncrementalTrade) {
	if c.current(in) != nil {
		c.onTrade(m)
	}
}

func (c *sbeClient) OnMarketDataSnapshot(in binancefix.Inbound, m *binancefix.MarketDataSnapshot) {
	if c.current(in) != nil {
		c.onSnapshot(in.Received.UTC(), m)
	}
}

func (c *sbeClient) OnMarketDataIncrementalBookTicker(in binancefix.Inbound, m *binancefix.MarketDataIncrementalBookTicker) {
	if c.current(in) != nil {
		c.onBookTicker(in.Received.UTC(), m)
	}
}

func (c *sbeClient) OnMarketDataIncrementalDepth(in binancefix.Inbound, m *binancefix.MarketDataIncrementalDepth) {
	if c.current(in) != nil {
		c.onDepth(in.Received.UTC(), m)
	}
}

func (c *sbeClient) onTrade(m *binancefix.MarketDataIncrementalTrade) {
	if c.cmd.kind != kindTrades {
		return
	}
	t := time.UnixMicro(m.TransactTime).UTC()
	for _, e := range m.MDEntries {
		te := &tradeEvent{Type: "trade", Symbol: string(m.Symbol), Time: t, ID: e.TradeID,
			Price: sbeDecimal(e.MDEntryPx, m.PriceExponent), Qty: sbeDecimal(e.MDEntrySize, m.QtyExponent)}
		switch {
		case e.AggressorSide == binancefix.Some(binancefix.SideBuy):
			te.Aggressor = "buy"
		case e.AggressorSide == binancefix.Some(binancefix.SideSell):
			te.Aggressor = "sell"
		}
		c.out.trade(te)
	}
	c.flush()
}

func (c *sbeClient) onSnapshot(received time.Time, m *binancefix.MarketDataSnapshot) {
	symbol := string(m.Symbol)
	bids := make([]level, 0, len(m.MDEntriesBids))
	for _, e := range m.MDEntriesBids {
		bids = append(bids, level{Price: sbeDecimal(e.MDEntryPx, m.PriceExponent), Qty: sbeDecimal(e.MDEntrySize, m.QtyExponent)})
	}
	asks := make([]level, 0, len(m.MDEntriesAsks))
	for _, e := range m.MDEntriesAsks {
		asks = append(asks, level{Price: sbeDecimal(e.MDEntryPx, m.PriceExponent), Qty: sbeDecimal(e.MDEntrySize, m.QtyExponent)})
	}
	switch c.cmd.kind {
	case kindTicker:
		q := &tickerEvent{Type: "ticker", Symbol: symbol, Time: received, UpdateID: m.LastBookUpdateID.Value}
		if len(bids) > 0 {
			q.Bid = &bids[0]
		}
		if len(asks) > 0 {
			q.Ask = &asks[0]
		}
		c.quotes[symbol] = q
		c.out.ticker(q)
	case kindDepth, kindBook:
		// The book is kept from it: its update ID is required.
		if !m.LastBookUpdateID.Valid {
			c.reconnect(symbol, errors.New("snapshot without LastBookUpdateID"))
			return
		}
		b := c.books[symbol]
		if b == nil {
			b = &book{symbol: symbol}
			c.books[symbol] = b
		}
		last := m.LastBookUpdateID.Value
		b.load(last, bids, asks)
		if c.cmd.kind == kindDepth {
			c.out.book(&bookEvent{Type: "snapshot", Symbol: symbol, Time: received, UpdateID: last, Bids: bids, Asks: asks})
		} else {
			c.drawBook(b, received)
		}
	}
	c.flush()
}

func (c *sbeClient) drawBook(b *book, received time.Time) {
	bids, asks := b.top(c.show)
	c.out.book(&bookEvent{Type: "book", Symbol: b.symbol, Time: received, UpdateID: b.lastID, Bids: bids, Asks: asks})
}

// onBookTicker updates the best bid and offer: an entry replaces its side,
// a missing size empties it.
func (c *sbeClient) onBookTicker(received time.Time, m *binancefix.MarketDataIncrementalBookTicker) {
	if c.cmd.kind != kindTicker {
		return
	}
	symbol := string(m.Symbol)
	q := c.quotes[symbol]
	if q == nil {
		q = &tickerEvent{Type: "ticker", Symbol: symbol}
		c.quotes[symbol] = q
	}
	if m.LastBookUpdateID < q.UpdateID {
		return // older than the quote
	}
	quote := func(px int64, size binancefix.Optional[int64]) *level {
		if !size.Valid {
			return nil
		}
		return &level{Price: sbeDecimal(px, m.PriceExponent), Qty: sbeDecimal(size.Value, m.QtyExponent)}
	}
	for _, e := range m.MDEntriesBids {
		q.Bid = quote(e.MDEntryPx, e.MDEntrySize)
	}
	for _, e := range m.MDEntriesAsks {
		q.Ask = quote(e.MDEntryPx, e.MDEntrySize)
	}
	if len(m.MDEntriesBids)+len(m.MDEntriesAsks) == 0 {
		return
	}
	q.Time, q.UpdateID = received, max(q.UpdateID, m.LastBookUpdateID)
	c.out.ticker(q)
	c.flush()
}

// onDepth applies a depth update to the book kept from the snapshot. SBE
// updates carry no action: Binance's rule infers it, a missing size
// deleting the level, a size changing it if the book has it, adding it if
// not. The depth command writes the update with the actions so inferred;
// the book command, the book's best levels.
func (c *sbeClient) onDepth(received time.Time, m *binancefix.MarketDataIncrementalDepth) {
	if c.cmd.kind != kindDepth && c.cmd.kind != kindBook {
		return
	}
	symbol := string(m.Symbol)
	b := c.books[symbol]
	if b == nil || !b.synced {
		return // waiting for a snapshot
	}
	de := &depthEvent{Type: "depth", Symbol: symbol, Time: received, FirstUpdateID: m.FirstBookUpdateID, LastUpdateID: m.LastBookUpdateID}
	applied := false
	entry := func(bid bool, px int64, size binancefix.Optional[int64]) error {
		ch := change{Side: "ask", Price: sbeDecimal(px, m.PriceExponent)}
		if bid {
			ch.Side = "bid"
		}
		switch {
		case !size.Valid:
			ch.Action = "delete"
		case b.has(bid, ch.Price):
			ch.Action, ch.Qty = "change", sbeDecimal(size.Value, m.QtyExponent)
		default:
			ch.Action, ch.Qty = "new", sbeDecimal(size.Value, m.QtyExponent)
		}
		ok, err := b.apply(m.FirstBookUpdateID, m.LastBookUpdateID, bid, !size.Valid, ch.Price, ch.Qty)
		if ok {
			applied = true
			de.Changes = append(de.Changes, ch)
		}
		return err
	}
	for _, e := range m.MDEntriesBids {
		if err := entry(true, e.MDEntryPx, e.MDEntrySize); err != nil {
			c.reconnect(symbol, err)
			return
		}
	}
	for _, e := range m.MDEntriesAsks {
		if err := entry(false, e.MDEntryPx, e.MDEntrySize); err != nil {
			c.reconnect(symbol, err)
			return
		}
	}
	if !applied {
		return // covered by the snapshot
	}
	if c.cmd.kind == kindDepth {
		c.out.depth(de)
	} else {
		c.drawBook(b, de.Time)
	}
	c.flush()
}

func (c *sbeClient) OnMarketDataRequestReject(in binancefix.Inbound, m *binancefix.MarketDataRequestReject) {
	id := string(m.MDReqID)
	sb := c.subs[id]
	symbol := ""
	if sb != nil {
		symbol = sb.symbol
		delete(c.subs, id)
	}
	var reason string
	switch m.MDReqRejReason {
	case binancefix.Some(binancefix.MdReqRejReasonDuplicateMdReqID):
		reason = "duplicate MDReqID"
	case binancefix.Some(binancefix.MdReqRejReasonTooManySubscriptions):
		reason = "too many subscriptions"
	}
	code := errorCode(m.ErrorCode)
	c.log.LogAttrs(c.ctx, slog.LevelError, "Binance refused a subscription", slog.String("symbol", symbol), slog.String("md_req_id", id),
		slog.String("reason", reason), slog.String("error_code", code), slog.String("text", string(m.Text)))
	if sb != nil && len(c.subs) == 0 {
		c.finish(fmt.Errorf("no subscription left: %s refused by Binance: %s (error code %s)", symbol, m.Text, code))
	}
}

// errorCode returns code as text, "" if null.
func errorCode(code binancefix.Optional[int32]) string {
	if !code.Valid {
		return ""
	}
	return strconv.Itoa(int(code.Value))
}

// OnReject tells of a request Binance could not process: a query fails
// the command, a subscription is gone.
func (c *sbeClient) OnReject(in binancefix.Inbound, m *binancefix.Reject) {
	id := ""
	switch {
	case in.Receipt != nil:
		id = c.receipts[in.Receipt]
	case in.Subscription != nil:
		id = in.Subscription.ID()
	}
	code := errorCode(m.ErrorCode)
	c.log.LogAttrs(c.ctx, slog.LevelError, "Binance rejected a request", slog.Uint64("ref_seq_num", uint64(m.RefSeqNum.Value)), slog.String("request", id),
		slog.String("ref_msg_type", string(m.RefMsgType)), slog.String("error_code", code), slog.String("text", string(m.Text)))
	err := fmt.Errorf("%s rejected by Binance: %s (error code %s)", id, m.Text, code)
	if _, ok := c.queries[id]; ok {
		c.finish(err)
	}
	if c.subs[id] != nil {
		delete(c.subs, id)
		if len(c.subs) == 0 {
			c.finish(fmt.Errorf("no subscription left: %w", err))
		}
	}
}

func (c *sbeClient) OnInstrumentList(in binancefix.Inbound, m *binancefix.InstrumentList) {
	id := string(m.InstrumentReqID)
	if _, ok := c.queries[id]; !ok {
		return
	}
	for _, e := range m.RelatedSym {
		qe, pe := e.QtyExponent, e.PriceExponent
		c.out.instrument(&instrumentEvent{
			Type:          "instrument",
			Symbol:        string(e.Symbol),
			Currency:      string(e.Currency),
			MinQty:        sbeOptional(e.MinTradeVol, qe),
			MaxQty:        sbeOptional(e.MaxTradeVol, qe),
			QtyStep:       sbeOptional(e.MinQtyIncrement, qe),
			MarketMinQty:  sbeOptional(e.MarketMinTradeVol, qe),
			MarketMaxQty:  sbeOptional(e.MarketMaxTradeVol, qe),
			MarketQtyStep: sbeOptional(e.MarketMinQtyIncrement, qe),
			TickSize:      sbeOptional(e.MinPriceIncrement, pe),
			MinPrice:      sbeOptional(e.StartPriceRange, pe),
			MaxPrice:      sbeOptional(e.EndPriceRange, pe),
		})
	}
	c.answered(id)
	c.flush()
}

func (c *sbeClient) OnLimitResponse(in binancefix.Inbound, m *binancefix.LimitResponse) {
	id := string(m.ReqID)
	if _, ok := c.queries[id]; !ok {
		return
	}
	for _, g := range m.LimitIndicators {
		e := &limitEvent{Type: "limit", Count: int64(g.LimitCount), Max: int64(g.LimitMax)}
		switch g.LimitType {
		case binancefix.LimitTypeOrderLimit:
			e.Limit = "orders"
		case binancefix.LimitTypeMessageLimit:
			e.Limit = "messages"
		case binancefix.LimitTypeSubscriptionLimit:
			e.Limit = "subscriptions"
		default:
			e.Limit = "type " + string(rune(g.LimitType))
		}
		if g.LimitResetInterval.Valid {
			e.Interval = strconv.FormatUint(uint64(g.LimitResetInterval.Value), 10)
			if g.LimitResetIntervalResolution.Valid {
				e.Interval += string(rune(g.LimitResetIntervalResolution.Value))
			}
		}
		c.out.limit(e)
	}
	c.answered(id)
	c.flush()
}

// answered records an answer to query id: once every query has one, and
// no more come for queryQuiet, the command completes.
func (c *sbeClient) answered(id string) {
	c.queries[id] = true
	for _, answered := range c.queries {
		if !answered {
			return
		}
	}
	if c.quiet != nil {
		c.quiet.Stop()
	}
	c.quiet = time.AfterFunc(queryQuiet, func() { c.finish(errDone) })
}

// OnNews tells of the server's maintenance: Binance asks clients to
// reconnect, to another server.
func (c *sbeClient) OnNews(e binancefix.NewsEvent) {
	c.log.LogAttrs(c.ctx, slog.LevelWarn, "Binance news", slog.String("headline", e.Headline))
	if c.maintenance || !c.cmd.stream() {
		return
	}
	c.maintenance = true
	c.log.LogAttrs(c.ctx, slog.LevelWarn, "server going into maintenance, reconnecting")
	c.bc.Reconnect(e.Epoch)
}

// traceSBE returns a binancefix.Config.Trace logging every message, field
// by field, as the FIX path's observer does: the Logon's credentials come
// redacted.
func traceSBE(log *slog.Logger) func(binancefix.TraceMessage) {
	return func(t binancefix.TraceMessage) {
		ctx := context.Background()
		if !log.Enabled(ctx, slog.LevelDebug) {
			return
		}
		dir := "SBE in"
		if t.Out {
			dir = "SBE out"
		}
		attrs := []slog.Attr{slog.Uint64("epoch", t.Epoch), slog.Uint64("seq", uint64(t.Header.SeqNum)),
			slog.Int("template_id", int(t.Header.TemplateId))}
		if t.Partial {
			// Written in part only: Binance may have read it, or not.
			attrs = append(attrs, slog.Bool("partly_written", true))
		}
		if t.Message != nil {
			attrs = append(attrs, slog.String("template", reflect.TypeOf(t.Message).Elem().Name()), slog.String("fields", sbeFields(t.Message)))
		} else if t.Body != nil {
			// Never a Logon's: binancefix keeps its credentials.
			attrs = append(attrs, slog.String("body", fmt.Sprintf("%x", t.Body)))
		}
		if t.Err != nil {
			attrs = append(attrs, slog.Any("error", t.Err))
		}
		log.LogAttrs(ctx, slog.LevelDebug, dir, attrs...)
	}
}

// sbeFields writes message m field by field: "Name=value", a group's
// entries' fields as "Group[i].Name=value", null fields left out.
func sbeFields(m any) string {
	var sb strings.Builder
	writeFields(&sb, "", reflect.ValueOf(m).Elem())
	return sb.String()
}

func writeFields(sb *strings.Builder, prefix string, v reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		f, fv := t.Field(i), v.Field(i)
		name := prefix + f.Name
		switch {
		case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.Uint8:
			field(sb, name, string(fv.Bytes()))
		case fv.Kind() == reflect.Slice:
			for j := range fv.Len() {
				writeFields(sb, fmt.Sprintf("%s[%d].", name, j), fv.Index(j))
			}
		case fv.Kind() == reflect.Struct && fv.FieldByName("Valid").IsValid() && fv.FieldByName("Value").IsValid():
			// An Optional: left out when null.
			if fv.FieldByName("Valid").Bool() {
				field(sb, name, fmt.Sprint(fv.FieldByName("Value").Interface()))
			}
		default:
			field(sb, name, fmt.Sprint(fv.Interface()))
		}
	}
}

// field writes name=value, quoting a value of spaces or quotes.
func field(sb *strings.Builder, name, value string) {
	if sb.Len() > 0 {
		sb.WriteByte(' ')
	}
	if strings.ContainsAny(value, " \"=") {
		value = strconv.Quote(value)
	}
	sb.WriteString(name + "=" + value)
}
