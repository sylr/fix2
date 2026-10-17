package binancefix

import (
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/tcp"
)

// The order entry endpoints of the FIX SBE API: port 9002 takes SBE
// requests and answers in SBE, over TLS.
const (
	Endpoint        = "fix-oe.binance.com:9002"
	TestnetEndpoint = "fix-oe.testnet.binance.vision:9002"
)

// The market data endpoints of the FIX SBE API, on port 9002 as well.
const (
	MarketDataEndpoint        = "fix-md.binance.com:9002"
	TestnetMarketDataEndpoint = "fix-md.testnet.binance.vision:9002"
)

// TargetCompID is the TargetCompID of every client Logon.
const TargetCompID = "SPOT"

// Kind is what a session is for: Binance serves order entry and market
// data on endpoints of their own, with limits of their own.
type Kind uint8

const (
	// OrderEntry sessions place orders and query limits (Send).
	OrderEntry Kind = iota
	// MarketData sessions subscribe to market data (Subscribe), list
	// instruments and query limits (Send). They place no orders.
	MarketData
	numKinds
)

func (k Kind) String() string {
	switch k {
	case OrderEntry:
		return "order entry"
	case MarketData:
		return "market data"
	}
	return "invalid"
}

// The bounds Binance sets.
const (
	// MinHeartBtInt and MaxHeartBtInt bound the heartbeat interval.
	MinHeartBtInt = 5 * time.Second
	MaxHeartBtInt = 60 * time.Second
	// MaxRecvWindow bounds RecvWindow.
	MaxRecvWindow = 60 * time.Second
	// MarketDataMessageLimit messages at most may reach Binance in any
	// MarketDataMessageWindow on a market data session, ours of every
	// kind counted: Logon, Heartbeats, TestRequests, requests, Logout.
	MarketDataMessageLimit  = 2000
	MarketDataMessageWindow = 60 * time.Second
	// MaxStreams bounds the streams of a market data connection: beyond,
	// Binance refuses a subscription with a MarketDataRequestReject of
	// MDReqRejReason TOO_MANY_SUBSCRIPTIONS. The client leaves it to
	// Binance.
	MaxStreams = 1000
	// MaxMarketDepth bounds a MarketDataRequest's MarketDepth: a
	// snapshot has as many levels per side at most.
	MaxMarketDepth = 5000
)

// MarketDataMessageReserve is the part of MarketDataMessageLimit kept for
// session messages: Subscribe, Unsubscribe and Send refuse a request
// (ErrRateLimited) that would leave less. At HeartBtInt's lower bound,
// 5s, a minute holds 14 Heartbeats of ours, 12 TestRequests of ours and
// answers to 12 of Binance's, and a Logout.
const MarketDataMessageReserve = 100

// Defaults of Config.
const (
	DefaultHeartBtInt    = 30 * time.Second
	DefaultDialTimeout   = 30 * time.Second
	DefaultLogonTimeout  = 10 * time.Second
	DefaultLogoutTimeout = 2 * time.Second
	DefaultMaxQueued     = 1024
	// DefaultMaxEvents and DefaultMaxEventBytes bound an order entry
	// session's events waiting for the Handler: 4096 of 4 KiB, an order
	// list's.
	DefaultMaxEvents     = 4096
	DefaultMaxEventBytes = 16 << 20
	// DefaultMaxFrameLength bounds an order entry session's frames:
	// Binance's are a few hundred bytes, an order list's a few kilobytes.
	DefaultMaxFrameLength = lesofh.DefaultMaxLength
)

