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

// TargetCompID is the TargetCompID of every client Logon.
const TargetCompID = "SPOT"

// The bounds Binance sets.
const (
	// MinHeartBtInt and MaxHeartBtInt bound the heartbeat interval.
	MinHeartBtInt = 5 * time.Second
	MaxHeartBtInt = 60 * time.Second
	// MaxRecvWindow bounds RecvWindow.
	MaxRecvWindow = 60 * time.Second
)

// Defaults of Config.
const (
	DefaultHeartBtInt    = 30 * time.Second
	DefaultDialTimeout   = 30 * time.Second
	DefaultLogonTimeout  = 10 * time.Second
	DefaultLogoutTimeout = 2 * time.Second
	DefaultMaxQueued     = 1024
	DefaultMaxEvents     = 4096
)

// Config configures a Client. APIKey, PrivateKey and SenderCompID are
// required; the rest has defaults.
type Config struct {
	// Endpoint is the host:port dialed over TLS when Dialer is nil,
	// Endpoint (production) if empty; TestnetEndpoint is the testnet's.
	Endpoint string
	// Dialer opens the connections, overriding Endpoint: a
	// transport/tcp.Dialer with TLS, or an in-memory transport in tests.
	Dialer transport.Dialer

	// APIKey is the Ed25519 API key, sent as Username; it needs the
	// FIX_API permission.
	APIKey string
	// PrivateKey signs the Logon (see LoadPrivateKey).
	PrivateKey ed25519.PrivateKey
	// SenderCompID identifies the session: 1 to 8 letters, digits, - or
	// _, unique among the account's sessions.
	SenderCompID string

	// HeartBtInt is the heartbeat interval, whole seconds from 5s to 60s,
	// DefaultHeartBtInt if 0.
	HeartBtInt time.Duration
	// MessageHandling tells whether Binance may reorder our requests,
	// MessageHandlingSequential if 0.
	MessageHandling MessageHandling
	// ResponseMode, if set, is sent: Binance's default is
	// ResponseModeEverything, every ExecutionReport of the account.
	ResponseMode ResponseMode
	// ExecutionReportType, if set, is sent: ExecutionReportTypeMini, with
	// ResponseModeOnlyAcks, makes Binance answer with ExecutionReportAck.
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
	// DefaultMaxQueued if 0: beyond, Send fails with ErrQueueFull. Session
	// messages (heartbeats, Logout) are queued apart, never refused.
	MaxQueued int
	// MaxEvents bounds the events received and not yet handed to the
	// Handler, DefaultMaxEvents if 0: beyond, reading waits for the
	// Handler.
	MaxEvents int
	// MaxFrameLength bounds a received frame, header included,
	// lesofh.DefaultMaxLength (64 KiB) if 0.
	MaxFrameLength int

	// Reconnect paces reconnections.
	Reconnect Reconnect

	// Logger logs the session's events, nothing if nil.
	Logger *slog.Logger
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
	// MaxAttempts connection attempts at most are made in any Window,
	// 15 in 30s if 0: Binance's limit for order entry.
	MaxAttempts int
	Window      time.Duration
	// MaxRefusals is how many Logons in a row Binance may refuse before
	// Run gives up, 3 if 0. A refusal may pass (the SenderCompID's
	// cooldown after a session ended without Logout, a connection limit)
	// or not (bad credentials): each is retried after the backoff, and
	// after twice HeartBtInt.
	MaxRefusals int
}

var senderCompID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,8}$`)

// withDefaults validates cfg and returns it with its defaults.
func (cfg Config) withDefaults() (Config, error) {
	var errs []error
	if cfg.APIKey == "" {
		errs = append(errs, errors.New("APIKey required"))
	}
	if len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		errs = append(errs, errors.New("PrivateKey: an Ed25519 private key required"))
	}
	if !senderCompID.MatchString(cfg.SenderCompID) {
		errs = append(errs, fmt.Errorf("SenderCompID %q: 1 to 8 letters, digits, - or _", cfg.SenderCompID))
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
	if cfg.RecvWindow < 0 || cfg.RecvWindow > MaxRecvWindow {
		errs = append(errs, fmt.Errorf("RecvWindow %v: up to %v", cfg.RecvWindow, MaxRecvWindow))
	}
	if len(errs) > 0 {
		return cfg, fmt.Errorf("binancefix: invalid Config: %w", errors.Join(errs...))
	}
	if cfg.Dialer == nil {
		if cfg.Endpoint == "" {
			cfg.Endpoint = Endpoint
		}
		cfg.Dialer = &tcp.Dialer{Addresses: []string{cfg.Endpoint}, TLS: &tls.Config{MinVersion: tls.VersionTLS12}}
	}
	cfg.DialTimeout = orDefault(cfg.DialTimeout, DefaultDialTimeout)
	cfg.LogonTimeout = orDefault(cfg.LogonTimeout, DefaultLogonTimeout)
	cfg.LogoutTimeout = orDefault(cfg.LogoutTimeout, DefaultLogoutTimeout)
	cfg.WriteTimeout = orDefault(cfg.WriteTimeout, cfg.HeartBtInt)
	cfg.MaxQueued = orDefault(cfg.MaxQueued, DefaultMaxQueued)
	cfg.MaxEvents = orDefault(cfg.MaxEvents, DefaultMaxEvents)
	cfg.MaxFrameLength = orDefault(cfg.MaxFrameLength, lesofh.DefaultMaxLength)
	r := &cfg.Reconnect
	r.MinBackoff = orDefault(r.MinBackoff, time.Second)
	r.MaxBackoff = max(orDefault(r.MaxBackoff, 30*time.Second), r.MinBackoff)
	r.MaxAttempts = orDefault(r.MaxAttempts, 15)
	r.Window = orDefault(r.Window, 30*time.Second)
	r.MaxRefusals = orDefault(r.MaxRefusals, 3)
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Handler == nil {
		cfg.Handler = BaseHandler{}
	}
	return cfg, nil
}

func orDefault[T time.Duration | int](v, d T) T {
	if v <= 0 {
		return d
	}
	return v
}
