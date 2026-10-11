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
	"strconv"
	"strings"
	"sync"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary/xmldict"
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

// Binance's FIX tags beyond the standard ones the session knows.
const (
	tagRawDataLength     = 95
	tagRawData           = 96
	tagCurrency          = 15
	tagRefSeqNum         = 45
	tagSymbol            = 55
	tagText              = 58
	tagTransactTime      = 60
	tagHeadline          = 148
	tagNoRelatedSym      = 146
	tagMDReqID           = 262
	tagSubscriptionType  = 263
	tagMarketDepth       = 264
	tagAggregatedBook    = 266
	tagNoMDEntryTypes    = 267
	tagNoMDEntries       = 268
	tagMDEntryType       = 269
	tagMDEntryPx         = 270
	tagMDEntrySize       = 271
	tagMDUpdateAction    = 279
	tagMDReqRejReason    = 281
	tagInstrumentReqID   = 320
	tagRefTagID          = 371
	tagRefMsgType        = 372
	tagRejectReason      = 373
	tagUsername          = 553
	tagListRequestType   = 559
	tagMinTradeVol       = 562
	tagMinPriceIncrement = 969
	tagTradeID           = 1003
	tagMaxTradeVol       = 1140
	tagAggressorSide     = 2446
	tagStartPriceRange   = 2551
	tagEndPriceRange     = 2552
	tagReqID             = 6136
	tagNoLimitIndicators = 25003
	tagLimitType         = 25004
	tagLimitCount        = 25005
	tagLimitMax          = 25006
	tagLimitInterval     = 25007
	tagLimitResolution   = 25008
	tagErrorCode         = 25016
	tagMessageHandling   = 25035
	tagMinQtyIncrement   = 25039
	tagMarketMinTradeVol = 25040
	tagMarketMaxTradeVol = 25041
	tagMarketMinQtyIncr  = 25042
	tagFirstBookUpdateID = 25043
	tagLastBookUpdateID  = 25044
)

// The repeating groups of the messages received.
var (
	snapshotEntries = fix2.GroupSpec{Counter: tagNoMDEntries, Delim: tagMDEntryType,
		Tags: []int{tagMDEntryType, tagMDEntryPx, tagMDEntrySize}}
	incrementalEntries = fix2.GroupSpec{Counter: tagNoMDEntries, Delim: tagMDUpdateAction,
		Tags: []int{tagMDUpdateAction, tagMDEntryPx, tagMDEntrySize, tagMDEntryType, tagSymbol, tagTransactTime,
			tagTradeID, tagAggressorSide, tagFirstBookUpdateID, tagLastBookUpdateID}}
	instruments = fix2.GroupSpec{Counter: tagNoRelatedSym, Delim: tagSymbol,
		Tags: []int{tagSymbol, tagCurrency, tagMinTradeVol, tagMaxTradeVol, tagMinQtyIncrement, tagMarketMinTradeVol,
			tagMarketMaxTradeVol, tagMarketMinQtyIncr, tagMinPriceIncrement, tagStartPriceRange, tagEndPriceRange}}
	limitIndicators = fix2.GroupSpec{Counter: tagNoLimitIndicators, Delim: tagLimitType,
		Tags: []int{tagLimitType, tagLimitCount, tagLimitMax, tagLimitInterval, tagLimitResolution}}
)

// errDone ends a command that completed.
var errDone = errors.New("done")

