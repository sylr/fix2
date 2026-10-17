package binance

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"
)

// logonSample is the sample FIX SBE Logon of Binance's FIX API
// documentation, its hexdump's bytes in order, from
// https://github.com/binance/binance-spot-api-docs/blob/f0ee79c8ce24dbeda6645211b9504e237fddef19/fix-api.md
// ("Sample FIX SBE Logon request message"): a little-endian SOFH, then the
// 20-byte header and the message.
const logonSample = `d1 00 00 00 50 eb
	0e 00 28 4e 01 00 00 00 01 00 00 00 58 7a 5f 99 db 1b 06 00
	00 1e 00 00 00 01 02 ff ff ff ff ff ff ff
	07 45 58 41 4d 50 4c 45
	04 53 50 4f 54
	58 00 34 4d 48 58 65 6c 56 56 63 70 6b 64 77 75 4c 62 6c 36 6e 37 33 48 51 55
	58 55 66 31 64 73 65 32 50 43 67 54 31 44 59 71 57 39 77 38 41 56 5a 31
	52 41 43 46 47 4d 2b 35 55 64 6c 47 50 72 51 48 72 67 74 53 33 43 76 73
	52 55 52 43 31 6f 6a 37 33 6a 38 67 43 41 3d 3d
	40 00 73 42 52 58 72 4a 78 32 44 73 4f 72 61 4d 58 4f 61 55 6f 76 45 68 67 56
	52 63 6a 4f 76 43 74 51 77 6e 57 6a 38 56 78 6b 4f 68 31 78 71 62 6f 53
	30 32 53 50 47 66 4b 69 32 68 38 73 70 5a 4a 62`

