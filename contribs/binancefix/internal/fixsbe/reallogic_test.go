package fixsbe

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"testing"
)

// message is what every generated message has.
type message interface {
	AppendWithHeader([]byte, MessageHeader) ([]byte, error)
	Decode([]byte) (int, error)
}

// newMessage returns a message of template tid, nil if none.
func newMessage(tid uint16) message {
	switch tid {
	case HeartbeatTemplateID:
		return new(Heartbeat)
	case TestRequestTemplateID:
		return new(TestRequest)
	case RejectTemplateID:
		return new(Reject)
	case LogoutTemplateID:
		return new(Logout)
	case LogonTemplateID:
		return new(Logon)
	case LogonAckTemplateID:
		return new(LogonAck)
	case NewsTemplateID:
		return new(News)
	case ExecutionReportTemplateID:
		return new(ExecutionReport)
	case ExecutionReportAckTemplateID:
		return new(ExecutionReportAck)
	case OrderCancelRejectTemplateID:
		return new(OrderCancelReject)
	case OrderCancelRequestAndNewOrderSingleTemplateID:
		return new(OrderCancelRequestAndNewOrderSingle)
	case NewOrderSingleTemplateID:
		return new(NewOrderSingle)
	case NewOrderListTemplateID:
		return new(NewOrderList)
	case OrderCancelRequestTemplateID:
		return new(OrderCancelRequest)
	case ListStatusTemplateID:
		return new(ListStatus)
	case OrderMassCancelRequestTemplateID:
		return new(OrderMassCancelRequest)
	case OrderMassCancelReportTemplateID:
		return new(OrderMassCancelReport)
	case OrderAmendKeepPriorityRequestTemplateID:
		return new(OrderAmendKeepPriorityRequest)
	case OrderAmendRejectTemplateID:
		return new(OrderAmendReject)
	case LimitQueryTemplateID:
		return new(LimitQuery)
	case LimitResponseTemplateID:
		return new(LimitResponse)
	case InstrumentListRequestTemplateID:
		return new(InstrumentListRequest)
	case InstrumentListTemplateID:
		return new(InstrumentList)
	case MarketDataRequestTemplateID:
		return new(MarketDataRequest)
	case MarketDataRequestRejectTemplateID:
		return new(MarketDataRequestReject)
	case MarketDataSnapshotTemplateID:
		return new(MarketDataSnapshot)
	case MarketDataIncrementalTradeTemplateID:
		return new(MarketDataIncrementalTrade)
	case MarketDataIncrementalBookTickerTemplateID:
		return new(MarketDataIncrementalBookTicker)
	case MarketDataIncrementalDepthTemplateID:
		return new(MarketDataIncrementalDepth)
	}
	return nil
}

// The messages Real Logic's SBE codecs encode from the same schema
// (../../testdata/reallogic, a copy of fix2's
// internal/gen/sbe/testdata/reallogic, whose binance package's tests check
// every field of them), decoded by these codecs, encode back to the same
// bytes, header included: every template of the schema.
func TestRealLogic(t *testing.T) {
	f, err := os.Open("../../testdata/reallogic/binance.hex")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[uint16]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		name, hx, _ := strings.Cut(sc.Text(), " ")
		raw, err := hex.DecodeString(hx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		h, _, err := ReadHeader(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m := newMessage(h.TemplateId)
		if m == nil {
			t.Errorf("%s: template %d unknown", name, h.TemplateId)
			continue
		}
		if !strings.HasPrefix(name, strings.TrimPrefix(reflect.TypeOf(m).String(), "*fixsbe.")) {
			t.Errorf("%s: template %d, %T", name, h.TemplateId, m)
		}
		seen[h.TemplateId] = true
		if n, err := m.Decode(raw); err != nil || n != len(raw) {
			t.Errorf("%s: Decode = %d, %v; want %d", name, n, err, len(raw))
			continue
		}
		b, err := m.AppendWithHeader(nil, MessageHeader{SeqNum: h.SeqNum, SendingTime: h.SendingTime})
		if err != nil || !bytes.Equal(b, raw) {
			t.Errorf("%s: encoded %v\n%x\nReal Logic\n%x", name, err, b, raw)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 29 {
		t.Errorf("%d templates of the schema's 29", len(seen))
	}
}