// queryQuiet is how long a query waits for more answers after the last.
const queryQuiet = 500 * time.Millisecond

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
	if o.dict != "" {
		d, err := xmldict.LoadFile(o.dict)
		if err != nil {
			return err
		}
		dicts.Session = d
	}
	ctx, finish := context.WithCancelCause(ctx)
	defer finish(nil)
	c := &client{
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
		ID:           session.SessionID{BeginString: session.FIX44, SenderCompID: sender, TargetCompID: "SPOT"},
		Dictionaries: dicts,
		HeartBtInt:   o.heartbeat,
		// Binance requires ResetSeqNumFlag on every Logon, and resends
		// nothing: every connection starts at 1.
		ResetOnLogon: true,
		Logger:       log,
		Handler:      c,
	}
	if o.trace {
		cfg.Observer = session.SlogObserver{Logger: log}
	}
	// TLS handshakes with Binance can take a while: dials include them.
	ini := session.NewInitiator(session.InitiatorOptions{Logger: log, DialTimeout: 30 * time.Second})
	if _, err := ini.Add(cfg, dialer); err != nil {
		return err
	}
	log.LogAttrs(ctx, slog.LevelInfo, "connecting", slog.String("endpoint", o.endpoint), slog.String("sender_comp_id", sender))
	ini.Run(ctx)
	if err := context.Cause(ctx); !errors.Is(err, errDone) && !errors.Is(err, context.Canceled) {
		return err
	}
	return out.flush()
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

// randomID returns n random letters and digits.
func randomID(n int) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
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
	finish  context.CancelCauseFunc

	mu          sync.Mutex
	loggedOn    bool              // logged on once at least
	maintenance bool              // the server announced maintenance: reconnecting
	n           int               // requests sent, for their IDs
	sent        map[uint64]string // request IDs by MsgSeqNum, for Rejects
	subs        map[string]*sub   // by MDReqID
	quotes      map[string]*tickerEvent
	books       map[string]*book
	queries     map[string]bool // the InstrumentReqIDs or ReqID not answered yet
	quiet       *time.Timer     // completes the query once answers stop
}

var _ session.LogonSigner = (*client)(nil)

// ToAdmin adds the API key and message handling mode to the Logon.
func (c *client) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix2.Builder) error {
	if msgType == "A" {
		b.SetString(tagUsername, c.apiKey).SetInt(tagMessageHandling, 2) // SEQUENTIAL
	}
	return nil
}

// SignLogon signs the Logon with the API key's private key.
func (c *client) SignLogon(_ context.Context, _ *session.Session, h session.LogonHeader, b *fix2.Builder) error {
	b.SetData(tagRawDataLength, tagRawData, signature(c.key, h))
	return nil
}

// OnLogon sends the command's requests: each connection starts afresh.
func (c *client) OnLogon(ctx context.Context, s *session.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log.LogAttrs(ctx, slog.LevelInfo, "logged on")
	c.loggedOn, c.maintenance = true, false
	c.sent = map[uint64]string{}
	c.subs = map[string]*sub{}
	c.quotes = map[string]*tickerEvent{}
	c.books = map[string]*book{}
	c.queries = map[string]bool{}
	switch c.cmd.kind {
	case kindInstruments:
		if len(c.symbols) == 0 {
			id := c.nextID("INSTRUMENTS", "")
			c.queryLocked(ctx, s, id, fix2.NewBuilder(session.FIX44, "x").SetString(tagInstrumentReqID, id).SetInt(tagListRequestType, 4))
		}
		for _, sym := range c.symbols {
			id := c.nextID("INSTRUMENT", sym)
			c.queryLocked(ctx, s, id, fix2.NewBuilder(session.FIX44, "x").SetString(tagInstrumentReqID, id).
				SetInt(tagListRequestType, 0).SetString(tagSymbol, sym))
		}
	case kindLimits:
		id := c.nextID("LIMITS", "")
		c.queryLocked(ctx, s, id, fix2.NewBuilder(session.FIX44, "XLQ").SetString(tagReqID, id))
	default:
		for _, sym := range c.symbols {
			c.subscribeLocked(ctx, s, sym)
		}
	}
}