func unhex(t *testing.T, dump string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(dump), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The sample decodes to the values its documentation lists. Its
// signature is not checked: only decoding is. Its sendingTime is
// 2024-06-27 09:17:25.223 UTC.
func TestLogonSample(t *testing.T) {
	raw := unhex(t, logonSample)
	// Binance's SOFH is little-endian, both fields: its length, header
	// included, and the encoding 0xEB50.
	if l := binary.LittleEndian.Uint32(raw); int(l) != len(raw) || binary.LittleEndian.Uint16(raw[4:]) != 0xEB50 {
		t.Fatalf("SOFH: length %d of %d, encoding %#x", l, len(raw), binary.LittleEndian.Uint16(raw[4:]))
	}
	msg := raw[6:]
	h, _, err := ReadHeader(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := MessageHeader{BlockLength: LogonBlockLength, TemplateId: LogonTemplateID, SchemaId: SchemaID, Version: 0, SeqNum: 1,
		SendingTime: time.Date(2024, 6, 27, 9, 17, 25, 223e6, time.UTC).UnixMicro()}
	if h != want {
		t.Fatalf("header %+v, want %+v", h, want)
	}
	var m Logon
	n, err := m.Decode(msg)
	if err != nil || n != len(msg) {
		t.Fatalf("Decode = %d, %v; want %d", n, err, len(msg))
	}
	if m.EncryptMethod != Some[uint8](0) || m.HeartBtInt != 30 || m.ResetSeqNumFlag != Some(BoolEnumTrue) ||
		m.MessageHandling != Some(MessageHandlingSequential) || m.ResponseMode.Valid || m.ExecutionReportType.Valid ||
		m.DropCopyFlag.Valid || m.RecvWindow.Valid {
		t.Errorf("fields %+v", m)
	}
	for _, c := range []struct{ name, got, want string }{
		{"SenderCompId", string(m.SenderCompId), "EXAMPLE"},
		{"TargetCompId", string(m.TargetCompId), "SPOT"},
		{"RawData", string(m.RawData), "4MHXelVVcpkdwuLbl6n73HQUXUf1dse2PCgT1DYqW9w8AVZ1RACFGM+5UdlGPrQHrgtS3CvsRURC1oj73j8gCA=="},
		{"Username", string(m.Username), "sBRXrJx2DsOraMXOaUovEhgVRcjOvCtQwnWj8VxkOh1xqboS02SPGfKi2h8spZJb"},
	} {
		if c.got != c.want {
			t.Errorf("%s %q, want %q", c.name, c.got, c.want)
		}
	}

	// Encoded back with its header, it is the sample but for the version,
	// the schema's: version 1.
	b, err := m.AppendWithHeader(nil, h)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(msg[6:], SchemaVersion)
	if !bytes.Equal(b, msg) {
		t.Errorf("encoded\n%x\nwant\n%x", b, msg)
	}
}

// message is what every generated message has, its header holding
// Binance's seqNum and sendingTime.
type message interface {
	AppendTo([]byte) ([]byte, error)
	AppendWithHeader([]byte, MessageHeader) ([]byte, error)
	Decode([]byte) (int, error)
	DecodeBody(MessageHeader, []byte) (int, error)
}

// roundTrip encodes m with the header elements seqNum and sendingTime,
// checks the header, and decodes the message back to m, with Decode and
// DecodeBody, returning its bytes.
func roundTrip[M any, P interface {
	*M
	message
}](t *testing.T, m P, templateID, blockLength int) []byte {
	t.Helper()
	const seq, sent = 4000000001, 1719479845223000
	b, err := m.AppendWithHeader(nil, MessageHeader{BlockLength: 9, TemplateId: 9, SchemaId: 9, Version: 9, SeqNum: seq, SendingTime: sent})
	if err != nil {
		t.Fatal(err)
	}
	h, body, err := ReadHeader(b)
	want := MessageHeader{BlockLength: uint16(blockLength), TemplateId: uint16(templateID), SchemaId: SchemaID, Version: SchemaVersion, SeqNum: seq, SendingTime: sent}
	if err != nil || h != want || len(b) != HeaderSize+len(body) || HeaderSize != 20 {
		t.Fatalf("header %+v, %v; want %+v", h, err, want)
	}
	if !bytes.Equal(b[8:20], []byte{0x01, 0x28, 0x6b, 0xee, 0x58, 0x7a, 0x5f, 0x99, 0xdb, 0x1b, 0x06, 0x00}) {
		t.Errorf("seqNum and sendingTime on the wire: %x", b[8:20])
	}
	var got M
	if n, err := P(&got).Decode(b); err != nil || n != len(b) {
		t.Fatalf("Decode = %d, %v; want %d", n, err, len(b))
	}
	if !reflect.DeepEqual(&got, (*M)(m)) {
		t.Errorf("decoded\n%+v\nwant\n%+v", got, *m)
	}
	var fromBody M
	if n, err := P(&fromBody).DecodeBody(h, body); err != nil || n != len(body) || !reflect.DeepEqual(&fromBody, (*M)(m)) {
		t.Errorf("DecodeBody = %d, %v: %+v", n, err, fromBody)
	}
	// AppendTo leaves the header's own elements zero.
	plain, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if ph, _, _ := ReadHeader(plain); ph.SeqNum != 0 || ph.SendingTime != 0 || !bytes.Equal(plain[20:], b[20:]) {
		t.Errorf("AppendTo: header %+v, body %x", ph, plain[20:])
	}
	return b
}

func TestSession(t *testing.T) {
	roundTrip(t, &Logon{EncryptMethod: Some[uint8](0), HeartBtInt: 30, ResetSeqNumFlag: Some(BoolEnumTrue),
		ResponseMode: Some(ResponseModeOnlyAcks), ExecutionReportType: Some(ExecutionReportTypeMini), RecvWindow: Some[uint32](5000000),
		SenderCompId: []byte("EXAMPLE"), TargetCompId: []byte("SPOT"), RawData: []byte("sig=="), Username: []byte("key")},
		LogonTemplateID, LogonBlockLength)
	roundTrip(t, &LogonAck{EncryptMethod: Some[uint8](0), HeartBtInt: 30, SbeSchemaIdVersionDeprecated: BoolEnumFalse, UUID: []byte("3f1c")},
		LogonAckTemplateID, LogonAckBlockLength)
	// An optional var string: empty is null.
	roundTrip(t, &Heartbeat{TestReqID: []byte{}}, HeartbeatTemplateID, HeartbeatBlockLength)
	roundTrip(t, &Heartbeat{TestReqID: []byte("t1")}, HeartbeatTemplateID, HeartbeatBlockLength)
	roundTrip(t, &TestRequest{TestReqID: []byte("t2")}, TestRequestTemplateID, TestRequestBlockLength)
	roundTrip(t, &Logout{Text: []byte{}}, LogoutTemplateID, LogoutBlockLength)
	roundTrip(t, &Logout{Text: []byte("bye")}, LogoutTemplateID, LogoutBlockLength)
	roundTrip(t, &News{Headline: []byte("maintenance")}, NewsTemplateID, NewsBlockLength)
}

// Reject's fields of bare primitive types, uint32 and int32, are those
// primitives, null as their primitives are when optional (§4.4.2).
func TestReject(t *testing.T) {
	b := roundTrip(t, &Reject{RefSeqNum: Some[uint32](7), SessionRejectReason: Some(SessionRejectReasonValueIsIncorrect),
		RefMsgType: []byte("D"), Text: []byte("bad")}, RejectTemplateID, RejectBlockLength)
	want := unhex(t, `0d 00 23 4e 01 00 01 00 01 28 6b ee 58 7a 5f 99 db 1b 06 00
		07 00 00 00 ff ff ff ff 05 00 00 00 80
		01 44 03 00 62 61 64`)
	if !bytes.Equal(b, want) {
		t.Errorf("encoded\n%x\nwant\n%x", b, want)
	}
	roundTrip(t, &Reject{RefSeqNum: Some[uint32](0xfffffffe), RefTagID: Some[uint32](0), ErrorCode: Some[int32](-1013),
		RefMsgType: []byte{}, Text: []byte{}}, RejectTemplateID, RejectBlockLength)
}

func TestOrders(t *testing.T) {
	roundTrip(t, &NewOrderSingle{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](150000000), OrdType: OrdTypeLimit,
		Price: Some[int64](6543210), Side: SideBuy, TimeInForce: Some(TimeInForceGoodTillCancel), PegOffsetValue: Some[uint8](3),
		TriggerTrailingDeltaBips: Some[uint64](0), SelfTradePreventionMode: Some(SelfTradePreventionModeExpireTaker),
		ClOrdID: []byte("c1"), Symbol: []byte("BTCUSDT")}, NewOrderSingleTemplateID, NewOrderSingleBlockLength)

	// The exponents and mantissas are left raw: 1.5 and 65432.10.
	er := &ExecutionReport{PriceExponent: -2, QtyExponent: -8, ExecID: Some[int64](1), OrderID: Some[int64](2),
		OrderQty: Some[int64](150000000), OrdType: OrdTypeLimit, Side: SideSell, Price: Some[int64](6543210),
		TransactTime: Some[int64](1719479845223000), ExecType: ExecTypeTrade, CumQty: 150000000, LastQty: 150000000,
		LastPx: Some[int64](6543210), OrdStatus: OrdStatusFilled, ExpiryReason: Some(ExpiryReasonUnfilledIocQuantityExpired),
		MiscFees: []MiscFees{
			{Exponent: -8, MiscFeeAmt: 150000, MiscFeeType: MiscFeeTypeExchangeFees, MiscFeeCurr: []byte("BTC")},
			{Exponent: -2, MiscFeeAmt: 98, MiscFeeType: MiscFeeTypeExchangeFees, MiscFeeCurr: []byte("USDT")},
		},
		ClOrdID: []byte("c1"), OrigClOrdID: []byte{}, Symbol: []byte("BTCUSDT"), SecondarySymbol: []byte{}, CounterSymbol: []byte{}, ErrorText: []byte{}}
	b := roundTrip(t, er, ExecutionReportTemplateID, ExecutionReportBlockLength)
	// MiscFees' dimension, smallGroupSize16Encoding: a uint8 blockLength,
	// a uint16 count.
	if dim := b[HeaderSize+ExecutionReportBlockLength:][:3]; !bytes.Equal(dim, []byte{10, 2, 0}) {
		t.Errorf("MiscFees' dimension %x", dim)
	}
	// Version 0's ExecutionReport has no ExpiryReason.
	b[6] = 0
	var v0 ExecutionReport
	if _, err := v0.Decode(b); err != nil || v0.ExpiryReason.Valid || v0.CumQty != er.CumQty || len(v0.MiscFees) != 2 {
		t.Errorf("version 0: %v, %+v", err, v0)
	}

	// Orders, of NewOrderList and of ListStatus, are two groups: each its
	// type and its layout, their entries holding the same
	// ListTriggeringInstructions.
	trig := []ListTriggeringInstructions{{ListTriggerType: ListTriggerTypeActivated, ListTriggerTriggerIndex: 0, ListTriggerAction: ListTriggerActionRelease}}
	nol := &NewOrderList{ContingencyType: ContingencyTypeOneTriggersTheOther, OPO: Some(BoolEnumFalse),
		Orders: []NewOrderListOrders{
			{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](1), OrdType: OrdTypeLimit, Price: Some[int64](100), Side: SideBuy,
				TimeInForce: Some(TimeInForceGoodTillCancel), ClOrdID: []byte("w"), Symbol: []byte("BTCUSDT")},
			{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](1), OrdType: OrdTypeMarket, Side: SideSell,
				ListTriggeringInstructions: trig, ClOrdID: []byte("p"), Symbol: []byte("BTCUSDT")},
		},
		ClListID: []byte("l1")}
	b = roundTrip(t, nol, NewOrderListTemplateID, NewOrderListBlockLength)
	// groupSize8Encoding: a uint16 blockLength, a uint8 count; the entry's
	// block, 23 fields, 75 bytes.
	if dim := b[HeaderSize+NewOrderListBlockLength:][:3]; !bytes.Equal(dim, []byte{75, 0, 2}) {
		t.Errorf("NewOrderList's Orders' dimension %x", dim)
	}
	ls := &ListStatus{ListID: Some[int64](9), ContingencyType: Some(ContingencyTypeOneTriggersTheOther), ListStatusType: ListStatusTypeExecStarted,
		ListOrderStatus: ListOrderStatusExecuting, TransactTime: Some[int64](1719479845223000),
		Orders: []ListStatusOrders{
			{OrderID: Some[int64](11), ClOrdID: []byte("w"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}},
			{OrderID: Some[int64](12), ErrorCode: Some[int32](-2010), OrdRejReason: Some(OrdRejReasonOther), ListTriggeringInstructions: trig,
				ClOrdID: []byte("p"), Symbol: []byte("BTCUSDT"), ErrorText: []byte("insufficient balance")},
		},
		ClListID: []byte("l1"), OrigClListID: []byte{}}
	b = roundTrip(t, ls, ListStatusTemplateID, ListStatusBlockLength)
	// smallGroupSize8Encoding: a uint8 blockLength, a uint8 count; the
	// entry's block, OrderID, OrdRejReason and ErrorCode, 13 bytes.
	if dim := b[HeaderSize+ListStatusBlockLength:][:2]; !bytes.Equal(dim, []byte{13, 2}) {
		t.Errorf("ListStatus's Orders' dimension %x", dim)
	}
}

