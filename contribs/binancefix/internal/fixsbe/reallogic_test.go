package fixsbe

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fixture is a message Real Logic's codecs encoded: its name and bytes,
// header included.
type fixture struct {
	name string
	b    []byte
}

// realLogic returns the messages of a fixture of binancefix's
// testdata/reallogic, in order (its README tells how they are made).
func realLogic(t testing.TB, file string) []fixture {
	t.Helper()
	f, err := os.Open("../../testdata/reallogic/" + file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var msgs []fixture
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		name, h, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		msgs = append(msgs, fixture{name, b})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return msgs
}

// The values testdata/reallogic/BinanceCheck.java.txt sets.
const (
	rlTS        = 1719479845223000 // 2024-06-27 09:17:25.223 UTC
	rlOrderID   = 1234567890123
	rlSignature = "4MHXelVVcpkdwuLbl6n73HQUXUf1dse2PCgT1DYqW9w8AVZ1RACFGM+5UdlGPrQHrgtS3CvsRURC1oj73j8gCA=="
	rlAPIKey    = "sBRXrJx2DsOraMXOaUovEhgVRcjOvCtQwnWj8VxkOh1xqboS02SPGfKi2h8spZJb"
	rlBalance   = "Account has insufficient balance for requested action."
)

// rlHeader returns the header elements of the i-th message of the
// fixtures: seqNum 4,000,000,000 + i, sendingTime rlTS + 1,001 i.
func rlHeader(i int) MessageHeader {
	return MessageHeader{SeqNum: 4000000000 + uint32(i), SendingTime: rlTS + 1001*int64(i)}
}

func repeat(s string, n int) []byte { return []byte(strings.Repeat(s, n/len(s)+1)[:n]) }

func b(s string) []byte { return []byte(s) }

// rlMessages returns, in the fixtures' order, the messages
// BinanceCheck.java.txt encodes, by name: every field as it sets it, the
// null ones not Valid, empty var data a null optional string (or an empty
// required one), empty groups nil.
func rlMessages() []struct {
	name string
	m    message
} {
	trig1 := []ListTriggeringInstructions{{ListTriggerType: ListTriggerTypeActivated, ListTriggerTriggerIndex: 0, ListTriggerAction: ListTriggerActionRelease}}
	trig2 := []ListTriggeringInstructions{
		{ListTriggerType: ListTriggerTypeFilled, ListTriggerTriggerIndex: 0, ListTriggerAction: ListTriggerActionRelease},
		{ListTriggerType: ListTriggerTypePartiallyFilled, ListTriggerTriggerIndex: 1, ListTriggerAction: ListTriggerActionCancel},
	}
	return []struct {
		name string
		m    message
	}{
		// Session.
		{"Logon", &Logon{EncryptMethod: Some[uint8](0), HeartBtInt: 30, ResetSeqNumFlag: Some(BoolEnumTrue),
			MessageHandling: Some(MessageHandlingSequential), ResponseMode: Some(ResponseModeOnlyAcks),
			ExecutionReportType: Some(ExecutionReportTypeMini), DropCopyFlag: Some(BoolEnumFalse), RecvWindow: Some[uint32](5000000),
			SenderCompId: b("EXAMPLE"), TargetCompId: b("SPOT"), RawData: b(rlSignature), Username: b(rlAPIKey)}},
		{"LogonNulls", &Logon{HeartBtInt: 5, SenderCompId: b("S"), TargetCompId: b("SPOT"), RawData: b(""), Username: b("k")}},
		{"LogonAck", &LogonAck{EncryptMethod: Some[uint8](0), HeartBtInt: 30, SbeSchemaIdVersionDeprecated: BoolEnumTrue,
			UUID: b("4392a152-0e2c-4b9b-a6a3-0f1c2d3e4f50")}},
		{"LogonAckNulls", &LogonAck{HeartBtInt: 60, SbeSchemaIdVersionDeprecated: BoolEnumFalse, UUID: b("u")}},
		{"Heartbeat", &Heartbeat{TestReqID: b("")}},
		{"HeartbeatTestReqID", &Heartbeat{TestReqID: b("tr-1")}},
		{"TestRequest", &TestRequest{TestReqID: b("tr-1")}},
		{"TestRequestLong", &TestRequest{TestReqID: repeat("0123456789abcdef", 254)}},
		{"Reject", &Reject{RefSeqNum: Some[uint32](7), RefTagID: Some[uint32](44), SessionRejectReason: Some(SessionRejectReasonValueIsIncorrect),
			ErrorCode: Some[int32](-1102), RefMsgType: b("D"),
			Text: b("Mandatory parameter 'price' was not sent, was empty/null, or malformed.")}},
		{"RejectNulls", &Reject{RefMsgType: b(""), Text: b("")}},
		{"Logout", &Logout{Text: b("Logout acknowledgment.")}},
		{"LogoutEmpty", &Logout{Text: b("")}},
		{"News", &News{Headline: b("You'll be disconnected in 30 seconds. Please reconnect.")}},
		{"NewsLong", &News{Headline: repeat("maintenance ", 300)}},

		// Order entry.
		{"NewOrderSingle", &NewOrderSingle{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](150000000), OrdType: OrdTypeLimit,
			ExecInst: Some(ExecInstParticipateDontInitiate), Price: Some[int64](6543210), TriggerType: Some(TriggerTypePriceMovement),
			TriggerAction: Some(TriggerActionActivate), TriggerPrice: Some[int64](6500000), TriggerPriceType: Some(TriggerPriceTypeLastTrade),
			TriggerPriceDirection: Some(TriggerPriceDirectionUp), TriggerTrailingDeltaBips: Some[uint64](250), PegOffsetValue: Some[uint8](3),
			PegPriceType: Some(PegPriceTypePrimaryPeg), PegMoveType: Some(PegMoveTypeFixed), PegOffsetType: Some(PegOffsetTypePriceTier),
			Side: SideBuy, TimeInForce: Some(TimeInForceGoodTillCancel), MaxFloor: Some[int64](50000000), CashOrderQty: Some[int64](10000000),
			TargetStrategy: Some[int32](1000001), StrategyID: Some[int64](-42), SelfTradePreventionMode: Some(SelfTradePreventionModeExpireTaker),
			SOR: Some(BoolEnumTrue), ClOrdID: b("nos-1"), Symbol: b("BTCUSDT")}},
		{"NewOrderSingleNulls", &NewOrderSingle{PriceExponent: 0, QtyExponent: -3, OrdType: OrdTypeMarket, Side: SideSell,
			ClOrdID: b("nos-2"), Symbol: b("ETHBTC")}},
		{"OrderCancelRequest", &OrderCancelRequest{OrderID: Some[int64](rlOrderID), CancelRestrictions: Some(CancelRestrictionsOnlyNew),
			ClOrdID: b("ocr-1"), OrigClOrdID: b("nos-1"), OrigClListID: b(""), Symbol: b("BTCUSDT")}},
		{"OrderCancelRequestList", &OrderCancelRequest{ListID: Some[int64](77), ClOrdID: b("ocr-2"), OrigClOrdID: b(""),
			OrigClListID: b("nol-1"), Symbol: b("BTCUSDT")}},
		{"OrderCancelRequestAndNewOrderSingle", &OrderCancelRequestAndNewOrderSingle{
			OrderCancelRequestAndNewOrderSingleMode: OrderCancelRequestAndNewOrderSingleModeAllowFailure,
			OrderRateLimitExceededMode:              Some(OrderRateLimitExceededModeCancelOnly), OrderID: Some[int64](rlOrderID),
			CancelRestrictions: Some(CancelRestrictionsOnlyPartiallyFilled), PriceExponent: -2, QtyExponent: -8,
			OrderQty: Some[int64](200000000), OrdType: OrdTypeLimit, Price: Some[int64](6543100),
			TriggerPriceDirection: Some(TriggerPriceDirectionDown), Side: SideBuy, TimeInForce: Some(TimeInForceImmediateOrCancel),
			StrategyID: Some[int64](7), CancelClOrdID: b("xcn-c"), OrigClOrdID: b("nos-1"), ClOrdID: b("xcn-n"), Symbol: b("BTCUSDT")}},
		{"OrderCancelRequestAndNewOrderSingleNulls", &OrderCancelRequestAndNewOrderSingle{
			OrderCancelRequestAndNewOrderSingleMode: OrderCancelRequestAndNewOrderSingleModeStopOnFailure,
			PriceExponent:                           -1, QtyExponent: -6, OrdType: OrdTypeStopLimit, ExecInst: Some(ExecInstParticipateDontInitiate),
			TriggerType: Some(TriggerTypePriceMovement), TriggerAction: Some(TriggerActionActivate), TriggerPrice: Some[int64](6400000),
			TriggerPriceType: Some(TriggerPriceTypeLastTrade), TriggerTrailingDeltaBips: Some[uint64](100), PegOffsetValue: Some[uint8](5),
			PegPriceType: Some(PegPriceTypeMarketPeg), PegMoveType: Some(PegMoveTypeFixed), PegOffsetType: Some(PegOffsetTypePriceTier),
			Side: SideSell, MaxFloor: Some[int64](1000), CashOrderQty: Some[int64](2000), TargetStrategy: Some[int32](1000002),
			SelfTradePreventionMode: Some(SelfTradePreventionModeExpireBoth),
			CancelClOrdID:           b(""), OrigClOrdID: b(""), ClOrdID: b("xcn-n2"), Symbol: b("ETHBTC")}},
		{"OrderAmendKeepPriorityRequest", &OrderAmendKeepPriorityRequest{OrderID: Some[int64](rlOrderID), QtyExponent: -8, OrderQty: 100000000,
			ClOrdID: b("amd-1"), OrigClOrdID: b("nos-1"), Symbol: b("BTCUSDT")}},
		{"OrderAmendKeepPriorityRequestNulls", &OrderAmendKeepPriorityRequest{QtyExponent: 0, OrderQty: 1,
			ClOrdID: b("amd-2"), OrigClOrdID: b(""), Symbol: b("ETHBTC")}},
		{"OrderMassCancelRequest", &OrderMassCancelRequest{MassCancelRequestType: MassCancelRequestTypeCancelSymbolOrders,
			Symbol: b("BTCUSDT"), ClOrdID: b("omc-1")}},
		{"LimitQuery", &LimitQuery{ReqID: b("lq-1")}},
		{"NewOrderList", &NewOrderList{ContingencyType: ContingencyTypeOneTriggersTheOther, OPO: Some(BoolEnumTrue),
			Orders: []NewOrderListOrders{
				{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](100000000), OrdType: OrdTypeLimit, Price: Some[int64](6543210),
					Side: SideBuy, TimeInForce: Some(TimeInForceGoodTillCancel), SelfTradePreventionMode: Some(SelfTradePreventionModeExpireMaker),
					ClOrdID: b("nol-a"), Symbol: b("BTCUSDT")},
				{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](100000000), OrdType: OrdTypeLimit,
					ExecInst: Some(ExecInstParticipateDontInitiate), Price: Some[int64](7000000), Side: SideSell,
					TimeInForce: Some(TimeInForceGoodTillCancel), StrategyID: Some[int64](3), ListTriggeringInstructions: trig1,
					ClOrdID: b("nol-b"), Symbol: b("BTCUSDT")},
				{PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](100000000), OrdType: OrdTypeStopLimit, Price: Some[int64](6000000),
					TriggerType: Some(TriggerTypePriceMovement), TriggerAction: Some(TriggerActionActivate), TriggerPrice: Some[int64](6100000),
					TriggerPriceType: Some(TriggerPriceTypeLastTrade), TriggerPriceDirection: Some(TriggerPriceDirectionDown),
					TriggerTrailingDeltaBips: Some[uint64](0), Side: SideSell, TimeInForce: Some(TimeInForceFillOrKill),
					TargetStrategy: Some[int32](-1), SelfTradePreventionMode: Some(SelfTradePreventionModeDecrement),
					ListTriggeringInstructions: trig2, ClOrdID: b("nol-c"), Symbol: b("BTCUSDT")},
			},
			ClListID: b("nol-1")}},
		{"NewOrderListOCO", &NewOrderList{ContingencyType: ContingencyTypeOneCancelsTheOther,
			Orders: []NewOrderListOrders{
				{PriceExponent: -2, QtyExponent: -8, OrdType: OrdTypePegged, PegOffsetValue: Some[uint8](2),
					PegPriceType: Some(PegPriceTypePrimaryPeg), PegMoveType: Some(PegMoveTypeFixed), PegOffsetType: Some(PegOffsetTypePriceTier),
					Side: SideBuy, MaxFloor: Some[int64](30000000), CashOrderQty: Some[int64](5000000), ClOrdID: b("oco-a"), Symbol: b("ETHBTC")},
				{OrdType: OrdTypeMarket, Side: SideSell, ClOrdID: b("oco-b"), Symbol: b("ETHBTC")},
			},
			ClListID: b("oco-1")}},

		// Order entry answers.
		{"ExecutionReport", &ExecutionReport{PriceExponent: -2, QtyExponent: -8, ExecID: Some[int64](1001), OrderID: Some[int64](rlOrderID),
			OrderQty: Some[int64](150000000), OrdType: OrdTypeLimit, Side: SideBuy, ExecInst: Some(ExecInstParticipateDontInitiate),
			Price: Some[int64](6543210), TriggerType: Some(TriggerTypePriceMovement), TriggerAction: Some(TriggerActionActivate),
			TriggerPrice: Some[int64](6500000), TriggerPriceType: Some(TriggerPriceTypeLastTrade),
			TriggerPriceDirection: Some(TriggerPriceDirectionUp), TriggerTrailingDeltaBips: Some[uint64](250), PegOffsetValue: Some[uint8](3),
			PegPriceType: Some(PegPriceTypePrimaryPeg), PegMoveType: Some(PegMoveTypeFixed), PegOffsetType: Some(PegOffsetTypePriceTier),
			PeggedPrice: Some[int64](6543000), TimeInForce: Some(TimeInForceGoodTillCancel), TransactTime: Some[int64](rlTS + 1),
			OrderCreationTime: Some[int64](rlTS), MaxFloor: Some[int64](50000000), ListID: Some[int64](77), CashOrderQty: Some[int64](10000000),
			TargetStrategy: Some[int32](1000001), StrategyID: Some[int64](-42), OrderCapacity: Some(OrderCapacityPrincipal),
			SelfTradePreventionMode: Some(SelfTradePreventionModeExpireTaker), ExecType: ExecTypeNew, CumQty: 0,
			LeavesQty: Some[int64](150000000), CumQuoteQty: Some[int64](0), AggressorIndicator: Some(BoolEnumFalse), TradeID: Some[int64](555),
			LastPx: Some[int64](6543200), LastQty: 0, OrdStatus: OrdStatusNew, SecondaryOrderID: Some[int64](rlOrderID + 1),
			SecondaryExternalAccountID: Some[int64](99), AllocID: Some[int64](1), MatchType: Some(MatchTypeAutoMatch),
			WorkingFloor: Some(WorkingFloorExchange), WorkingIndicator: Some(BoolEnumTrue), WorkingTime: Some[int64](rlTS + 2),
			TrailingTime: Some[int64](rlTS + 3), PreventedMatchID: Some[int64](4), PreventedExecutionPrice: Some[int64](6543000),
			PreventedExecutionQty: Some[int64](1000), TradeGroupID: Some[int64](5), CounterOrderID: Some[int64](rlOrderID + 2),
			PreventedQty: Some[int64](2000), LastPreventedQty: Some[int64](1000), SOR: Some(BoolEnumTrue), OrdRejReason: Some(OrdRejReasonOther),
			ErrorCode: Some[int32](-1013), ExpiryReason: Some(ExpiryReasonExecutionRulePriceRangeExceeded),
			MiscFees: []MiscFees{
				{Exponent: -8, MiscFeeAmt: 150000, MiscFeeType: MiscFeeTypeExchangeFees, MiscFeeCurr: b("BTC")},
				{Exponent: 2, MiscFeeAmt: -98, MiscFeeType: MiscFeeTypeExchangeFees, MiscFeeCurr: b("USDT")},
			},
			ClOrdID: b("nos-1"), OrigClOrdID: b("orig-1"), Symbol: b("BTCUSDT"), SecondarySymbol: b("BTCUSDC"), CounterSymbol: b("ETHUSDT"),
			ErrorText: b("Filter failure: PRICE_FILTER")}},
		{"ExecutionReportNulls", &ExecutionReport{OrdType: OrdTypeMarket, Side: SideSell, ExecType: ExecTypeCanceled, OrdStatus: OrdStatusCanceled,
			ClOrdID: b(""), OrigClOrdID: b(""), Symbol: b("ETHBTC"), SecondarySymbol: b(""), CounterSymbol: b(""), ErrorText: b("")}},
		{"ExecutionReportAck", &ExecutionReportAck{OrderID: Some[int64](98765), TransactTime: Some[int64](rlTS + 4), ExecType: ExecTypeNew,
			OrdStatus: OrdStatusNew, ClOrdID: b("xcn-n"), Symbol: b("BTCUSDT"), ErrorText: b("")}},
		{"ExecutionReportAckRejected", &ExecutionReportAck{ListID: Some[int64](77), ExecType: ExecTypeRejected, OrdStatus: OrdStatusRejected,
			OrdRejReason: Some(OrdRejReasonOther), ErrorCode: Some[int32](-2010), ClOrdID: b(""), Symbol: b("BTCUSDT"), ErrorText: b(rlBalance)}},
		{"OrderCancelReject", &OrderCancelReject{ListID: Some[int64](77), CxlRejResponseTo: CxlRejResponseToOrderCancelRequest, ErrorCode: -2011,
			ClOrdID: b("ocr-1"), OrigClOrdID: b("nos-1"), OrigClListID: b(""), Symbol: b("BTCUSDT"), ErrorText: b("Unknown order sent.")}},
		{"OrderCancelRejectList", &OrderCancelReject{OrderID: Some[int64](rlOrderID), CancelRestrictions: Some(CancelRestrictionsOnlyNew),
			CxlRejResponseTo: CxlRejResponseToNonRepresentable, ErrorCode: -2026, ClOrdID: b("ocr-2"), OrigClOrdID: b(""),
			OrigClListID: b("nol-1"), Symbol: b("BTCUSDT"), ErrorText: b("Order was canceled or expired.")}},
		{"OrderAmendReject", &OrderAmendReject{OrderID: Some[int64](rlOrderID), QtyExponent: -8, OrderQty: 100000000, ErrorCode: -2038,
			ClOrdID: b("amd-1"), OrigClOrdID: b(""), Symbol: b("BTCUSDT"), ErrorText: b("Order amend (quantity increase) is not supported.")}},
		{"OrderAmendRejectNulls", &OrderAmendReject{QtyExponent: 0, OrderQty: 1, ErrorCode: -2013,
			ClOrdID: b("amd-2"), OrigClOrdID: b("nos-2"), Symbol: b("ETHBTC"), ErrorText: b("Order does not exist.")}},
		{"ListStatus", &ListStatus{ListID: Some[int64](77), ContingencyType: Some(ContingencyTypeOneTriggersTheOther),
			ListStatusType: ListStatusTypeExecStarted, ListOrderStatus: ListOrderStatusExecuting, TransactTime: Some[int64](rlTS + 5),
			Orders: []ListStatusOrders{
				{OrderID: Some[int64](11), ClOrdID: b("nol-a"), Symbol: b("BTCUSDT"), ErrorText: b("")},
				{OrderID: Some[int64](12), ListTriggeringInstructions: trig1, ClOrdID: b("nol-b"), Symbol: b("BTCUSDT"), ErrorText: b("")},
				{OrdRejReason: Some(OrdRejReasonOther), ErrorCode: Some[int32](-2010), ListTriggeringInstructions: trig2,
					ClOrdID: b("nol-c"), Symbol: b("BTCUSDT"), ErrorText: b(rlBalance)},
			},
			ClListID: b("nol-1"), OrigClListID: b("")}},
		{"ListStatusEmpty", &ListStatus{ListStatusType: ListStatusTypeAllDone, ListOrderStatus: ListOrderStatusAllDone,
			ListRejectReason: Some(ListRejectReasonOther), ClListID: b(""), OrigClListID: b("nol-0")}},
		{"OrderMassCancelReport", &OrderMassCancelReport{MassCancelRequestType: MassCancelRequestTypeCancelSymbolOrders,
			MassCancelResponse: MassCancelResponseCancelSymbolOrders, TotalAffectedOrders: Some[int64](3), Symbol: b("BTCUSDT"),
			ClOrdID: b("omc-1"), ErrorText: b("")}},
		{"OrderMassCancelReportRejected", &OrderMassCancelReport{MassCancelRequestType: MassCancelRequestTypeCancelSymbolOrders,
			MassCancelResponse: MassCancelResponseCancelRequestRejected, MassCancelRejectReason: Some(MassCancelRejectReasonOther),
			ErrorCode: Some[int32](-2011), Symbol: b("ETHBTC"), ClOrdID: b("omc-2"), ErrorText: b("Unknown order sent.")}},
		// The last indicator's LimitMax, required, is uint64's null value,
		// which SBE forbids a producer to set (§4.4.2). The generated
		// codecs are permissive there, as Real Logic's are: they encode and
		// decode a required field's null as its raw value, never refusing
		// it; refusing is the application's (contribs/binancefix refuses
		// it in the session messages it acts on).
		{"LimitResponse", &LimitResponse{LimitIndicators: []LimitIndicators{
			{LimitType: LimitTypeOrderLimit, LimitCount: 2, LimitMax: 200, LimitResetInterval: Some[uint32](10),
				LimitResetIntervalResolution: Some(LimitResetIntervalResolutionSecond)},
			{LimitType: LimitTypeMessageLimit, LimitCount: 5, LimitMax: 10000, LimitResetInterval: Some[uint32](1),
				LimitResetIntervalResolution: Some(LimitResetIntervalResolutionDay)},
			{LimitType: LimitTypeSubscriptionLimit, LimitCount: 0, LimitMax: math.MaxUint64},
		}, ReqID: b("lq-1")}},
		{"LimitResponseEmpty", &LimitResponse{ReqID: b("lq-2")}},

		// Market data.
		{"InstrumentListRequest", &InstrumentListRequest{InstrumentListRequestType: InstrumentListRequestTypeSingleInstrument,
			InstrumentReqID: b("ilr-1"), Symbol: b("BTCUSDT")}},
		{"InstrumentListRequestAll", &InstrumentListRequest{InstrumentListRequestType: InstrumentListRequestTypeAllInstruments,
			InstrumentReqID: b("ilr-2"), Symbol: b("")}},
		{"InstrumentList", &InstrumentList{RelatedSym: []InstrumentListRelatedSym{
			{PriceExponent: -2, QtyExponent: -8, MinTradeVol: Some[int64](1000), MaxTradeVol: Some[int64](900000000000),
				MinQtyIncrement: Some[int64](1000), MarketMinTradeVol: Some[int64](0), MarketMaxTradeVol: Some[int64](12000000000),
				MarketMinQtyIncrement: Some[int64](1000), StartPriceRange: Some[int64](1), EndPriceRange: Some[int64](100000000000),
				MinPriceIncrement: Some[int64](1), Symbol: b("BTCUSDT"), Currency: b("BTC")},
			{PriceExponent: 3, QtyExponent: 0, Symbol: b("ETHBTC"), Currency: b("ETH")},
		}, InstrumentReqID: b("ilr-1")}},
		{"MarketDataRequest", &MarketDataRequest{SubscriptionRequestType: SubscriptionRequestTypeSubscribe, MarketDepth: Some[uint16](10),
			AggregatedBook: Some(BoolEnumTrue), RelatedSym: []MarketDataRequestRelatedSym{{Symbol: b("BTCUSDT")}, {Symbol: b("ETHBTC")}},
			MDEntryTypes: []MDEntryTypes{{MDEntryType: MdEntryTypeBid}, {MDEntryType: MdEntryTypeOffer}}, MDReqID: b("mdr-1")}},
		{"MarketDataRequestNulls", &MarketDataRequest{SubscriptionRequestType: SubscriptionRequestTypeUnsubscribe,
			MDEntryTypes: []MDEntryTypes{{MDEntryType: MdEntryTypeTrade}}, MDReqID: b("mdr-1")}},
		{"MarketDataRequestReject", &MarketDataRequestReject{MDReqRejReason: Some(MdReqRejReasonDuplicateMdReqID), ErrorCode: Some[int32](-1110),
			MDReqID: b("mdr-1"), Text: b("Duplicate MDReqID.")}},
		{"MarketDataRequestRejectNulls", &MarketDataRequestReject{MDReqID: b("mdr-2"), Text: b("")}},
		{"MarketDataSnapshot", &MarketDataSnapshot{LastBookUpdateID: Some[int64](123456789), PriceExponent: -2, QtyExponent: -8,
			MDEntriesBids: []MarketDataSnapshotMDEntriesBids{{MDEntryPx: 6543210, MDEntrySize: 150000000}, {MDEntryPx: 6543200, MDEntrySize: 1},
				{MDEntryPx: 6543100, MDEntrySize: 99999999999}},
			MDEntriesAsks: []MarketDataSnapshotMDEntriesAsks{{MDEntryPx: 6543300, MDEntrySize: 2}, {MDEntryPx: 6543400, MDEntrySize: 3}},
			Symbol:        b("BTCUSDT")}},
		{"MarketDataSnapshotEmpty", &MarketDataSnapshot{PriceExponent: -6, QtyExponent: -4, Symbol: b("ETHBTC")}},
		{"MarketDataIncrementalTrade", &MarketDataIncrementalTrade{TransactTime: rlTS + 6, PriceExponent: -2, QtyExponent: -8,
			MDEntries: []MDEntries{
				{TradeID: 1000001, MDEntryPx: 6543210, MDEntrySize: 1000, AggressorSide: Some(SideBuy)},
				{TradeID: 1000002, MDEntryPx: 6543200, MDEntrySize: 2000, AggressorSide: Some(SideSell)},
				{TradeID: 1000003, MDEntryPx: 6543100, MDEntrySize: 3000},
			},
			Symbol: b("BTCUSDT")}},
		{"MarketDataIncrementalBookTicker", &MarketDataIncrementalBookTicker{LastBookUpdateID: 123456790, PriceExponent: -2, QtyExponent: -8,
			MDEntriesBids: []MarketDataIncrementalBookTickerMDEntriesBids{{MDEntryPx: 6543210, MDEntrySize: Some[int64](150000000)}},
			MDEntriesAsks: []MarketDataIncrementalBookTickerMDEntriesAsks{{MDEntryPx: 6543300}},
			Symbol:        b("BTCUSDT")}},
		{"MarketDataIncrementalBookTickerSides", &MarketDataIncrementalBookTicker{LastBookUpdateID: 123456791, PriceExponent: -2, QtyExponent: -8,
			MDEntriesBids: []MarketDataIncrementalBookTickerMDEntriesBids{{MDEntryPx: 6543100}},
			MDEntriesAsks: []MarketDataIncrementalBookTickerMDEntriesAsks{{MDEntryPx: 6543400, MDEntrySize: Some[int64](7)}},
			Symbol:        b("BTCUSDT")}},
		{"MarketDataIncrementalDepth", &MarketDataIncrementalDepth{FirstBookUpdateID: 123456791, LastBookUpdateID: 123456795,
			PriceExponent: -2, QtyExponent: -8,
			MDEntriesBids: []MarketDataIncrementalDepthMDEntriesBids{{MDEntryPx: 6543210, MDEntrySize: Some[int64](0)}, {MDEntryPx: 6543000}},
			Symbol:        b("BTCUSDT")}},
		{"MarketDataIncrementalDepthAsks", &MarketDataIncrementalDepth{FirstBookUpdateID: 123456796, LastBookUpdateID: 123456796,
			PriceExponent: -2, QtyExponent: -8,
			MDEntriesAsks: []MarketDataIncrementalDepthMDEntriesAsks{{MDEntryPx: 6543300, MDEntrySize: Some[int64](4)}, {MDEntryPx: 6543500}},
			Symbol:        b("BTCUSDT")}},
	}
}