// Defaults of Config for market data sessions.
const (
	// DefaultMarketDataMaxFrameLength bounds a market data session's
	// frames. The largest MarketDataSnapshot, MaxMarketDepth levels per
	// side, takes 160,063 bytes framed: 6 of SOFH, 20 of header, a block
	// of 10, two group dimensions of 3, 16 per level, a 20-byte symbol
	// and its length. An InstrumentList takes 31 bytes and its
	// InstrumentReqID, then per instrument 76 and its symbol and currency:
	// 106 with a 20-byte symbol and a 10-byte currency, 91 with the usual
	// 10 and 5. 1 MiB holds that snapshot six times over, and a list of
	// 9,800 to 11,500 instruments in one message, about three times
	// Binance Spot's. A longer list may come in several messages.
	DefaultMarketDataMaxFrameLength = 1 << 20
	// DefaultMarketDataMaxEvents and DefaultMarketDataMaxEventBytes bound
	// a market data session's events waiting for the Handler: a burst of
	// small updates (a trade takes about 100 bytes), or 200 snapshots of
	// the largest.
	DefaultMarketDataMaxEvents     = 16384
	DefaultMarketDataMaxEventBytes = 32 << 20
)

// Config configures a Client. APIKey, PrivateKey and SenderCompID are
// required; the rest has defaults, some of them by Kind.
type Config struct {
	// Kind is what the session is for, OrderEntry if 0.
	Kind Kind
	// Endpoint is the host:port dialed over TLS when Dialer is nil, by
	// default production's of Kind: Endpoint or MarketDataEndpoint;
	// TestnetEndpoint and TestnetMarketDataEndpoint are the testnet's.
	Endpoint string
	// Dialer opens the connections, overriding Endpoint: a
	// transport/tcp.Dialer with TLS, or an in-memory transport in tests.
	Dialer transport.Dialer

	// APIKey is the Ed25519 API key, sent as Username; it needs the
	// FIX_API permission, or for market data FIX_API or
	// FIX_API_READ_ONLY.
	APIKey string
	// PrivateKey signs the Logon (see LoadPrivateKey).
	PrivateKey ed25519.PrivateKey
	// SenderCompID identifies the session: 1 to 8 letters, digits, - or
	// _, unique among the account's sessions.
	SenderCompID string
	// SignatureTime is the text form of the Logon's sendingTime in the
	// payload it signs. Leave it zero: SignatureTimeMicros, the form
	// Binance checks (settled on the testnet). The other forms are
	// diagnostics, for cmd/binancefix's logon command (see SignatureTime).
	SignatureTime SignatureTime

	// HeartBtInt is the heartbeat interval, whole seconds from 5s to 60s,
	// DefaultHeartBtInt if 0.
	HeartBtInt time.Duration
	// MessageHandling tells whether Binance may reorder our requests,
	// MessageHandlingSequential if 0.
	MessageHandling MessageHandling
	// ResponseMode, if set, is sent: Binance's default is
	// ResponseModeEverything, every ExecutionReport of the account. Order
	// entry only.
	ResponseMode ResponseMode
	// ExecutionReportType, if set, is sent: ExecutionReportTypeMini, with
	// ResponseModeOnlyAcks, makes Binance answer with ExecutionReportAck.
	// Order entry only.
	ExecutionReportType ExecutionReportType
	// RecvWindow, if set, is how long after their SendingTime requests
	// stay valid, up to 60s, in microseconds on the wire; Binance applies
	// 5s to the Logon otherwise, and no window to other requests.
	RecvWindow time.Duration

	// DialTimeout bounds a dial, TLS handshake included,
	// DefaultDialTimeout if 0.
	DialTimeout time.Duration
	// LogonTimeout bounds the wait for Binance's LogonAck,
	// DefaultLogonTimeout if 0.
	LogonTimeout time.Duration
	// LogoutTimeout bounds the wait for the peer's Logout once ours is
	// sent, DefaultLogoutTimeout if 0.
	LogoutTimeout time.Duration
	// WriteTimeout bounds a write to the connection, HeartBtInt if 0: a
	// peer not reading ends the connection.
	WriteTimeout time.Duration

	// MaxQueued bounds the requests waiting to be written,
	// DefaultMaxQueued if 0: beyond, Send, Subscribe and Unsubscribe fail
	// with ErrQueueFull. Session messages (heartbeats, Logout) are queued
	// apart, never refused.
	MaxQueued int
	// MaxEvents bounds the events received and not yet handed to the
	// Handler, and MaxEventBytes the bytes of their messages (their
	// decoded form takes about as much again), News headlines, LogonEvent
	// UUIDs and LogoutEvent errors (Binance's Logout text) included: by
	// default
	// DefaultMaxEvents and DefaultMaxEventBytes, for market data
	// DefaultMarketDataMaxEvents and DefaultMarketDataMaxEventBytes.
	// Beyond either, reading waits for the Handler, and so does a
	// connection's end, Run reconnecting once its LogoutEvent is queued;
	// an event larger than MaxEventBytes waits for the queue to be empty.
	MaxEvents     int
	MaxEventBytes int
	// MaxFrameLength bounds a received frame, header included, by default
	// DefaultMaxFrameLength (64 KiB), for market data
	// DefaultMarketDataMaxFrameLength (1 MiB): a longer frame ends the
	// connection (ProtocolError).
	MaxFrameLength int

	// Reconnect paces reconnections.
	Reconnect Reconnect

	// Logger logs the session's events, nothing if nil.
	Logger *slog.Logger
	// Trace, if set, is called with every message of the session, decoded
	// (TraceMessage): ours once written, stamped, a write failing part way
	// tracing what the connection took (the message it cut marked
	// Partial), the Logon's credentials redacted; Binance's as they are
	// read, before they are handled, those the Handler never sees
	// included. It is called from the connection's goroutines,
	// concurrently, an answer possibly traced before its request: it must
	// be safe for concurrent use, and quick, its messages valid during the
	// call only.
	Trace func(TraceMessage)
	// Handler receives the session's events and Binance's messages,
	// BaseHandler if nil.
	Handler Handler
}