func TestMarketData(t *testing.T) {
	roundTrip(t, &MarketDataIncrementalDepth{FirstBookUpdateID: 100, LastBookUpdateID: 102, PriceExponent: -2, QtyExponent: -8,
		MDEntriesBids: []MarketDataIncrementalDepthMDEntriesBids{{MDEntryPx: 6543210, MDEntrySize: Some[int64](5)}, {MDEntryPx: 6543200}},
		MDEntriesAsks: []MarketDataIncrementalDepthMDEntriesAsks{{MDEntryPx: 6543300, MDEntrySize: Some[int64](0)}},
		Symbol:        []byte("BTCUSDT")}, MarketDataIncrementalDepthTemplateID, MarketDataIncrementalDepthBlockLength)
	// Empty sides decode as no entries.
	roundTrip(t, &MarketDataIncrementalDepth{FirstBookUpdateID: 1, LastBookUpdateID: 1, Symbol: []byte("ETHBTC")},
		MarketDataIncrementalDepthTemplateID, MarketDataIncrementalDepthBlockLength)
	roundTrip(t, &MarketDataSnapshot{LastBookUpdateID: Some[int64](7),
		MDEntriesBids: []MarketDataSnapshotMDEntriesBids{{MDEntryPx: 1, MDEntrySize: 2}}, Symbol: []byte("BNBBTC")},
		MarketDataSnapshotTemplateID, MarketDataSnapshotBlockLength)
	roundTrip(t, &MarketDataIncrementalTrade{TransactTime: 1719479845223000,
		MDEntries: []MDEntries{{TradeID: 1, MDEntryPx: 2, MDEntrySize: 3, AggressorSide: Some(SideBuy)}, {TradeID: 2, MDEntryPx: 2, MDEntrySize: 3}},
		Symbol:    []byte("BTCUSDT")}, MarketDataIncrementalTradeTemplateID, MarketDataIncrementalTradeBlockLength)
}