// templateOf returns the template ID and block length of m's message.
func templateOf(t *testing.T, m message) (uint16, uint16) {
	t.Helper()
	b, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := ReadHeader(b)
	return h.TemplateId, h.BlockLength
}

// The messages Real Logic's SBE 1.33.0 encodes from Binance's schema,
// with the values of BinanceCheck.java.txt, decode to those values, every
// field, header included, and encode back, with the same seqNum and
// sendingTime, to the same bytes.
func TestRealLogic(t *testing.T) {
	fixtures := realLogic(t, "binance.hex")
	want := rlMessages()
	if len(fixtures) != len(want) {
		t.Fatalf("%d fixtures, %d messages", len(fixtures), len(want))
	}
	for i, w := range want {
		f := fixtures[i]
		t.Run(w.name, func(t *testing.T) {
			if f.name != w.name {
				t.Fatalf("fixture %d is %s", i, f.name)
			}
			tid, blockLength := templateOf(t, w.m)
			h, _, err := ReadHeader(f.b)
			wh := rlHeader(i)
			wh.BlockLength, wh.TemplateId, wh.SchemaId, wh.Version = blockLength, tid, SchemaID, SchemaVersion
			if err != nil || h != wh {
				t.Errorf("header %+v, %v; want %+v", h, err, wh)
			}
			got := reflect.New(reflect.TypeOf(w.m).Elem()).Interface().(message)
			if n, err := got.Decode(f.b); err != nil || n != len(f.b) {
				t.Fatalf("Decode = %d, %v; want %d", n, err, len(f.b))
			}
			if !reflect.DeepEqual(got, w.m) {
				t.Errorf("decoded\n%+v\nwant\n%+v", got, w.m)
			}
			b, err := w.m.AppendWithHeader(nil, rlHeader(i))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, f.b) {
				t.Errorf("encoded\n%x\nReal Logic\n%x", b, f.b)
			}
		})
	}
}

