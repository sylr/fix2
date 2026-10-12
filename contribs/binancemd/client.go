package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/contribs/binancemd/internal/spotmd"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/tcp"
)

// kind is what a command asks for.
type kind int

const (
	kindTrades kind = iota
	kindTicker
	kindDepth
	kindBook
	kindInstruments
	kindLimits
)

type command struct{ kind kind }

// stream reports whether the command streams market data until stopped.
func (c command) stream() bool { return c.kind <= kindBook }

var commands = map[string]command{
	"trades":      {kindTrades},
	"ticker":      {kindTicker},
	"depth":       {kindDepth},
	"book":        {kindBook},
	"instruments": {kindInstruments},
	"limits":      {kindLimits},
}

// beginString is the FIX version of Binance's FIX API.
var beginString = spotmd.Version.BeginString

// initiatorOptions are the initiator's: TLS handshakes with Binance can
// take a while, and dials include them.
var initiatorOptions = session.InitiatorOptions{DialTimeout: 30 * time.Second}

// outputTimeout bounds the wait, once the session stopped, for what was
// received to be written: an output still blocked then fails the command
// with errOutputBlocked.
var outputTimeout = 5 * time.Second

// errDone ends a command that completed.
var errDone = errors.New("done")

const (
	// queryQuiet is how long a query waits for more answers after the last:
	// a long one may come in several messages.
	queryQuiet = 500 * time.Millisecond
	// maxRefusals is how many Logons in a row Binance may refuse, after
	// one succeeded, before the command gives up.
	maxRefusals = 5
)

// stream runs cmd until ctx is done or it completes.
func stream(ctx context.Context, o *options, cmd command, symbols []string, out output, log *slog.Logger) error {
	key, err := loadKey(o.keyFile)
	if err != nil {
		return err
	}
	return streamWith(ctx, o, cmd, symbols, key, out, log, &tcp.Dialer{
		Addresses: []string{o.endpoint},
		TLS:       &tls.Config{MinVersion: tls.VersionTLS12},
	})
}

// streamWith runs cmd over connections from dialer.
func streamWith(ctx context.Context, o *options, cmd command, symbols []string, key ed25519.PrivateKey, out output, log *slog.Logger, dialer transport.Dialer) error {
	sender := o.sender
	if sender == "" {
		sender = randomID(8)
	}
	var dicts session.Dictionaries
	if o.validate {
		dicts.Session = spotmd.Dictionary()
	}
	ctx, finish := context.WithCancelCause(ctx)
	defer finish(nil)
	c := &client{
		ctx:     ctx,
		apiKey:  o.apiKey,
		key:     key,
		cmd:     cmd,
		symbols: symbols,
		levels:  o.levels,
		show:    o.show,
		out:     out,
		log:     log,
		finish:  finish,
	}
	cfg := session.Config{
		ID:           session.SessionID{BeginString: beginString, SenderCompID: sender, TargetCompID: "SPOT"},
		Dictionaries: dicts,
		HeartBtInt:   o.heartbeat,
		// Binance requires ResetSeqNumFlag on every Logon, and resends
		// nothing: every connection starts at 1.
		ResetOnLogon: true,
		Logger:       log,
		Handler:      c,
	}
	if o.trace {
		// Every message, field by field; Username (553) holds the API key.
		cfg.Observer = session.SlogObserver{
			Logger:     log,
			Tags:       append(slices.Clone(session.SensitiveTags), spotmd.TagUsername),
			Dictionary: spotmd.Dictionary(),
		}
	}
	opts := initiatorOptions
	opts.Logger = log
	ini := session.NewInitiator(opts)
	s, err := ini.Add(cfg, dialer)
	if err != nil {
		return err
	}
	// A failed write stops the command, though no event follows to be
	// handed to the output.
	go func() {
		if err := out.failure(ctx); err != nil {
			finish(fmt.Errorf("writing: %w", err))
		}
	}()
	log.LogAttrs(ctx, slog.LevelInfo, "connecting", slog.String("endpoint", o.endpoint), slog.String("sender_comp_id", sender))
	runErr := ini.Run(ctx)
	// What was received is written, even when the command fails, once
	// FromApp is done with the output: it may outlast Run, waiting for a
	// blocked output.
	octx, cancel := context.WithTimeout(context.Background(), outputTimeout)
	defer cancel()
	endErr := errOutputBlocked
	select {
	case <-s.Done():
		endErr = out.end(octx)
	case <-octx.Done():
	}
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

// loadKey reads the Ed25519 private key of PEM file path.
func loadKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("private key %s: no PEM block", path)
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("private key %s: PEM block %q, want an unencrypted PKCS #8 \"PRIVATE KEY\"", path, block.Type)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key %s: %w", path, err)
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key %s: a %T, the FIX API takes Ed25519 keys only", path, k)
	}
	return ek, nil
}

