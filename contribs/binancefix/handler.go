package binancefix

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Handler receives a Client's events, one at a time, in the order they
// happened, on a goroutine of the Client's: a connection's OnLogon first,
// then its messages, then its OnLogout, then the next connection's.
//
// Messages passed to a method are valid during the call only: their []byte
// fields alias a buffer the Client may reuse. Copy what is kept.
//
// A method may call Send, Subscribe, Unsubscribe, Stop and Reconnect,
// which never wait. It must not wait for Run to return, which waits for
// it, nor for a Receipt, whose answer may be queued behind the call. While
// a method runs, received messages wait in a queue of Config.MaxEvents
// messages and Config.MaxEventBytes bytes; once it is full, reading waits
// too, and with it the answers to Binance's TestRequests: a Handler slower
// than Binance for long gets the connection closed. Our heartbeats go on
// meanwhile.
//
// Embed BaseHandler to implement only some methods: methods may be added.
type Handler interface {
	// OnLogon tells of a connection logged on: requests may be sent.
	OnLogon(LogonEvent)
	// OnLogout tells of a logged-on connection's end, with the requests
	// it left unanswered.
	OnLogout(LogoutEvent)
	// OnNews tells of a News: Binance announcing maintenance, every 10
	// seconds until it closes the connection. Reconnect(e.Epoch), or a
	// Client of another SenderCompID, moves to another server.
	OnNews(NewsEvent)
	// OnReject receives a Reject: Binance refusing a request it could not
	// process (in.Receipt), or one of our session messages.
	OnReject(in Inbound, m *Reject)
	OnExecutionReport(in Inbound, m *ExecutionReport)
	OnExecutionReportAck(in Inbound, m *ExecutionReportAck)
	OnOrderCancelReject(in Inbound, m *OrderCancelReject)
	OnOrderAmendReject(in Inbound, m *OrderAmendReject)
	OnListStatus(in Inbound, m *ListStatus)
	OnOrderMassCancelReport(in Inbound, m *OrderMassCancelReport)
	OnLimitResponse(in Inbound, m *LimitResponse)
	// OnInstrumentList receives an InstrumentList, answering an
	// InstrumentListRequest (in.Receipt) if it is the first carrying its
	// InstrumentReqID: a long list may come in several.
	OnInstrumentList(in Inbound, m *InstrumentList)
	// OnMarketDataRequestReject receives Binance's refusal of a
	// subscription (in.Subscription, by MDReqID), or of its unsubscribe.
	OnMarketDataRequestReject(in Inbound, m *MarketDataRequestReject)
	// OnMarketDataSnapshot, OnMarketDataIncrementalTrade,
	// OnMarketDataIncrementalBookTicker and OnMarketDataIncrementalDepth
	// receive market data, checked (see doc.go), with the subscription of
	// its stream on the connection (in.Subscription), nil if none.
	OnMarketDataSnapshot(in Inbound, m *MarketDataSnapshot)
	OnMarketDataIncrementalTrade(in Inbound, m *MarketDataIncrementalTrade)
	OnMarketDataIncrementalBookTicker(in Inbound, m *MarketDataIncrementalBookTicker)
	OnMarketDataIncrementalDepth(in Inbound, m *MarketDataIncrementalDepth)
	// OnUnknown receives a message of a template the Client does not
	// handle, or that it could not decode (err).
	OnUnknown(in Inbound, body []byte, err error)
}

// Inbound tells of a received message.
type Inbound struct {
	// Epoch is the connection's.
	Epoch uint64
	// Header is the message's: Header.SeqNum, Header.SendingTime
	// (microseconds since the epoch, UTC).
	Header MessageHeader
	// Received is when the message was read.
	Received time.Time
	// Receipt is the request the message answers first, nil if none:
	// only the first answer of a request is matched (see doc.go).
	Receipt *Receipt
	// Subscription is, for market data, the subscription of the message's
	// stream on the connection, unsubscribed or not; for a
	// MarketDataRequestReject, the one of its MDReqID; for a Reject, the
	// one whose request or unsubscribe it names. Nil if none.
	Subscription *Subscription
}

