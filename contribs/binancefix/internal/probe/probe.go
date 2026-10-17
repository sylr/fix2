// Package probe settles the assumptions the binancefix client makes about
// Binance, Binance documenting them only in part, by checking them against
// the Spot testnet: scripted, bounded exchanges of messages, each judged
// confirmed, contradicted or inconclusive. cmd/binancefix runs them
// (logon, probe), and so do the opt-in testnet tests of binancefix.
//
// Run refuses any endpoint but the testnet's. It places small orders far
// from the market, cancels whatever it placed when done, also on error or
// interrupt, and sends its requests at a pace far below Binance's limits.
package probe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/tcp"
)

// Verdict is what a probe found of an assumption.
type Verdict string

const (
	// Confirmed: Binance answered as the client assumes.
	Confirmed Verdict = "confirmed"
	// Contradicted: Binance answered otherwise; the client is to change.
	Contradicted Verdict = "contradicted"
	// Inconclusive: the case did not arise (a refusal, no answer, the
	// connection lost): it tells nothing of the assumption.
	Inconclusive Verdict = "inconclusive"
)

// Check is the verdict of one case of a probe.
type Check struct {
	Name    string  `json:"name"`
	Verdict Verdict `json:"verdict"`
	Detail  string  `json:"detail"`
}

// Result is what a probe found.
type Result struct {
	Probe      string  `json:"probe"`
	Letter     string  `json:"letter"`
	Assumption string  `json:"assumption"`
	Verdict    Verdict `json:"verdict"`
	Checks     []Check `json:"checks"`
	// Messages are those of the session while the probe ran, its
	// cleanup included.
	Messages []Message `json:"messages"`
}

// combine returns the verdict of checks: contradicted if one is,
// inconclusive if one is (or none was made), confirmed otherwise.
func combine(cs []Check) Verdict {
	v := Confirmed
	if len(cs) == 0 {
		v = Inconclusive
	}
	for _, c := range cs {
		switch c.Verdict {
		case Contradicted:
			return Contradicted
		case Inconclusive:
			v = Inconclusive
		}
	}
	return v
}

// Probe describes a probe.
type Probe struct {
	Name       string
	Letter     string
	Assumption string
	// Priced is set when the probe rests limit orders at
	// Options.PriceFar, which it requires.
	Priced bool
	run    func(context.Context, *session, string) []Check
}

// Probes returns the probes, in the order Run runs them.
func Probes() []Probe { return slices.Clone(catalog) }

// Options configures Logon and Run.
type Options struct {
	// Endpoint is the host:port dialed over TLS, the testnet's
	// (binancefix.TestnetEndpoint) if empty. Run refuses another.
	Endpoint string
	// Dialer, if set, opens the connections instead: a fake Binance in
	// tests. Endpoint is checked all the same.
	Dialer transport.Dialer

	APIKey       string
	PrivateKey   ed25519.PrivateKey
	SenderCompID string
	// HeartBtInt is the heartbeat interval, 10s if 0.
	HeartBtInt time.Duration
	// SignatureTime is the form of the Logon's SendingTime signed.
	SignatureTime binancefix.SignatureTime

	// Symbol is the symbol traded, BTCUSDT if empty.
	Symbol string
	// PriceFar is the price of the limit orders, buying below the
	// market: never filled, within the symbol's price filters (see
	// PriceFarHelp). Required by the probes resting limit orders
	// (Probe.Priced); no default, the market moving.
	PriceFar string
	// Qty is the orders' quantity: small, above the symbol's minimum
	// notional at PriceFar.
	Qty string

	// Interval is the least time between two requests, 200ms if 0.
	Interval time.Duration
	// Wait bounds the wait for an answer, 10s if 0: Binance's own
	// timeout.
	Wait time.Duration
	// Settle is how long late messages are waited for once a request is
	// answered, 1s if 0.
	Settle time.Duration
	// CleanupTimeout bounds the cleanup, 60s if 0: cancelling what was
	// placed, logging on again first if the connection was lost.
	CleanupTimeout time.Duration

	// Sleep waits between Logon attempts (LogonAuto), a timer if nil.
	Sleep func(context.Context, time.Duration) error
	// Logf, if set, tells of progress.
	Logf func(format string, args ...any)
	// Logger logs the client's session events, nothing if nil.
	Logger *slog.Logger
}

// Defaults of Options.
const (
	DefaultHeartBtInt = 10 * time.Second
	DefaultSymbol     = "BTCUSDT"
	// DefaultQty is 0.001 BTC: above BTCUSDT's 5 USDT minimum notional at
	// any price above 5,000.
	DefaultQty      = "0.001"
	DefaultInterval = 200 * time.Millisecond
	DefaultWait     = 10 * time.Second
	DefaultSettle   = time.Second
	DefaultCleanup  = 60 * time.Second
)