// randomID returns n random letters and digits, n at most 26.
func randomID(n int) string {
	return rand.Text()[:n]
}

// signature returns the RawData (96) of a Logon with header h: the base64
// Ed25519 signature of its MsgType, SenderCompID, TargetCompID, MsgSeqNum
// and SendingTime, separated by SOH.
func signature(key ed25519.PrivateKey, h session.LogonHeader) []byte {
	payload := strings.Join([]string{"A", h.SenderCompID, h.TargetCompID, strconv.FormatUint(h.MsgSeqNum, 10), h.SendingTime}, "\x01")
	return base64.StdEncoding.AppendEncode(nil, ed25519.Sign(key, []byte(payload)))
}

// sub is a market data subscription.
type sub struct {
	id     string // MDReqID
	symbol string
}

// client is the session's handler: it logs on, subscribes, and writes what
// it receives.
type client struct {
	session.BaseHandler
	apiKey  string
	key     ed25519.PrivateKey
	cmd     command
	symbols []string
	levels  int
	show    int
	out     output
	log     *slog.Logger
	ctx     context.Context // done once the command ends
	finish  context.CancelCauseFunc

	mu sync.Mutex
	// loggedOn, connected and refusals follow the Logons FromAdmin sees,
	// in order with the Logouts and Rejects that may follow them: OnLogon
	// runs later, on another goroutine.
	loggedOn    bool              // logged on once at least
	connected   bool              // logged on, on the current connection
	refusals    int               // Logons refused in a row
	maintenance bool              // the server announced maintenance: reconnecting
	n           int               // requests sent, for their IDs
	sent        map[uint64]string // request IDs by MsgSeqNum, for Rejects
	subs        map[string]*sub   // by MDReqID
	quotes      map[string]*tickerEvent
	books       map[string]*book
	queries     map[string]bool // the InstrumentReqIDs or ReqID sent: answered or not
	quiet       *time.Timer     // completes the queries once answers stop
}

var _ session.LogonSigner = (*client)(nil)

// ToAdmin adds the API key and message handling mode to the Logon.
func (c *client) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix2.Builder) error {
	if msgType == "A" {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
		b.SetString(spotmd.TagUsername, c.apiKey).SetString(spotmd.TagMessageHandling, string(spotmd.MessageHandlingSequential))
	}
	return nil
}

// SignLogon signs the Logon with the API key's private key.
func (c *client) SignLogon(_ context.Context, _ *session.Session, h session.LogonHeader, b *fix2.Builder) error {
	b.SetData(spotmd.TagRawDataLength, spotmd.TagRawData, signature(c.key, h))
	return nil
}

