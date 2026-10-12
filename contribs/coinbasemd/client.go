package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/contribs/coinbasemd/internal/fixt"
	"github.com/sylr/fix2/contribs/coinbasemd/internal/md"
	"github.com/sylr/fix2/dictionary"
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
)

type command struct{ kind kind }

var commands = map[string]command{
	"trades":      {kindTrades},
	"ticker":      {kindTicker},
	"depth":       {kindDepth},
	"book":        {kindBook},
	"instruments": {kindInstruments},
}

const (
	// targetCompID is Coinbase's CompID.
	targetCompID = "Coinbase"
	// applVerID is the application version Coinbase speaks: FIX 5.0 SP2,
	// though its dictionary declares FIX 5.0.
	applVerID = "9"
	// maxRefusals is how many Logons in a row Coinbase may refuse, after
	// one succeeded, before the command gives up.
	maxRefusals = 5
	// bookLevels is the depth of L2 subscriptions: Coinbase's 10 best
	// levels of each side.
	bookLevels = 10
	// tagRawDataLength is RawDataLength, which Coinbase's Logon requires
	// though its dictionary leaves it out.
	tagRawDataLength = 95
)

// errDone ends a command that completed.
var errDone = errors.New("done")

// initiatorOptions are the initiator's: dials include the TLS handshake.
var initiatorOptions = session.InitiatorOptions{DialTimeout: 30 * time.Second}

// outputTimeout bounds the wait, once the session stopped, for what was
// received to be written: an output still blocked then fails the command
// with errOutputBlocked.
var outputTimeout = 5 * time.Second

// stream runs cmd until ctx is done or it completes.
func stream(ctx context.Context, o *options, cmd command, symbols []string, out output, log *slog.Logger) error {
	cfg, err := tlsConfig(o.caFile, log)
	if err != nil {
		return err
	}
	return streamWith(ctx, o, cmd, symbols, out, log, &tcp.Dialer{Addresses: []string{o.endpoint}, TLS: cfg})
}

// macOSRoots is macOS's bundle of the roots it trusts.
const macOSRoots = "/etc/ssl/cert.pem"

// tlsConfig returns the TLS configuration of connections to Coinbase, which
// accepts TLS 1.2 only. With caFile, certificates are verified against its
// roots by Go's own verifier. So they are on macOS by default, against its
// roots: its verifier rejects Coinbase's FIX certificates, which carry no
// Certificate Transparency timestamps ("certificate is not standards
// compliant"). Go's verifier still checks the chain and the host name.
func tlsConfig(caFile string, log *slog.Logger) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" && runtime.GOOS == "darwin" {
		if _, err := os.Stat(macOSRoots); err == nil {
			caFile = macOSRoots
			log.LogAttrs(context.Background(), slog.LevelInfo, "verifying Coinbase's certificate with Go's verifier: macOS's rejects it, as it lacks Certificate Transparency timestamps",
				slog.String("roots", caFile))
		}
	}
	if caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("--ca-file: %w", err)
	}
	cfg.RootCAs = x509.NewCertPool()
	if !cfg.RootCAs.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("--ca-file: no PEM certificate in %s", caFile)
	}
	return cfg, nil
}

// streamWith runs cmd over connections from dialer.
func streamWith(ctx context.Context, o *options, cmd command, symbols []string, out output, log *slog.Logger, dialer transport.Dialer) error {
	key, err := base64.StdEncoding.DecodeString(o.apiSecret.value)
	if err != nil {
		return fmt.Errorf("API secret: not base64: %w", err)
	}
	dicts := session.Dictionaries{DefaultApplVerID: applVerID}
	if o.validate {
		dicts.Session = fixt.Dictionary()
		dicts.Apps = map[string]*dictionary.Dict{applVerID: md.Dictionary()}
	}
	ctx, finish := context.WithCancelCause(ctx)
	defer finish(nil)
	c := &client{
		apiKey:     o.apiKey.value,
		secret:     key,
		passphrase: o.passphrase.value,
		cmd:        cmd,
		symbols:    symbols,
		show:       o.show,
		out:        out,
		log:        log,
		ctx:        ctx,
		finish:     finish,
	}
	cfg := session.Config{
		// The API key is the SenderCompID.
		ID:           session.SessionID{BeginString: session.FIXT11, SenderCompID: o.apiKey.value, TargetCompID: targetCompID},
		Dictionaries: dicts,
		HeartBtInt:   o.heartbeat,
		// Every connection starts afresh: Coinbase resends nothing.
		ResetOnLogon: true,
		// SendingTime has exactly milliseconds: the Logon's signature
		// covers it as written.
		Timestamps: session.MillisecondTimestamps,
		Logger:     log,
		Handler:    c,
	}
	if o.trace {
		// Every message, field by field; Password (554) and the signature
		// are redacted.
		cfg.Observer = session.SlogObserver{Logger: log, Dictionary: dictionary.Compose(fixt.Dictionary(), md.Dictionary())}
	}
	opts := initiatorOptions
	opts.Logger = log
	ini := session.NewInitiator(opts)
	s, err := ini.Add(cfg, dialer)
	if err != nil {
		return err
	}
	log.LogAttrs(ctx, slog.LevelInfo, "connecting", slog.String("endpoint", o.endpoint))
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
		return errors.Join(err, endErr)
	}
	return endErr
}