// xMembers are a message's or a group's members in the schema.
type xMembers struct {
	Name   string `xml:"name,attr"`
	Fields []struct {
		Name     string `xml:"name,attr"`
		Presence string `xml:"presence,attr"`
	} `xml:"field"`
	Groups []xMembers `xml:"group"`
	Data   []struct {
		Name string `xml:"name,attr"`
		Type string `xml:"type,attr"`
	} `xml:"data"`
}

// coverage records, for every optional field and optional data field of
// the schema, by path (Message.Group.Field), whether some message holds it
// set and some null.
type coverage map[string]*[2]bool // set, null

// walk records what v, a message or group entry of members x, holds, its
// groups' entries included; enumerating, v is a zero value, and each
// group is walked once, by the zero value of an entry.
func (c coverage) walk(t *testing.T, path string, x xMembers, v reflect.Value, enumerate bool) {
	t.Helper()
	field := func(name string) reflect.Value {
		f := v.FieldByName(name)
		if !f.IsValid() {
			t.Fatalf("%s: no field %s in %s", path, name, v.Type())
		}
		return f
	}
	mark := func(name string, set bool) {
		k := path + "." + name
		if c[k] == nil {
			c[k] = new([2]bool)
		}
		if set {
			c[k][0] = true
		} else {
			c[k][1] = true
		}
	}
	for _, f := range x.Fields {
		if f.Presence == "optional" {
			mark(f.Name, field(f.Name).FieldByName("Valid").Bool())
		}
	}
	for _, d := range x.Data {
		// Binance's optional strings: empty is null.
		if strings.HasPrefix(d.Type, "optional") {
			mark(d.Name, field(d.Name).Len() > 0)
		}
	}
	for _, g := range x.Groups {
		entries := field(g.Name)
		if enumerate {
			// An entry's zero value: the group's optionals, all null.
			c.walk(t, path+"."+g.Name, g, reflect.New(entries.Type().Elem()).Elem(), true)
			continue
		}
		for i := range entries.Len() {
			c.walk(t, path+"."+g.Name, g, entries.Index(i), false)
		}
	}
}