// OnLogon sends the command's requests: each connection starts afresh.
func (c *client) OnLogon(ctx context.Context, s *session.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log.LogAttrs(ctx, slog.LevelInfo, "logged on")
	c.maintenance = false
	c.sent = map[uint64]string{}
	c.subs = map[string]*sub{}
	c.quotes = map[string]*tickerEvent{}
	c.books = map[string]*book{}
	c.queries = map[string]bool{}
	switch c.cmd.kind {
	case kindInstruments:
		if len(c.symbols) == 0 {
			id := c.nextID("INSTRUMENTS", "")
			c.queryLocked(ctx, s, id, spotmd.BuildInstrumentListRequest(beginString).SetInstrumentReqID(id).
				SetInstrumentListRequestType(spotmd.InstrumentListRequestTypeAllInstruments))
		}
		for _, sym := range c.symbols {
			id := c.nextID("INSTRUMENT", sym)
			c.queryLocked(ctx, s, id, spotmd.BuildInstrumentListRequest(beginString).SetInstrumentReqID(id).
				SetInstrumentListRequestType(spotmd.InstrumentListRequestTypeSingleInstrument).SetSymbol(sym))
		}
	case kindLimits:
		id := c.nextID("LIMITS", "")
		c.queryLocked(ctx, s, id, spotmd.BuildLimitQuery(beginString).SetReqID(id))
	default:
		for _, sym := range c.symbols {
			c.subscribeLocked(ctx, s, sym)
		}
	}
}

// OnLogout tells of a disconnection: the initiator reconnects, unless the
// command ends.
func (c *client) OnLogout(ctx context.Context, _ *session.Session, reason error) {
	// connected stays set: the answer to our Logout may come after this.
	if c.ctx.Err() != nil {
		c.log.LogAttrs(ctx, slog.LevelInfo, "logged out")
		return
	}
	c.log.LogAttrs(ctx, slog.LevelWarn, "disconnected, reconnecting", slog.Any("reason", reason))
}

// nextID returns a new request ID for prefix and symbol, unique on the
// connection.
func (c *client) nextID(prefix, symbol string) string {
	c.n++
	if symbol == "" {
		return prefix + "_" + strconv.Itoa(c.n)
	}
	return prefix + "_" + symbol + "_" + strconv.Itoa(c.n)
}

// queryLocked sends query m, of ID id.
func (c *client) queryLocked(ctx context.Context, s *session.Session, id string, m session.Outgoing) {
	r, err := s.Send(ctx, m)
	if err != nil {
		c.finish(fmt.Errorf("sending %s: %w", id, err))
		return
	}
	c.sent[r.Seq()] = id
	c.queries[id] = false
}

// subscribeLocked subscribes to symbol's stream.
func (c *client) subscribeLocked(ctx context.Context, s *session.Session, symbol string) {
	var id string
	var depth int
	var types []spotmd.MDEntryType
	switch c.cmd.kind {
	case kindTrades:
		id, depth, types = c.nextID("TRADES", symbol), 1, []spotmd.MDEntryType{spotmd.MDEntryTypeTrade}
	case kindTicker:
		id, depth, types = c.nextID("TICKER", symbol), 1, []spotmd.MDEntryType{spotmd.MDEntryTypeBid, spotmd.MDEntryTypeOffer}
	default:
		id, depth, types = c.nextID("DEPTH", symbol), c.levels, []spotmd.MDEntryType{spotmd.MDEntryTypeBid, spotmd.MDEntryTypeOffer}
	}
	req := request(id, spotmd.SubscriptionRequestTypeSubscribe, depth).
		SetAggregatedBook(spotmd.AggregatedBookBookEntriesToBeAggregated).
		AddNoRelatedSymEntry(spotmd.NewNoRelatedSymEntry().SetSymbol(symbol))
	for _, t := range types {
		req.AddNoMDEntryTypesEntry(spotmd.NewNoMDEntryTypesEntry().SetMDEntryType(t))
	}
	r, err := s.Send(ctx, req)
	if err != nil {
		c.log.LogAttrs(ctx, slog.LevelError, "subscribing failed", slog.String("symbol", symbol), slog.Any("error", err))
		return
	}
	c.sent[r.Seq()] = id
	c.subs[id] = &sub{id: id, symbol: symbol}
	// Binance acknowledges book subscriptions with a snapshot, trades with
	// nothing: say what to wait for.
	msg := "subscribed, waiting for the snapshot"
	if c.cmd.kind == kindTrades {
		msg = "subscribed, waiting for trades"
	}
	c.log.LogAttrs(ctx, slog.LevelInfo, msg, slog.String("symbol", symbol), slog.String("md_req_id", id))
}

