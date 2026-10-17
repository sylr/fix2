package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/365
//
// FXCM answers a TradingSessionStatusRequest with a TradingSessionStatus
// listing its instruments in NoRelatedSym (146), their fields in another
// order than its dictionary's: the session rejected it, Incorrect NumInGroup
// (#366). It must be delivered by a session skipping
// dictionary.CheckGroupOrder, which would reject that order.
func TestIssue365_FXCMTradingSessionStatusDelivered(t *testing.T) {
	_, rec, p := loggedOn(t, func(c *session.Config) {
		c.Dictionaries = session.Dictionaries{Session: dictXML(t, i365Dict)}
		c.TolerateValidationFailures = dictionary.CheckGroupOrder
	})
	// The first three instruments of the issue's message.
	p.send("h", 0, "58", "Market is closed. Trading operations are disabled.Not in use for now",
		"60", "20190301-15:34:21.288", "325", "N", "335", "1", "336", "FXCM", "339", "2", "340", "2",
		"625", "GBDEMO", "9019", "0", "9030", "Coordinated Universal Time", "146", "3",
		"55", "USOil", "15", "USD", "228", "1", "231", "10", "460", "2", "561", "1", "9000", "2001",
		"9001", "3", "9002", "0.01", "9003", "0", "9004", "0", "9005", "2001", "9076", "T", "9080", "3",
		"9090", "0", "9091", "0", "9092", "0", "9093", "0", "9094", "5000", "9095", "1", "9096", "O",
		"55", "NZD/CAD", "15", "NZD", "228", "1", "231", "1", "460", "4", "561", "1", "9000", "91",
		"9001", "5", "9002", "0.0001", "9003", "-0.0143", "9004", "-0.0234", "9005", "91", "9076", "D", "9080", "1",
		"9090", "0", "9091", "0", "9092", "0", "9093", "0", "9094", "50000000", "9095", "1", "9096", "O",
		"55", "AUD/JPY", "15", "AUD", "228", "1", "231", "1", "460", "4", "561", "1", "9000", "17",
		"9001", "3", "9002", "0.01", "9003", "0.1374", "9004", "-0.3315", "9005", "1150", "9076", "D", "9080", "1",
		"9090", "0", "9091", "0", "9092", "0", "9093", "0", "9094", "50000000", "9095", "1", "9096", "O",
		"9016", "2", "9017", "TP_172", "9018", "Y", "9017", "FixSupport", "9018", "FIXONLY")
	rec.wait(t, "TradingSessionStatus", func() bool { return len(rec.apps) == 1 })
	rec.mu.Lock()
	m := rec.apps[0]
	rec.mu.Unlock()
	syms := m.GroupsOf(&fix.GroupSpec{Counter: 146, Delim: 55, Tags: []int{55, 460, 228, 231, 15, 561,
		9000, 9001, 9002, 9003, 9004, 9005, 9076, 9080, 9090, 9091, 9092, 9093, 9094, 9095, 9096}})
	if len(syms) != 3 || string(syms[2].Get(55)) != "AUD/JPY" {
		t.Fatalf("got %d instruments, want 3", len(syms))
	}
}