// maxRequests bounds the requests of a Run, cleanup included.
const maxRequests = 100

// PriceFarHelp tells how to pick the price of the probes' limit orders.
const PriceFarHelp = `pick it inside both PERCENT_PRICE_BY_SIDE bands of the symbol and below the market, so that the buys rest:
bids from bidMultiplierDown×avg to bidMultiplierUp×avg, asks from askMultiplierDown×avg to askMultiplierUp×avg
(the filters from https://testnet.binance.vision/api/v3/exchangeInfo?symbol=BTCUSDT,
avg from https://testnet.binance.vision/api/v3/avgPrice?symbol=BTCUSDT);
e.g. 70000 for BTCUSDT at about 81,500 on 2026-10-17 (bids 0.5×–1.2×, asks 0.8×–2×)`

// ErrPriceRequired is Run's error when a probe resting limit orders is
// selected without Options.PriceFar.
var ErrPriceRequired = errors.New("probe: the price of the limit orders is required (--price-far): " + PriceFarHelp)

// PriceRequired reports whether the probes named (all if none) need
// Options.PriceFar: those resting limit orders, order, amend,
// cancel-replace, list-cancel and mass-cancel; logon-seqnum, fill (a
// market order) and limits do not.
func PriceRequired(names ...string) bool {
	ps, err := selectProbes(names)
	return err != nil || slices.ContainsFunc(ps, func(p Probe) bool { return p.Priced })
}

// ErrNotTestnet is Run's error for an endpoint other than the testnet's.
var ErrNotTestnet = errors.New("probe: the probes run against the Spot testnet only, " + binancefix.TestnetEndpoint)

// IsTestnet reports whether endpoint, host:port, is the Spot testnet's
// order entry endpoint.
func IsTestnet(endpoint string) bool {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	th, _, _ := net.SplitHostPort(binancefix.TestnetEndpoint)
	return strings.EqualFold(strings.TrimSuffix(host, "."), th)
}

var symbolPattern = regexp.MustCompile(`^[A-Z0-9]{1,20}$`)