// Every optional field of Binance's schema, data included, groups'
// included, is set in some fixture and null in some other: what the
// cross-check claims of its coverage.
func TestRealLogicCoverage(t *testing.T) {
	raw, err := os.ReadFile("spot-fixsbe-1_1.xml")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Messages []xMembers `xml:"message"`
	}
	if err := xml.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Messages) != 29 {
		t.Fatalf("%d messages in the schema", len(schema.Messages))
	}
	byName := map[string]xMembers{}
	for _, m := range schema.Messages {
		byName[m.Name] = m
	}
	all, seen := coverage{}, coverage{}
	templates := map[string]bool{}
	for _, f := range realLogic(t, "binance.hex") {
		h, _, err := ReadHeader(f.b)
		if err != nil {
			t.Fatal(err)
		}
		var m message
		for _, w := range rlMessages() {
			if tid, _ := templateOf(t, w.m); tid == h.TemplateId {
				m = reflect.New(reflect.TypeOf(w.m).Elem()).Interface().(message)
			}
		}
		if _, err := m.Decode(f.b); err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		name := reflect.TypeOf(m).Elem().Name()
		x, ok := byName[name]
		if !ok {
			t.Fatalf("%s: no message %s in the schema", f.name, name)
		}
		templates[name] = true
		seen.walk(t, name, x, reflect.ValueOf(m).Elem(), false)
	}
	// Every optional of the schema, from its every message's zero value.
	for _, x := range schema.Messages {
		if !templates[x.Name] {
			t.Errorf("no fixture of %s", x.Name)
			continue
		}
		all.walk(t, x.Name, x, zeroOf(t, x.Name), true)
	}
	var missing []string
	for k := range all {
		s := seen[k]
		switch {
		case s == nil:
			missing = append(missing, k+" never")
		case !s[0]:
			missing = append(missing, k+" never set")
		case !s[1]:
			missing = append(missing, k+" never null")
		}
	}
	slices.Sort(missing)
	for _, m := range missing {
		t.Error(m)
	}
	t.Logf("%d optional fields, each set and null", len(all))
}