// request returns a MarketDataRequest id of type typ at depth.
func request(id string, typ spotmd.SubscriptionRequestType, depth int) *spotmd.MarketDataRequestBuilder {
	return spotmd.BuildMarketDataRequest(beginString).SetMDReqID(id).SetSubscriptionRequestType(typ).SetMarketDepth(depth)
}

// resubscribeLocked replaces subscription sb, whose book has a gap.
func (c *client) resubscribeLocked(ctx context.Context, s *session.Session, sb *sub) {
	delete(c.subs, sb.id)
	if _, err := s.Send(ctx, request(sb.id, spotmd.SubscriptionRequestTypeUnsubscribe, c.levels)); err != nil {
		c.log.LogAttrs(ctx, slog.LevelError, "unsubscribing failed", slog.String("symbol", sb.symbol), slog.Any("error", err))
	}
	c.subscribeLocked(ctx, s, sb.symbol)
}

// FromAdmin follows Logons, and reports Rejects and Logouts.
func (c *client) FromAdmin(ctx context.Context, _ *session.Session, m *fix2.Message, _ session.Inbound) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch string(m.MsgType()) {
	case "A":
		c.loggedOn, c.connected, c.refusals = true, true, 0
	case "3":
		r := spotmd.WrapReject(m)
		ref, _, _ := r.RefSeqNum()
		refMsgType, _ := r.RefMsgType()
		text, _ := r.Text()
		code, _ := r.ErrorCodeBytes()
		if !c.connected && refMsgType == "A" {
			c.refusedLocked(ctx, fmt.Errorf("logon rejected by Binance: %s (error code %s)", text, code))
			return
		}
		id := c.sent[uint64(ref)]
		reason, _ := r.SessionRejectReason()
		refTag, _ := r.RefTagIDBytes()
		c.log.LogAttrs(ctx, slog.LevelError, "Binance rejected a request", slog.Int("ref_seq_num", ref), slog.String("request", id),
			slog.String("ref_msg_type", refMsgType), slog.String("ref_tag_id", string(refTag)), slog.String("reason", string(reason)),
			slog.String("error_code", string(code)), slog.String("text", text))
		err := fmt.Errorf("%s rejected by Binance: %s (error code %s)", id, text, code)
		if _, ok := c.queries[id]; ok {
			c.finish(err)
		}
		if c.subs[id] != nil {
			delete(c.subs, id)
			if len(c.subs) == 0 {
				c.finish(fmt.Errorf("no subscription left: %w", err))
			}
		}
	case "5":
		text, _ := spotmd.WrapLogout(m).Text()
		if !c.connected {
			c.refusedLocked(ctx, fmt.Errorf("logon refused by Binance: %s", text))
			return
		}
		if c.ctx.Err() != nil || c.maintenance {
			return // the answer to ours
		}
		c.log.LogAttrs(ctx, slog.LevelWarn, "Binance logged out", slog.String("text", text))
	}
}

// refusedLocked handles a refused Logon: credentials do not get better by
// retrying, but once logged on, a refusal may be passing.
func (c *client) refusedLocked(ctx context.Context, err error) {
	c.refusals++
	if !c.loggedOn || c.refusals >= maxRefusals {
		c.finish(err)
		return
	}
	c.log.LogAttrs(ctx, slog.LevelError, "logon refused, retrying", slog.Int("refusals", c.refusals), slog.Any("error", err))
}

// FromApp writes what it receives: it renders the events with the lock
// held, and hands them to the output without, so that a blocked output
// stalls FromApp alone, never FromAdmin.
func (c *client) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	c.mu.Lock()
	err := spotmd.Dispatch(m, &dispatcher{c: c, ctx: ctx, s: s, received: in.Received.UTC()})
	c.mu.Unlock()
	if errors.Is(err, fix2.ErrUnsupportedMessageType) {
		// Never rejected: Binance would not care.
		c.log.LogAttrs(ctx, slog.LevelDebug, "message ignored", slog.String("msg_type", string(m.MsgType())))
	}
	if err := c.out.flush(); err != nil {
		c.finish(fmt.Errorf("writing: %w", err))
	}
	return nil
}

