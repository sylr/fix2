package binancefix

import (
	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
)

// Redacted replaces the credentials of the Logon a trace shows: its
// Username, the API key, and its RawData, the signature.
const Redacted = "[redacted]"

// TraceMessage is a message written or read, decoded, as Config.Trace
// receives it.
type TraceMessage struct {
	// Epoch is the connection's.
	Epoch uint64
	// Out is set for a message of ours, written; unset for Binance's,
	// read.
	Out bool
	// Partial is set for a message of ours of which the connection took
	// the first bytes only before failing: Binance may have read it, or
	// not. Those it took none of are not traced.
	Partial bool
	// Header is the message's.
	Header MessageHeader
	// Message is the message decoded: *Logon, *MarketDataSnapshot... the
	// Logon's credentials Redacted. It is nil for a template the schema
	// does not have, or a message that does not decode (Err): Body holds
	// the message after its header, but a Logon's, never shown, decoded
	// or not, its credentials in it.
	Message any
	Body    []byte
	Err     error
}

// decoders decode the bodies of the schema's templates.
var decoders = map[uint16]func(MessageHeader, []byte) (any, error){
	fixsbe.LogonTemplateID:                               decodeAs[fixsbe.Logon],
	fixsbe.LogonAckTemplateID:                            decodeAs[fixsbe.LogonAck],
	fixsbe.LogoutTemplateID:                              decodeAs[fixsbe.Logout],
	fixsbe.HeartbeatTemplateID:                           decodeAs[fixsbe.Heartbeat],
	fixsbe.TestRequestTemplateID:                         decodeAs[fixsbe.TestRequest],
	fixsbe.RejectTemplateID:                              decodeAs[fixsbe.Reject],
	fixsbe.NewsTemplateID:                                decodeAs[fixsbe.News],
	fixsbe.NewOrderSingleTemplateID:                      decodeAs[fixsbe.NewOrderSingle],
	fixsbe.OrderCancelRequestTemplateID:                  decodeAs[fixsbe.OrderCancelRequest],
	fixsbe.OrderCancelRequestAndNewOrderSingleTemplateID: decodeAs[fixsbe.OrderCancelRequestAndNewOrderSingle],
	fixsbe.OrderAmendKeepPriorityRequestTemplateID:       decodeAs[fixsbe.OrderAmendKeepPriorityRequest],
	fixsbe.NewOrderListTemplateID:                        decodeAs[fixsbe.NewOrderList],
	fixsbe.OrderMassCancelRequestTemplateID:              decodeAs[fixsbe.OrderMassCancelRequest],
	fixsbe.LimitQueryTemplateID:                          decodeAs[fixsbe.LimitQuery],
	fixsbe.ExecutionReportTemplateID:                     decodeAs[fixsbe.ExecutionReport],
	fixsbe.ExecutionReportAckTemplateID:                  decodeAs[fixsbe.ExecutionReportAck],
	fixsbe.OrderCancelRejectTemplateID:                   decodeAs[fixsbe.OrderCancelReject],
	fixsbe.OrderAmendRejectTemplateID:                    decodeAs[fixsbe.OrderAmendReject],
	fixsbe.ListStatusTemplateID:                          decodeAs[fixsbe.ListStatus],
	fixsbe.OrderMassCancelReportTemplateID:               decodeAs[fixsbe.OrderMassCancelReport],
	fixsbe.LimitResponseTemplateID:                       decodeAs[fixsbe.LimitResponse],
	fixsbe.InstrumentListRequestTemplateID:               decodeAs[fixsbe.InstrumentListRequest],
	fixsbe.InstrumentListTemplateID:                      decodeAs[fixsbe.InstrumentList],
	fixsbe.MarketDataRequestTemplateID:                   decodeAs[fixsbe.MarketDataRequest],
	fixsbe.MarketDataRequestRejectTemplateID:             decodeAs[fixsbe.MarketDataRequestReject],
	fixsbe.MarketDataSnapshotTemplateID:                  decodeAs[fixsbe.MarketDataSnapshot],
	fixsbe.MarketDataIncrementalTradeTemplateID:          decodeAs[fixsbe.MarketDataIncrementalTrade],
	fixsbe.MarketDataIncrementalBookTickerTemplateID:     decodeAs[fixsbe.MarketDataIncrementalBookTicker],
	fixsbe.MarketDataIncrementalDepthTemplateID:          decodeAs[fixsbe.MarketDataIncrementalDepth],
}

// decodeAs decodes body, of header h, as an M.
func decodeAs[M any, P interface {
	*M
	DecodeBody(MessageHeader, []byte) (int, error)
}](h MessageHeader, body []byte) (any, error) {
	m := P(new(M))
	if _, err := m.DecodeBody(h, body); err != nil {
		return nil, err
	}
	return m, nil
}

// trace hands msg, a message without its framing header, partly written
// if partial, to Config.Trace, if set.
func (k *conn) trace(out, partial bool, msg []byte) {
	f := k.c.cfg.Trace
	if f == nil {
		return
	}
	t := TraceMessage{Epoch: k.epoch, Out: out, Partial: partial}
	h, body, err := fixsbe.ReadHeader(msg)
	if err != nil {
		// Shorter than a header: no credentials.
		t.Body, t.Err = msg, err
		f(t)
		return
	}
	t.Header = h
	if decode := decoders[h.TemplateId]; decode != nil {
		t.Message, t.Err = decode(h, body)
	}
	if t.Message == nil && h.TemplateId != fixsbe.LogonTemplateID {
		t.Body = body
	}
	if l, ok := t.Message.(*fixsbe.Logon); ok {
		// A copy's: the frame keeps its credentials.
		l.Username, l.RawData = []byte(Redacted), []byte(Redacted)
	}
	f(t)
}

// traceWritten traces the frames of buf the connection took the first
// written bytes of: those it took wholly, then the one it took part of.
func (k *conn) traceWritten(buf []byte, written int) {
	if k.c.cfg.Trace == nil {
		return
	}
	for off := 0; off < written && len(buf)-off >= lesofh.HeaderLen; {
		n, err := lesofh.ParseHeader(buf[off:])
		if err != nil || n > len(buf)-off {
			return // the writer's own frames: never
		}
		k.trace(true, off+n > written, buf[off+lesofh.HeaderLen:off+n])
		off += n
	}
}