// OnLogout tells of a disconnection: the initiator reconnects.
func (c *client) OnLogout(ctx context.Context, _ *session.Session, reason error) {
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

// queryLocked sends query b, of ID id.
func (c *client) queryLocked(ctx context.Context, s *session.Session, id string, b *fix2.Builder) {
	r, err := s.Send(ctx, b)
	if err != nil {
		c.finish(fmt.Errorf("sending %s: %w", id, err))
		return
	}
	c.sent[r.Seq()] = id
	c.queries[id] = true
}

// subscribeLocked subscribes to symbol's stream.
func (c *client) subscribeLocked(ctx context.Context, s *session.Session, symbol string) {
	var b *fix2.Builder
	var id string
	switch c.cmd.kind {
	case kindTrades:
		id = c.nextID("TRADES", symbol)
		b = c.request(id, true, 1).SetInt(tagNoRelatedSym, 1).AppendString(tagSymbol, symbol).
			SetInt(tagNoMDEntryTypes, 1).AppendString(tagMDEntryType, "2")
	case kindTicker:
		id = c.nextID("TICKER", symbol)
		b = c.request(id, true, 1).SetInt(tagNoRelatedSym, 1).AppendString(tagSymbol, symbol).
			SetInt(tagNoMDEntryTypes, 2).AppendString(tagMDEntryType, "0").AppendString(tagMDEntryType, "1")
	default:
		id = c.nextID("DEPTH", symbol)
		b = c.request(id, true, c.levels).SetInt(tagNoRelatedSym, 1).AppendString(tagSymbol, symbol).
			SetInt(tagNoMDEntryTypes, 2).AppendString(tagMDEntryType, "0").AppendString(tagMDEntryType, "1")
	}
	r, err := s.Send(ctx, b)
	if err != nil {
		c.log.LogAttrs(ctx, slog.LevelError, "subscribing failed", slog.String("symbol", symbol), slog.Any("error", err))
		return
	}
	c.sent[r.Seq()] = id
	c.subs[id] = &sub{id: id, symbol: symbol}
	c.log.LogAttrs(ctx, slog.LevelDebug, "subscribing", slog.String("symbol", symbol), slog.String("md_req_id", id))
}

// request returns a MarketDataRequest id subscribing, or unsubscribing,
// at depth.
func (c *client) request(id string, subscribe bool, depth int) *fix2.Builder {
	typ := "1"
	if !subscribe {
		typ = "2"
	}
	b := fix2.NewBuilder(session.FIX44, "V").SetString(tagMDReqID, id).SetString(tagSubscriptionType, typ).SetInt(tagMarketDepth, depth)
	if subscribe {
		b.SetString(tagAggregatedBook, "Y")
	}
	return b
}

// resubscribeLocked replaces subscription sb, whose book has a gap.
func (c *client) resubscribeLocked(ctx context.Context, s *session.Session, sb *sub) {
	delete(c.subs, sb.id)
	if _, err := s.Send(ctx, c.request(sb.id, false, c.levels)); err != nil {
		c.log.LogAttrs(ctx, slog.LevelError, "unsubscribing failed", slog.String("symbol", sb.symbol), slog.Any("error", err))
	}
	c.subscribeLocked(ctx, s, sb.symbol)
}

// FromAdmin reports Rejects and Logouts.
func (c *client) FromAdmin(ctx context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) {
	switch string(m.MsgType()) {
	case "3":
		c.mu.Lock()
		defer c.mu.Unlock()
		ref, _ := fix2.ParseInt(m.Get(tagRefSeqNum))
		id := c.sent[uint64(ref)]
		attrs := []slog.Attr{slog.Int("ref_seq_num", ref), slog.String("request", id),
			slog.String("ref_msg_type", string(m.Get(tagRefMsgType))), slog.String("ref_tag_id", string(m.Get(tagRefTagID))),
			slog.String("reason", string(m.Get(tagRejectReason))), slog.String("error_code", string(m.Get(tagErrorCode))),
			slog.String("text", string(m.Get(tagText)))}
		if !c.loggedOn && string(m.Get(tagRefMsgType)) == "A" {
			// Credentials do not get better by retrying.
			c.finish(fmt.Errorf("Binance rejected the logon: %s (error code %s)", m.Get(tagText), m.Get(tagErrorCode)))
			return
		}
		c.log.LogAttrs(ctx, slog.LevelError, "Binance rejected a request", attrs...)
		err := fmt.Errorf("Binance rejected %s: %s (error code %s)", id, m.Get(tagText), m.Get(tagErrorCode))
		if c.queries[id] {
			c.finish(err)
		}
		if c.subs[id] != nil {
			delete(c.subs, id)
			if len(c.subs) == 0 {
				c.finish(fmt.Errorf("no subscription left: %w", err))
			}
		}
	case "5":
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.loggedOn {
			// Credentials do not get better by retrying.
			c.finish(fmt.Errorf("Binance refused the logon: %s", m.Get(tagText)))
			return
		}
		c.log.LogAttrs(ctx, slog.LevelWarn, "Binance logged out", slog.String("text", string(m.Get(tagText))))
	}
}

// FromApp writes what it receives.
func (c *client) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch string(m.MsgType()) {
	case "W":
		c.onSnapshot(m, in)
	case "X":
		c.onIncremental(ctx, s, m, in)
	case "Y":
		c.onRequestReject(ctx, m)
	case "y":
		c.onInstruments(m)
	case "XLR":
		c.onLimits(m)
	case "B":
		c.onNews(ctx, s, m)
	default:
		c.log.LogAttrs(ctx, slog.LevelDebug, "message ignored", slog.String("msg_type", string(m.MsgType())))
	}
	if err := c.out.flush(); err != nil {
		c.finish(fmt.Errorf("writing: %w", err))
	}
	return nil
}