// signature returns the RawData (96) of a Logon with header h: the base64
// HMAC-SHA256, keyed with the API secret, of its SendingTime, MsgType,
// MsgSeqNum, SenderCompID, TargetCompID and the passphrase, separated by
// SOH.
func signature(secret []byte, passphrase string, h session.LogonHeader) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strings.Join([]string{h.SendingTime, "A", strconv.FormatUint(h.MsgSeqNum, 10), h.SenderCompID, h.TargetCompID, passphrase}, "\x01")))
	return base64.StdEncoding.AppendEncode(nil, mac.Sum(nil))
}

// sub is a market data subscription.
type sub struct {
	id     string // MDReqID
	symbol string
	// bids and asks gather the fragments of a snapshot.
	bids, asks []level
}

// client is the session's handler: it logs on, subscribes, and writes what
// it receives.
type client struct {
	session.BaseHandler
	apiKey     string
	secret     []byte
	passphrase string
	cmd        command
	symbols    []string
	show       int
	out        output
	log        *slog.Logger
	ctx        context.Context // done once the command ends
	finish     context.CancelCauseFunc

	mu sync.Mutex
	// loggedOn, connected and refusals follow the Logons FromAdmin sees,
	// in order with the Logouts and Rejects that may follow them: OnLogon
	// runs later, on another goroutine.
	loggedOn  bool              // logged on once at least
	connected bool              // logged on, on the current connection
	refusals  int               // Logons refused in a row
	n         int               // requests sent, for their IDs
	sent      map[uint64]string // request IDs by MsgSeqNum, for Rejects
	subs      map[string]*sub   // by MDReqID
	quotes    map[string]*tickerEvent
	books     map[string]*book
	query     string          // the SecurityReqID of the instruments asked for
	listed    map[string]bool // the symbols listed
}

var _ session.LogonSigner = (*client)(nil)

// ToAdmin adds the API key and passphrase to the Logon.
func (c *client) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix2.Builder) error {
	if msgType == "A" {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
		b.SetString(fixt.TagUsername, c.apiKey).SetString(fixt.TagPassword, c.passphrase)
	}
	return nil
}

// SignLogon signs the Logon with the API secret.
func (c *client) SignLogon(_ context.Context, _ *session.Session, h session.LogonHeader, b *fix2.Builder) error {
	b.SetData(tagRawDataLength, fixt.TagRawData, signature(c.secret, c.passphrase, h))
	return nil
}