// dispatcher handles a received message for c, whose lock it holds.
type dispatcher struct {
	spotmd.UnimplementedHandler
	c        *client
	ctx      context.Context
	s        *session.Session
	received time.Time
}

func (d *dispatcher) OnMarketDataSnapshot(w *spotmd.MarketDataSnapshot) error {
	c := d.c
	id, _ := w.MDReqID()
	if c.subs[id] == nil {
		return nil // of a subscription replaced
	}
	symbol, _ := w.Symbol()
	last, _, _ := w.LastBookUpdateID()
	var bids, asks []level
	for _, e := range w.NoMDEntriesEntries() {
		px, _ := e.MDEntryPxBytes()
		qty, _ := e.MDEntrySizeBytes()
		l := level{Price: normDecimal(string(px)), Qty: normDecimal(string(qty))}
		switch typ, _ := e.MDEntryType(); typ {
		case spotmd.MDEntryTypeBid:
			bids = append(bids, l)
		case spotmd.MDEntryTypeOffer:
			asks = append(asks, l)
		}
	}
	switch c.cmd.kind {
	case kindTicker:
		q := &tickerEvent{Type: "ticker", Symbol: symbol, Time: d.received, UpdateID: int64(last)}
		if len(bids) > 0 {
			q.Bid = &bids[0]
		}
		if len(asks) > 0 {
			q.Ask = &asks[0]
		}
		c.quotes[symbol] = q
		c.out.ticker(q)
	case kindDepth:
		c.out.book(&bookEvent{Type: "snapshot", Symbol: symbol, Time: d.received, UpdateID: int64(last), Bids: bids, Asks: asks})
	case kindBook:
		b := c.books[symbol]
		if b == nil {
			b = &book{symbol: symbol}
			c.books[symbol] = b
		}
		b.load(int64(last), bids, asks)
		c.drawBook(b, d.received)
	}
	return nil
}

// entry is an entry of a MarketDataIncrementalRefresh, with the values it
// takes from the entries before it.
type entry struct {
	action        spotmd.MDUpdateAction
	typ           spotmd.MDEntryType
	price, qty    string
	symbol        string
	first, last   int64
	tradeID       int64
	time          time.Time
	aggressorSide string
}

func (d *dispatcher) OnMarketDataIncrementalRefresh(x *spotmd.MarketDataIncrementalRefresh) error {
	c := d.c
	id, _ := x.MDReqID()
	sb := c.subs[id]
	if sb == nil {
		return nil // of a subscription replaced
	}
	var entries []entry
	prev := entry{symbol: sb.symbol}
	for _, g := range x.NoMDEntriesEntries() {
		// Symbol and the book update IDs default to the previous entry's.
		e := entry{symbol: prev.symbol, first: prev.first, last: prev.last, time: d.received}
		e.action, _ = g.MDUpdateAction()
		e.typ, _ = g.MDEntryType()
		px, _ := g.MDEntryPxBytes()
		e.price = normDecimal(string(px))
		if v, ok := g.MDEntrySizeBytes(); ok {
			e.qty = normDecimal(string(v))
		}
		if v, ok := g.Symbol(); ok {
			e.symbol = v
		}
		if v, ok, err := g.FirstBookUpdateID(); ok && err == nil {
			e.first = int64(v)
		}
		if v, ok, err := g.LastBookUpdateID(); ok && err == nil {
			e.last = int64(v)
		}
		if v, ok, err := g.TradeID(); ok && err == nil {
			e.tradeID = int64(v)
		}
		if t, ok, err := g.TransactTime(); ok && err == nil {
			e.time = t
		}
		switch side, _ := g.AggressorSide(); side {
		case spotmd.AggressorSideBuy:
			e.aggressorSide = "buy"
		case spotmd.AggressorSideSell:
			e.aggressorSide = "sell"
		}
		entries = append(entries, e)
		prev = e
	}
	switch c.cmd.kind {
	case kindTrades:
		for _, e := range entries {
			if e.typ == spotmd.MDEntryTypeTrade {
				c.out.trade(&tradeEvent{Type: "trade", Symbol: e.symbol, Time: e.time, ID: e.tradeID, Price: e.price, Qty: e.qty, Aggressor: e.aggressorSide})
			}
		}
	case kindTicker:
		c.onTicker(entries, d.received)
	case kindDepth:
		var de *depthEvent
		for _, e := range entries {
			if de == nil || de.Symbol != e.symbol || de.FirstUpdateID != e.first || de.LastUpdateID != e.last {
				if de != nil {
					c.out.depth(de)
				}
				de = &depthEvent{Type: "depth", Symbol: e.symbol, Time: d.received, FirstUpdateID: e.first, LastUpdateID: e.last}
			}
			de.Changes = append(de.Changes, change{Side: side(e.typ), Action: action(e.action), Price: e.price, Qty: e.qty})
		}
		if de != nil {
			c.out.depth(de)
		}
	case kindBook:
		var drawn []*book
		for _, e := range entries {
			b := c.books[e.symbol]
			if b == nil || !b.synced {
				continue // waiting for a snapshot
			}
			applied, err := b.apply(e.first, e.last, e.typ == spotmd.MDEntryTypeBid, e.action == spotmd.MDUpdateActionDelete, e.price, e.qty)
			if err != nil {
				c.log.LogAttrs(d.ctx, slog.LevelWarn, "order book out of sync, subscribing again", slog.String("symbol", e.symbol), slog.Any("error", err))
				c.resubscribeLocked(d.ctx, d.s, sb)
				return nil
			}
			if applied && (len(drawn) == 0 || drawn[len(drawn)-1] != b) {
				drawn = append(drawn, b)
			}
		}
		for _, b := range drawn {
			c.drawBook(b, d.received)
		}
	}
	return nil
}