// Reconnect paces reconnections. The zero value takes the defaults.
type Reconnect struct {
	// Disabled makes Run return once its first connection ends.
	Disabled bool
	// MinBackoff is the wait after a connection ends or fails, 1s if 0;
	// it doubles at every failure in a row, up to MaxBackoff, 30s if 0.
	MinBackoff, MaxBackoff time.Duration
	// MaxAttempts connection attempts at most are made in any Window, if
	// 0 Binance's limit: 15 in 30s for order entry, 300 in 300s for
	// market data.
	MaxAttempts int
	Window      time.Duration
	// MaxRefusals is how many Logons in a row Binance may refuse before
	// Run gives up, 3 if 0. A refusal may pass (the SenderCompID's
	// cooldown after a session ended without Logout, a connection limit)
	// or not (bad credentials): each is retried after the backoff, and
	// after twice HeartBtInt.
	MaxRefusals int
	// MaxInitialRefusals, if set, is how many Logons in a row Binance may
	// refuse before one ever succeeded, MaxRefusals otherwise: 1 gives up
	// at the first refusal, bad credentials being likelier than a
	// cooldown before any connection.
	MaxInitialRefusals int
}

var senderCompID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,8}$`)

// withDefaults validates cfg and returns it with its defaults.
func (cfg Config) withDefaults() (Config, error) {
	var errs []error
	if cfg.Kind >= numKinds {
		errs = append(errs, fmt.Errorf("Kind %d unknown", cfg.Kind))
	}
	if cfg.APIKey == "" {
		errs = append(errs, errors.New("APIKey required"))
	}
	if len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		errs = append(errs, errors.New("PrivateKey: an Ed25519 private key required"))
	}
	if !senderCompID.MatchString(cfg.SenderCompID) {
		errs = append(errs, fmt.Errorf("SenderCompID %q: 1 to 8 letters, digits, - or _", cfg.SenderCompID))
	}
	if cfg.SignatureTime >= numSignatureTimes {
		errs = append(errs, fmt.Errorf("SignatureTime %d unknown", cfg.SignatureTime))
	}
	if cfg.HeartBtInt == 0 {
		cfg.HeartBtInt = DefaultHeartBtInt
	}
	if cfg.HeartBtInt < MinHeartBtInt || cfg.HeartBtInt > MaxHeartBtInt || cfg.HeartBtInt%time.Second != 0 {
		errs = append(errs, fmt.Errorf("HeartBtInt %v: whole seconds from %v to %v", cfg.HeartBtInt, MinHeartBtInt, MaxHeartBtInt))
	}
	if cfg.MessageHandling == 0 {
		cfg.MessageHandling = fixsbe.MessageHandlingSequential
	}
	if cfg.MessageHandling != fixsbe.MessageHandlingSequential && cfg.MessageHandling != fixsbe.MessageHandlingUnordered {
		errs = append(errs, fmt.Errorf("MessageHandling %d unknown", cfg.MessageHandling))
	}
	if cfg.ResponseMode != 0 && cfg.ResponseMode != fixsbe.ResponseModeEverything && cfg.ResponseMode != fixsbe.ResponseModeOnlyAcks {
		errs = append(errs, fmt.Errorf("ResponseMode %d unknown", cfg.ResponseMode))
	}
	if cfg.ExecutionReportType != 0 && cfg.ExecutionReportType != fixsbe.ExecutionReportTypeFull && cfg.ExecutionReportType != fixsbe.ExecutionReportTypeMini {
		errs = append(errs, fmt.Errorf("ExecutionReportType %d unknown", cfg.ExecutionReportType))
	}
	if cfg.Kind == MarketData && (cfg.ResponseMode != 0 || cfg.ExecutionReportType != 0) {
		errs = append(errs, errors.New("ResponseMode, ExecutionReportType: order entry only"))
	}
	if cfg.RecvWindow < 0 || cfg.RecvWindow > MaxRecvWindow {
		errs = append(errs, fmt.Errorf("RecvWindow %v: up to %v", cfg.RecvWindow, MaxRecvWindow))
	}
	if len(errs) > 0 {
		return cfg, fmt.Errorf("binancefix: invalid Config: %w", errors.Join(errs...))
	}
	md := cfg.Kind == MarketData
	if cfg.Dialer == nil {
		if cfg.Endpoint == "" {
			cfg.Endpoint = byKind(md, Endpoint, MarketDataEndpoint)
		}
		cfg.Dialer = &tcp.Dialer{Addresses: []string{cfg.Endpoint}, TLS: &tls.Config{MinVersion: tls.VersionTLS12}}
	}
	cfg.DialTimeout = orDefault(cfg.DialTimeout, DefaultDialTimeout)
	cfg.LogonTimeout = orDefault(cfg.LogonTimeout, DefaultLogonTimeout)
	cfg.LogoutTimeout = orDefault(cfg.LogoutTimeout, DefaultLogoutTimeout)
	cfg.WriteTimeout = orDefault(cfg.WriteTimeout, cfg.HeartBtInt)
	cfg.MaxQueued = orDefault(cfg.MaxQueued, DefaultMaxQueued)
	cfg.MaxEvents = orDefault(cfg.MaxEvents, byKind(md, DefaultMaxEvents, DefaultMarketDataMaxEvents))
	cfg.MaxEventBytes = orDefault(cfg.MaxEventBytes, byKind(md, DefaultMaxEventBytes, DefaultMarketDataMaxEventBytes))
	cfg.MaxFrameLength = orDefault(cfg.MaxFrameLength, byKind(md, DefaultMaxFrameLength, DefaultMarketDataMaxFrameLength))
	r := &cfg.Reconnect
	r.MinBackoff = orDefault(r.MinBackoff, time.Second)
	r.MaxBackoff = max(orDefault(r.MaxBackoff, 30*time.Second), r.MinBackoff)
	r.MaxAttempts = orDefault(r.MaxAttempts, byKind(md, 15, 300))
	r.Window = orDefault(r.Window, byKind(md, 30*time.Second, 300*time.Second))
	r.MaxRefusals = orDefault(r.MaxRefusals, 3)
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Handler == nil {
		cfg.Handler = BaseHandler{}
	}
	return cfg, nil
}

// byKind returns oe, or md for a market data session.
func byKind[T any](isMD bool, oe, md T) T {
	if isMD {
		return md
	}
	return oe
}

func orDefault[T time.Duration | int](v, d T) T {
	if v <= 0 {
		return d
	}
	return v
}
