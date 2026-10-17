package fakebinance

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
)

// The market data endpoint, spoken to frame by frame, refuses what the
// client never sends: an MDReqID of a subscription of the connection, an
// unsubscribe of none, an order.
func TestMarketDataRefusals(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	x := New(Options{PublicKey: pub, Kind: binancefix.MarketData})
	c, err := x.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	fr := lesofh.NewReader(c, 1<<20)
	seq := uint32(1)
	send := func(m interface {
		AppendWithHeader([]byte, fixsbe.MessageHeader) ([]byte, error)
	}, at time.Time) uint32 {
		t.Helper()
		b, err := m.AppendWithHeader(nil, fixsbe.MessageHeader{SeqNum: seq, SendingTime: at.UnixMicro()})
		if err != nil {
			t.Fatal(err)
		}
		f, err := lesofh.AppendFrame(nil, b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(f); err != nil {
			t.Fatal(err)
		}
		seq++
		return seq - 1
	}
	next := func(tid uint16) []byte {
		t.Helper()
		b, err := fr.Next()
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := fixsbe.ReadHeader(b)
		if err != nil || h.TemplateId != tid {
			t.Fatalf("template %d, %v; want %d", h.TemplateId, err, tid)
		}
		return b
	}

	at := time.Now().Truncate(time.Microsecond)
	payload := strings.Join([]string{"A", "MD1", binancefix.TargetCompID, "1", binancefix.SignatureTimeMicros.Format(at)}, "\x01")
	send(&fixsbe.Logon{HeartBtInt: 30, SenderCompId: []byte("MD1"), TargetCompId: []byte(binancefix.TargetCompID), Username: []byte("k"),
		RawData: []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload))))}, at)
	next(fixsbe.LogonAckTemplateID)

	req := &fixsbe.MarketDataRequest{SubscriptionRequestType: fixsbe.SubscriptionRequestTypeSubscribe, MarketDepth: fixsbe.Some[uint16](1),
		RelatedSym: []fixsbe.MarketDataRequestRelatedSym{{Symbol: []byte("BTCUSDT")}}, MDEntryTypes: []fixsbe.MDEntryTypes{{MDEntryType: fixsbe.MdEntryTypeTrade}},
		MDReqID: []byte("A")}
	send(req, time.Now())
	send(req, time.Now())
	var rej fixsbe.MarketDataRequestReject
	if _, err := rej.Decode(next(fixsbe.MarketDataRequestRejectTemplateID)); err != nil || string(rej.MDReqID) != "A" ||
		rej.MDReqRejReason != fixsbe.Some(fixsbe.MdReqRejReasonDuplicateMdReqID) {
		t.Errorf("duplicate MDReqID: %+v, %v", rej, err)
	}
	send(&fixsbe.MarketDataRequest{SubscriptionRequestType: fixsbe.SubscriptionRequestTypeUnsubscribe, MDReqID: []byte("Z")}, time.Now())
	if _, err := rej.Decode(next(fixsbe.MarketDataRequestRejectTemplateID)); err != nil || string(rej.MDReqID) != "Z" || rej.MDReqRejReason.Valid {
		t.Errorf("unsubscribe of none: %+v, %v", rej, err)
	}
	orderSeq := send(&fixsbe.NewOrderSingle{ClOrdID: []byte("o"), Symbol: []byte("BTCUSDT")}, time.Now())
	var r fixsbe.Reject
	if _, err := r.Decode(next(fixsbe.RejectTemplateID)); err != nil || r.RefSeqNum != fixsbe.Some(orderSeq) {
		t.Errorf("order: %+v, %v", r, err)
	}
	send(&fixsbe.LimitQuery{ReqID: []byte("L")}, time.Now())
	next(fixsbe.LimitResponseTemplateID)
	if n := x.Publish("BTCUSDT"); n != 1 {
		t.Errorf("%d published", n)
	}
	next(fixsbe.MarketDataIncrementalTradeTemplateID)
}