// zeroOf returns the zero value of the message named name.
func zeroOf(t *testing.T, name string) reflect.Value {
	t.Helper()
	for _, w := range rlMessages() {
		if reflect.TypeOf(w.m).Elem().Name() == name {
			return reflect.New(reflect.TypeOf(w.m).Elem()).Elem()
		}
	}
	t.Fatalf("no message %s", name)
	return reflect.Value{}
}

// The framed fixtures are the messages after Binance's little-endian
// SOFH: their length, header included, and the encoding 0xEB50.
func TestRealLogicFrames(t *testing.T) {
	msgs, frames := realLogic(t, "binance.hex"), realLogic(t, "binance-sofh.hex")
	if len(frames) != len(msgs) {
		t.Fatalf("%d frames, %d messages", len(frames), len(msgs))
	}
	for i, f := range frames {
		if f.name != msgs[i].name || len(f.b) < 6 || int(binary.LittleEndian.Uint32(f.b)) != len(f.b) ||
			!bytes.Equal(f.b[4:6], []byte{0x50, 0xeb}) || !bytes.Equal(f.b[6:], msgs[i].b) {
			t.Errorf("frame %s: %x", f.name, f.b)
		}
	}
}

// The vendored schema is the pinned one (generate.go): the codecs and the
// fixtures are of it, and fix2 keeps the same file as its generator's test
// data (internal/gen/sbe/testdata/binance).
func TestSchemaPinned(t *testing.T) {
	raw, err := os.ReadFile("spot-fixsbe-1_1.xml")
	if err != nil {
		t.Fatal(err)
	}
	const pinned = "6b44afe558d439ddc94323fd6ff026a949184ddd79a2cf033f610194c2537e70"
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != pinned {
		t.Errorf("spot-fixsbe-1_1.xml: SHA-256 %x, want %s", sum, pinned)
	}
}

// A varString8 holds 254 bytes at most, its uint8 length's maxValue (255
// is uint8's null value): Real Logic refuses to encode 255 ("length >
// maxValue for type: 255"), and so do these codecs, which refuse to decode
// it too.
func TestVarString8Bound(t *testing.T) {
	if _, err := (&TestRequest{TestReqID: repeat("x", 255)}).AppendTo(nil); !errors.Is(err, ErrLength) {
		t.Errorf("encoding 255 bytes: %v", err)
	}
	b, err := (&TestRequest{TestReqID: repeat("x", 254)}).AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	b[HeaderSize] = 255
	b = append(b, 'x')
	var m TestRequest
	if _, err := m.Decode(b); !errors.Is(err, ErrMalformed) {
		t.Errorf("decoding a length of 255: %v", err)
	}
}