// i365Dict is FXCM's FIX 4.4 TradingSessionStatus as its dictionary may have
// it: NoRelatedSym lists the Instrument component in FIX order, then
// Currency, RoundLot and FXCM's fields, an order the message does not follow.
// It defines the session messages the test exchanges too: Logon and
// Heartbeat are validated against it, as any message.
const i365Dict = `<fix type="FIX" major="4" minor="4" servicepack="0">
  <header>
    <field name="BeginString" required="Y"/>
    <field name="BodyLength" required="Y"/>
    <field name="MsgType" required="Y"/>
    <field name="SenderCompID" required="Y"/>
    <field name="TargetCompID" required="Y"/>
    <field name="MsgSeqNum" required="Y"/>
    <field name="SenderSubID" required="N"/>
    <field name="TargetSubID" required="N"/>
    <field name="PossDupFlag" required="N"/>
    <field name="SendingTime" required="Y"/>
    <field name="OrigSendingTime" required="N"/>
  </header>
  <trailer>
    <field name="CheckSum" required="Y"/>
  </trailer>
  <messages>
    <message name="Heartbeat" msgtype="0" msgcat="admin">
      <field name="TestReqID" required="N"/>
    </message>
    <message name="Logon" msgtype="A" msgcat="admin">
      <field name="EncryptMethod" required="Y"/>
      <field name="HeartBtInt" required="Y"/>
    </message>
    <message name="TradingSessionStatus" msgtype="h" msgcat="app">
      <field name="TradSesReqID" required="N"/>
      <field name="TradingSessionID" required="Y"/>
      <field name="TradingSessionSubID" required="N"/>
      <field name="TradSesMode" required="N"/>
      <field name="UnsolicitedIndicator" required="N"/>
      <field name="TradSesStatus" required="Y"/>
      <field name="TransactTime" required="N"/>
      <field name="Text" required="N"/>
      <field name="FXCMServerTimeZone" required="N"/>
      <field name="FXCMServerTimeZoneName" required="N"/>
      <group name="NoRelatedSym" required="N">
        <component name="Instrument" required="N"/>
        <field name="Currency" required="N"/>
        <field name="RoundLot" required="N"/>
        <field name="FXCMSymID" required="N"/>
        <field name="FXCMSymPrecision" required="N"/>
        <field name="FXCMSymPointSize" required="N"/>
        <field name="FXCMSymInterestBuy" required="N"/>
        <field name="FXCMSymInterestSell" required="N"/>
        <field name="FXCMSymSortOrder" required="N"/>
        <field name="FXCMSubscriptionStatus" required="N"/>
        <field name="FXCMProductID" required="N"/>
        <field name="FXCMCondDistStop" required="N"/>
        <field name="FXCMCondDistLimit" required="N"/>
        <field name="FXCMCondDistEntryStop" required="N"/>
        <field name="FXCMCondDistEntryLimit" required="N"/>
        <field name="FXCMMaxQuantity" required="N"/>
        <field name="FXCMMinQuantity" required="N"/>
        <field name="FXCMTradingStatus" required="N"/>
      </group>
      <group name="FXCMNoParam" required="N">
        <field name="FXCMParamName" required="N"/>
        <field name="FXCMParamValue" required="N"/>
      </group>
    </message>
  </messages>
  <components>
    <component name="Instrument">
      <field name="Symbol" required="N"/>
      <field name="Product" required="N"/>
      <field name="Factor" required="N"/>
      <field name="ContractMultiplier" required="N"/>
    </component>
  </components>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="15" name="Currency" type="CURRENCY"/>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="43" name="PossDupFlag" type="BOOLEAN"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="50" name="SenderSubID" type="STRING"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="57" name="TargetSubID" type="STRING"/>
    <field number="58" name="Text" type="STRING"/>
    <field number="60" name="TransactTime" type="UTCTIMESTAMP"/>
    <field number="98" name="EncryptMethod" type="INT"/>
    <field number="108" name="HeartBtInt" type="INT"/>
    <field number="112" name="TestReqID" type="STRING"/>
    <field number="122" name="OrigSendingTime" type="UTCTIMESTAMP"/>
    <field number="146" name="NoRelatedSym" type="NUMINGROUP"/>
    <field number="228" name="Factor" type="FLOAT"/>
    <field number="231" name="ContractMultiplier" type="FLOAT"/>
    <field number="325" name="UnsolicitedIndicator" type="BOOLEAN"/>
    <field number="335" name="TradSesReqID" type="STRING"/>
    <field number="336" name="TradingSessionID" type="STRING"/>
    <field number="339" name="TradSesMode" type="INT"/>
    <field number="340" name="TradSesStatus" type="INT"/>
    <field number="460" name="Product" type="INT"/>
    <field number="561" name="RoundLot" type="QTY"/>
    <field number="625" name="TradingSessionSubID" type="STRING"/>
    <field number="9000" name="FXCMSymID" type="INT"/>
    <field number="9001" name="FXCMSymPrecision" type="INT"/>
    <field number="9002" name="FXCMSymPointSize" type="FLOAT"/>
    <field number="9003" name="FXCMSymInterestBuy" type="FLOAT"/>
    <field number="9004" name="FXCMSymInterestSell" type="FLOAT"/>
    <field number="9005" name="FXCMSymSortOrder" type="INT"/>
    <field number="9016" name="FXCMNoParam" type="NUMINGROUP"/>
    <field number="9017" name="FXCMParamName" type="STRING"/>
    <field number="9018" name="FXCMParamValue" type="STRING"/>
    <field number="9019" name="FXCMServerTimeZone" type="INT"/>
    <field number="9030" name="FXCMServerTimeZoneName" type="STRING"/>
    <field number="9076" name="FXCMSubscriptionStatus" type="STRING"/>
    <field number="9080" name="FXCMProductID" type="INT"/>
    <field number="9090" name="FXCMCondDistStop" type="FLOAT"/>
    <field number="9091" name="FXCMCondDistLimit" type="FLOAT"/>
    <field number="9092" name="FXCMCondDistEntryStop" type="FLOAT"/>
    <field number="9093" name="FXCMCondDistEntryLimit" type="FLOAT"/>
    <field number="9094" name="FXCMMaxQuantity" type="QTY"/>
    <field number="9095" name="FXCMMinQuantity" type="QTY"/>
    <field number="9096" name="FXCMTradingStatus" type="STRING"/>
  </fields>
</fix>`