// onTicker updates the best bid and offer of book ticker entries: each
// replaces the quote of its side.
func (c *client) onTicker(entries []entry, received time.Time) {
	var changed []*tickerEvent
	for _, e := range entries {
		q := c.quotes[e.symbol]
		if q == nil {
			q = &tickerEvent{Type: "ticker", Symbol: e.symbol}
			c.quotes[e.symbol] = q
		}
		if e.last != 0 && e.last < q.UpdateID {
			continue // older than the quote
		}
		var l *level
		if e.action != spotmd.MDUpdateActionDelete {
			l = &level{Price: e.price, Qty: e.qty}
		}
		switch e.typ {
		case spotmd.MDEntryTypeBid:
			q.Bid = l
		case spotmd.MDEntryTypeOffer:
			q.Ask = l
		default:
			continue
		}
		q.Time, q.UpdateID = received, max(q.UpdateID, e.last)
		if len(changed) == 0 || changed[len(changed)-1] != q {
			changed = append(changed, q)
		}
	}
	for _, q := range changed {
		c.out.ticker(q)
	}
}

func (c *client) drawBook(b *book, received time.Time) {
	bids, asks := b.top(c.show)
	c.out.book(&bookEvent{Type: "book", Symbol: b.symbol, Time: received, UpdateID: b.lastID, Bids: bids, Asks: asks})
}

func (d *dispatcher) OnMarketDataRequestReject(y *spotmd.MarketDataRequestReject) error {
	c := d.c
	id, _ := y.MDReqID()
	sb := c.subs[id]
	symbol := ""
	if sb != nil {
		symbol = sb.symbol
		delete(c.subs, id)
	}
	var reason string
	switch r, _ := y.MDReqRejReason(); r {
	case spotmd.MDReqRejReasonDuplicateMdreqid:
		reason = "duplicate MDReqID"
	case spotmd.MDReqRejReasonTooManySubscriptions:
		reason = "too many subscriptions"
	}
	text, _ := y.Text()
	code, _ := y.ErrorCodeBytes()
	c.log.LogAttrs(d.ctx, slog.LevelError, "Binance refused a subscription", slog.String("symbol", symbol), slog.String("md_req_id", id),
		slog.String("reason", reason), slog.String("error_code", string(code)), slog.String("text", text))
	if sb != nil && len(c.subs) == 0 {
		c.finish(fmt.Errorf("no subscription left: %s refused by Binance: %s (error code %s)", symbol, text, code))
	}
	return nil
}