// OnLogon sends the command's requests: each connection starts afresh.
func (c *client) OnLogon(ctx context.Context, s *session.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log.LogAttrs(ctx, slog.LevelInfo, "logged on")
	c.sent = map[uint64]string{}
	c.subs = map[string]*sub{}
	c.quotes = map[string]*tickerEvent{}
	c.books = map[string]*book{}
	c.listed = map[string]bool{}
	if c.cmd.kind == kindInstruments {
		c.query = c.nextID("INSTRUMENTS", "")
		r, err := s.Send(ctx, md.BuildSecurityListRequest(session.FIXT11).SetSecurityReqID(c.query).
			SetSecurityListRequestType(md.SecurityListRequestTypeAllSecurities))
		if err != nil {
			c.finish(fmt.Errorf("asking for instruments: %w", err))
			return
		}
		c.sent[r.Seq()] = c.query
		return
	}
	for _, sym := range c.symbols {
		c.subscribeLocked(ctx, s, sym)
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

// depth returns the MarketDepth of the command's subscriptions: L1, the best
// bid and offer, for trades and tickers, L2 for books.
func (c *client) depth() int {
	if c.cmd.kind == kindDepth || c.cmd.kind == kindBook {
		return bookLevels
	}
	return 1
}

// subscribeLocked subscribes to symbol.
func (c *client) subscribeLocked(ctx context.Context, s *session.Session, symbol string) {
	prefix := "DEPTH"
	switch c.cmd.kind {
	case kindTrades:
		prefix = "TRADES"
	case kindTicker:
		prefix = "TICKER"
	}
	id := c.nextID(prefix, symbol)
	req := md.BuildMarketDataRequest(session.FIXT11).SetMDReqID(id).
		SetSubscriptionRequestType(md.SubscriptionRequestTypeSnapshotPlusUpdates).SetMarketDepth(c.depth()).
		AddNoRelatedSymEntry(md.NewNoRelatedSymEntry().SetSymbol(symbol))
	r, err := s.Send(ctx, req)
	if err != nil {
		c.log.LogAttrs(ctx, slog.LevelError, "subscribing failed", slog.String("symbol", symbol), slog.Any("error", err))
		return
	}
	c.sent[r.Seq()] = id
	c.subs[id] = &sub{id: id, symbol: symbol}
	msg := "subscribed, waiting for the snapshot"
	if c.cmd.kind == kindTrades {
		msg = "subscribed, waiting for trades"
	}
	c.log.LogAttrs(ctx, slog.LevelInfo, msg, slog.String("symbol", symbol), slog.String("md_req_id", id))
}

// FromAdmin follows Logons, and reports Rejects and Logouts.
func (c *client) FromAdmin(ctx context.Context, _ *session.Session, m *fix2.Message, _ session.Inbound) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch string(m.MsgType()) {
	case "A":
		c.loggedOn, c.connected, c.refusals = true, true, 0
	case "3":
		r := fixt.WrapReject(m)
		ref, _, _ := r.RefSeqNum()
		refMsgType, _ := r.RefMsgType()
		text, _ := r.Text()
		if !c.connected && refMsgType == "A" {
			c.refusedLocked(ctx, fmt.Errorf("logon rejected by Coinbase: %s", text))
			return
		}
		reason, _ := r.SessionRejectReason()
		c.rejectedLocked(ctx, c.sent[uint64(ref)], refMsgType, string(reason), text)
	case "5":
		text, _ := fixt.WrapLogout(m).Text()
		if !c.connected {
			c.refusedLocked(ctx, fmt.Errorf("logon refused by Coinbase: %s", text))
			return
		}
		if c.ctx.Err() != nil {
			return // the answer to ours
		}
		c.log.LogAttrs(ctx, slog.LevelWarn, "Coinbase logged out", slog.String("text", text))
	}
}

// rejectedLocked handles the rejection of request id.
func (c *client) rejectedLocked(ctx context.Context, id, refMsgType, reason, text string) {
	c.log.LogAttrs(ctx, slog.LevelError, "Coinbase rejected a request", slog.String("request", id),
		slog.String("ref_msg_type", refMsgType), slog.String("reason", reason), slog.String("text", text))
	err := fmt.Errorf("%s rejected by Coinbase: %s", id, text)
	if id != "" && id == c.query {
		c.finish(err)
	}
	if c.subs[id] != nil {
		delete(c.subs, id)
		if len(c.subs) == 0 {
			c.finish(fmt.Errorf("no subscription left: %w", err))
		}
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
	err := md.Dispatch(m, &dispatcher{c: c, ctx: ctx, s: s, received: in.Received.UTC()})
	c.mu.Unlock()
	if errors.Is(err, fix2.ErrUnsupportedMessageType) {
		// Never rejected: Coinbase would not care.
		c.log.LogAttrs(ctx, slog.LevelDebug, "message ignored", slog.String("msg_type", string(m.MsgType())))
	}
	if err := c.out.flush(); err != nil {
		c.finish(fmt.Errorf("writing: %w", err))
	}
	return nil
}

// dispatcher handles a received message for c, whose lock it holds.
type dispatcher struct {
	md.UnimplementedHandler
	c        *client
	ctx      context.Context
	s        *session.Session
	received time.Time
}

func (d *dispatcher) OnMarketDataSnapshotFullRefresh(w *md.MarketDataSnapshotFullRefresh) error {
	c := d.c
	id, _ := w.MDReqID()
	sb := c.subs[id]
	if sb == nil {
		return nil
	}
	for _, e := range w.NoMDEntriesEntries() {
		px, _ := e.MDEntryPx()
		qty, _ := e.MDEntrySize()
		l := level{Price: normDecimal(px), Qty: normDecimal(qty)}
		switch typ, _ := e.MDEntryType(); typ {
		case md.MDEntryTypeBid:
			sb.bids = append(sb.bids, l)
		case md.MDEntryTypeOffer:
			sb.asks = append(sb.asks, l)
		}
	}
	// A snapshot may come in fragments: it is complete with the last.
	if last, ok := w.LastFragment(); ok && last == md.LastFragmentNo {
		return nil
	}
	bids, asks := sb.bids, sb.asks
	sb.bids, sb.asks = nil, nil
	// Coinbase sends levels best first; sorting makes it certain.
	slices.SortStableFunc(bids, func(a, b level) int { return cmpDecimal(b.Price, a.Price) })
	slices.SortStableFunc(asks, func(a, b level) int { return cmpDecimal(a.Price, b.Price) })
	symbol := sb.symbol
	if v, ok := w.Symbol(); ok {
		symbol = v
	}
	switch c.cmd.kind {
	case kindTicker:
		q := &tickerEvent{Type: "ticker", Symbol: symbol, Time: d.received}
		if len(bids) > 0 {
			q.Bid = &bids[0]
		}
		if len(asks) > 0 {
			q.Ask = &asks[0]
		}
		c.quotes[symbol] = q
		c.out.ticker(q)
	case kindDepth:
		c.out.book(&bookEvent{Type: "snapshot", Symbol: symbol, Time: d.received, Bids: bids, Asks: asks})
	case kindBook:
		b := c.books[symbol]
		if b == nil {
			b = &book{symbol: symbol}
			c.books[symbol] = b
		}
		b.load(bids, asks)
		c.drawBook(b, d.received)
	}
	return nil
}

// entry is an entry of a MarketDataIncrementalRefresh.
type entry struct {
	action     md.MDUpdateAction
	typ        md.MDEntryType
	price, qty string
	symbol     string
	tradeID    string
	time       time.Time
	aggressor  string
}

func (d *dispatcher) OnMarketDataIncrementalRefresh(x *md.MarketDataIncrementalRefresh) error {
	c := d.c
	id, _ := x.MDReqID()
	sb := c.subs[id]
	if sb == nil {
		return nil
	}
	var entries []entry
	for _, g := range x.NoMDEntriesEntries() {
		e := entry{symbol: sb.symbol, time: d.received}
		e.action, _ = g.MDUpdateAction()
		e.typ, _ = g.MDEntryType()
		if v, ok := g.MDEntryPx(); ok {
			e.price = normDecimal(v)
		}
		if v, ok := g.MDEntrySize(); ok {
			e.qty = normDecimal(v)
		}
		if v, ok := g.Symbol(); ok {
			e.symbol = v
		}
		if t, ok, err := g.TransactTime(); ok && err == nil {
			e.time = t
		}
		// Trades carry a TradeID, else the aggressive order's ID.
		if v, ok := g.TradeID(); ok {
			e.tradeID = v
		} else if v, ok := g.OrderID(); ok {
			e.tradeID = v
		}
		switch side, _ := g.AggressorSide(); side {
		case md.AggressorSideBuy:
			e.aggressor = "buy"
		case md.AggressorSideSell:
			e.aggressor = "sell"
		}
		entries = append(entries, e)
	}
	switch c.cmd.kind {
	case kindTrades:
		for _, e := range entries {
			if e.typ == md.MDEntryTypeTrade {
				c.out.trade(&tradeEvent{Type: "trade", Symbol: e.symbol, Time: e.time, ID: e.tradeID, Price: e.price, Qty: e.qty, Aggressor: e.aggressor})
			}
		}
	case kindTicker:
		c.onTicker(entries, d.received)
	case kindDepth:
		for _, e := range entries {
			if e.typ != md.MDEntryTypeBid && e.typ != md.MDEntryTypeOffer {
				continue
			}
			c.out.depth(&depthEvent{Type: "depth", Symbol: e.symbol, Time: d.received,
				Changes: []change{{Side: side(e.typ), Action: action(e.action), Price: e.price, Qty: e.qty}}})
		}
	case kindBook:
		var drawn []*book
		for _, e := range entries {
			b := c.books[e.symbol]
			if b == nil || !b.synced || e.typ != md.MDEntryTypeBid && e.typ != md.MDEntryTypeOffer {
				continue
			}
			qty := e.qty
			if e.action == md.MDUpdateActionDelete {
				qty = "0"
			}
			b.set(e.typ == md.MDEntryTypeBid, e.price, qty)
			if len(drawn) == 0 || drawn[len(drawn)-1] != b {
				drawn = append(drawn, b)
			}
		}
		for _, b := range drawn {
			c.drawBook(b, d.received)
		}
	}
	return nil
}

// onTicker updates the best bid and offer: each entry replaces the quote
// of its side.
func (c *client) onTicker(entries []entry, received time.Time) {
	var changed []*tickerEvent
	for _, e := range entries {
		q := c.quotes[e.symbol]
		if q == nil {
			q = &tickerEvent{Type: "ticker", Symbol: e.symbol}
			c.quotes[e.symbol] = q
		}
		var l *level
		if e.action != md.MDUpdateActionDelete {
			l = &level{Price: e.price, Qty: e.qty}
		}
		switch e.typ {
		case md.MDEntryTypeBid:
			q.Bid = l
		case md.MDEntryTypeOffer:
			q.Ask = l
		default:
			continue
		}
		q.Time = received
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
	c.out.book(&bookEvent{Type: "book", Symbol: b.symbol, Time: received, Bids: bids, Asks: asks})
}

func (d *dispatcher) OnMarketDataRequestReject(y *md.MarketDataRequestReject) error {
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
	case md.MDReqRejReasonUnknownSymbol:
		reason = "unknown symbol"
	case md.MDReqRejReasonDuplicateMdreqid:
		reason = "duplicate MDReqID"
	case md.MDReqRejReasonUnsupportedMarketdepth:
		reason = "unsupported market depth"
	default:
		reason = "other"
	}
	text, _ := y.Text()
	c.log.LogAttrs(d.ctx, slog.LevelError, "Coinbase refused a subscription", slog.String("symbol", symbol), slog.String("md_req_id", id),
		slog.String("reason", reason), slog.String("text", text))
	if sb != nil && len(c.subs) == 0 {
		c.finish(fmt.Errorf("no subscription left: %s refused by Coinbase: %s (%s)", symbol, reason, text))
	}
	return nil
}

func (d *dispatcher) OnBusinessMessageReject(j *md.BusinessMessageReject) error {
	ref, _, _ := j.RefSeqNum()
	refMsgType, _ := j.RefMsgType()
	reason, _ := j.BusinessRejectReason()
	text, _ := j.Text()
	d.c.rejectedLocked(d.ctx, d.c.sent[uint64(ref)], refMsgType, string(reason), text)
	return nil
}

func (d *dispatcher) OnSecurityList(y *md.SecurityList) error {
	c := d.c
	id, _ := y.SecurityReqID()
	if id != c.query {
		return nil
	}
	if r, _ := y.SecurityRequestResult(); r == md.SecurityRequestResultInvalidOrUnsupportedRequest {
		c.finish(errors.New("instruments: request refused by Coinbase"))
		return nil
	}
	for _, g := range y.NoRelatedSymEntries() {
		symbol, _ := g.Symbol()
		if len(c.symbols) > 0 && !slices.Contains(c.symbols, symbol) {
			continue
		}
		e := &instrumentEvent{Type: "instrument", Symbol: symbol}
		e.Currency, _ = g.Currency()
		if v, ok := g.MinTradeVol(); ok {
			e.MinNotional = normDecimal(v)
		}
		if v, ok := g.MinPriceIncrement(); ok {
			e.TickSize = normDecimal(v)
		}
		if v, ok := g.MinSizeIncrement(); ok {
			e.SizeStep = normDecimal(v)
		}
		e.Status, _ = g.MDSecurityTradingStatus()
		c.out.instrument(e)
		c.listed[symbol] = true
	}
	// The list may come in fragments: it is complete with the last.
	if last, ok := y.LastFragment(); !ok || last == md.LastFragmentYes {
		var missing []string
		for _, s := range c.symbols {
			if !c.listed[s] {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			c.finish(fmt.Errorf("not listed by Coinbase: %s", strings.Join(missing, ", ")))
			return nil
		}
		c.finish(errDone)
	}
	return nil
}

// OnSecurityStatus tells of an instrument's trading status changing.
func (d *dispatcher) OnSecurityStatus(f *md.SecurityStatus) error {
	symbol, _ := f.Symbol()
	status, _ := f.MDSecurityTradingStatus()
	d.c.log.LogAttrs(d.ctx, slog.LevelInfo, "trading status", slog.String("symbol", symbol), slog.String("status", status))
	return nil
}

func side(typ md.MDEntryType) string {
	switch typ {
	case md.MDEntryTypeBid:
		return "bid"
	case md.MDEntryTypeOffer:
		return "ask"
	case md.MDEntryTypeTrade:
		return "trade"
	}
	return string(typ)
}

func action(a md.MDUpdateAction) string {
	switch a {
	case md.MDUpdateActionNew:
		return "new"
	case md.MDUpdateActionChange:
		return "change"
	case md.MDUpdateActionDelete:
		return "delete"
	}
	return string(a)
}