func (c *client) onSnapshot(m *fix2.Message, in session.Inbound) {
	sb := c.subs[string(m.Get(tagMDReqID))]
	if sb == nil {
		return // of a subscription replaced
	}
	symbol := string(m.Get(tagSymbol))
	last, _ := parseInt64(m.Get(tagLastBookUpdateID))
	var bids, asks []level
	for _, e := range m.GroupsOf(&snapshotEntries) {
		l := level{Price: normDecimal(string(e.Get(tagMDEntryPx))), Qty: normDecimal(string(e.Get(tagMDEntrySize)))}
		switch string(e.Get(tagMDEntryType)) {
		case "0":
			bids = append(bids, l)
		case "1":
			asks = append(asks, l)
		}
	}
	switch c.cmd.kind {
	case kindTicker:
		q := &tickerEvent{Type: "ticker", Symbol: symbol, Time: in.Received.UTC(), UpdateID: last}
		if len(bids) > 0 {
			q.Bid = &bids[0]
		}
		if len(asks) > 0 {
			q.Ask = &asks[0]
		}
		c.quotes[symbol] = q
		c.out.ticker(q)
	case kindDepth:
		c.out.book(&bookEvent{Type: "snapshot", Symbol: symbol, Time: in.Received.UTC(), UpdateID: last, Bids: bids, Asks: asks})
	case kindBook:
		b := c.books[symbol]
		if b == nil {
			b = &book{symbol: symbol}
			c.books[symbol] = b
		}
		b.load(last, bids, asks)
		c.drawBook(b, in.Received)
	}
}

// entry is an entry of a MarketDataIncrementalRefresh, with the values it
// takes from the entries before it.
type entry struct {
	action, typ   string
	price, qty    string
	symbol        string
	first, last   int64
	tradeID       int64
	time          time.Time
	aggressorSide string
}

