// Package binancefixtest exports, for the tests of programs built on
// binancefix, the in-memory fake of Binance's FIX SBE endpoints that
// binancefix's own tests use (internal/fakebinance): an order entry or
// market data endpoint (Options.Kind) that checks the Logon's signature
// and answers requests as Binance does, or as told. It is for tests only:
// it is no exchange, and its behaviour changes with binancefix's tests.
//
// An Exchange is a transport.Dialer: give it as binancefix.Config.Dialer.
// Its methods drive the market data endpoint: Publish (an update of each
// stream of a symbol subscribed), Broadcast (any message, as scripted),
// News, Disconnect.
package binancefixtest

import "github.com/sylr/fix2/contribs/binancefix/internal/fakebinance"

type (
	// Exchange is the fake: Dial connects to it.
	Exchange = fakebinance.Exchange
	// Options configures an Exchange.
	Options = fakebinance.Options
	// Behaviour tells how the order entry endpoint answers.
	Behaviour = fakebinance.Behaviour
	// MarketData tells how the market data endpoint answers.
	MarketData = fakebinance.MarketData
	// Request is a request the exchange received.
	Request = fakebinance.Request
	// Logon is a Logon the exchange received.
	Logon = fakebinance.Logon
	// Message is a message the exchange sends: a binancefix codec's.
	Message = fakebinance.Message
)

// New returns an exchange of options o.
func New(o Options) *Exchange { return fakebinance.New(o) }