// LogonEvent tells of a connection logged on.
type LogonEvent struct {
	// Epoch numbers the Client's connections from 1.
	Epoch uint64
	// HeartBtInt is the interval Binance acknowledged.
	HeartBtInt time.Duration
	// UUID identifies the server: two connections of one UUID are on one
	// server.
	UUID string
	// SchemaDeprecated is set when Binance tells that our schema, id 1
	// version 1, is deprecated: a newer schema is to be generated before
	// Binance retires it.
	SchemaDeprecated bool
}

// LogoutEvent tells of a logged-on connection's end.
type LogoutEvent struct {
	Epoch uint64
	// Err is why the connection ended: nil for a Logout of ours (Stop,
	// Reconnect, Run's context done), a *PeerLogoutError for Binance's, a
	// *SequenceError, ErrPeerSilent, or a read or write error.
	Err error
	// Clean is set when our Logout was written: Binance accepts the
	// SenderCompID again at once. Without, it refuses it for twice
	// HeartBtInt, and Run waits that long before reconnecting.
	Clean bool
	// NeverWritten are the connection's requests Binance never saw, in the
	// order they were sent: safe to send again.
	NeverWritten []*Receipt
	// PossiblyTransmitted are the connection's requests written, or partly
	// written, and unanswered, in the order they were written: the
	// application reconciles them.
	PossiblyTransmitted []*Receipt
	// Subscriptions are the connection's subscriptions, unsubscribed or
	// not, in the order they were made: they ended with it, and are not
	// carried over. Subscribing again, once logged on again, is the
	// application's call.
	Subscriptions []*Subscription
}

// NewsEvent tells of a News.
type NewsEvent struct {
	Inbound
	Headline string
	// Remaining is how long before Binance closes the connection, read
	// from the headline, -1 if it does not tell.
	Remaining time.Duration
}

var countdown = regexp.MustCompile(`disconnected in (\d+) seconds`)

// remaining reads the time left in a News headline.
func remaining(headline string) time.Duration {
	if m := countdown.FindStringSubmatch(headline); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	// "Your connection is about to be closed. Please reconnect.", sent
	// when 10 seconds are left.
	if strings.Contains(headline, "about to be closed") {
		return 10 * time.Second
	}
	return -1
}

// BaseHandler implements Handler, doing nothing.
type BaseHandler struct{}

var _ Handler = BaseHandler{}

func (BaseHandler) OnLogon(LogonEvent)                                {}
func (BaseHandler) OnLogout(LogoutEvent)                              {}
func (BaseHandler) OnNews(NewsEvent)                                  {}
func (BaseHandler) OnReject(Inbound, *Reject)                         {}
func (BaseHandler) OnExecutionReport(Inbound, *ExecutionReport)       {}
func (BaseHandler) OnExecutionReportAck(Inbound, *ExecutionReportAck) {}
func (BaseHandler) OnOrderCancelReject(Inbound, *OrderCancelReject)   {}
func (BaseHandler) OnOrderAmendReject(Inbound, *OrderAmendReject)     {}
func (BaseHandler) OnListStatus(Inbound, *ListStatus)                 {}
func (BaseHandler) OnOrderMassCancelReport(Inbound, *OrderMassCancelReport) {
}
func (BaseHandler) OnLimitResponse(Inbound, *LimitResponse)                     {}
func (BaseHandler) OnInstrumentList(Inbound, *InstrumentList)                   {}
func (BaseHandler) OnMarketDataRequestReject(Inbound, *MarketDataRequestReject) {}
func (BaseHandler) OnMarketDataSnapshot(Inbound, *MarketDataSnapshot)           {}
func (BaseHandler) OnMarketDataIncrementalTrade(Inbound, *MarketDataIncrementalTrade) {
}
func (BaseHandler) OnMarketDataIncrementalBookTicker(Inbound, *MarketDataIncrementalBookTicker) {
}
func (BaseHandler) OnMarketDataIncrementalDepth(Inbound, *MarketDataIncrementalDepth) {
}
func (BaseHandler) OnUnknown(Inbound, []byte, error) {}