func (o Options) withDefaults() Options {
	if o.Endpoint == "" {
		o.Endpoint = binancefix.TestnetEndpoint
	}
	if o.HeartBtInt == 0 {
		o.HeartBtInt = DefaultHeartBtInt
	}
	if o.Symbol == "" {
		o.Symbol = DefaultSymbol
	}
	if o.Qty == "" {
		o.Qty = DefaultQty
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Wait <= 0 {
		o.Wait = DefaultWait
	}
	if o.Settle <= 0 {
		o.Settle = DefaultSettle
	}
	if o.CleanupTimeout <= 0 {
		o.CleanupTimeout = DefaultCleanup
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.SenderCompID == "" {
		o.SenderCompID = RandomID("P", 8)
	}
	return o
}

func (o Options) dialer() transport.Dialer {
	if o.Dialer != nil {
		return o.Dialer
	}
	return &tcp.Dialer{Addresses: []string{o.Endpoint}, TLS: &tls.Config{MinVersion: tls.VersionTLS12}}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RandomID returns prefix followed by random letters and digits, n
// characters in all.
func RandomID(prefix string, n int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := []byte(prefix)
	for len(b) < n {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		b = append(b, alphabet[i.Int64()])
	}
	return string(b)
}

// decimal is a positive decimal number, its mantissa and exponent as SBE
// carries them.
type decimal struct {
	m int64
	e int8
}

// parseDecimal parses s, digits with at most one dot, positive, of at most
// 18 digits.
func parseDecimal(s string) (decimal, error) {
	digits, frac, dot := strings.Cut(s, ".")
	if dot && frac == "" || digits == "" && frac == "" {
		return decimal{}, fmt.Errorf("%q: not a decimal number", s)
	}
	all := strings.TrimLeft(digits+frac, "0")
	if len(all) > 18 || len(frac) > 18 {
		return decimal{}, fmt.Errorf("%q: more than 18 digits", s)
	}
	for _, c := range digits + frac {
		if c < '0' || c > '9' {
			return decimal{}, fmt.Errorf("%q: not a decimal number", s)
		}
	}
	m, _ := strconv.ParseInt("0"+all, 10, 64)
	if m <= 0 {
		return decimal{}, fmt.Errorf("%q: not positive", s)
	}
	return decimal{m: m, e: int8(-len(frac))}, nil
}

func (d decimal) String() string {
	if d.e >= 0 {
		return strconv.FormatInt(d.m, 10) + strings.Repeat("0", int(d.e))
	}
	neg := d.m < 0
	m := d.m
	if neg {
		m = -m
	}
	s := strconv.FormatInt(m, 10)
	n := int(-d.e)
	if len(s) <= n {
		s = strings.Repeat("0", n-len(s)+1) + s
	}
	s = s[:len(s)-n] + "." + s[len(s)-n:]
	if neg {
		s = "-" + s
	}
	return s
}

// half returns d/2, one more digit if need be.
func (d decimal) half() decimal {
	if d.m%2 == 0 {
		return decimal{d.m / 2, d.e}
	}
	if d.m > math.MaxInt64/5 || d.e == math.MinInt8 {
		return decimal{d.m / 2, d.e}
	}
	return decimal{d.m * 5, d.e - 1}
}

// Report is what Run found.
type Report struct {
	Endpoint      string       `json:"endpoint"`
	SenderCompID  string       `json:"sender_comp_id"`
	SignatureTime string       `json:"signature_time"`
	Symbol        string       `json:"symbol"`
	PriceFar      string       `json:"price_far,omitempty"`
	Qty           string       `json:"qty"`
	Logon         *LogonResult `json:"logon,omitempty"`
	Results       []Result     `json:"results"`
	// Cleanup tells of the cancellations of what the probes left open.
	Cleanup []Cleanup `json:"cleanup,omitempty"`
	// Leftover are the orders possibly still open after the cleanup, by
	// ClOrdID: to cancel by hand.
	Leftover []string `json:"leftover,omitempty"`
	// Position is what the market orders bought (or sold) and the cleanup
	// could not trade back, exactly: to trade back by hand.
	Position string `json:"position,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Cleanup is the cancellation of an order a probe placed.
type Cleanup struct {
	Probe   string `json:"probe"`
	ClOrdID string `json:"cl_ord_id"`
	OrderID *int64 `json:"order_id,omitempty"`
	// Outcome is "canceled", "gone" (Binance knows it no more: never
	// placed, or done), "refused", "unanswered" or "not sent".
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// run is what the sessions of a Run share.
type run struct {
	ctx      context.Context // Run's: done, the probes stop
	o        Options
	price    decimal
	qty      decimal
	prefix   string
	n        int
	pace     *pacer
	orders   *tracker
	position *position
}

func newRun(o Options) *run {
	return &run{o: o, prefix: RandomID("p", 7), pace: &pacer{gap: o.Interval}, orders: newTracker(), position: newPosition()}
}

// id returns a new request ID, unique to the run: its prefix, tag, and a
// counter.
func (rn *run) id(tag string) string {
	rn.n++
	return rn.prefix + "-" + tag + strconv.Itoa(rn.n)
}

// Run runs the probes named (all if none), in the order Probes lists them,
// against the testnet, then cancels what they left open. It returns the
// report so far with an error when it could not go on: the Logon refused,
// the connection lost, ctx done.
func Run(ctx context.Context, o Options, names ...string) (*Report, error) {
	o = o.withDefaults()
	rep := &Report{Endpoint: o.Endpoint, SenderCompID: o.SenderCompID, SignatureTime: o.SignatureTime.String(),
		Symbol: o.Symbol, PriceFar: o.PriceFar, Qty: o.Qty, Results: []Result{}}
	fail := func(err error) (*Report, error) {
		rep.Error = err.Error()
		return rep, err
	}
	if !IsTestnet(o.Endpoint) {
		return fail(fmt.Errorf("%w: not %s", ErrNotTestnet, o.Endpoint))
	}
	selected, err := selectProbes(names)
	if err != nil {
		return fail(err)
	}
	rn := newRun(o)
	rn.ctx = ctx
	if o.PriceFar == "" {
		if slices.ContainsFunc(selected, func(p Probe) bool { return p.Priced }) {
			return fail(ErrPriceRequired)
		}
	} else if rn.price, err = parseDecimal(o.PriceFar); err != nil {
		return fail(fmt.Errorf("price: %w", err))
	}
	if rn.qty, err = parseDecimal(o.Qty); err != nil {
		return fail(fmt.Errorf("quantity: %w", err))
	}
	if !symbolPattern.MatchString(o.Symbol) {
		return fail(fmt.Errorf("symbol %q: 1 to 20 capital letters and digits", o.Symbol))
	}

	o.Logf("logging on to %s as %s", o.Endpoint, o.SenderCompID)
	s, lr, err := open(ctx, rn, o.SenderCompID)
	rep.Logon = lr
	if err != nil {
		if slices.ContainsFunc(selected, func(p Probe) bool { return p.Name == "logon-seqnum" }) && lr.LogonAck != nil {
			rep.Results = append(rep.Results, Result{Probe: catalog[0].Name, Letter: catalog[0].Letter, Assumption: catalog[0].Assumption,
				Checks: logonSeqChecks(lr), Messages: lr.Messages})
			rep.Results[0].Verdict = combine(rep.Results[0].Checks)
		}
		return fail(fmt.Errorf("logon: %w", err))
	}
	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), o.CleanupTimeout)
	}
	var stopped error
	for _, p := range selected {
		if err := ctx.Err(); err != nil {
			stopped = fmt.Errorf("interrupted: %w", err)
			break
		}
		if !s.alive() {
			stopped = fmt.Errorf("connection lost: %v", s.endErr())
			break
		}
		o.Logf("probe (%s) %s", p.Letter, p.Name)
		mark := s.rec.mark()
		checks := p.run(ctx, s, p.Name)
		if s.alive() {
			cctx, cancel := cleanupCtx()
			rep.Cleanup = append(rep.Cleanup, s.cancelOpen(cctx, p.Name)...)
			cancel()
		}
		res := Result{Probe: p.Name, Letter: p.Letter, Assumption: p.Assumption, Checks: checks, Verdict: combine(checks), Messages: s.rec.since(mark)}
		if p.Name == catalog[0].Name {
			res.Messages = lr.Messages
		}
		rep.Results = append(rep.Results, res)
	}
	// Lost during the last probe, the connection is a failure all the
	// same, whatever the cleanup does.
	if stopped == nil && !s.alive() {
		stopped = fmt.Errorf("connection lost: %v", s.endErr())
	}
	if stopped == nil && ctx.Err() != nil {
		stopped = fmt.Errorf("interrupted: %w", ctx.Err())
	}

	// Whatever is left open, and what the market orders bought, on the
	// session or, the connection lost, a new one, of another
	// SenderCompID: Binance refuses the first for twice HeartBtInt.
	cctx, cancel := cleanupCtx()
	defer cancel()
	// The position is to trade back while it is not flat, or a market
	// order's fills are not known yet; never while one's are unknown for
	// good: an unseen fill would be traded twice.
	unsettled := func() bool {
		return len(rn.position.unknown()) == 0 && (!rn.position.settled() || len(rn.position.pending()) > 0)
	}
	if unsettled() || len(rn.orders.open("")) > 0 {
		cs := s
		if !s.alive() {
			s.close()
			o.Logf("connection lost: logging on again to clean up")
			var err error
			cs, _, err = open(cctx, rn, otherSender(o.SenderCompID))
			if err != nil {
				rep.Cleanup = append(rep.Cleanup, Cleanup{Outcome: "not sent", Detail: "logon: " + err.Error()})
			}
		}
		// Each attempt resolves the market orders whose fills are not
		// known before trading anything back.
		for i := 0; i < maxFlatten && cs.alive() && cctx.Err() == nil && unsettled(); i++ {
			rep.Cleanup = append(rep.Cleanup, Cleanup{Probe: "fill", Outcome: "trade back", Detail: cs.flatten(cctx, "fill")})
		}
		if cs.alive() {
			rep.Cleanup = append(rep.Cleanup, cs.cancelOpen(cctx, "")...)
		}
		cs.close()
	}
	s.close()
	for _, t := range rn.orders.open("") {
		rep.Leftover = append(rep.Leftover, rn.orders.current(t))
	}
	if !rn.position.settled() || len(rn.position.pending()) > 0 {
		rep.Position = fmt.Sprintf("%s %s", o.Symbol, rn.position)
		if p := rn.position.pending(); len(p) > 0 {
			rep.Position += fmt.Sprintf("; the fills of %s not resolved", strings.Join(p, ", "))
		}
	}
	if stopped != nil {
		return fail(stopped)
	}
	var left []string
	if len(rep.Leftover) > 0 {
		left = append(left, "orders possibly left open: "+strings.Join(rep.Leftover, ", "))
	}
	if rep.Position != "" {
		left = append(left, "position not traded back: "+rep.Position)
	}
	if len(left) > 0 {
		return fail(errors.New(strings.Join(left, "; ")))
	}
	return rep, nil
}

// maxFlatten bounds the attempts of the cleanup to trade the position back.
const maxFlatten = 3

// otherSender returns a SenderCompID other than s.
func otherSender(s string) string {
	c := byte('C')
	if strings.HasSuffix(s, "C") {
		c = 'D'
	}
	if len(s) < 8 {
		return s + string(c)
	}
	return s[:7] + string(c)
}

func selectProbes(names []string) ([]Probe, error) {
	if len(names) == 0 {
		return Probes(), nil
	}
	var ps []Probe
	for _, p := range catalog {
		if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, p.Name) || strings.EqualFold(n, p.Letter) }) {
			ps = append(ps, p)
		}
	}
	for _, n := range names {
		if !slices.ContainsFunc(catalog, func(p Probe) bool { return strings.EqualFold(n, p.Name) || strings.EqualFold(n, p.Letter) }) {
			var known []string
			for _, p := range catalog {
				known = append(known, p.Name)
			}
			return nil, fmt.Errorf("unknown probe %q: %s", n, strings.Join(known, ", "))
		}
	}
	return ps, nil
}