func (c *client) onIncremental(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) {
	sb := c.subs[string(m.Get(tagMDReqID))]
	if sb == nil {
		return // of a subscription replaced
	}
	received := in.Received.UTC()
	var entries []entry
	prev := entry{symbol: sb.symbol}
	for _, g := range m.GroupsOf(&incrementalEntries) {
		e := entry{
			action: string(g.Get(tagMDUpdateAction)),
			typ:    string(g.Get(tagMDEntryType)),
			price:  normDecimal(string(g.Get(tagMDEntryPx))),
			symbol: prev.symbol, first: prev.first, last: prev.last,
			time: received,
		}
		if v := g.Get(tagMDEntrySize); v != nil {
			e.qty = normDecimal(string(v))
		}
		if v := g.Get(tagSymbol); v != nil {
			e.symbol = string(v)
		}
		if v, err := parseInt64(g.Get(tagFirstBookUpdateID)); err == nil {
			e.first = v
		}
		if v, err := parseInt64(g.Get(tagLastBookUpdateID)); err == nil {
			e.last = v
		}
		e.tradeID, _ = parseInt64(g.Get(tagTradeID))
		if t, err := fix2.ParseUTCTimestamp(g.Get(tagTransactTime)); err == nil {
			e.time = t
		}
		switch string(g.Get(tagAggressorSide)) {
		case "1":
			e.aggressorSide = "buy"
		case "2":
			e.aggressorSide = "sell"
		}
		entries = append(entries, e)
		prev = e
	}
	switch c.cmd.kind {
	case kindTrades:
		for _, e := range entries {
			if e.typ == "2" {
				c.out.trade(&tradeEvent{Type: "trade", Symbol: e.symbol, Time: e.time, ID: e.tradeID, Price: e.price, Qty: e.qty, Aggressor: e.aggressorSide})
			}
		}
	case kindTicker:
		c.onTicker(entries, received)
	case kindDepth:
		var d *depthEvent
		for _, e := range entries {
			if d == nil || d.Symbol != e.symbol || d.FirstUpdateID != e.first || d.LastUpdateID != e.last {
				if d != nil {
					c.out.depth(d)
				}
				d = &depthEvent{Type: "depth", Symbol: e.symbol, Time: received, FirstUpdateID: e.first, LastUpdateID: e.last}
			}
			d.Changes = append(d.Changes, change{Side: side(e.typ), Action: action(e.action), Price: e.price, Qty: e.qty})
		}
		if d != nil {
			c.out.depth(d)
		}
	case kindBook:
		var drawn []*book
		for _, e := range entries {
			b := c.books[e.symbol]
			if b == nil || !b.synced {
				continue // waiting for a snapshot
			}
			applied, err := b.apply(e.first, e.last, e.typ == "0", e.action == "2", e.price, e.qty)
			if err != nil {
				c.log.LogAttrs(ctx, slog.LevelWarn, "order book out of sync, subscribing again", slog.String("symbol", e.symbol), slog.Any("error", err))
				c.resubscribeLocked(ctx, s, sb)
				return
			}
			if applied && (len(drawn) == 0 || drawn[len(drawn)-1] != b) {
				drawn = append(drawn, b)
			}
		}
		for _, b := range drawn {
			c.drawBook(b, received)
		}
	}
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
		if e.action != "2" {
			l = &level{Price: e.price, Qty: e.qty}
		}
		switch e.typ {
		case "0":
			q.Bid = l
		case "1":
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
	c.out.book(&bookEvent{Type: "book", Symbol: b.symbol, Time: received.UTC(), UpdateID: b.lastID, Bids: bids, Asks: asks})
}

func (c *client) onRequestReject(ctx context.Context, m *fix2.Message) {
	id := string(m.Get(tagMDReqID))
	sb := c.subs[id]
	symbol := ""
	if sb != nil {
		symbol = sb.symbol
		delete(c.subs, id)
	}
	reason := map[string]string{"1": "duplicate MDReqID", "2": "too many subscriptions"}[string(m.Get(tagMDReqRejReason))]
	c.log.LogAttrs(ctx, slog.LevelError, "Binance refused a subscription", slog.String("symbol", symbol), slog.String("md_req_id", id),
		slog.String("reason", reason), slog.String("error_code", string(m.Get(tagErrorCode))), slog.String("text", string(m.Get(tagText))))
	if sb != nil && len(c.subs) == 0 {
		c.finish(fmt.Errorf("no subscription left: Binance refused %s: %s (error code %s)", symbol, m.Get(tagText), m.Get(tagErrorCode)))
	}
}

func (c *client) onInstruments(m *fix2.Message) {
	id := string(m.Get(tagInstrumentReqID))
	if !c.queries[id] {
		return
	}
	for _, g := range m.GroupsOf(&instruments) {
		c.out.instrument(&instrumentEvent{
			Type:          "instrument",
			Symbol:        string(g.Get(tagSymbol)),
			Currency:      string(g.Get(tagCurrency)),
			MinQty:        decimalOrEmpty(g.Get(tagMinTradeVol)),
			MaxQty:        decimalOrEmpty(g.Get(tagMaxTradeVol)),
			QtyStep:       decimalOrEmpty(g.Get(tagMinQtyIncrement)),
			MarketMinQty:  decimalOrEmpty(g.Get(tagMarketMinTradeVol)),
			MarketMaxQty:  decimalOrEmpty(g.Get(tagMarketMaxTradeVol)),
			MarketQtyStep: decimalOrEmpty(g.Get(tagMarketMinQtyIncr)),
			TickSize:      decimalOrEmpty(g.Get(tagMinPriceIncrement)),
			MinPrice:      decimalOrEmpty(g.Get(tagStartPriceRange)),
			MaxPrice:      decimalOrEmpty(g.Get(tagEndPriceRange)),
		})
	}
	c.answeredLocked(id)
}

func (c *client) onLimits(m *fix2.Message) {
	id := string(m.Get(tagReqID))
	if !c.queries[id] {
		return
	}
	for _, g := range m.GroupsOf(&limitIndicators) {
		e := &limitEvent{Type: "limit"}
		e.Limit = map[string]string{"1": "orders", "2": "messages", "3": "subscriptions"}[string(g.Get(tagLimitType))]
		if e.Limit == "" {
			e.Limit = "type " + string(g.Get(tagLimitType))
		}
		e.Count, _ = parseInt64(g.Get(tagLimitCount))
		e.Max, _ = parseInt64(g.Get(tagLimitMax))
		if v := g.Get(tagLimitInterval); v != nil {
			e.Interval = string(v) + string(g.Get(tagLimitResolution))
		}
		c.out.limit(e)
	}
	c.answeredLocked(id)
}

// answeredLocked records the answer to query id: once every query is
// answered, and no more answers come for a while, the command completes.
// Long answers may come in several messages.
func (c *client) answeredLocked(id string) {
	delete(c.queries, id)
	if len(c.queries) > 0 {
		return
	}
	if c.quiet != nil {
		c.quiet.Stop()
	}
	c.quiet = time.AfterFunc(queryQuiet, func() { c.finish(errDone) })
}

// onNews tells of the server's maintenance: Binance asks clients to
// reconnect, to another server.
func (c *client) onNews(ctx context.Context, s *session.Session, m *fix2.Message) {
	headline := string(m.Get(tagHeadline))
	c.log.LogAttrs(ctx, slog.LevelWarn, "Binance news", slog.String("headline", headline))
	if c.maintenance || !c.cmd.stream() {
		return
	}
	c.maintenance = true
	c.log.LogAttrs(ctx, slog.LevelWarn, "server going into maintenance, reconnecting")
	// Logout waits for the connection to end: not from a callback.
	go s.Logout(context.Background(), "reconnecting for maintenance")
}

func side(typ string) string {
	switch typ {
	case "0":
		return "bid"
	case "1":
		return "ask"
	case "2":
		return "trade"
	}
	return typ
}

func action(a string) string {
	switch a {
	case "0":
		return "new"
	case "1":
		return "change"
	case "2":
		return "delete"
	}
	return a
}

func decimalOrEmpty(v []byte) string {
	if v == nil {
		return ""
	}
	return normDecimal(string(v))
}

func parseInt64(v []byte) (int64, error) {
	return strconv.ParseInt(string(v), 10, 64)
}