func (d *dispatcher) OnInstrumentList(y *spotmd.InstrumentList) error {
	c := d.c
	id, _ := y.InstrumentReqID()
	if _, ok := c.queries[id]; !ok {
		return nil
	}
	for _, g := range y.NoRelatedSymEntries() {
		symbol, _ := g.Symbol()
		currency, _ := g.Currency()
		c.out.instrument(&instrumentEvent{
			Type:          "instrument",
			Symbol:        symbol,
			Currency:      currency,
			MinQty:        decimal(g.MinTradeVolBytes()),
			MaxQty:        decimal(g.MaxTradeVolBytes()),
			QtyStep:       decimal(g.MinQtyIncrementBytes()),
			MarketMinQty:  decimal(g.MarketMinTradeVolBytes()),
			MarketMaxQty:  decimal(g.MarketMaxTradeVolBytes()),
			MarketQtyStep: decimal(g.MarketMinQtyIncrementBytes()),
			TickSize:      decimal(g.MinPriceIncrementBytes()),
			MinPrice:      decimal(g.StartPriceRangeBytes()),
			MaxPrice:      decimal(g.EndPriceRangeBytes()),
		})
	}
	c.answeredLocked(id)
	return nil
}

func (d *dispatcher) OnLimitResponse(r *spotmd.LimitResponse) error {
	c := d.c
	id, _ := r.ReqID()
	if _, ok := c.queries[id]; !ok {
		return nil
	}
	for _, g := range r.NoLimitIndicatorsEntries() {
		e := &limitEvent{Type: "limit"}
		// Binance sends ORDER_LIMIT (1), which its market data dictionary
		// leaves out.
		switch t, _ := g.LimitType(); t {
		case "1":
			e.Limit = "orders"
		case spotmd.LimitTypeMessageLimit:
			e.Limit = "messages"
		case spotmd.LimitTypeSubscriptionLimit:
			e.Limit = "subscriptions"
		default:
			e.Limit = "type " + string(t)
		}
		count, _, _ := g.LimitCount()
		limit, _, _ := g.LimitMax()
		e.Count, e.Max = int64(count), int64(limit)
		if v, ok := g.LimitResetIntervalBytes(); ok {
			unit, _ := g.LimitResetIntervalResolution()
			e.Interval = string(v) + string(unit)
		}
		c.out.limit(e)
	}
	c.answeredLocked(id)
	return nil
}

// answeredLocked records an answer to query id: once every query has one,
// and no more come for queryQuiet, the command completes.
func (c *client) answeredLocked(id string) {
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
func (d *dispatcher) OnNews(n *spotmd.News) error {
	c := d.c
	headline, _ := n.Headline()
	c.log.LogAttrs(d.ctx, slog.LevelWarn, "Binance news", slog.String("headline", headline))
	if c.maintenance || !c.cmd.stream() {
		return nil
	}
	c.maintenance = true
	c.log.LogAttrs(d.ctx, slog.LevelWarn, "server going into maintenance, reconnecting")
	// Logout waits for the connection to end: not from a callback.
	go func() {
		if err := d.s.Logout(context.Background(), "reconnecting for maintenance"); err != nil {
			c.log.LogAttrs(context.Background(), slog.LevelWarn, "logging out for maintenance", slog.Any("error", err))
		}
	}()
	return nil
}

func side(typ spotmd.MDEntryType) string {
	switch typ {
	case spotmd.MDEntryTypeBid:
		return "bid"
	case spotmd.MDEntryTypeOffer:
		return "ask"
	case spotmd.MDEntryTypeTrade:
		return "trade"
	}
	return string(typ)
}

func action(a spotmd.MDUpdateAction) string {
	switch a {
	case spotmd.MDUpdateActionNew:
		return "new"
	case spotmd.MDUpdateActionChange:
		return "change"
	case spotmd.MDUpdateActionDelete:
		return "delete"
	}
	return string(a)
}

// decimal returns the normalized decimal v, "" if absent.
func decimal(v []byte, ok bool) string {
	if !ok {
		return ""
	}
	return normDecimal(string(v))
}
